package pgstore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.kenn.io/kata/internal/db"
)

var _ db.IssuePublicationStore = (*Store)(nil)

func (s *Store) publicationTx(ctx context.Context, projectID int64, fn func(*sql.Tx) error) error {
	return s.withSerializableTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, projectID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `SELECT id FROM projects WHERE id=$1 FOR UPDATE`, projectID); err != nil {
			return err
		}
		return fn(tx)
	})
}

func pendingPublicationTx(ctx context.Context, tx db.Transaction, projectUID string) (*db.IssuePublication, error) {
	var raw string
	err := tx.QueryRowContext(ctx, `SELECT value FROM meta WHERE key=$1`, db.IssuePublicationMetaPrefix+projectUID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var p db.IssuePublication
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return nil, fmt.Errorf("invalid publication recovery state: %w", db.ErrImportValidation)
	}
	return &p, nil
}

// PendingIssuePublication reads the recovery record from a read-only snapshot.
func (s *Store) PendingIssuePublication(ctx context.Context, projectID int64) (*db.IssuePublication, error) {
	tx, err := s.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	var projectUID string
	if err := tx.QueryRowContext(ctx, `SELECT uid FROM projects WHERE id=$1`, projectID).Scan(&projectUID); err != nil {
		return nil, err
	}
	result, err := pendingPublicationTx(ctx, tx, projectUID)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return result, nil
}

// ClaimIssuePublication claims the binding and validates the native issue for publishing or recovery.
func (s *Store) ClaimIssuePublication(ctx context.Context, projectID, issueID int64, provider string, now, staleBefore time.Time, recovery bool) (db.IssuePublicationClaim, error) {
	var result db.IssuePublicationClaim
	err := s.publicationTx(ctx, projectID, func(tx *sql.Tx) error {
		result = db.IssuePublicationClaim{}
		b, err := scanIssueSyncBinding(tx.QueryRowContext(ctx, issueSyncBindingSelect+` WHERE b.project_id=$1`, projectID))
		if err != nil {
			return err
		}
		if b.Provider != provider {
			return db.ErrIssueSyncProjectAlreadyBound
		}
		issue, err := scanIssue(ctx, tx.QueryRowContext(ctx, issueSelect+` WHERE i.id=$1 AND i.project_id=$2 AND i.deleted_at IS NULL AND p.deleted_at IS NULL`, issueID, projectID))
		if err != nil {
			return err
		}
		if err := rejectFederationSpokeIssueSyncProject(ctx, tx, projectID); err != nil {
			return err
		}
		pending, err := pendingPublicationTx(ctx, tx, issue.ProjectUID)
		if err != nil {
			return err
		}
		owned, err := publicationOwnershipTx(ctx, tx, projectID, issueID, b.SourceKey)
		if err != nil {
			return err
		}
		if owned != nil {
			result = db.IssuePublicationClaim{Binding: b, Issue: issue, Mapping: owned}
			return nil
		}
		if pending != nil && pending.IssueUID != issue.UID {
			return fmt.Errorf("%w: another issue has an unresolved publication", db.ErrImportValidation)
		}
		if !b.Enabled && !recovery {
			return db.ErrIssueSyncNotEnabled
		}
		if issue.Status != "open" && pending == nil && !recovery {
			return fmt.Errorf("%w: only open issues can be published", db.ErrImportValidation)
		}
		res, err := tx.ExecContext(ctx, `UPDATE issue_sync_status SET sync_started_at=$1, last_attempt_at=$1 WHERE binding_id=$2 AND (sync_started_at IS NULL OR sync_started_at<$3)`, storedTime(now), b.ID, storedTime(staleBefore))
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			return db.ErrIssueSyncAlreadyRunning
		}
		var contentRevision int64
		if err := tx.QueryRowContext(ctx, `SELECT content_revision FROM issues WHERE id=$1`, issueID).Scan(&contentRevision); err != nil {
			return err
		}
		result = db.IssuePublicationClaim{Binding: b, Issue: issue, Pending: pending, Guard: db.IssuePublicationGuard{ContentRevision: contentRevision, Recovery: recovery, ProjectID: projectID, ProjectUID: issue.ProjectUID, IssueID: issueID, IssueUID: issue.UID, BindingID: b.ID, Provider: provider, SourceKey: b.SourceKey, StartedAt: now, BindingUpdatedAt: b.UpdatedAt}}
		return nil
	})
	return result, err
}

func publicationBindingTx(ctx context.Context, tx *sql.Tx, g db.IssuePublicationGuard) (db.IssueSyncBinding, error) {
	projectID := g.ProjectID
	b, err := scanIssueSyncBinding(tx.QueryRowContext(ctx, issueSyncBindingSelect+` WHERE b.project_id=$1`, projectID))
	if err != nil {
		return b, err
	}
	if b.ID != g.BindingID || b.Provider != g.Provider || b.SourceKey != g.SourceKey || !b.UpdatedAt.Equal(g.BindingUpdatedAt) {
		return b, db.ErrIssueSyncBindingChanged
	}
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM issue_sync_status WHERE binding_id=$1 AND sync_started_at=$2`, b.ID, storedTime(g.StartedAt)).Scan(&n); err != nil {
		return b, err
	}
	if n != 1 {
		return b, db.ErrIssueSyncAlreadyRunning
	}
	return b, nil
}

// BeginIssuePublication saves a prepared request under the current claim.
func (s *Store) BeginIssuePublication(ctx context.Context, params db.IssuePublicationBegin) (db.IssuePublication, error) {
	var result db.IssuePublication
	g := params.Guard
	if g.Recovery {
		return result, db.ErrImportValidation
	}
	err := s.publicationTx(ctx, g.ProjectID, func(tx *sql.Tx) error {
		b, err := publicationBindingTx(ctx, tx, g)
		if err != nil {
			return err
		}
		if !b.Enabled {
			return db.ErrIssueSyncNotEnabled
		}
		old, err := pendingPublicationTx(ctx, tx, g.ProjectUID)
		if err != nil {
			return err
		}
		if old != nil {
			return fmt.Errorf("%w: publication already pending", db.ErrImportValidation)
		}
		issue, err := scanIssue(ctx, tx.QueryRowContext(ctx, issueSelect+` WHERE i.id=$1 AND i.project_id=$2 AND i.deleted_at IS NULL AND p.deleted_at IS NULL`, g.IssueID, g.ProjectID))
		if err != nil {
			return err
		}
		var contentRevision int64
		if err := tx.QueryRowContext(ctx, `SELECT content_revision FROM issues WHERE id=$1`, g.IssueID).Scan(&contentRevision); err != nil {
			return err
		}
		if issue.UID != g.IssueUID || issue.ProjectUID != g.ProjectUID || contentRevision != g.ContentRevision || issue.Revision != params.Publication.Revision || issue.Status != "open" {
			return fmt.Errorf("%w: publication issue changed", db.ErrImportValidation)
		}
		owned, err := publicationOwnershipTx(ctx, tx, g.ProjectID, g.IssueID, b.SourceKey)
		if err != nil {
			return err
		}
		if owned != nil {
			return fmt.Errorf("%w: issue already published", db.ErrImportValidation)
		}
		p := params.Publication
		if p.AttemptUID == "" || p.Actor == "" || len(p.Payload) == 0 {
			return db.ErrImportValidation
		}
		p.Version, p.InstanceUID, p.ProjectUID, p.IssueUID = 1, s.InstanceUID(), g.ProjectUID, g.IssueUID
		p.BindingID, p.Provider, p.SourceKey = b.ID, b.Provider, b.SourceKey
		configDigest := sha256.Sum256(b.Config)
		p.ConfigDigest = hex.EncodeToString(configDigest[:])
		payloadDigest := sha256.Sum256(p.Payload)
		p.PayloadDigest = hex.EncodeToString(payloadDigest[:])
		p.CreatedAt, p.UpdatedAt, p.Phase = g.StartedAt, g.StartedAt, "prepared"
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(id),0) FROM events WHERE issue_id=$1`, g.IssueID).Scan(&p.SnapshotEventID); err != nil {
			return err
		}
		raw, err := json.Marshal(p)
		if err != nil {
			return err
		}
		if len(raw) > 2*1024*1024-128 {
			return fmt.Errorf("%w: publication exceeds 2 MiB", db.ErrImportValidation)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO meta(key,value) VALUES($1,$2)`, db.IssuePublicationMetaPrefix+g.ProjectUID, string(raw)); err != nil {
			return err
		}
		result = p
		return nil
	})
	return result, err
}

// DispatchIssuePublication durably marks the attempt before its single remote mutation.
func (s *Store) DispatchIssuePublication(ctx context.Context, g db.IssuePublicationGuard, attemptUID string) error {
	if g.Recovery {
		return fmt.Errorf("%w: recovery cannot create", db.ErrImportValidation)
	}
	return s.publicationTx(ctx, g.ProjectID, func(tx *sql.Tx) error {
		b, err := publicationBindingTx(ctx, tx, g)
		if err != nil {
			return err
		}
		if !b.Enabled {
			return db.ErrIssueSyncNotEnabled
		}
		p, err := pendingPublicationTx(ctx, tx, g.ProjectUID)
		if err != nil {
			return err
		}
		if p == nil || p.AttemptUID != attemptUID || p.Phase != "prepared" || p.IssueUID != g.IssueUID {
			return fmt.Errorf("%w: publication cannot dispatch", db.ErrImportValidation)
		}
		p.Phase, p.UpdatedAt = "dispatching", time.Now().UTC()
		raw, err := json.Marshal(p)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE meta SET value=$1 WHERE key=$2`, string(raw), db.IssuePublicationMetaPrefix+g.ProjectUID)
		return err
	})
}

// publicationOwnershipTx rejects competing issue-source and external-root owners.
func publicationOwnershipTx(ctx context.Context, tx *sql.Tx, projectID, issueID int64, source string) (*db.ImportMapping, error) {
	var root int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM external_root_bindings WHERE issue_id=$1 AND active=1`, issueID).Scan(&root); err != nil {
		return nil, err
	}
	if root != 0 {
		return nil, db.ErrExternalRootIssueSyncConflict
	}
	rows, err := tx.QueryContext(ctx, `SELECT id FROM import_mappings WHERE project_id=$1 AND issue_id=$2 AND object_type='issue' ORDER BY id LIMIT 2`, projectID, issueID)
	if err != nil {
		return nil, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, nil
	}
	m, err := scanImportMapping(tx.QueryRowContext(ctx, importMappingSelect+` WHERE id=$1`, ids[0]))
	if err != nil {
		return nil, err
	}
	if len(ids) > 1 || m.Source != source {
		return nil, fmt.Errorf("%w: issue already has another source owner", db.ErrImportValidation)
	}
	return &m, nil
}

// RecordIssuePublicationCreated retains a confirmed remote identity for attachment or recovery.
func (s *Store) RecordIssuePublicationCreated(ctx context.Context, g db.IssuePublicationGuard, attemptUID string, remote db.IssuePublicationRemote) error {
	return s.publicationTx(ctx, g.ProjectID, func(tx *sql.Tx) error {
		p, err := pendingPublicationTx(ctx, tx, g.ProjectUID)
		if err != nil {
			return err
		}
		if p == nil || p.AttemptUID != attemptUID || p.IssueUID != g.IssueUID || p.BindingID != g.BindingID || (p.Phase != "dispatching" && p.Phase != "created") {
			return db.ErrImportValidation
		}
		if remote.ExternalID == "" {
			return db.ErrImportValidation
		}
		if p.Remote != nil && p.Remote.ExternalID != remote.ExternalID {
			return db.ErrImportValidation
		}
		// A confirmed identity replaces the one-send request. Recovery reads the
		// remote object; it never resends this payload. Keep its digest for the
		// attempt record while making room for the confirmed recovery metadata.
		p.Payload = nil
		p.Remote, p.Phase, p.UpdatedAt = &remote, "created", time.Now().UTC()
		raw, err := json.Marshal(p)
		if err != nil {
			return err
		}
		if len(raw) > 2*1024*1024-128 {
			return db.ErrImportValidation
		}
		_, err = tx.ExecContext(ctx, `UPDATE meta SET value=$1 WHERE key=$2`, string(raw), db.IssuePublicationMetaPrefix+g.ProjectUID)
		return err
	})
}

// AttachIssuePublication atomically attaches the verified identity to the original issue.
func (s *Store) AttachIssuePublication(ctx context.Context, params db.IssuePublicationAttach) (db.ImportMapping, []db.Event, error) {
	var mapping db.ImportMapping
	g := params.Guard
	err := s.publicationTx(ctx, g.ProjectID, func(tx *sql.Tx) error {
		b, err := publicationBindingTx(ctx, tx, g)
		if err != nil {
			return err
		}
		p, err := pendingPublicationTx(ctx, tx, g.ProjectUID)
		if err != nil {
			return err
		}
		if p == nil {
			if !params.RestoreAdoption || b.Enabled {
				return db.ErrImportValidation
			}
		} else {
			if p.AttemptUID != params.AttemptUID || p.IssueUID != g.IssueUID || p.BindingID != b.ID || p.SourceKey != b.SourceKey {
				return db.ErrImportValidation
			}
			digest := sha256.Sum256(b.Config)
			if !g.Recovery && p.ConfigDigest != hex.EncodeToString(digest[:]) {
				return db.ErrIssueSyncBindingChanged
			}
			if p.Remote != nil && p.Remote.ExternalID != params.Remote.ExternalID {
				return db.ErrImportValidation
			}
		}
		issue, err := scanIssue(ctx, tx.QueryRowContext(ctx, issueSelect+` WHERE i.id=$1 AND i.project_id=$2 AND i.deleted_at IS NULL AND p.deleted_at IS NULL`, g.IssueID, g.ProjectID))
		if err != nil {
			return err
		}
		if issue.UID != g.IssueUID || issue.ProjectUID != g.ProjectUID {
			return db.ErrImportValidation
		}
		if err := rejectFederationSpokeIssueSyncProject(ctx, tx, g.ProjectID); err != nil {
			return err
		}
		owned, err := publicationOwnershipTx(ctx, tx, g.ProjectID, g.IssueID, b.SourceKey)
		if err != nil {
			return err
		}
		remote := params.Remote
		if remote.ExternalID == "" || (remote.Status != "open" && remote.Status != "closed") {
			return db.ErrImportValidation
		}
		if owned != nil && owned.ExternalID != remote.ExternalID {
			return db.ErrImportValidation
		}
		var sourceAt any
		if remote.SourceUpdatedAt != nil {
			sourceAt = remote.SourceUpdatedAt.UTC().Format(time.RFC3339Nano)
		}
		for _, alias := range remote.LegacyExternalIDs {
			if alias == "" || alias == remote.ExternalID {
				continue
			}
			legacy, lookupErr := importMappingBySourceTx(ctx, tx, g.ProjectID, b.SourceKey, "issue", alias)
			if errors.Is(lookupErr, db.ErrNotFound) {
				continue
			}
			if lookupErr != nil {
				return lookupErr
			}
			if legacy.IssueID == nil || *legacy.IssueID != g.IssueID {
				return fmt.Errorf("%w: remote object already maps to another issue", db.ErrImportValidation)
			}
		}
		var found bool
		mapping, found, err = adoptImportMappingTx(ctx, tx, g.ProjectID, b.SourceKey, "issue", remote.ExternalID, remote.LegacyExternalIDs)
		if err != nil {
			return err
		}
		if !found {
			if _, err = tx.ExecContext(ctx, `INSERT INTO import_mappings(source,external_id,object_type,project_id,issue_id,source_updated_at) VALUES($1,$2,'issue',$3,$4,$5) ON CONFLICT(source,external_id,object_type,project_id) DO NOTHING`, b.SourceKey, remote.ExternalID, g.ProjectID, g.IssueID, sourceAt); err != nil {
				return err
			}
			mapping, err = scanImportMapping(tx.QueryRowContext(ctx, importMappingSelect+` WHERE project_id=$1 AND source=$2 AND object_type='issue' AND external_id=$3`, g.ProjectID, b.SourceKey, remote.ExternalID))
			if err != nil {
				return err
			}
		}
		if mapping.IssueID == nil || *mapping.IssueID != g.IssueID {
			return fmt.Errorf("%w: remote object already maps to another issue", db.ErrImportValidation)
		}
		var observedAt any
		var rawStatus any
		if !remote.Version.IsZero() {
			observedAt = remote.Version.UTC().Format(time.RFC3339Nano)
			rawStatus = remote.RawStatus
		}
		var pendingEvent any
		mode, err := db.IssueStatusMode(b.Config)
		if err != nil {
			return err
		}
		if p != nil && mode == "two-way" && p.PendingEventUID != "" {
			pendingEvent = p.PendingEventUID
		}
		// TickTick imports use repeated raw observations to preserve local state.
		// Do not acknowledge a remote transition that attachment has not applied,
		// unless an intervening two-way intent owns the next status delivery.
		if b.Provider == "ticktick" && remote.Status != issue.Status && pendingEvent == nil {
			rawStatus, observedAt = nil, nil
		}
		if _, err = tx.ExecContext(ctx, `UPDATE import_mappings SET observed_status=$1,observed_status_at=$2,remote_locator=NULLIF($3,''),pending_event_uid=$4 WHERE id=$5`, rawStatus, observedAt, remote.Locator, pendingEvent, mapping.ID); err != nil {
			return err
		}
		if p != nil {
			_, err = tx.ExecContext(ctx, `DELETE FROM meta WHERE key=$1`, db.IssuePublicationMetaPrefix+g.ProjectUID)
		}
		return err
	})
	return mapping, nil, err
}

// ReleaseIssuePublication releases the active claim, optionally retaining its recovery record.
func (s *Store) ReleaseIssuePublication(ctx context.Context, g db.IssuePublicationGuard, retain bool) error {
	if retain {
		return nil
	}
	return s.publicationTx(ctx, g.ProjectID, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE issue_sync_status SET sync_started_at=NULL WHERE binding_id=$1 AND sync_started_at=$2`, g.BindingID, storedTime(g.StartedAt))
		return err
	})
}

func ensurePublicationImportAllowedTx(ctx context.Context, tx *sql.Tx, projectUID, source string) error {
	p, err := pendingPublicationTx(ctx, tx, projectUID)
	if err != nil {
		return err
	}
	if p != nil && p.SourceKey == source {
		return fmt.Errorf("%w: unresolved publication %s blocks content import", db.ErrImportValidation, p.AttemptUID)
	}
	return nil
}

// ResolveIssuePublication records a local administrative audit event. The
// private publication journal and its resolutions are not federation authority.
func (s *Store) ResolveIssuePublication(ctx context.Context, params db.IssuePublicationResolve) ([]db.Event, error) {
	if strings.TrimSpace(params.Message) == "" || strings.TrimSpace(params.Actor) == "" || params.AttemptUID == "" {
		return nil, db.ErrImportValidation
	}
	g := params.Guard
	var events []db.Event
	err := s.publicationTx(ctx, g.ProjectID, func(tx *sql.Tx) error {
		events = nil
		if _, err := publicationBindingTx(ctx, tx, g); err != nil {
			return err
		}
		pending, err := pendingPublicationTx(ctx, tx, g.ProjectUID)
		if err != nil {
			return err
		}
		if pending == nil || pending.AttemptUID != params.AttemptUID || pending.IssueUID != g.IssueUID || pending.BindingID != g.BindingID {
			return db.ErrImportValidation
		}
		payload, err := json.Marshal(struct {
			AttemptUID string `json:"attempt_uid"`
			Message    string `json:"message"`
			Provider   string `json:"provider"`
			Resolution string `json:"resolution"`
		}{params.AttemptUID, params.Message, pending.Provider, "not_created"})
		if err != nil {
			return err
		}
		event, err := s.insertEventTx(ctx, tx, eventInsert{ProjectID: g.ProjectID, ProjectUID: g.ProjectUID, IssueID: &g.IssueID, IssueUID: &g.IssueUID, Type: "issue.publication_resolved", Actor: params.Actor, Payload: string(payload), SkipRelay: true})
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM meta WHERE key=$1`, db.IssuePublicationMetaPrefix+g.ProjectUID); err != nil {
			return err
		}
		events = []db.Event{event}
		return nil
	})
	return events, err
}

func pendingPublicationError(ctx context.Context, tx db.Transaction, p *db.IssuePublication) error {
	issueRef := p.IssueUID
	var projectName, shortID string
	err := tx.QueryRowContext(ctx, `SELECT pr.name, i.short_id FROM projects pr JOIN issues i ON i.project_id=pr.id WHERE pr.uid=$1 AND i.uid=$2`, p.ProjectUID, p.IssueUID).Scan(&projectName, &shortID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if projectName != "" && shortID != "" {
		issueRef = projectName + "#" + shortID
	}
	return &db.IssuePublicationPendingError{AttemptUID: p.AttemptUID, Provider: p.Provider, IssueUID: p.IssueUID, IssueRef: issueRef, Phase: p.Phase}
}

// ensureNoPendingIssuePublicationTx preserves recovery identities. issueID zero
// protects the whole project; otherwise unrelated issues remain writable.
func ensureNoPendingIssuePublicationTx(ctx context.Context, tx db.Transaction, projectID, issueID int64) error {
	var projectUID string
	err := tx.QueryRowContext(ctx, `SELECT uid FROM projects WHERE id=$1 FOR UPDATE`, projectID).Scan(&projectUID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	p, err := pendingPublicationTx(ctx, tx, projectUID)
	if err != nil || p == nil {
		return err
	}
	if issueID != 0 {
		var issueUID string
		err = tx.QueryRowContext(ctx, `SELECT uid FROM issues WHERE id=$1 AND project_id=$2`, issueID, projectID).Scan(&issueUID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if issueUID != p.IssueUID {
			return nil
		}
	}
	return pendingPublicationError(ctx, tx, p)
}

func ensurePublicationMappingAllowedTx(ctx context.Context, tx *sql.Tx, params db.ImportMappingParams) error {
	if params.ObjectType != "issue" {
		return nil
	}
	if params.IssueID != nil {
		if err := ensureNoPendingIssuePublicationTx(ctx, tx, params.ProjectID, *params.IssueID); err != nil {
			return err
		}
	}
	var projectUID string
	err := tx.QueryRowContext(ctx, `SELECT uid FROM projects WHERE id=$1 FOR UPDATE`, params.ProjectID).Scan(&projectUID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	p, err := pendingPublicationTx(ctx, tx, projectUID)
	if err != nil {
		return err
	}
	if p != nil && p.SourceKey == params.Source && p.Remote != nil && p.Remote.ExternalID == params.ExternalID {
		return pendingPublicationError(ctx, tx, p)
	}
	return nil
}

// Full replay has already acquired the backend's exclusive restore lock here.
// It cannot replace the live identities while leaving an unresolved operation.
func ensurePublicationReplayAllowedTx(ctx context.Context, tx db.Transaction) error {
	var raw string
	err := tx.QueryRowContext(ctx, `SELECT value FROM meta WHERE substr(key,1,length($1))=$1 ORDER BY key LIMIT 1`, db.IssuePublicationMetaPrefix).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	var p db.IssuePublication
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return fmt.Errorf("%w: invalid publication recovery state blocks restore", db.ErrImportValidation)
	}
	return pendingPublicationError(ctx, tx, &p)
}

// Federation applies the same lifecycle rule without blocking ordinary edits.
func ensurePublicationProjectionTx(ctx context.Context, tx *sql.Tx, projectID int64, projection db.FoldProjection) error {
	var projectUID string
	if err := tx.QueryRowContext(ctx, `SELECT uid FROM projects WHERE id=$1`, projectID).Scan(&projectUID); err != nil {
		return err
	}
	pending, err := pendingPublicationTx(ctx, tx, projectUID)
	if err != nil || pending == nil {
		return err
	}
	issue, ok := projection.Issues[pending.IssueUID]
	if !ok || issue.DeletedAt != nil {
		return pendingPublicationError(ctx, tx, pending)
	}
	return nil
}

// A one-way transition revokes publication intent in the binding update's
// transaction, just as it revokes pending intent on existing import mappings.
func clearPublicationStatusIntentTx(ctx context.Context, tx *sql.Tx, binding db.IssueSyncBinding) error {
	var projectUID string
	if err := tx.QueryRowContext(ctx, `SELECT uid FROM projects WHERE id=$1`, binding.ProjectID).Scan(&projectUID); err != nil {
		return err
	}
	pending, err := pendingPublicationTx(ctx, tx, projectUID)
	if err != nil || pending == nil {
		return err
	}
	if pending.SourceKey != binding.SourceKey || pending.PendingEventUID == "" {
		return nil
	}
	pending.PendingEventUID = ""
	return savePublicationIntentTx(ctx, tx, pending)
}

// The pending publication has no mapping yet, so preserve its explicit status
// intent in the bounded journal until attachment creates that mapping.
func publicationStatusIntentTx(ctx context.Context, tx *sql.Tx, event db.Event) error {
	p, err := pendingPublicationTx(ctx, tx, event.ProjectUID)
	if err != nil || p == nil || event.IssueUID == nil || p.IssueUID != *event.IssueUID {
		return err
	}
	b, err := publicationStatusBindingTx(ctx, tx, event.ProjectID)
	if err != nil {
		return err
	}
	mode, err := db.IssueStatusMode(b.Config)
	if err != nil || mode != "two-way" {
		return err
	}
	p.PendingEventUID = event.UID
	return savePublicationIntentTx(ctx, tx, p)
}
func publicationStatusBindingTx(ctx context.Context, tx *sql.Tx, projectID int64) (db.IssueSyncBinding, error) {
	return scanIssueSyncBinding(tx.QueryRowContext(ctx, issueSyncBindingSelect+` WHERE b.project_id=$1`, projectID))
}
func savePublicationIntentTx(ctx context.Context, tx *sql.Tx, p *db.IssuePublication) error {
	raw, err := json.Marshal(p)
	if err != nil {
		return err
	}
	if len(raw) > 2*1024*1024 {
		return db.ErrImportValidation
	}
	_, err = tx.ExecContext(ctx, `UPDATE meta SET value=$1 WHERE key=$2`, string(raw), db.IssuePublicationMetaPrefix+p.ProjectUID)
	return err
}
func projectPublicationTx(ctx context.Context, tx *sql.Tx, projectID int64) (*db.IssuePublication, error) {
	var projectUID string
	if err := tx.QueryRowContext(ctx, `SELECT uid FROM projects WHERE id=$1`, projectID).Scan(&projectUID); err != nil {
		return nil, err
	}
	return pendingPublicationTx(ctx, tx, projectUID)
}

// A move must preserve any destination identity reserved for recovery.
func ensurePublicationMoveAllowedTx(ctx context.Context, tx *sql.Tx, issueID, sourceProjectID, targetProjectID int64) error {
	rows, err := tx.QueryContext(ctx, `SELECT source, external_id FROM import_mappings WHERE issue_id=$1 AND project_id=$2 AND object_type='issue'`, issueID, sourceProjectID)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	var mappings []db.ImportMappingParams
	for rows.Next() {
		m := db.ImportMappingParams{ProjectID: targetProjectID, ObjectType: "issue", IssueID: &issueID}
		if err := rows.Scan(&m.Source, &m.ExternalID); err != nil {
			return err
		}
		mappings = append(mappings, m)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, m := range mappings {
		if err := ensurePublicationMappingAllowedTx(ctx, tx, m); err != nil {
			return err
		}
	}
	return nil
}
