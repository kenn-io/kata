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
			if record.Link != nil {
				skippedLinkIDs[record.Link.ID] = struct{}{}
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
	switch record.Kind {
	case db.ImportKindMeta:
		return replayLinkInserted, pgReplayMeta(ctx, tx, record.Meta, opts)
	case db.ImportKindProject:
		return replayLinkInserted, pgReplayProject(ctx, tx, record.Project)
	case db.ImportKindProjectAlias:
		return replayLinkInserted, pgReplayAlias(ctx, tx, record.Alias)
	case db.ImportKindIssueSyncBinding:
		return replayLinkInserted, pgReplayIssueSyncBinding(
			ctx, tx, record.IssueSyncBinding, opts.PreserveIssueSyncBindingEnabled,
		)
	case db.ImportKindIssueSyncStatus:
		return replayLinkInserted, pgReplayIssueSyncStatus(ctx, tx, record.IssueSyncStatus)
	case db.ImportKindRecurrence:
		return replayLinkInserted, pgReplayRecurrence(ctx, tx, record.Recurrence)
	case db.ImportKindIssue:
		return replayLinkInserted, pgReplayIssue(ctx, tx, record.Issue)
	case db.ImportKindIssueEmbedding:
		return replayLinkInserted, nil
	case db.ImportKindComment:
		return replayLinkInserted, pgReplayComment(ctx, tx, record.Comment)
	case db.ImportKindIssueLabel:
		return replayLinkInserted, pgReplayLabel(ctx, tx, record.Label)
	case db.ImportKindLink:
		return pgReplayLink(ctx, tx, record.Link)
	case db.ImportKindImportMapping:
		return pgReplayImportMapping(ctx, tx, record.ImportMapping, skippedLinkIDs)
	case db.ImportKindExternalFieldMapping:
		return replayLinkInserted, pgReplayExternalFieldMapping(
			ctx, tx, record.ExternalFieldMapping, opts.MergeProject,
		)
	case db.ImportKindExternalRootBinding:
		return replayLinkInserted, pgReplayExternalRootBinding(
			ctx, tx, record.ExternalRootBinding, opts.PreserveExternalRootBindingsEnabled,
		)
	case db.ImportKindExternalFieldState:
		return replayLinkInserted, pgReplayExternalFieldState(ctx, tx, record.ExternalFieldState)
	case db.ImportKindFederationBinding:
		return replayLinkInserted, pgReplayFederationBinding(ctx, tx, record.FederationBinding)
	case db.ImportKindFederationSyncStatus:
		return replayLinkInserted, pgReplayFederationSyncStatus(ctx, tx, record.FederationSyncStatus)
	case db.ImportKindFederationQuarantine:
		return replayLinkInserted, pgReplayFederationQuarantine(ctx, tx, record.FederationQuarantine)
	case db.ImportKindFederationEnrollment:
		return replayLinkInserted, pgReplayFederationEnrollment(ctx, tx, record.FederationEnrollment)
	case db.ImportKindIssueClaim:
		return replayLinkInserted, pgReplayIssueClaim(ctx, tx, record.IssueClaim)
	case db.ImportKindPendingClaimRequest:
		return replayLinkInserted, pgReplayPendingClaim(ctx, tx, record.PendingClaimRequest, opts)
	case db.ImportKindEvent:
		return replayLinkInserted, pgReplayEvent(ctx, tx, record.Event, opts)
	case db.ImportKindPurgeLog:
		return replayLinkInserted, pgReplayPurgeLog(ctx, tx, record.PurgeLog)
	case db.ImportKindProjectPurgeLog:
		return replayLinkInserted, pgReplayProjectPurgeLog(ctx, tx, record.ProjectPurgeLog)
	case db.ImportKindSQLiteSequence:
		if record.Sequence.Seq > sequenceFloors[record.Sequence.Name] {
			sequenceFloors[record.Sequence.Name] = record.Sequence.Seq
		}
		return replayLinkInserted, nil
	default:
		return replayLinkInserted, fmt.Errorf("import: unsupported kind %q", record.Kind)
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
	var mappingID int64
	var externalFieldName, storedKinds, updatedAt string
	var nullable, writable int
	err := queryer.QueryRowContext(ctx, `SELECT id, external_field_name, accepted_kinds_json,
		       nullable, writable, updated_at
		FROM external_field_mappings
		WHERE connector_instance = ? AND kata_field = ? AND external_field_id = ?
		  AND schema_revision = ? AND created_at::timestamptz = ?::timestamptz`,
		mapping.ConnectorInstance, mapping.KataField, mapping.ExternalFieldID,
		mapping.SchemaRevision, formatExternalObservationTime(mapping.CreatedAt),
	).Scan(&mappingID, &externalFieldName, &storedKinds, &nullable, &writable, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, pgReplayError(db.ImportKindExternalFieldMapping, err)
	}
	if externalFieldName != mapping.ExternalFieldName || storedKinds != acceptedKinds ||
		nullable != boolInt(mapping.Nullable) || writable != boolInt(mapping.Writable) {
		return false, fmt.Errorf("import %s: project merge mapping identity conflicts with target descriptor",
			db.ImportKindExternalFieldMapping)
	}
	storedUpdatedAt, err := parseStoredTime(updatedAt)
	if err != nil {
		return false, pgReplayError(db.ImportKindExternalFieldMapping, err)
	}
	if mapping.UpdatedAt.After(storedUpdatedAt) {
		if _, err := queryer.ExecContext(ctx,
			`UPDATE external_field_mappings SET updated_at=? WHERE id=?`,
			formatExternalObservationTime(mapping.UpdatedAt), mappingID); err != nil {
			return false, pgReplayError(db.ImportKindExternalFieldMapping, err)
		}
	}
	return true, nil
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
	var bindingID, mappingID int64
	var bindingConnector string
	if err := queryer.QueryRowContext(ctx, `SELECT b.id, m.id, b.connector_instance
		FROM external_root_bindings b
		JOIN external_field_mappings m
		  ON m.connector_instance = ? AND m.kata_field = ?
		 AND m.external_field_id = ? AND m.schema_revision = ?
		 AND m.created_at::timestamptz = ?::timestamptz
		WHERE b.uid = ?`,
		state.MappingConnectorInstance, state.MappingKataField,
		state.MappingExternalFieldID, state.MappingSchemaRevision,
		formatExternalObservationTime(state.MappingCreatedAt), state.BindingUID,
	).Scan(&bindingID, &mappingID, &bindingConnector); err != nil {
		return pgReplayError(db.ImportKindExternalFieldState, err)
	}
	if bindingConnector != state.MappingConnectorInstance {
		return pgReplayError(db.ImportKindExternalFieldState, fmt.Errorf(
			"%w: field state mapping connector does not match binding", db.ErrExternalRootValidation,
		))
	}
	_, err := queryer.ExecContext(ctx, `INSERT INTO external_field_states(
		binding_id, mapping_id, baseline_json, conflicted, conflict_kata,
		conflict_external, conflict_at, updated_at
	) VALUES(?, ?, ?, ?, ?, ?, ?, ?)`,
		bindingID, mappingID, pgNullableRawJSON(state.Baseline), boolInt(state.Conflicted),
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
