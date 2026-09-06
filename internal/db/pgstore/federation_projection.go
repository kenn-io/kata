package pgstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/shortid"
)

// EnableProjectFederation writes a replay baseline and binds a local project as a hub.
func (s *Store) EnableProjectFederation(
	ctx context.Context,
	projectID int64,
	actor string,
) (db.FederationBinding, error) {
	var output db.FederationBinding
	err := s.withSerializableTx(ctx, func(tx *sql.Tx) error {
		output = db.FederationBinding{}
		var err error
		output, err = s.enableProjectFederationTx(ctx, tx, projectID, actor)
		return err
	})
	return output, err
}

func (s *Store) enableProjectFederationTx(
	ctx context.Context,
	tx *sql.Tx,
	projectID int64,
	actor string,
) (db.FederationBinding, error) {
	project, err := scanProject(tx.QueryRowContext(ctx,
		projectSelect+` WHERE id=$1 AND deleted_at IS NULL FOR UPDATE`, projectID))
	if err != nil {
		return db.FederationBinding{}, err
	}
	existing, err := scanFederationBinding(tx.QueryRowContext(ctx,
		federationBindingSelect+` WHERE project_id=$1 FOR UPDATE`, projectID))
	if err == nil {
		if existing.Role != db.FederationRoleHub {
			return db.FederationBinding{}, fmt.Errorf("project %d already has %q federation binding", projectID, existing.Role)
		}
		if existing.Enabled {
			projectIDs, err := federationBindingGroupProjectIDs(ctx, tx, existing)
			if err != nil {
				return db.FederationBinding{}, err
			}
			m := newFederatedLinkGroupMaterialization(projectIDs, 0)
			if err := reconcileFederatedLinkGroup(ctx, tx, m, nil); err != nil {
				return db.FederationBinding{}, err
			}
		}
		return existing, nil
	}
	if !errors.Is(err, db.ErrNotFound) {
		return db.FederationBinding{}, err
	}
	enableEvent, err := s.insertFederationBaselineEventsTx(ctx, tx, project, actor)
	if err != nil {
		return db.FederationBinding{}, err
	}
	pullCursor := max(enableEvent.ID-1, 0)
	if _, err := tx.ExecContext(ctx, `INSERT INTO federation_bindings(
project_id,role,hub_url,hub_project_id,hub_project_uid,
replay_horizon_event_id,pull_cursor_event_id,enabled
) VALUES($1,$2,'',0,$3,$4,$5,1)`, project.ID, string(db.FederationRoleHub),
		project.UID, enableEvent.ID, pullCursor); err != nil {
		return db.FederationBinding{}, mapSQLError(err, nil)
	}
	binding, err := scanFederationBinding(tx.QueryRowContext(ctx,
		federationBindingSelect+` WHERE project_id=$1`, project.ID))
	if err != nil {
		return db.FederationBinding{}, err
	}
	if err := reconcileFederationBindingTransitionLinks(ctx, tx, nil, binding); err != nil {
		return db.FederationBinding{}, err
	}
	return binding, nil
}

// RefreshProjectFederationBaseline writes a new baseline for an enabled hub binding.
func (s *Store) RefreshProjectFederationBaseline(
	ctx context.Context,
	projectID int64,
	actor string,
) (db.FederationBinding, bool, error) {
	var output db.FederationBinding
	changed := false
	err := s.withSerializableTx(ctx, func(tx *sql.Tx) error {
		output = db.FederationBinding{}
		changed = false
		project, err := scanProject(tx.QueryRowContext(ctx,
			projectSelect+` WHERE id=$1 AND deleted_at IS NULL FOR UPDATE`, projectID))
		if err != nil {
			return err
		}
		existing, err := scanFederationBinding(tx.QueryRowContext(ctx,
			federationBindingSelect+` WHERE project_id=$1 FOR UPDATE`, projectID))
		if errors.Is(err, db.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		output = existing
		if existing.Role != db.FederationRoleHub || !existing.Enabled {
			return nil
		}
		enableEvent, err := s.insertFederationBaselineEventsTx(ctx, tx, project, actor)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE federation_bindings SET
replay_horizon_event_id=$1,pull_cursor_event_id=$2,
updated_at=to_char(now() AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.MS"Z"')
WHERE project_id=$3`, enableEvent.ID, enableEvent.ID-1, project.ID); err != nil {
			return mapSQLError(err, nil)
		}
		output, err = scanFederationBinding(tx.QueryRowContext(ctx,
			federationBindingSelect+` WHERE project_id=$1`, projectID))
		changed = err == nil
		return err
	})
	return output, changed, err
}

// LeaveFederationReplica detaches a spoke while preserving its materialized project state.
func (s *Store) LeaveFederationReplica(ctx context.Context, projectID int64) (db.LeaveFederationResult, error) {
	var output db.LeaveFederationResult
	err := s.withSerializableTx(ctx, func(tx *sql.Tx) error {
		output = db.LeaveFederationResult{}
		project, err := scanProject(tx.QueryRowContext(ctx,
			projectSelect+` WHERE id=$1 FOR UPDATE`, projectID))
		if err != nil {
			return err
		}
		output = db.LeaveFederationResult{ProjectID: project.ID, ProjectUID: project.UID}
		binding, err := scanFederationBinding(tx.QueryRowContext(ctx,
			federationBindingSelect+` WHERE project_id=$1 FOR UPDATE`, projectID))
		if errors.Is(err, db.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if binding.Role != db.FederationRoleSpoke {
			return db.ErrFederationNotSpoke
		}
		for _, query := range []string{
			`DELETE FROM federation_quarantine WHERE project_id=$1`,
			`DELETE FROM federation_sync_status WHERE project_id=$1`,
			`DELETE FROM federation_bindings WHERE project_id=$1`,
			`DELETE FROM pending_claim_requests WHERE project_id=$1`,
			`DELETE FROM issue_claims WHERE project_id=$1`,
		} {
			if _, err := tx.ExecContext(ctx, query, projectID); err != nil {
				return mapSQLError(err, nil)
			}
		}
		if err := reconcileFederationBindingTransitionLinks(
			ctx, tx, &binding, db.FederationBinding{},
		); err != nil {
			return err
		}
		output.Role = db.FederationRoleSpoke
		return nil
	})
	return output, err
}

func (s *Store) insertFederationBaselineEventsTx(
	ctx context.Context,
	tx *sql.Tx,
	project db.Project,
	actor string,
) (db.Event, error) {
	payload, err := json.Marshal(struct {
		ProjectUID  string      `json:"project_uid"`
		ProjectName string      `json:"project_name"`
		Metadata    db.JSONBlob `json:"metadata"`
	}{ProjectUID: project.UID, ProjectName: project.Name, Metadata: project.Metadata})
	if err != nil {
		return db.Event{}, fmt.Errorf("marshal federation enable payload: %w", err)
	}
	enableEvent, err := s.insertEventTx(ctx, tx, eventInsert{
		ProjectID: project.ID, ProjectUID: project.UID, ProjectName: project.Name,
		Type: "project.federation_enabled", Actor: actor, Payload: string(payload),
	})
	if err != nil {
		return db.Event{}, err
	}
	boundary := db.EventHLCTimestamp{
		PhysicalMS: enableEvent.HLCPhysicalMS,
		Counter:    enableEvent.HLCCounter,
	}
	createdAt := formatStoredTime(enableEvent.CreatedAt)
	issues, err := federationIssuesForSnapshot(ctx, tx, project.ID)
	if err != nil {
		return db.Event{}, err
	}
	for _, issue := range issues {
		snapshot, err := federationIssueSnapshotPayload(ctx, tx, issue)
		if err != nil {
			return db.Event{}, err
		}
		issueUID := issue.UID
		if _, err := s.insertEventTx(ctx, tx, eventInsert{
			ProjectID: project.ID, ProjectUID: project.UID, ProjectName: project.Name,
			IssueID: &issue.ID, IssueUID: &issueUID, Type: "issue.snapshot",
			Actor: actor, Payload: snapshot, HLC: &boundary, CreatedAt: createdAt,
		}); err != nil {
			return db.Event{}, err
		}
	}
	return enableEvent, nil
}

func federationIssuesForSnapshot(ctx context.Context, tx *sql.Tx, projectID int64) ([]db.Issue, error) {
	rows, err := tx.QueryContext(ctx, issueSelect+` WHERE i.project_id=$1 ORDER BY i.id ASC`, projectID)
	if err != nil {
		return nil, mapSQLError(err, nil)
	}
	defer func() { _ = rows.Close() }()
	var output []db.Issue
	for rows.Next() {
		issue, err := scanIssue(rows)
		if err != nil {
			return nil, err
		}
		output = append(output, issue)
	}
	return output, mapSQLError(rows.Err(), nil)
}

func federationIssueSnapshotPayload(ctx context.Context, tx *sql.Tx, issue db.Issue) (string, error) {
	labels, err := federationIssueLabels(ctx, tx, issue.ID)
	if err != nil {
		return "", err
	}
	links, err := federationIssueLinks(ctx, tx, issue.ID)
	if err != nil {
		return "", err
	}
	comments, err := federationIssueComments(ctx, tx, issue.ID)
	if err != nil {
		return "", err
	}
	recurrenceUID, err := federationIssueRecurrenceUID(ctx, tx, issue.RecurrenceID)
	if err != nil {
		return "", err
	}
	occurrenceKey := ""
	if issue.OccurrenceKey != nil {
		occurrenceKey = *issue.OccurrenceKey
	}
	payload, err := json.Marshal(issueCreatedPayload{
		UID: issue.UID, ShortID: issue.ShortID, Title: issue.Title, Body: issue.Body,
		Author: issue.Author, Owner: issue.Owner, Priority: issue.Priority, Status: issue.Status,
		ClosedReason: issue.ClosedReason, ClosedAt: optionalStoredTime(issue.ClosedAt),
		DeletedAt: optionalStoredTime(issue.DeletedAt), Metadata: json.RawMessage(issue.Metadata),
		Labels: labels, Links: links, Comments: comments,
		CreatedAt: formatStoredTime(issue.CreatedAt), UpdatedAt: formatStoredTime(issue.UpdatedAt),
		Revision: issue.Revision, RecurrenceUID: recurrenceUID, OccurrenceKey: occurrenceKey,
	})
	if err != nil {
		return "", fmt.Errorf("marshal federation issue snapshot: %w", err)
	}
	return string(payload), nil
}

func optionalStoredTime(value *time.Time) *string {
	if value == nil {
		return nil
	}
	formatted := formatStoredTime(*value)
	return &formatted
}

func federationIssueLabels(ctx context.Context, tx *sql.Tx, issueID int64) ([]string, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT label FROM issue_labels WHERE issue_id=$1 ORDER BY label ASC`, issueID)
	if err != nil {
		return nil, mapSQLError(err, nil)
	}
	defer func() { _ = rows.Close() }()
	var output []string
	for rows.Next() {
		var label string
		if err := rows.Scan(&label); err != nil {
			return nil, mapSQLError(err, nil)
		}
		output = append(output, label)
	}
	return output, mapSQLError(rows.Err(), nil)
}

func federationIssueLinks(ctx context.Context, tx *sql.Tx, issueID int64) ([]createdLink, error) {
	rows, err := tx.QueryContext(ctx, `SELECT l.type,peer.short_id,peer.uid,FALSE,l.author,l.created_at
FROM links l JOIN issues peer ON peer.id=l.to_issue_id WHERE l.from_issue_id=$1
UNION ALL
SELECT l.type,peer.short_id,peer.uid,CASE WHEN l.type='related' THEN FALSE ELSE TRUE END,l.author,l.created_at
FROM links l JOIN issues peer ON peer.id=l.from_issue_id WHERE l.to_issue_id=$1
AND peer.project_id<>(SELECT project_id FROM issues WHERE id=$1)
ORDER BY 1 ASC,3 ASC,4 ASC`, issueID)
	if err != nil {
		return nil, mapSQLError(err, nil)
	}
	defer func() { _ = rows.Close() }()
	var output []createdLink
	for rows.Next() {
		var link createdLink
		if err := rows.Scan(&link.Type, &link.ToShortID, &link.ToIssueUID, &link.Incoming, &link.Author, &link.CreatedAt); err != nil {
			return nil, mapSQLError(err, nil)
		}
		output = append(output, link)
	}
	return output, mapSQLError(rows.Err(), nil)
}

func federationIssueComments(
	ctx context.Context,
	tx *sql.Tx,
	issueID int64,
) ([]issueSnapshotComment, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT uid,author,body,created_at FROM comments WHERE issue_id=$1 ORDER BY id ASC`, issueID)
	if err != nil {
		return nil, mapSQLError(err, nil)
	}
	defer func() { _ = rows.Close() }()
	var output []issueSnapshotComment
	for rows.Next() {
		var comment issueSnapshotComment
		if err := rows.Scan(&comment.CommentUID, &comment.Author, &comment.Body, &comment.CreatedAt); err != nil {
			return nil, mapSQLError(err, nil)
		}
		output = append(output, comment)
	}
	return output, mapSQLError(rows.Err(), nil)
}

func federationIssueRecurrenceUID(ctx context.Context, tx *sql.Tx, recurrenceID *int64) (string, error) {
	if recurrenceID == nil {
		return "", nil
	}
	var recurrenceUID string
	if err := tx.QueryRowContext(ctx,
		`SELECT uid FROM recurrences WHERE id=$1`, *recurrenceID).Scan(&recurrenceUID); err != nil {
		return "", mapSQLError(err, nil)
	}
	return recurrenceUID, nil
}

// MaterializeFederatedProject rebuilds one project's read model from portable events.
func (s *Store) MaterializeFederatedProject(ctx context.Context, projectID int64) error {
	return s.withSerializableTx(ctx, func(tx *sql.Tx) error {
		return s.materializeFederatedProjectTx(ctx, tx, projectID, true)
	})
}

// federatedMaterialization names everything one materialization pass operates
// on. Before it existed, five reconcile steps each rediscovered these facts
// from SQL — and because federationBindingGroupProjectIDs returns the current
// project as a member of its own group, the current project's whole event log
// was read and folded twice per pass.
//
// events caches RAW events, never a merged db.FoldProjection: db.FoldEvents
// resolves conflicts across its entire input by HLC order, so folding per
// project and merging is not equivalent to folding the concatenation. The
// current project's cache entry is its complete stream; group-only entries
// retain the existing link-affecting event filter because links are their sole
// consumer.
type federatedMaterialization struct {
	projectID       int64
	binding         db.FederationBinding
	groupProjectIDs []int64
	events          map[int64][]db.FoldEvent
	existingIssues  map[string]federatedIssueRow
	observeFold     func(projectID int64)
}

func (m *federatedMaterialization) foldEvents(
	ctx context.Context,
	tx *sql.Tx,
	projectID int64,
) ([]db.FoldEvent, error) {
	if cached, ok := m.events[projectID]; ok {
		return cached, nil
	}
	if m.observeFold != nil {
		m.observeFold(projectID)
	}
	events, err := federationFoldEvents(ctx, tx, projectID)
	if err != nil {
		return nil, err
	}
	m.events[projectID] = events
	return events, nil
}

// materializeFederatedProjectTx rebuilds the project's projection from its
// event log. reconcileLinks controls the binding-group link pass, whose cost
// scales with every federated project in the group rather than with this
// project; callers that know their events cannot affect link state pass false.
func (s *Store) materializeFederatedProjectTx(
	ctx context.Context,
	tx *sql.Tx,
	projectID int64,
	reconcileLinks bool,
) error {
	binding, err := scanFederationBinding(tx.QueryRowContext(ctx,
		federationBindingSelect+` WHERE project_id=$1 FOR UPDATE`, projectID))
	if err != nil {
		return err
	}
	m := &federatedMaterialization{
		projectID:   projectID,
		binding:     binding,
		events:      map[int64][]db.FoldEvent{},
		observeFold: s.federationFoldObserver,
	}
	events, err := m.foldEvents(ctx, tx, projectID)
	if err != nil {
		return err
	}
	projection := db.FoldEvents(events)
	m.existingIssues, err = federatedIssueRowsByUID(ctx, tx, projectID)
	if err != nil {
		return err
	}
	issueIDs, err := s.reconcileFederatedIssues(ctx, tx, m, projection)
	if err != nil {
		return err
	}
	if err := reconcileFederatedComments(ctx, tx, projectID, issueIDs, projection); err != nil {
		return err
	}
	if err := reconcileFederatedLabels(ctx, tx, projectID, issueIDs, projection); err != nil {
		return err
	}
	if reconcileLinks {
		// federationBindingGroupProjectIDs takes FOR UPDATE on the group's
		// binding rows. Its position here — after this project's events are read
		// — is the transaction's lock-acquisition order under concurrent
		// materialization of two projects in one group. Do not hoist it into the
		// struct's construction above.
		m.groupProjectIDs, err = federationBindingGroupProjectIDs(ctx, tx, m.binding)
		if err != nil {
			return err
		}
		if err := reconcileFederatedLinkGroup(ctx, tx, m, issueIDs); err != nil {
			return err
		}
	}
	if err := pruneFederatedIssues(ctx, tx, m, issueIDs); err != nil {
		return err
	}
	if metadata := projection.ProjectMetadata[m.binding.HubProjectUID]; len(metadata) > 0 {
		_, err := tx.ExecContext(ctx, `UPDATE projects SET metadata=$1,revision=revision+1
WHERE id=$2 AND metadata IS DISTINCT FROM $1`, string(metadata), projectID)
		return mapSQLError(err, nil)
	}
	return nil
}

type federatedIssueRow struct {
	id      int64
	shortID string
	title   string
	body    string
	deleted bool
}

func (s *Store) reconcileFederatedIssues(
	ctx context.Context,
	tx *sql.Tx,
	m *federatedMaterialization,
	projection db.FoldProjection,
) (map[string]int64, error) {
	existing := m.existingIssues
	projectID := m.projectID
	output := map[string]int64{}
	for _, issue := range projection.SortedIssues() {
		issueUID := issue.UID
		row, exists := existing[issueUID]
		shortIDValue, err := resolveFederatedIssueShortID(ctx, tx, projectID, issue, row, exists)
		if err != nil {
			return nil, err
		}
		metadata := json.RawMessage(`{}`)
		if value := projection.IssueMetadata[issueUID]; len(value) > 0 {
			metadata = value
		}
		updatedAt := issue.UpdatedAt
		if updatedAt == "" {
			updatedAt = issue.CreatedAt
		}
		if exists {
			if err := rejectExternalRootContentMutationTx(
				ctx, tx, row.id,
				issue.Title != row.title || issue.Body != row.body || (issue.DeletedAt != nil) != row.deleted,
			); err != nil {
				if errors.Is(err, db.ErrExternalRootContentOwned) {
					return nil, fmt.Errorf("%w: %w", db.ErrFederationIngestValidation, err)
				}
				return nil, err
			}
			_, err := tx.ExecContext(ctx, `UPDATE issues SET short_id=$1,title=$2,body=$3,status=$4,
closed_reason=$5,owner=$6,priority=$7,author=$8,created_at=$9,updated_at=$10,
closed_at=$11,deleted_at=$12,metadata=$13,
revision=revision+1,content_revision=content_revision+CASE
WHEN title IS DISTINCT FROM $2 OR body IS DISTINCT FROM $3 THEN 1 ELSE 0 END
WHERE id=$14 AND ROW(short_id,title,body,status,closed_reason,owner,priority,author,
created_at,updated_at,closed_at,deleted_at,metadata) IS DISTINCT FROM
ROW($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`,
				shortIDValue, issue.Title, issue.Body, nonEmptyFederationStatus(issue.Status),
				issue.ClosedReason, issue.Owner, issue.Priority, nonEmptyFederationAuthor(issue.Author),
				nonEmptyFederationTime(issue.CreatedAt), nonEmptyFederationTime(updatedAt),
				optionalFederationString(issue.ClosedAt), optionalFederationString(issue.DeletedAt),
				string(metadata), row.id)
			if err != nil {
				return nil, mapSQLError(err, nil)
			}
			output[issueUID] = row.id
			continue
		}
		var issueID int64
		err = tx.QueryRowContext(ctx, `INSERT INTO issues(
uid,project_id,short_id,title,body,status,closed_reason,owner,priority,author,
created_at,updated_at,closed_at,deleted_at,metadata,revision
) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,1) RETURNING id`,
			issue.UID, projectID, shortIDValue, issue.Title, issue.Body,
			nonEmptyFederationStatus(issue.Status), issue.ClosedReason, issue.Owner, issue.Priority,
			nonEmptyFederationAuthor(issue.Author), nonEmptyFederationTime(issue.CreatedAt),
			nonEmptyFederationTime(updatedAt), optionalFederationString(issue.ClosedAt),
			optionalFederationString(issue.DeletedAt), string(metadata)).Scan(&issueID)
		if err != nil {
			return nil, mapSQLError(err, nil)
		}
		output[issueUID] = issueID
	}
	return output, nil
}

func resolveFederatedIssueShortID(
	ctx context.Context,
	tx *sql.Tx,
	projectID int64,
	issue db.FoldIssue,
	existing federatedIssueRow,
	hasExisting bool,
) (string, error) {
	minimum := shortid.MinLength
	if issue.ShortID != "" {
		if !shortid.Valid(issue.ShortID) {
			return "", fmt.Errorf("invalid federated short_id %q", issue.ShortID)
		}
		expected, err := shortid.Derive(issue.UID, len(issue.ShortID))
		if err != nil || expected != issue.ShortID {
			return "", fmt.Errorf("federated short_id %q does not match uid %q", issue.ShortID, issue.UID)
		}
		minimum = len(issue.ShortID)
	}
	if hasExisting && len(existing.shortID) > minimum {
		minimum = len(existing.shortID)
	}
	for length := minimum; length <= shortid.MaxLength; length++ {
		candidate, err := shortid.Derive(issue.UID, length)
		if err != nil {
			return "", err
		}
		var occupied bool
		err = tx.QueryRowContext(ctx, `SELECT EXISTS(
SELECT 1 FROM issues WHERE project_id=$1 AND short_id=$2 AND uid<>$3
UNION ALL SELECT 1 FROM purge_log WHERE project_id=$1 AND short_id=$2
)`, projectID, candidate, issue.UID).Scan(&occupied)
		if err != nil {
			return "", mapSQLError(err, nil)
		}
		if !occupied {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("short_id auto-extend exhausted for uid %s", issue.UID)
}

func federatedIssueRowsByUID(
	ctx context.Context,
	tx *sql.Tx,
	projectID int64,
) (map[string]federatedIssueRow, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT uid,id,short_id,title,body,deleted_at IS NOT NULL FROM issues WHERE project_id=$1`, projectID)
	if err != nil {
		return nil, mapSQLError(err, nil)
	}
	defer func() { _ = rows.Close() }()
	output := map[string]federatedIssueRow{}
	for rows.Next() {
		var issueUID string
		var row federatedIssueRow
		if err := rows.Scan(&issueUID, &row.id, &row.shortID, &row.title, &row.body, &row.deleted); err != nil {
			return nil, mapSQLError(err, nil)
		}
		output[issueUID] = row
	}
	return output, mapSQLError(rows.Err(), nil)
}

func reconcileFederatedComments(
	ctx context.Context,
	tx *sql.Tx,
	projectID int64,
	issueIDs map[string]int64,
	projection db.FoldProjection,
) error {
	existing, err := federatedCommentRowsByUID(ctx, tx, projectID)
	if err != nil {
		return err
	}
	sorted := projection.SortedComments()
	desired := make(map[string]struct{}, len(sorted))
	for _, comment := range sorted {
		desired[comment.UID] = struct{}{}
	}
	for commentUID, row := range existing {
		if _, ok := desired[commentUID]; ok {
			continue
		}
		owned, err := federatedExternalCommentOwnedTx(ctx, tx, row.id)
		if err != nil {
			return err
		}
		if owned {
			return fmt.Errorf("%w: %w", db.ErrFederationIngestValidation, db.ErrExternalCommentContentOwned)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM comments WHERE id=$1`, row.id); err != nil {
			return mapSQLError(err, nil)
		}
	}
	for _, comment := range sorted {
		commentUID := comment.UID
		issueID, ok := issueIDs[comment.IssueUID]
		if !ok {
			return fmt.Errorf("federated comment %s references unknown issue %s", commentUID, comment.IssueUID)
		}
		if row, ok := existing[commentUID]; ok {
			// A fold visits all existing comments, even when the incoming batch
			// changed none of them. Avoid per-comment queries and writes while
			// the ingest transaction holds the event-ordering fence.
			if row.issueID == issueID && row.author == nonEmptyFederationAuthor(comment.Author) &&
				row.body == comment.Body && row.createdAt == nonEmptyFederationTime(comment.CreatedAt) {
				continue
			}
			owned, err := federatedExternalCommentOwnedTx(ctx, tx, row.id)
			if err != nil {
				return err
			}
			if owned {
				if comment.Body != row.body {
					return fmt.Errorf("%w: %w", db.ErrFederationIngestValidation, db.ErrExternalCommentContentOwned)
				}
				continue
			}
			_, err = tx.ExecContext(ctx, `UPDATE comments SET issue_id=$1,author=$2,body=$3,created_at=$4
WHERE id=$5`, issueID, nonEmptyFederationAuthor(comment.Author), comment.Body,
				nonEmptyFederationTime(comment.CreatedAt), row.id)
			if err != nil {
				return mapSQLError(err, nil)
			}
			continue
		}
		_, err := tx.ExecContext(ctx,
			`INSERT INTO comments(uid,issue_id,author,body,created_at) VALUES($1,$2,$3,$4,$5)`,
			comment.UID, issueID, nonEmptyFederationAuthor(comment.Author), comment.Body,
			nonEmptyFederationTime(comment.CreatedAt))
		if err != nil {
			return mapSQLError(err, nil)
		}
	}
	return nil
}

type federatedCommentRow struct {
	id        int64
	issueID   int64
	author    string
	body      string
	createdAt string
}

func federatedExternalCommentOwnedTx(ctx context.Context, tx *sql.Tx, commentID int64) (bool, error) {
	var owned bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (
		SELECT 1
		FROM import_mappings m
		JOIN comments c ON c.id = m.comment_id AND c.issue_id = m.issue_id
		JOIN external_root_bindings b
		  ON b.project_id = m.project_id
		 AND b.issue_id = m.issue_id
		 AND m.source = 'connector:' || b.connector_instance || ':binding:' || b.uid
		WHERE m.object_type = 'comment' AND m.comment_id = $1 AND b.active = 1
	)`, commentID).Scan(&owned); err != nil {
		return false, fmt.Errorf("check federated external comment ownership: %w", mapSQLError(err, nil))
	}
	return owned, nil
}

func federatedCommentRowsByUID(
	ctx context.Context,
	tx *sql.Tx,
	projectID int64,
) (map[string]federatedCommentRow, error) {
	rows, err := tx.QueryContext(ctx, `SELECT c.uid,c.id,c.issue_id,c.author,c.body,c.created_at FROM comments c
JOIN issues i ON i.id=c.issue_id WHERE i.project_id=$1`, projectID)
	if err != nil {
		return nil, mapSQLError(err, nil)
	}
	defer func() { _ = rows.Close() }()
	output := map[string]federatedCommentRow{}
	for rows.Next() {
		var commentUID string
		var row federatedCommentRow
		if err := rows.Scan(&commentUID, &row.id, &row.issueID, &row.author, &row.body, &row.createdAt); err != nil {
			return nil, mapSQLError(err, nil)
		}
		output[commentUID] = row
	}
	return output, mapSQLError(rows.Err(), nil)
}

type federatedLabelKey struct {
	issueID int64
	label   string
}

func reconcileFederatedLabels(
	ctx context.Context,
	tx *sql.Tx,
	projectID int64,
	issueIDs map[string]int64,
	projection db.FoldProjection,
) error {
	existing, err := federatedLabelKeys(ctx, tx, projectID)
	if err != nil {
		return err
	}
	desired := map[federatedLabelKey]struct{}{}
	for _, key := range projection.PresentLabels() {
		issueID, ok := issueIDs[key.IssueUID]
		if !ok {
			return fmt.Errorf("federated label %s references unknown issue %s", key.Label, key.IssueUID)
		}
		desired[federatedLabelKey{issueID: issueID, label: key.Label}] = struct{}{}
	}
	for key := range existing {
		if _, ok := desired[key]; ok {
			continue
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM issue_labels WHERE issue_id=$1 AND label=$2`, key.issueID, key.label); err != nil {
			return mapSQLError(err, nil)
		}
	}
	for key := range desired {
		if _, ok := existing[key]; ok {
			continue
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO issue_labels(issue_id,label,author) VALUES($1,$2,'federation')`,
			key.issueID, key.label); err != nil {
			return mapSQLError(err, nil)
		}
	}
	return nil
}

func federatedLabelKeys(
	ctx context.Context,
	tx *sql.Tx,
	projectID int64,
) (map[federatedLabelKey]struct{}, error) {
	rows, err := tx.QueryContext(ctx, `SELECT il.issue_id,il.label FROM issue_labels il
JOIN issues i ON i.id=il.issue_id WHERE i.project_id=$1`, projectID)
	if err != nil {
		return nil, mapSQLError(err, nil)
	}
	defer func() { _ = rows.Close() }()
	output := map[federatedLabelKey]struct{}{}
	for rows.Next() {
		var key federatedLabelKey
		if err := rows.Scan(&key.issueID, &key.label); err != nil {
			return nil, mapSQLError(err, nil)
		}
		output[key] = struct{}{}
	}
	return output, mapSQLError(rows.Err(), nil)
}

func pruneFederatedIssues(
	ctx context.Context,
	tx *sql.Tx,
	m *federatedMaterialization,
	issueIDs map[string]int64,
) error {
	// Candidates come from the snapshot taken before reconcile inserted
	// anything, so a row this pass just created is structurally unable to be a
	// prune candidate.
	var candidates []int64
	for issueUID, row := range m.existingIssues {
		if _, ok := issueIDs[issueUID]; ok {
			continue
		}
		candidates = append(candidates, row.id)
	}
	if len(candidates) == 0 {
		return nil
	}
	// One anti-join instead of one count(*) per candidate.
	rows, err := tx.QueryContext(ctx, `SELECT candidate.id
FROM unnest($1::bigint[]) AS candidate(id)
WHERE NOT EXISTS (
  SELECT 1 FROM events e WHERE e.issue_id=candidate.id OR e.related_issue_id=candidate.id
)`, candidates)
	if err != nil {
		return mapSQLError(err, nil)
	}
	var unreferenced []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return mapSQLError(err, nil)
		}
		unreferenced = append(unreferenced, id)
	}
	if err := rows.Close(); err != nil {
		return mapSQLError(err, nil)
	}
	if err := rows.Err(); err != nil {
		return mapSQLError(err, nil)
	}
	if len(unreferenced) == 0 {
		return nil
	}
	_, err = tx.ExecContext(ctx, `DELETE FROM issues WHERE id = ANY($1::bigint[])`, unreferenced)
	return mapSQLError(err, nil)
}

func nonEmptyFederationTime(value string) string {
	if value != "" {
		return value
	}
	return "1970-01-01T00:00:00.000Z"
}

func nonEmptyFederationStatus(value string) string {
	if value != "" {
		return value
	}
	return "open"
}

func optionalFederationString(value *string) any {
	if value == nil || strings.TrimSpace(*value) == "" {
		return nil
	}
	return *value
}
