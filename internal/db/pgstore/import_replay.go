package pgstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"go.kenn.io/kata/internal/db"
)

// ImportReplay atomically restores normalized JSONL records into Postgres.
// Explicit source identities are preserved, identity sequences are advanced,
// and the token projection is rebuilt from its authoritative events.
func (s *Store) ImportReplay(ctx context.Context, records []db.ImportRecord, opts db.ImportOptions) error {
	if err := db.ValidateImportRecords(records); err != nil {
		return err
	}
	var finalInstanceUID string
	err := s.withTx(ctx, sql.LevelReadCommitted, func(tx *sql.Tx) error {
		if err := s.importReplayTx(ctx, tx, db.OrderImportRecords(records), opts); err != nil {
			return err
		}
		if err := tx.QueryRowContext(ctx,
			`SELECT value FROM meta WHERE key='instance_uid'`).Scan(&finalInstanceUID); err != nil {
			return fmt.Errorf("read restored instance_uid: %w", mapSQLError(err, nil))
		}
		return nil
	})
	if err != nil {
		return err
	}
	s.instanceUID = finalInstanceUID
	return nil
}

type replayLinkSkip int

const (
	replayLinkInserted replayLinkSkip = iota
	replayLinkMissingPeer
	replayLinkDuplicate
	replayLinkMapping
)

func (s *Store) importReplayTx(
	ctx context.Context,
	tx *sql.Tx,
	records []db.ImportRecord,
	opts db.ImportOptions,
) error {
	preservedInstanceUID := s.instanceUID
	var err error
	if opts.MergeProject {
		records, err = s.prepareProjectMergeTx(ctx, tx, records)
		if err != nil {
			return err
		}
	} else {
		preservedInstanceUID, err = s.pgReplayClearTarget(ctx, tx, opts)
		if err != nil {
			return err
		}
	}

	var missingPeers, duplicates, skippedMappings int
	skippedLinkIDs := make(map[int64]struct{})
	sequenceFloors := make(map[string]int64)
	for _, record := range records {
		skip, err := s.importReplayRecord(ctx, tx, record, opts, skippedLinkIDs, sequenceFloors)
		if err != nil {
			return err
		}
		switch skip {
		case replayLinkMissingPeer:
			missingPeers++
			if link, ok := record.(*db.LinkExport); ok {
				skippedLinkIDs[link.ID] = struct{}{}
			}
		case replayLinkDuplicate:
			duplicates++
		case replayLinkMapping:
			skippedMappings++
		}
	}
	if missingPeers > 0 {
		fmt.Fprintf(os.Stderr,
			"note: skipped %d link record(s) whose peer issue is not in this envelope or database\n",
			missingPeers)
	}
	if duplicates > 0 {
		fmt.Fprintf(os.Stderr,
			"note: skipped %d duplicate link record(s) (edge already present)\n",
			duplicates)
	}
	if skippedMappings > 0 {
		fmt.Fprintf(os.Stderr,
			"note: skipped %d import mapping record(s) referencing skipped link(s)\n",
			skippedMappings)
	}

	if err := pgReplayEnsureSystemProject(ctx, tx); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO meta(key,value) VALUES('instance_uid',$1) ON CONFLICT(key) DO NOTHING`,
		preservedInstanceUID); err != nil {
		return fmt.Errorf("restore target instance_uid: %w", mapSQLError(err, nil))
	}
	if !opts.MergeProject {
		if err := s.replayAPITokens(ctx, tx); err != nil {
			return err
		}
		if err := pgReplayRecordSchemaVersion(ctx, tx); err != nil {
			return err
		}
	}
	if err := s.reconcileReplayIdentities(ctx, tx, sequenceFloors, opts.MergeProject); err != nil {
		return err
	}
	return nil
}

// pgReplayClearTarget makes replay an atomic whole-schema replacement. The
// table inventory comes from the selected schema, so future migration-owned
// tables participate without a second hand-maintained list. NewInstance keeps
// the target identity in memory while meta is replaced with the snapshot.
func (s *Store) pgReplayClearTarget(
	ctx context.Context,
	tx *sql.Tx,
	opts db.ImportOptions,
) (string, error) {
	if err := acquireExclusiveServingLease(ctx, tx, s.schema); err != nil {
		return "", fmt.Errorf("quiesce serving daemons for import: %w", err)
	}
	if err := acquireSchemaMigrationLock(ctx, tx); err != nil {
		return "", fmt.Errorf("lock schema migrations for import: %w", mapSQLError(err, nil))
	}
	allTables, err := schemaTableNames(ctx, tx)
	if err != nil {
		return "", err
	}
	if len(allTables) == 0 {
		return "", nil
	}
	quotedAll := make([]string, len(allTables))
	for i, table := range allTables {
		quotedAll[i] = quoteIdentifier(table)
	}
	if _, err := tx.ExecContext(ctx,
		`LOCK TABLE `+strings.Join(quotedAll, ", ")+` IN ACCESS EXCLUSIVE MODE`); err != nil {
		return "", fmt.Errorf("lock import target tables: %w", mapSQLError(err, nil))
	}
	if opts.RequireFreshTarget {
		if err := validateFreshSchema(ctx, tx, allTables, s.instanceUID); err != nil {
			return "", fmt.Errorf("import requires a fresh target: %w", err)
		}
	}
	var preservedInstanceUID string
	if err := tx.QueryRowContext(ctx,
		`SELECT value FROM meta WHERE key='instance_uid'`).Scan(&preservedInstanceUID); err != nil {
		return "", fmt.Errorf("preserve target instance_uid: %w", mapSQLError(err, nil))
	}
	if _, err := tx.ExecContext(ctx,
		`TRUNCATE TABLE `+strings.Join(quotedAll, ", ")+` RESTART IDENTITY`); err != nil {
		return "", fmt.Errorf("clear import target: %w", mapSQLError(err, nil))
	}
	return preservedInstanceUID, nil
}

func (s *Store) importReplayRecord(
	ctx context.Context,
	tx *sql.Tx,
	record db.ImportRecord,
	opts db.ImportOptions,
	skippedLinkIDs map[int64]struct{},
	sequenceFloors map[string]int64,
) (replayLinkSkip, error) {
	switch rec := record.(type) {
	case *db.MetaKV:
		return replayLinkInserted, pgReplayMeta(ctx, tx, rec, opts)
	case *db.ProjectExport:
		return replayLinkInserted, pgReplayProject(ctx, tx, rec)
	case *db.AliasExport:
		return replayLinkInserted, pgReplayAlias(ctx, tx, rec)
	case *db.IssueSyncBindingExport:
		return replayLinkInserted, pgReplayIssueSyncBinding(
			ctx, tx, rec, opts.PreserveIssueSyncBindingEnabled,
		)
	case *db.IssueSyncStatusExport:
		return replayLinkInserted, pgReplayIssueSyncStatus(ctx, tx, rec)
	case *db.RecurrenceExport:
		return replayLinkInserted, pgReplayRecurrence(ctx, tx, rec)
	case *db.IssueExport:
		return replayLinkInserted, pgReplayIssue(ctx, tx, rec)
	case *db.IssueEmbeddingExport:
		return replayLinkInserted, nil
	case *db.CommentExport:
		return replayLinkInserted, pgReplayComment(ctx, tx, rec)
	case *db.IssueLabelExport:
		return replayLinkInserted, pgReplayLabel(ctx, tx, rec)
	case *db.LinkExport:
		return pgReplayLink(ctx, tx, rec)
	case *db.ImportMappingExport:
		return pgReplayImportMapping(ctx, tx, rec, skippedLinkIDs)
	case *db.ExternalFieldMappingExport:
		return replayLinkInserted, pgReplayExternalFieldMapping(ctx, tx, rec, opts.MergeProject)
	case *db.ExternalRootBindingExport:
		return replayLinkInserted, pgReplayExternalRootBinding(
			ctx, tx, rec, opts.PreserveExternalRootBindingsEnabled,
		)
	case *db.ExternalFieldStateExport:
		return replayLinkInserted, pgReplayExternalFieldState(ctx, tx, rec)
	case *db.FederationBindingExport:
		return replayLinkInserted, pgReplayFederationBinding(ctx, tx, rec)
	case *db.FederationSyncStatusExport:
		return replayLinkInserted, pgReplayFederationSyncStatus(ctx, tx, rec)
	case *db.FederationQuarantineExport:
		return replayLinkInserted, pgReplayFederationQuarantine(ctx, tx, rec)
	case *db.FederationEnrollmentExport:
		return replayLinkInserted, pgReplayFederationEnrollment(ctx, tx, rec)
	case *db.IssueClaimExport:
		return replayLinkInserted, pgReplayIssueClaim(ctx, tx, rec)
	case *db.PendingClaimRequestExport:
		return replayLinkInserted, pgReplayPendingClaim(ctx, tx, rec, opts)
	case *db.EventExport:
		return replayLinkInserted, pgReplayEvent(ctx, tx, rec, opts)
	case *db.PurgeLogExport:
		return replayLinkInserted, pgReplayPurgeLog(ctx, tx, rec)
	case *db.ProjectPurgeLogExport:
		return replayLinkInserted, pgReplayProjectPurgeLog(ctx, tx, rec)
	case *db.SequenceExport:
		if rec.Seq > sequenceFloors[rec.Name] {
			sequenceFloors[rec.Name] = rec.Seq
		}
		return replayLinkInserted, nil
	default:
		// ValidateImportRecords rejects unknown types before the transaction
		// opens; this arm exists so a future payload type cannot be replayed
		// silently.
		return replayLinkInserted, fmt.Errorf("import: unsupported record type %T", record)
	}
}

func pgReplayError(kind string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("import %s: %w", kind, mapSQLError(err, nil))
}

func pgReplayExternalFieldMapping(
	ctx context.Context,
	tx *sql.Tx,
	mapping *db.ExternalFieldMappingExport,
	mergeProject bool,
) error {
	normalized, err := db.NormalizeExternalFieldMappingExport(*mapping)
	if err != nil {
		return pgReplayError(db.ImportKindExternalFieldMapping, err)
	}
	mapping = &normalized
	acceptedKinds, err := json.Marshal(mapping.AcceptedKinds)
	if err != nil {
		return pgReplayError(db.ImportKindExternalFieldMapping, err)
	}
	if mergeProject {
		reused, err := pgReuseProjectMergeExternalFieldMapping(ctx, tx, mapping, string(acceptedKinds))
		if err != nil {
			return err
		}
		if reused {
			return nil
		}
	}
	active := mapping.Active
	if mergeProject {
		active = false
	}
	_, err = (externalRootTxQueryer{tx}).ExecContext(ctx, `INSERT INTO external_field_mappings(
		connector_instance, kata_field, external_field_id, external_field_name,
		accepted_kinds_json, nullable, writable, schema_revision, active,
		created_at, updated_at
	) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		mapping.ConnectorInstance, mapping.KataField, mapping.ExternalFieldID,
		mapping.ExternalFieldName, string(acceptedKinds), boolInt(mapping.Nullable),
		boolInt(mapping.Writable), mapping.SchemaRevision, boolInt(active),
		formatExternalObservationTime(mapping.CreatedAt), formatExternalObservationTime(mapping.UpdatedAt))
	return pgReplayError(db.ImportKindExternalFieldMapping, err)
}

func pgReuseProjectMergeExternalFieldMapping(
	ctx context.Context,
	tx *sql.Tx,
	mapping *db.ExternalFieldMappingExport,
	acceptedKinds string,
) (bool, error) {
	queryer := externalRootTxQueryer{tx}
	candidate, found, err := pgReplayExternalFieldMappingByIdentity(ctx, queryer,
		mapping.ConnectorInstance, mapping.KataField, mapping.ExternalFieldID,
		mapping.SchemaRevision, formatExternalObservationTime(mapping.CreatedAt),
	)
	if err != nil {
		return false, pgReplayError(db.ImportKindExternalFieldMapping, err)
	}
	if !found {
		return false, nil
	}
	if candidate.externalFieldName != mapping.ExternalFieldName || candidate.acceptedKinds != acceptedKinds ||
		candidate.nullable != boolInt(mapping.Nullable) || candidate.writable != boolInt(mapping.Writable) {
		return false, fmt.Errorf("import %s: project merge mapping identity conflicts with target descriptor",
			db.ImportKindExternalFieldMapping)
	}
	storedUpdatedAt, err := parseStoredTime(candidate.updatedAt)
	if err != nil {
		return false, pgReplayError(db.ImportKindExternalFieldMapping, err)
	}
	if mapping.UpdatedAt.After(storedUpdatedAt) {
		if _, err := queryer.ExecContext(ctx,
			`UPDATE external_field_mappings SET updated_at=? WHERE id=?`,
			formatExternalObservationTime(mapping.UpdatedAt), candidate.id); err != nil {
			return false, pgReplayError(db.ImportKindExternalFieldMapping, err)
		}
	}
	return true, nil
}

type pgReplayExternalFieldMappingCandidate struct {
	id                int64
	externalFieldName string
	acceptedKinds     string
	nullable          int
	writable          int
	updatedAt         string
}

func pgReplayExternalFieldMappingByIdentity(
	ctx context.Context,
	queryer externalRootTxQueryer,
	connectorInstance string,
	kataField string,
	externalFieldID string,
	schemaRevision string,
	createdAt string,
) (pgReplayExternalFieldMappingCandidate, bool, error) {
	wantCreatedAt, err := parseStoredTime(createdAt)
	if err != nil {
		return pgReplayExternalFieldMappingCandidate{}, false, err
	}
	rows, err := queryer.QueryContext(ctx, `SELECT id, external_field_name, accepted_kinds_json,
		       nullable, writable, created_at, updated_at
		FROM external_field_mappings
		WHERE connector_instance = $1 AND kata_field = $2 AND external_field_id = $3
		  AND schema_revision = $4`,
		connectorInstance, kataField, externalFieldID, schemaRevision,
	)
	if err != nil {
		return pgReplayExternalFieldMappingCandidate{}, false, err
	}
	defer func() { _ = rows.Close() }()

	var match pgReplayExternalFieldMappingCandidate
	found := false
	for rows.Next() {
		var candidate pgReplayExternalFieldMappingCandidate
		var storedCreatedAt string
		if err := rows.Scan(&candidate.id, &candidate.externalFieldName, &candidate.acceptedKinds,
			&candidate.nullable, &candidate.writable, &storedCreatedAt, &candidate.updatedAt); err != nil {
			return pgReplayExternalFieldMappingCandidate{}, false, err
		}
		parsedCreatedAt, err := parseStoredTime(storedCreatedAt)
		if err != nil {
			return pgReplayExternalFieldMappingCandidate{}, false, err
		}
		if !parsedCreatedAt.Equal(wantCreatedAt) {
			continue
		}
		if found {
			return pgReplayExternalFieldMappingCandidate{}, false, fmt.Errorf(
				"%w: multiple rows match portable identity", db.ErrExternalFieldMappingValidation,
			)
		}
		match = candidate
		found = true
	}
	if err := rows.Err(); err != nil {
		return pgReplayExternalFieldMappingCandidate{}, false, err
	}
	return match, found, nil
}

func pgReplayExternalRootBinding(
	ctx context.Context,
	tx *sql.Tx,
	binding *db.ExternalRootBindingExport,
	preserveEnabled bool,
) error {
	if err := db.ValidateExternalRootBindingReplayIdentity(*binding); err != nil {
		return pgReplayError(db.ImportKindExternalRootBinding, err)
	}
	queryer := externalRootTxQueryer{tx}
	var projectID, issueID, rootMappingID int64
	err := queryer.QueryRowContext(ctx, `SELECT p.id, i.id, rm.id
		FROM projects p
		JOIN issues i ON i.project_id = p.id
		JOIN import_mappings rm
		  ON rm.project_id = p.id AND rm.issue_id = i.id AND rm.object_type = 'issue'
		WHERE p.uid = ? AND i.uid = ? AND rm.source = ? AND rm.external_id = ?`,
		binding.ProjectUID, binding.IssueUID, binding.RootMappingSource,
		binding.RootMappingExternalID,
	).Scan(&projectID, &issueID, &rootMappingID)
	if err != nil {
		return pgReplayError(db.ImportKindExternalRootBinding, err)
	}
	var invalidCommentMappings int
	if err := queryer.QueryRowContext(ctx, `SELECT COUNT(*)
		FROM import_mappings m
		LEFT JOIN comments c ON c.id = m.comment_id
		WHERE m.project_id = ? AND m.object_type = 'comment'
		  AND m.source = 'connector:' || ? || ':binding:' || ?
		  AND (m.issue_id IS NULL OR m.comment_id IS NULL OR m.issue_id != ?
		       OR c.issue_id IS NULL OR c.issue_id != m.issue_id)`,
		projectID, binding.ConnectorInstance, binding.UID, issueID,
	).Scan(&invalidCommentMappings); err != nil {
		return pgReplayError(db.ImportKindExternalRootBinding, err)
	}
	if invalidCommentMappings != 0 {
		return pgReplayError(db.ImportKindExternalRootBinding, fmt.Errorf(
			"%w: external comment mapping does not belong to its binding issue", db.ErrExternalRootValidation,
		))
	}
	if binding.Active {
		var readOnlySpoke int
		err := queryer.QueryRowContext(ctx, `SELECT 1 FROM federation_bindings
			WHERE project_id = ? AND role = ? AND enabled = 1 AND push_enabled = 0 LIMIT 1`,
			projectID, db.FederationRoleSpoke).Scan(&readOnlySpoke)
		if err == nil {
			return pgReplayError(db.ImportKindExternalRootBinding, db.ErrExternalRootFederationConflict)
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return pgReplayError(db.ImportKindExternalRootBinding, err)
		}
		if err := rejectIssueSyncManagedExternalRootTx(ctx, tx, projectID, issueID); err != nil {
			return pgReplayError(db.ImportKindExternalRootBinding, err)
		}
	}
	receiveCommentsAfter := time.Time{}
	if binding.ReceiveCommentsAfter != nil {
		receiveCommentsAfter = *binding.ReceiveCommentsAfter
	}
	if err := db.ValidateCreateExternalRootBindingParams(db.CreateExternalRootBindingParams{
		ProjectID: projectID, IssueID: issueID, ConnectorInstance: binding.ConnectorInstance,
		ExternalRootKey: binding.ExternalRootKey, ExternalAccountKey: binding.ExternalAccountKey,
		Actor: "import-replay", ReceiveCommentsAfter: receiveCommentsAfter,
		PublishComments: binding.PublishComments, PublishCommentsAfter: binding.PublishCommentsAfter,
	}); err != nil {
		return pgReplayError(db.ImportKindExternalRootBinding, err)
	}
	if err := lockExternalRootConnectorTx(ctx, tx, binding.ConnectorInstance); err != nil {
		return pgReplayError(db.ImportKindExternalRootBinding, err)
	}
	if err := rejectExternalRootAccountIdentityChange(
		ctx, tx, binding.ConnectorInstance, binding.ExternalAccountKey,
	); err != nil {
		return pgReplayError(db.ImportKindExternalRootBinding, err)
	}
	var historicalIssueID int64
	historyErr := queryer.QueryRowContext(ctx, `SELECT issue_id FROM external_root_bindings
		WHERE connector_instance = ? AND external_root_key = ? ORDER BY id LIMIT 1 FOR SHARE`,
		binding.ConnectorInstance, binding.ExternalRootKey).Scan(&historicalIssueID)
	if historyErr == nil && historicalIssueID != issueID {
		return pgReplayError(db.ImportKindExternalRootBinding, db.ErrExternalRootAlreadyBound)
	}
	if historyErr != nil && !errors.Is(historyErr, sql.ErrNoRows) {
		return pgReplayError(db.ImportKindExternalRootBinding, historyErr)
	}
	if binding.PendingCommentUID != "" {
		var count int
		if err := queryer.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM comments WHERE uid = ? AND issue_id = ?`,
			binding.PendingCommentUID, issueID).Scan(&count); err != nil {
			return pgReplayError(db.ImportKindExternalRootBinding, err)
		}
		if count != 1 {
			return fmt.Errorf("import %s: pending comment %q not found for issue %q",
				db.ImportKindExternalRootBinding, binding.PendingCommentUID, binding.IssueUID)
		}
	}
	enabled := binding.Enabled
	pausedReason := binding.PausedReason
	if binding.Active && !preserveEnabled {
		enabled = false
		if binding.Enabled {
			pausedReason = "restore_reconfirmation_required"
		}
	}
	_, err = queryer.ExecContext(ctx, `INSERT INTO external_root_bindings(
		uid, project_id, issue_id, root_mapping_id, connector_instance,
		external_root_key, external_account_key, active, enabled, paused_reason,
		receive_comments, receive_comments_after, publish_comments,
		publish_comments_after, complete_external, claim_token, claim_started_at,
		last_external_state, last_external_revision, pending_comment_uid,
		pending_comment_started_at, last_attempt_at, last_success_at, last_error_at,
		last_error, consecutive_failures, next_attempt_at, created_at, updated_at,
		unbound_at
	) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, '', NULL, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		binding.UID, projectID, issueID, rootMappingID, binding.ConnectorInstance,
		binding.ExternalRootKey, binding.ExternalAccountKey, boolInt(binding.Active),
		boolInt(enabled), pausedReason, boolInt(binding.ReceiveComments),
		nullableExternalObservationTime(binding.ReceiveCommentsAfter), boolInt(binding.PublishComments),
		nullableExternalObservationTime(binding.PublishCommentsAfter), boolInt(binding.CompleteExternal),
		binding.LastExternalState, binding.LastExternalRevision, binding.PendingCommentUID,
		nullableStoredTime(binding.PendingCommentStartedAt), nullableStoredTime(binding.LastAttemptAt),
		nullableStoredTime(binding.LastSuccessAt), nullableStoredTime(binding.LastErrorAt),
		binding.LastError, binding.ConsecutiveFailures, nullableStoredTime(binding.NextAttemptAt),
		formatStoredTime(binding.CreatedAt), formatStoredTime(binding.UpdatedAt),
		nullableStoredTime(binding.UnboundAt))
	return pgReplayError(db.ImportKindExternalRootBinding, err)
}

func pgReplayExternalFieldState(ctx context.Context, tx *sql.Tx, state *db.ExternalFieldStateExport) error {
	if err := db.ValidateExternalFieldStateExport(*state); err != nil {
		return pgReplayError(db.ImportKindExternalFieldState, err)
	}
	queryer := externalRootTxQueryer{tx}
	var bindingID int64
	var bindingConnector string
	if err := queryer.QueryRowContext(ctx, `SELECT id, connector_instance
		FROM external_root_bindings WHERE uid = ?`, state.BindingUID,
	).Scan(&bindingID, &bindingConnector); err != nil {
		return pgReplayError(db.ImportKindExternalFieldState, err)
	}
	if bindingConnector != state.MappingConnectorInstance {
		return pgReplayError(db.ImportKindExternalFieldState, fmt.Errorf(
			"%w: field state mapping connector does not match binding", db.ErrExternalRootValidation,
		))
	}
	mapping, found, err := pgReplayExternalFieldMappingByIdentity(ctx, queryer,
		state.MappingConnectorInstance, state.MappingKataField,
		state.MappingExternalFieldID, state.MappingSchemaRevision,
		formatExternalObservationTime(state.MappingCreatedAt),
	)
	if err != nil {
		return pgReplayError(db.ImportKindExternalFieldState, err)
	}
	if !found {
		return pgReplayError(db.ImportKindExternalFieldState, sql.ErrNoRows)
	}
	_, err = queryer.ExecContext(ctx, `INSERT INTO external_field_states(
		binding_id, mapping_id, baseline_json, conflicted, conflict_kata,
		conflict_external, conflict_at, updated_at
	) VALUES(?, ?, ?, ?, ?, ?, ?, ?)`,
		bindingID, mapping.id, pgNullableRawJSON(state.Baseline), boolInt(state.Conflicted),
		pgNullableRawJSON(state.ConflictKata), pgNullableRawJSON(state.ConflictExternal),
		nullableStoredTime(state.ConflictAt), formatStoredTime(state.UpdatedAt))
	return pgReplayError(db.ImportKindExternalFieldState, err)
}

func pgNullableRawJSON(value json.RawMessage) any {
	if len(value) == 0 {
		return nil
	}
	return string(value)
}

func nullableExternalObservationTime(value *time.Time) any {
	if value == nil {
		return nil
	}
	return formatExternalObservationTime(*value)
}
