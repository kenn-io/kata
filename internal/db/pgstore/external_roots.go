//nolint:revive // Exported Store methods implement db.Storage; that interface owns their contract documentation.
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
	"go.kenn.io/kata/internal/metadata"
	katauid "go.kenn.io/kata/internal/uid"
)

const externalRootBindingSelect = `SELECT
 b.id, b.uid, b.project_id, b.issue_id, b.root_mapping_id,
 b.connector_instance, b.external_root_key, b.external_account_key,
 b.active, b.enabled, b.paused_reason,
 b.receive_comments, b.receive_comments_after,
 b.publish_comments, b.publish_comments_after, b.complete_external,
 b.claim_token, b.claim_started_at,
 b.last_external_state, b.last_external_revision,
 b.pending_comment_uid, b.pending_comment_started_at,
 b.last_attempt_at, b.last_success_at, b.last_error_at, b.last_error,
 b.consecutive_failures, b.next_attempt_at,
 b.created_at, b.updated_at, b.unbound_at
 FROM external_root_bindings b`

const externalFieldMappingSelect = `SELECT
 m.id, m.connector_instance, m.kata_field, m.external_field_id,
 m.external_field_name, m.accepted_kinds_json, m.nullable, m.writable,
 m.schema_revision, m.active, m.created_at, m.updated_at
 FROM external_field_mappings m`

const externalFieldStateSelect = `SELECT
 s.binding_id, s.mapping_id, s.baseline_json, s.conflicted,
 s.conflict_kata, s.conflict_external, s.conflict_at, s.updated_at
 FROM external_field_states s`

func (d *Store) externalRootTimestamp() string {
	if d.externalRootNow != nil {
		return formatStoredTime(d.externalRootNow())
	}
	return nowStoredTimestamp()
}

func rejectExternalRootContentMutationTx(ctx context.Context, tx *sql.Tx, issueID int64, contentChanged bool) error {
	if !contentChanged {
		return nil
	}
	var bindingID int64
	err := tx.QueryRowContext(ctx, `
		SELECT id FROM external_root_bindings
		 WHERE issue_id=$1 AND active=1
		 LIMIT 1`, issueID).Scan(&bindingID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("check external root content ownership: %w", mapSQLError(err, nil))
	}
	return db.ErrExternalRootContentOwned
}

func rejectIssueSyncManagedExternalRootTx(ctx context.Context, tx *sql.Tx, projectID, issueID int64) error {
	var mappingID int64
	err := tx.QueryRowContext(ctx, `SELECT m.id
 FROM import_mappings m
 JOIN issue_sync_bindings b ON b.project_id=m.project_id AND b.source_key=m.source
 WHERE m.project_id=$1 AND m.issue_id=$2 AND m.object_type='issue'
 LIMIT 1`, projectID, issueID).Scan(&mappingID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("check issue sync external root ownership: %w", mapSQLError(err, nil))
	}
	return db.ErrExternalRootIssueSyncConflict
}

func lockExternalRootConnectorTx(ctx context.Context, tx *sql.Tx, connectorInstance string) error {
	if _, err := tx.ExecContext(ctx,
		`SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`,
		"external-root-field-mapping:"+connectorInstance,
	); err != nil {
		return fmt.Errorf("lock external root connector: %w", mapSQLError(err, nil))
	}
	return nil
}

func lockAndRejectFreshExternalRootClaimsForConnectorTx(
	ctx context.Context,
	tx *sql.Tx,
	connectorInstance string,
	staleBefore time.Time,
) error {
	if err := lockExternalRootConnectorTx(ctx, tx, connectorInstance); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE external_root_bindings
 SET claim_token=claim_token
 WHERE connector_instance=$1 AND active=1 AND enabled=1`, connectorInstance); err != nil {
		return fmt.Errorf("lock external root claims for field mapping: %w", mapSQLError(err, nil))
	}
	var bindingID int64
	err := tx.QueryRowContext(ctx, `SELECT id FROM external_root_bindings
 WHERE connector_instance=$1 AND active=1 AND enabled=1 AND claim_token<>''
   AND (claim_started_at IS NULL OR claim_started_at >= $2)
 LIMIT 1`, connectorInstance, formatStoredTime(staleBefore)).Scan(&bindingID)
	if err == nil {
		return db.ErrExternalRootClaimActive
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("check external root claims for field mapping: %w", mapSQLError(err, nil))
	}
	if _, err := tx.ExecContext(ctx, `UPDATE external_root_bindings
 SET claim_token='', claim_started_at=NULL,
     updated_at=to_char(now() AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.MS"Z"')
 WHERE connector_instance=$1 AND active=1 AND enabled=1 AND claim_token<>''
   AND claim_started_at<$2`, connectorInstance, formatStoredTime(staleBefore)); err != nil {
		return fmt.Errorf("invalidate stale external root claims for field mapping: %w", mapSQLError(err, nil))
	}
	return nil
}

func lockAndRejectFreshExternalRootClaimsForIssueTx(
	ctx context.Context,
	tx *sql.Tx,
	issueID int64,
	staleBefore time.Time,
) error {
	if _, err := tx.ExecContext(ctx, `UPDATE external_root_bindings
 SET claim_token=claim_token WHERE issue_id=$1 AND active=1`, issueID); err != nil {
		return fmt.Errorf("lock external root claims for issue relocation: %w", mapSQLError(err, nil))
	}
	var bindingID int64
	err := tx.QueryRowContext(ctx, `SELECT id FROM external_root_bindings
 WHERE issue_id=$1 AND active=1 AND claim_token<>''
   AND (claim_started_at IS NULL OR claim_started_at >= $2)
 LIMIT 1`, issueID, formatStoredTime(staleBefore)).Scan(&bindingID)
	if err == nil {
		return db.ErrExternalRootClaimActive
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("check external root claims for issue relocation: %w", mapSQLError(err, nil))
	}
	if _, err := tx.ExecContext(ctx, `UPDATE external_root_bindings
 SET claim_token='', claim_started_at=NULL,
     updated_at=to_char(now() AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.MS"Z"')
 WHERE issue_id=$1 AND active=1 AND claim_token<>''
   AND claim_started_at<$2`, issueID, formatStoredTime(staleBefore)); err != nil {
		return fmt.Errorf("invalidate stale external root claims for issue relocation: %w", mapSQLError(err, nil))
	}
	return nil
}

func lockAndRejectFreshExternalRootClaimsForProjectTx(
	ctx context.Context,
	tx *sql.Tx,
	projectID int64,
	staleBefore time.Time,
) error {
	if _, err := tx.ExecContext(ctx, `UPDATE external_root_bindings
 SET claim_token=claim_token WHERE project_id=$1 AND active=1`, projectID); err != nil {
		return fmt.Errorf("lock external root claims for project relocation: %w", mapSQLError(err, nil))
	}
	var bindingID int64
	err := tx.QueryRowContext(ctx, `SELECT id FROM external_root_bindings
 WHERE project_id=$1 AND active=1 AND claim_token<>''
   AND (claim_started_at IS NULL OR claim_started_at >= $2)
 LIMIT 1`, projectID, formatStoredTime(staleBefore)).Scan(&bindingID)
	if err == nil {
		return db.ErrExternalRootClaimActive
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("check external root claims for project relocation: %w", mapSQLError(err, nil))
	}
	if _, err := tx.ExecContext(ctx, `UPDATE external_root_bindings
 SET claim_token='', claim_started_at=NULL,
     updated_at=to_char(now() AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.MS"Z"')
 WHERE project_id=$1 AND active=1 AND claim_token<>''
   AND claim_started_at<$2`, projectID, formatStoredTime(staleBefore)); err != nil {
		return fmt.Errorf("invalidate stale external root claims for project relocation: %w", mapSQLError(err, nil))
	}
	return nil
}

func rejectReopenDuringExternalRootClaimTx(ctx context.Context, tx *sql.Tx, issueID int64) error {
	var bindingID int64
	err := tx.QueryRowContext(ctx, `SELECT id FROM external_root_bindings
	 WHERE issue_id=$1 AND active=1 AND enabled=1 AND complete_external=1 AND claim_token<>''
 LIMIT 1`, issueID).Scan(&bindingID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("check external root completion claim: %w", mapSQLError(err, nil))
	}
	return db.ErrExternalRootClaimActive
}

func lockExternalRootClaimIssueTx(ctx context.Context, tx *sql.Tx, bindingID int64) error {
	var issueID int64
	err := tx.QueryRowContext(ctx, `SELECT i.id
 FROM external_root_bindings b
 JOIN issues i ON i.id=b.issue_id
 WHERE b.id=$1
 FOR UPDATE OF i`, bindingID).Scan(&issueID)
	if errors.Is(err, sql.ErrNoRows) {
		return db.ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("lock external root issue for claim: %w", mapSQLError(err, nil))
	}
	return nil
}

func rejectExternalRootAccountIdentityChange(
	ctx context.Context,
	tx *sql.Tx,
	connectorInstance string,
	externalAccountKey string,
) error {
	var conflictingBindingID int64
	err := tx.QueryRowContext(ctx, `SELECT id FROM external_root_bindings
		WHERE connector_instance=$1 AND external_account_key<>$2 ORDER BY id LIMIT 1 FOR SHARE`,
		connectorInstance, externalAccountKey).Scan(&conflictingBindingID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read connector account identity: %w", mapSQLError(err, nil))
	}
	return fmt.Errorf("%w: connector instance account identity changed", db.ErrExternalRootValidation)
}

// markPreBindingCommentsTx durably marks every local comment that exists
// when a publishing binding is created, mirroring the SQLite store: outbound
// publication trusts these markers rather than a timestamp frontier, so
// comments committed after the binding transaction — even with an earlier
// timestamp from truncation or clock rollback — still publish. The binding
// records the zero time as its publish frontier.
func markPreBindingCommentsTx(
	ctx context.Context,
	tx *sql.Tx,
	params db.CreateExternalRootBindingParams,
) error {
	rows, err := tx.QueryContext(ctx,
		`SELECT c.id, c.uid FROM comments c
		  WHERE c.issue_id=$1
		    AND NOT EXISTS (SELECT 1 FROM import_mappings m WHERE m.comment_id=c.id)`, params.IssueID,
	)
	if err != nil {
		return fmt.Errorf("read pre-binding local comments: %w", mapSQLError(err, nil))
	}
	type preBindingComment struct {
		id  int64
		uid string
	}
	var comments []preBindingComment
	for rows.Next() {
		var comment preBindingComment
		if err := rows.Scan(&comment.id, &comment.uid); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan pre-binding local comment: %w", err)
		}
		comments = append(comments, comment)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("read pre-binding local comments: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close pre-binding local comments: %w", err)
	}
	issueID := params.IssueID
	for _, comment := range comments {
		if err := upsertPendingExternalCommentMappingTx(ctx, tx, db.ImportMappingParams{
			Source:     db.ExternalRootSkippedCommentMappingSource(params.ConnectorInstance),
			ExternalID: comment.uid, ObjectType: "comment",
			ProjectID: params.ProjectID, IssueID: &issueID, CommentID: &comment.id,
		}); err != nil {
			return fmt.Errorf("mark pre-binding local comment %s: %w", comment.uid, err)
		}
	}
	return nil
}

type externalRootStoreQueryer struct{ *Store }

func (q externalRootStoreQueryer) QueryContext(
	ctx context.Context,
	query string,
	args ...any,
) (*sql.Rows, error) {
	return q.Store.QueryContext(ctx, externalRootSQL(query), args...)
}

func (q externalRootStoreQueryer) QueryRowContext(
	ctx context.Context,
	query string,
	args ...any,
) *sql.Row {
	return q.Store.QueryRowContext(ctx, externalRootSQL(query), args...)
}

type externalRootTxQueryer struct{ *sql.Tx }

func (q externalRootTxQueryer) ExecContext(
	ctx context.Context,
	query string,
	args ...any,
) (sql.Result, error) {
	return q.Tx.ExecContext(ctx, externalRootSQL(query), args...)
}

func (q externalRootTxQueryer) QueryRowContext(
	ctx context.Context,
	query string,
	args ...any,
) *sql.Row {
	return q.Tx.QueryRowContext(ctx, externalRootSQL(query), args...)
}

func externalRootSQL(query string) string {
	var out strings.Builder
	out.Grow(len(query) + 16)
	placeholder := 1
	for _, character := range query {
		if character != '?' {
			out.WriteRune(character)
			continue
		}
		fmt.Fprintf(&out, "$%d", placeholder)
		placeholder++
	}
	return out.String()
}

func retryExternalRootWrite1[T any](ctx context.Context, store *Store, op func() (T, error)) (T, error) {
	var value T
	err := store.RetryTransient(ctx, func() error {
		var err error
		value, err = op()
		return err
	})
	return value, mapSQLError(err, nil)
}

func retryExternalRootWrite2[A, B any](
	ctx context.Context,
	store *Store,
	op func() (A, B, error),
) (A, B, error) {
	var first A
	var second B
	err := store.RetryTransient(ctx, func() error {
		var err error
		first, second, err = op()
		return err
	})
	return first, second, mapSQLError(err, nil)
}

func retryExternalRootWrite3[A, B, C any](
	ctx context.Context,
	store *Store,
	op func() (A, B, C, error),
) (A, B, C, error) {
	var first A
	var second B
	var third C
	err := store.RetryTransient(ctx, func() error {
		var err error
		first, second, third, err = op()
		return err
	})
	return first, second, third, mapSQLError(err, nil)
}

func (d *Store) CreateExternalRootBinding(
	ctx context.Context,
	params db.CreateExternalRootBindingParams,
) (db.ExternalRootBinding, db.Event, error) {
	if err := db.ValidateCreateExternalRootBindingParams(params); err != nil {
		return db.ExternalRootBinding{}, db.Event{}, err
	}
	params.ConnectorInstance = strings.TrimSpace(params.ConnectorInstance)
	params.ExternalRootKey = strings.TrimSpace(params.ExternalRootKey)
	params.ExternalAccountKey = strings.TrimSpace(params.ExternalAccountKey)
	params.InitialClaimToken = strings.TrimSpace(params.InitialClaimToken)
	bindingUID, err := katauid.New()
	if err != nil {
		return db.ExternalRootBinding{}, db.Event{}, fmt.Errorf("generate external root binding uid: %w", err)
	}
	return retryExternalRootWrite2(ctx, d, func() (db.ExternalRootBinding, db.Event, error) {
		tx, err := d.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
		if err != nil {
			return db.ExternalRootBinding{}, db.Event{}, err
		}
		defer func() { _ = tx.Rollback() }()

		// externalRootIssueForEvent locks the issue and applies the transactional
		// federated-spoke write gate before binding or mapping creation.
		issue, projectName, err := externalRootIssueForEvent(ctx, tx, params.IssueID)
		if err != nil {
			return db.ExternalRootBinding{}, db.Event{}, err
		}
		if issue.ProjectID != params.ProjectID {
			return db.ExternalRootBinding{}, db.Event{}, fmt.Errorf("%w: issue is not in project", db.ErrExternalRootValidation)
		}
		if err := lockExternalRootConnectorTx(ctx, tx, params.ConnectorInstance); err != nil {
			return db.ExternalRootBinding{}, db.Event{}, err
		}
		if err := rejectIssueSyncManagedExternalRootTx(ctx, tx, params.ProjectID, issue.ID); err != nil {
			return db.ExternalRootBinding{}, db.Event{}, err
		}
		if err := rejectExternalRootAccountIdentityChange(
			ctx, tx, params.ConnectorInstance, params.ExternalAccountKey,
		); err != nil {
			return db.ExternalRootBinding{}, db.Event{}, err
		}
		issueID := issue.ID
		var historicalIssueID int64
		historyErr := tx.QueryRowContext(ctx, `SELECT issue_id FROM external_root_bindings
 WHERE connector_instance=$1 AND external_root_key=$2 ORDER BY id LIMIT 1 FOR SHARE`,
			params.ConnectorInstance, params.ExternalRootKey).Scan(&historicalIssueID)
		if historyErr == nil && historicalIssueID != issueID {
			return db.ExternalRootBinding{}, db.Event{}, db.ErrExternalRootAlreadyBound
		}
		if historyErr != nil && !errors.Is(historyErr, sql.ErrNoRows) {
			return db.ExternalRootBinding{}, db.Event{}, mapSQLError(historyErr, nil)
		}
		existingMapping, mappingErr := importMappingBySourceTx(
			ctx, tx, params.ProjectID, "connector:"+params.ConnectorInstance, "issue", params.ExternalRootKey,
		)
		if mappingErr == nil && (existingMapping.IssueID == nil || *existingMapping.IssueID != issueID) {
			return db.ExternalRootBinding{}, db.Event{}, db.ErrExternalRootAlreadyBound
		}
		if mappingErr != nil && !errors.Is(mappingErr, db.ErrNotFound) {
			return db.ExternalRootBinding{}, db.Event{}, mappingErr
		}
		mapping, err := upsertImportMappingTx(ctx, tx, db.ImportMappingParams{
			Source: "connector:" + params.ConnectorInstance, ExternalID: params.ExternalRootKey,
			ObjectType: "issue", ProjectID: params.ProjectID, IssueID: &issueID,
		})
		if err != nil {
			return db.ExternalRootBinding{}, db.Event{}, err
		}
		stamp := d.externalRootTimestamp()
		var receiveAfter any
		if !params.ReceiveCommentsAfter.IsZero() {
			receiveAfter = formatExternalObservationTime(params.ReceiveCommentsAfter)
		}
		var publishAfter any
		if params.UseLocalPublishFrontier {
			if err := markPreBindingCommentsTx(ctx, tx, params); err != nil {
				return db.ExternalRootBinding{}, db.Event{}, err
			}
			publishAfter = formatExternalObservationTime(time.Time{})
		} else if params.PublishCommentsAfter != nil {
			publishAfter = formatExternalObservationTime(*params.PublishCommentsAfter)
		}
		var initialClaimStartedAt any
		if params.InitialClaimToken != "" {
			initialClaimStartedAt = formatStoredTime(params.InitialClaimStartedAt)
		}
		var bindingID int64
		err = (externalRootTxQueryer{tx}).QueryRowContext(ctx, `INSERT INTO external_root_bindings(
 uid, project_id, issue_id, root_mapping_id,
 connector_instance, external_root_key, external_account_key,
 receive_comments_after, publish_comments, publish_comments_after,
 claim_token, claim_started_at, created_at, updated_at
) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?) RETURNING id`,
			bindingUID, params.ProjectID, params.IssueID, mapping.ID,
			params.ConnectorInstance, params.ExternalRootKey, params.ExternalAccountKey,
			receiveAfter, boolInt(params.PublishComments), publishAfter,
			params.InitialClaimToken, initialClaimStartedAt, stamp, stamp).Scan(&bindingID)
		if err != nil {
			return db.ExternalRootBinding{}, db.Event{}, classifyExternalRootBindingInsertError(err)
		}
		binding, err := externalRootBindingByIDTx(ctx, tx, bindingID)
		if err != nil {
			return db.ExternalRootBinding{}, db.Event{}, err
		}
		if params.UseCommentIdentityFrontier {
			if err := seedExternalCommentIdentityFrontierTx(ctx, tx, binding, params.InitialCommentRevisions); err != nil {
				return db.ExternalRootBinding{}, db.Event{}, err
			}
		}
		payload, err := db.MarshalExternalRootAuditPayload(binding, "bound", params.Actor, "", "")
		if err != nil {
			return db.ExternalRootBinding{}, db.Event{}, err
		}
		eventActor, err := effectiveLocalMutationActorTx(ctx, tx, issue.ProjectID, params.Actor)
		if err != nil {
			return db.ExternalRootBinding{}, db.Event{}, err
		}
		event, err := d.insertEventTx(ctx, tx, eventInsert{
			ProjectID: issue.ProjectID, ProjectUID: issue.ProjectUID, ProjectName: projectName,
			IssueID: &issue.ID, IssueUID: &issue.UID,
			Type: "issue.external_root_bound", Actor: eventActor, Payload: payload,
		})
		if err != nil {
			return db.ExternalRootBinding{}, db.Event{}, err
		}
		if err := tx.Commit(); err != nil {
			return db.ExternalRootBinding{}, db.Event{}, err
		}
		return binding, event, nil
	})
}

func seedExternalCommentIdentityFrontierTx(
	ctx context.Context,
	tx *sql.Tx,
	binding db.ExternalRootBinding,
	comments []db.ExternalCommentRevision,
) error {
	issueID := binding.IssueID
	source := db.ExternalRootCommentRevisionMappingSource(binding)
	if _, err := upsertImportMappingTx(ctx, tx, db.ImportMappingParams{
		Source: source, ExternalID: db.ExternalCommentFrontierExternalID,
		ObjectType: db.ExternalRevisionAnchorObjectType, ProjectID: binding.ProjectID, IssueID: &issueID,
	}); err != nil {
		return fmt.Errorf("record external comment identity frontier: %w", err)
	}
	for _, comment := range comments {
		if _, err := upsertImportMappingTx(ctx, tx, db.ImportMappingParams{
			Source:     source,
			ExternalID: db.ExternalCommentRevisionMappingExternalID(comment.ExternalID, comment.Revision),
			ObjectType: db.ExternalRevisionAnchorObjectType,
			ProjectID:  binding.ProjectID, IssueID: &issueID,
		}); err != nil {
			return fmt.Errorf("record initial external comment revision: %w", err)
		}
	}
	return nil
}

func classifyExternalRootBindingInsertError(err error) error {
	return fmt.Errorf("insert external root binding: %w", mapSQLError(err, map[string]error{
		"idx_external_root_bindings_active_issue": db.ErrExternalRootIssueAlreadyBound,
		"idx_external_root_bindings_active_root":  db.ErrExternalRootAlreadyBound,
	}))
}

func (d *Store) ExternalRootBindingByIssue(ctx context.Context, issueID int64) (db.ExternalRootBinding, error) {
	return scanExternalRootBinding((externalRootStoreQueryer{d}).QueryRowContext(ctx,
		externalRootBindingSelect+` WHERE b.issue_id=? AND b.active=1`, issueID))
}

func (d *Store) ExternalRootBindingByID(ctx context.Context, bindingID int64) (db.ExternalRootBinding, error) {
	return scanExternalRootBinding((externalRootStoreQueryer{d}).QueryRowContext(ctx,
		externalRootBindingSelect+` WHERE b.id=?`, bindingID))
}

func externalRootBindingByIDTx(ctx context.Context, tx *sql.Tx, bindingID int64) (db.ExternalRootBinding, error) {
	return scanExternalRootBinding((externalRootTxQueryer{tx}).QueryRowContext(ctx,
		externalRootBindingSelect+` WHERE b.id=?`, bindingID))
}

func (d *Store) ExternalRootBindingByExternalKey(
	ctx context.Context,
	connectorInstance string,
	externalRootKey string,
) (db.ExternalRootBinding, error) {
	return scanExternalRootBinding((externalRootStoreQueryer{d}).QueryRowContext(ctx, externalRootBindingSelect+`
 WHERE b.connector_instance=? AND b.external_root_key=? AND b.active=1`,
		strings.TrimSpace(connectorInstance), strings.TrimSpace(externalRootKey)))
}

func (d *Store) ListDueExternalRootBindings(
	ctx context.Context,
	now, staleBefore time.Time,
	limit int,
) ([]db.ExternalRootBinding, error) {
	if limit <= 0 {
		return []db.ExternalRootBinding{}, nil
	}
	rows, err := (externalRootStoreQueryer{d}).QueryContext(ctx, externalRootBindingSelect+`
 JOIN projects project ON project.id=b.project_id
 WHERE b.active=1 AND b.enabled=1 AND project.deleted_at IS NULL
   AND (b.next_attempt_at IS NULL OR b.next_attempt_at<=?)
	AND (b.claim_token='' OR (b.claim_started_at IS NOT NULL AND b.claim_started_at<?))
 ORDER BY COALESCE(b.next_attempt_at, ''), b.id
	LIMIT ?`, formatStoredTime(now), formatStoredTime(staleBefore), limit)
	if err != nil {
		return nil, fmt.Errorf("list due external root bindings: %w", err)
	}
	defer func() { _ = rows.Close() }()
	bindings := make([]db.ExternalRootBinding, 0)
	for rows.Next() {
		binding, err := scanExternalRootBinding(rows)
		if err != nil {
			return nil, err
		}
		bindings = append(bindings, binding)
	}
	return bindings, rows.Err()
}

func (d *Store) ClaimExternalRootBinding(
	ctx context.Context,
	bindingID int64,
	token string,
	now time.Time,
	staleBefore time.Time,
) (db.ExternalRootBinding, bool, error) {
	token = strings.TrimSpace(token)
	if bindingID <= 0 || token == "" || now.IsZero() || staleBefore.IsZero() {
		return db.ExternalRootBinding{}, false, fmt.Errorf("%w: binding, token, and claim times are required", db.ErrExternalRootValidation)
	}
	return retryExternalRootWrite2(ctx, d, func() (db.ExternalRootBinding, bool, error) {
		tx, err := d.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
		if err != nil {
			return db.ExternalRootBinding{}, false, err
		}
		defer func() { _ = tx.Rollback() }()
		if err := lockExternalRootClaimIssueTx(ctx, tx, bindingID); err != nil {
			return db.ExternalRootBinding{}, false, err
		}
		current, err := externalRootBindingByIDTx(ctx, tx, bindingID)
		if err != nil {
			return db.ExternalRootBinding{}, false, err
		}
		if err := lockExternalRootConnectorTx(ctx, tx, current.ConnectorInstance); err != nil {
			return db.ExternalRootBinding{}, false, err
		}
		result, err := (externalRootTxQueryer{tx}).ExecContext(ctx, `UPDATE external_root_bindings
 SET claim_token=?, claim_started_at=?, last_attempt_at=?, updated_at=?
 WHERE id=? AND active=1 AND enabled=1
   AND (next_attempt_at IS NULL OR next_attempt_at<=?)
   AND (claim_token='' OR claim_started_at<?)`,
			token, formatStoredTime(now), formatStoredTime(now), formatStoredTime(now), bindingID, formatStoredTime(now), formatStoredTime(staleBefore))
		if err != nil {
			return db.ExternalRootBinding{}, false, fmt.Errorf("claim external root binding: %w", err)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return db.ExternalRootBinding{}, false, err
		}
		binding, err := externalRootBindingByIDTx(ctx, tx, bindingID)
		if err != nil {
			return db.ExternalRootBinding{}, false, err
		}
		if err := tx.Commit(); err != nil {
			return db.ExternalRootBinding{}, false, err
		}
		return binding, affected == 1, nil
	})
}

func (d *Store) ClaimExternalRootBindingForManualReconcile(
	ctx context.Context,
	bindingID int64,
	token string,
	now time.Time,
	staleBefore time.Time,
) (db.ExternalRootBinding, bool, error) {
	return d.claimExternalRootBindingWithoutDue(ctx, bindingID, token, now, staleBefore, true, true, true)
}

func (d *Store) ClaimExternalRootBindingForManualAction(
	ctx context.Context,
	bindingID int64,
	token string,
	now time.Time,
	staleBefore time.Time,
) (db.ExternalRootBinding, bool, error) {
	return d.claimExternalRootBindingWithoutDue(ctx, bindingID, token, now, staleBefore, false, false, false)
}

func (d *Store) claimExternalRootBindingWithoutDue(
	ctx context.Context,
	bindingID int64,
	token string,
	now time.Time,
	staleBefore time.Time,
	requireEnabled bool,
	recordAttempt bool,
	lockIssue bool,
) (db.ExternalRootBinding, bool, error) {
	token = strings.TrimSpace(token)
	if bindingID <= 0 || token == "" || now.IsZero() || staleBefore.IsZero() {
		return db.ExternalRootBinding{}, false, fmt.Errorf("%w: binding, token, and claim times are required", db.ErrExternalRootValidation)
	}
	enabledClause := ""
	if requireEnabled {
		enabledClause = " AND enabled=1"
	}
	setClause := "claim_token=?, claim_started_at=?, updated_at=?"
	args := []any{token, formatStoredTime(now), formatStoredTime(now), bindingID, formatStoredTime(staleBefore)}
	if recordAttempt {
		setClause = "claim_token=?, claim_started_at=?, last_attempt_at=?, updated_at=?"
		args = []any{token, formatStoredTime(now), formatStoredTime(now), formatStoredTime(now), bindingID, formatStoredTime(staleBefore)}
	}
	return retryExternalRootWrite2(ctx, d, func() (db.ExternalRootBinding, bool, error) {
		tx, err := d.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
		if err != nil {
			return db.ExternalRootBinding{}, false, err
		}
		defer func() { _ = tx.Rollback() }()
		if lockIssue {
			if err := lockExternalRootClaimIssueTx(ctx, tx, bindingID); err != nil {
				return db.ExternalRootBinding{}, false, err
			}
		}
		current, err := externalRootBindingByIDTx(ctx, tx, bindingID)
		if err != nil {
			return db.ExternalRootBinding{}, false, err
		}
		if err := lockExternalRootConnectorTx(ctx, tx, current.ConnectorInstance); err != nil {
			return db.ExternalRootBinding{}, false, err
		}
		result, err := (externalRootTxQueryer{tx}).ExecContext(ctx, `UPDATE external_root_bindings SET `+setClause+`
 WHERE id=? AND active=1`+enabledClause+`
			AND (claim_token='' OR claim_started_at<?)`, args...)
		if err != nil {
			return db.ExternalRootBinding{}, false, fmt.Errorf("claim external root binding for manual action: %w", err)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return db.ExternalRootBinding{}, false, err
		}
		binding, err := externalRootBindingByIDTx(ctx, tx, bindingID)
		if err != nil {
			return db.ExternalRootBinding{}, false, err
		}
		if err := tx.Commit(); err != nil {
			return db.ExternalRootBinding{}, false, err
		}
		return binding, affected == 1, nil
	})
}

func (d *Store) ReleaseExternalRootClaim(
	ctx context.Context,
	bindingID int64,
	token string,
) (db.ExternalRootBinding, error) {
	token = strings.TrimSpace(token)
	if bindingID <= 0 || token == "" {
		return db.ExternalRootBinding{}, fmt.Errorf("%w: binding and claim token are required", db.ErrExternalRootValidation)
	}
	return retryExternalRootWrite1(ctx, d, func() (db.ExternalRootBinding, error) {
		tx, err := d.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
		if err != nil {
			return db.ExternalRootBinding{}, err
		}
		defer func() { _ = tx.Rollback() }()
		result, err := (externalRootTxQueryer{tx}).ExecContext(ctx, `UPDATE external_root_bindings
 SET claim_token='', claim_started_at=NULL, updated_at=?
 WHERE id=? AND claim_token=? AND active=1`, nowStoredTimestamp(), bindingID, token)
		if err != nil {
			return db.ExternalRootBinding{}, fmt.Errorf("release external root claim: %w", err)
		}
		if err := requireExternalRootClaimMutation(ctx, tx, bindingID, result); err != nil {
			return db.ExternalRootBinding{}, err
		}
		binding, err := externalRootBindingByIDTx(ctx, tx, bindingID)
		if err != nil {
			return db.ExternalRootBinding{}, err
		}
		if err := tx.Commit(); err != nil {
			return db.ExternalRootBinding{}, err
		}
		return binding, nil
	})
}

func (d *Store) RenewExternalRootClaim(
	ctx context.Context,
	bindingID int64,
	token string,
	at time.Time,
) (db.ExternalRootBinding, error) {
	if bindingID <= 0 || strings.TrimSpace(token) == "" || at.IsZero() {
		return db.ExternalRootBinding{}, fmt.Errorf(
			"%w: binding, claim token, and renewal time are required", db.ErrExternalRootValidation,
		)
	}
	return d.updateClaimedExternalRootWithPolicy(ctx, bindingID, token, true,
		` claim_started_at=?, updated_at=?`, formatStoredTime(at), formatStoredTime(at))
}

func (d *Store) RecordExternalRootSuccess(
	ctx context.Context,
	params db.ExternalRootSuccessParams,
) (db.ExternalRootBinding, error) {
	if err := db.ValidateExternalRootSuccessParams(params); err != nil {
		return db.ExternalRootBinding{}, err
	}
	return d.updateClaimedExternalRoot(ctx, params.BindingID, params.ClaimToken, `
 last_attempt_at=?, last_success_at=?, last_error_at=NULL, last_error='',
 consecutive_failures=0, next_attempt_at=?, last_external_state=?,
 last_external_revision=?, claim_token='', claim_started_at=NULL, updated_at=?`,
		formatStoredTime(params.At), formatStoredTime(params.At), formatStoredTime(params.NextAttemptAt),
		params.ExternalState, params.ExternalRevision, formatStoredTime(params.At))
}

func (d *Store) RecordExternalRootError(
	ctx context.Context,
	params db.ExternalRootErrorParams,
) (db.ExternalRootBinding, error) {
	if err := db.ValidateExternalRootErrorParams(params); err != nil {
		return db.ExternalRootBinding{}, err
	}
	hasExternalState := strings.TrimSpace(params.ExternalState) != ""
	if hasExternalState {
		return d.updateClaimedExternalRoot(ctx, params.BindingID, params.ClaimToken, `
 last_attempt_at=?, last_error_at=?, last_error=?, consecutive_failures=consecutive_failures+1,
 next_attempt_at=?, last_external_state=?, last_external_revision=?,
 claim_token='', claim_started_at=NULL, updated_at=?`,
			formatStoredTime(params.At), formatStoredTime(params.At), db.SafeExternalRootError(params.Error),
			formatStoredTime(params.NextAttemptAt), params.ExternalState, params.ExternalRevision, formatStoredTime(params.At))
	}
	return d.updateClaimedExternalRoot(ctx, params.BindingID, params.ClaimToken, `
 last_attempt_at=?, last_error_at=?, last_error=?, consecutive_failures=consecutive_failures+1,
 next_attempt_at=?, claim_token='', claim_started_at=NULL, updated_at=?`,
		formatStoredTime(params.At), formatStoredTime(params.At), db.SafeExternalRootError(params.Error),
		formatStoredTime(params.NextAttemptAt), formatStoredTime(params.At))
}

func (d *Store) ApplyExternalRootProjection(
	ctx context.Context,
	params db.ExternalRootProjectionParams,
) (db.Issue, *db.Event, bool, error) {
	if err := db.ValidateExternalRootProjectionParams(params); err != nil {
		return db.Issue{}, nil, false, err
	}
	return retryExternalRootWrite3(ctx, d, func() (db.Issue, *db.Event, bool, error) {
		tx, err := d.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
		if err != nil {
			return db.Issue{}, nil, false, err
		}
		defer func() { _ = tx.Rollback() }()
		binding, issue, project, err := externalRootProjectionContext(
			ctx, tx, params.BindingID, params.ClaimToken, params.IntegrationActor,
		)
		if err != nil {
			return db.Issue{}, nil, false, err
		}
		source := "connector:" + binding.ConnectorInstance
		mapping, err := importMappingBySourceTx(
			ctx, tx, binding.ProjectID, source, "issue", binding.ExternalRootKey,
		)
		if err != nil {
			return db.Issue{}, nil, false, err
		}
		if mapping.ID != binding.RootMappingID || mapping.IssueID == nil || *mapping.IssueID != issue.ID {
			return db.Issue{}, nil, false, fmt.Errorf(
				"%w: external root mapping does not match binding", db.ErrExternalRootValidation,
			)
		}
		revisionSeen, err := externalRootRevisionSeenTx(ctx, tx, binding, params.ExternalRevision)
		if err != nil {
			return db.Issue{}, nil, false, err
		}
		if mapping.SourceUpdatedAt != nil && params.ExternalUpdatedAt.Before(*mapping.SourceUpdatedAt) {
			return db.Issue{}, nil, false, fmt.Errorf(
				"%w: external root observation is stale", db.ErrExternalRootValidation,
			)
		}
		if revisionSeen &&
			(issue.Title != params.Title || issue.Body != params.Body) {
			return db.Issue{}, nil, false, fmt.Errorf(
				"%w: external root revision changed content", db.ErrExternalRootValidation,
			)
		}
		issueID := issue.ID
		sourceUpdatedAt := params.ExternalUpdatedAt.UTC()
		if _, err := upsertImportMappingTx(ctx, tx, db.ImportMappingParams{
			Source: source, ExternalID: binding.ExternalRootKey,
			ObjectType: "issue", ProjectID: binding.ProjectID, IssueID: &issueID,
			SourceUpdatedAt: &sourceUpdatedAt,
		}); err != nil {
			return db.Issue{}, nil, false, err
		}
		if err := recordExternalRootRevisionTx(ctx, tx, binding, params.ExternalRevision); err != nil {
			return db.Issue{}, nil, false, err
		}
		if issue.Title == params.Title && issue.Body == params.Body {
			if err := tx.Commit(); err != nil {
				return db.Issue{}, nil, false, err
			}
			return issue, nil, false, nil
		}
		updatedAt := nowStoredTimestamp()
		if _, err := tx.ExecContext(ctx, `UPDATE issues
 SET title=$1, body=$2, updated_at=$3, content_revision=content_revision+1 WHERE id=$4`,
			params.Title, params.Body, updatedAt, issue.ID); err != nil {
			return db.Issue{}, nil, false, fmt.Errorf("apply external root projection: %w", mapSQLError(err, nil))
		}
		payload, err := db.MarshalExternalRootProjectionPayload(binding, issue, params, updatedAt)
		if err != nil {
			return db.Issue{}, nil, false, err
		}
		eventActor, err := effectiveLocalMutationActorTx(ctx, tx, issue.ProjectID, params.IntegrationActor)
		if err != nil {
			return db.Issue{}, nil, false, err
		}
		event, err := d.insertEventTx(ctx, tx,
			issueEventInput(issue, project, "issue.updated", eventActor, payload))
		if err != nil {
			return db.Issue{}, nil, false, err
		}
		updated, err := scanIssue(tx.QueryRowContext(ctx, issueSelect+` WHERE i.id=$1`, issue.ID))
		if err != nil {
			return db.Issue{}, nil, false, err
		}
		if err := tx.Commit(); err != nil {
			return db.Issue{}, nil, false, err
		}
		return updated, &event, true, nil
	})
}

func externalRootRevisionSeenTx(
	ctx context.Context,
	query rowQueryer,
	binding db.ExternalRootBinding,
	revision string,
) (bool, error) {
	_, err := importMappingBySourceTx(
		ctx, query, binding.ProjectID, db.ExternalRootRevisionMappingSource(binding),
		db.ExternalRevisionAnchorObjectType,
		db.ExternalRootRevisionMappingExternalID(binding.ExternalRootKey, revision),
	)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, db.ErrNotFound) {
		return false, nil
	}
	return false, err
}

func recordExternalRootRevisionTx(
	ctx context.Context,
	query rowQueryer,
	binding db.ExternalRootBinding,
	revision string,
) error {
	issueID := binding.IssueID
	_, err := upsertImportMappingTx(ctx, query, db.ImportMappingParams{
		Source:     db.ExternalRootRevisionMappingSource(binding),
		ExternalID: db.ExternalRootRevisionMappingExternalID(binding.ExternalRootKey, revision),
		ObjectType: db.ExternalRevisionAnchorObjectType,
		ProjectID:  binding.ProjectID, IssueID: &issueID,
	})
	if err != nil {
		return fmt.Errorf("record external root revision: %w", err)
	}
	return nil
}

func (d *Store) UpsertExternalCommentProjection(
	ctx context.Context,
	params db.ExternalCommentProjectionParams,
) (db.Comment, *db.Event, bool, error) {
	if err := db.ValidateExternalCommentProjectionParams(params); err != nil {
		return db.Comment{}, nil, false, err
	}
	return retryExternalRootWrite3(ctx, d, func() (db.Comment, *db.Event, bool, error) {
		tx, err := d.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
		if err != nil {
			return db.Comment{}, nil, false, err
		}
		defer func() { _ = tx.Rollback() }()
		binding, issue, project, err := externalRootProjectionContext(
			ctx, tx, params.BindingID, params.ClaimToken, params.IntegrationActor,
		)
		if err != nil {
			return db.Comment{}, nil, false, err
		}
		integrationActor, err := effectiveLocalMutationActorTx(
			ctx, tx, issue.ProjectID, params.IntegrationActor,
		)
		if err != nil {
			return db.Comment{}, nil, false, err
		}
		source := db.ExternalRootCommentMappingSource(binding)
		mapping, mappingErr := importMappingBySourceTx(ctx, tx, binding.ProjectID, source, "comment", params.ExternalID)
		if mappingErr != nil && !errors.Is(mappingErr, db.ErrNotFound) {
			return db.Comment{}, nil, false, mappingErr
		}
		revisionSeen, err := externalCommentRevisionSeenTx(ctx, tx, binding, params.ExternalID, params.ExternalRevision)
		if err != nil {
			return db.Comment{}, nil, false, err
		}
		if errors.Is(mappingErr, db.ErrNotFound) && revisionSeen {
			return db.Comment{}, nil, false, fmt.Errorf(
				"%w: external comment revision predates the inbound projection", db.ErrExternalRootValidation,
			)
		}
		desiredBody := params.Body
		if params.Deleted {
			desiredBody = "[deleted externally]"
		}
		if mappingErr == nil {
			if mapping.IssueID == nil || *mapping.IssueID != issue.ID || mapping.CommentID == nil {
				return db.Comment{}, nil, false, fmt.Errorf("%w: external comment mapping does not match binding", db.ErrExternalRootValidation)
			}
			comment, err := scanComment(tx.QueryRowContext(ctx,
				commentSelect+` WHERE id=$1 FOR UPDATE`, *mapping.CommentID))
			if err != nil {
				return db.Comment{}, nil, false, err
			}
			if comment.IssueID != issue.ID {
				return db.Comment{}, nil, false, fmt.Errorf("%w: external comment mapping does not match binding", db.ErrExternalRootValidation)
			}
			if mapping.SourceUpdatedAt != nil && params.ExternalUpdatedAt.Before(*mapping.SourceUpdatedAt) {
				if err := tx.Commit(); err != nil {
					return db.Comment{}, nil, false, err
				}
				return comment, nil, false, nil
			}
			if revisionSeen && comment.Body != desiredBody {
				return db.Comment{}, nil, false, fmt.Errorf(
					"%w: external comment revision changed content", db.ErrExternalRootValidation,
				)
			}
			commentID := comment.ID
			issueID := issue.ID
			sourceUpdatedAt := params.ExternalUpdatedAt.UTC()
			if _, err := upsertImportMappingTx(ctx, tx, db.ImportMappingParams{
				Source: source, ExternalID: params.ExternalID, ObjectType: "comment",
				ProjectID: binding.ProjectID, IssueID: &issueID, CommentID: &commentID,
				SourceUpdatedAt: &sourceUpdatedAt,
			}); err != nil {
				return db.Comment{}, nil, false, err
			}
			if err := recordExternalCommentRevisionTx(ctx, tx, binding, params.ExternalID, params.ExternalRevision); err != nil {
				return db.Comment{}, nil, false, err
			}
			if comment.Body == desiredBody {
				if err := tx.Commit(); err != nil {
					return db.Comment{}, nil, false, err
				}
				return comment, nil, false, nil
			}
			mutationAt := nowStoredTimestamp()
			comment, err = scanComment(tx.QueryRowContext(ctx, `UPDATE comments SET body=$1 WHERE id=$2
 RETURNING id,uid,issue_id,author,body,created_at`, desiredBody, comment.ID))
			if err != nil {
				return db.Comment{}, nil, false, err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE issues SET updated_at=$1 WHERE id=$2`, mutationAt, issue.ID); err != nil {
				return db.Comment{}, nil, false, fmt.Errorf("touch issue: %w", mapSQLError(err, nil))
			}
			payload, err := db.MarshalExternalCommentProjectionPayload(binding, comment, params, mutationAt, false)
			if err != nil {
				return db.Comment{}, nil, false, err
			}
			event, err := d.insertEventTx(ctx, tx,
				issueEventInput(issue, project, "issue.comment_edited", integrationActor, payload))
			if err != nil {
				return db.Comment{}, nil, false, err
			}
			if err := tx.Commit(); err != nil {
				return db.Comment{}, nil, false, err
			}
			return comment, &event, true, nil
		}

		commentUID, err := katauid.New()
		if err != nil {
			return db.Comment{}, nil, false, fmt.Errorf("generate external comment projection uid: %w", err)
		}
		comment, err := scanComment(tx.QueryRowContext(ctx, `INSERT INTO comments(uid,issue_id,author,body,created_at)
 VALUES($1,$2,$3,$4,$5) RETURNING id,uid,issue_id,author,body,created_at`,
			commentUID, issue.ID, integrationActor, desiredBody, formatExternalObservationTime(params.ExternalCreatedAt)))
		if err != nil {
			return db.Comment{}, nil, false, fmt.Errorf("insert external comment projection: %w", err)
		}
		commentID := comment.ID
		issueID := issue.ID
		sourceUpdatedAt := params.ExternalUpdatedAt.UTC()
		if _, err := upsertImportMappingTx(ctx, tx, db.ImportMappingParams{
			Source: source, ExternalID: params.ExternalID, ObjectType: "comment",
			ProjectID: binding.ProjectID, IssueID: &issueID, CommentID: &commentID,
			SourceUpdatedAt: &sourceUpdatedAt,
		}); err != nil {
			return db.Comment{}, nil, false, err
		}
		if err := recordExternalCommentRevisionTx(ctx, tx, binding, params.ExternalID, params.ExternalRevision); err != nil {
			return db.Comment{}, nil, false, err
		}
		mutationAt := nowStoredTimestamp()
		if _, err := tx.ExecContext(ctx, `UPDATE issues SET updated_at=$1 WHERE id=$2`, mutationAt, issue.ID); err != nil {
			return db.Comment{}, nil, false, fmt.Errorf("touch issue: %w", mapSQLError(err, nil))
		}
		payload, err := db.MarshalExternalCommentProjectionPayload(binding, comment, params, mutationAt, true)
		if err != nil {
			return db.Comment{}, nil, false, err
		}
		event, err := d.insertEventTx(ctx, tx,
			issueEventInput(issue, project, "issue.commented", integrationActor, payload))
		if err != nil {
			return db.Comment{}, nil, false, err
		}
		if err := tx.Commit(); err != nil {
			return db.Comment{}, nil, false, err
		}
		return comment, &event, true, nil
	})
}

func externalCommentRevisionSeenTx(
	ctx context.Context,
	query rowQueryer,
	binding db.ExternalRootBinding,
	externalID string,
	revision string,
) (bool, error) {
	_, err := importMappingBySourceTx(
		ctx, query, binding.ProjectID, db.ExternalRootCommentRevisionMappingSource(binding),
		db.ExternalRevisionAnchorObjectType,
		db.ExternalCommentRevisionMappingExternalID(externalID, revision),
	)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, db.ErrNotFound) {
		return false, nil
	}
	return false, err
}

func recordExternalCommentRevisionTx(
	ctx context.Context,
	query rowQueryer,
	binding db.ExternalRootBinding,
	externalID string,
	revision string,
) error {
	issueID := binding.IssueID
	_, err := upsertImportMappingTx(ctx, query, db.ImportMappingParams{
		Source:     db.ExternalRootCommentRevisionMappingSource(binding),
		ExternalID: db.ExternalCommentRevisionMappingExternalID(externalID, revision),
		ObjectType: db.ExternalRevisionAnchorObjectType,
		ProjectID:  binding.ProjectID, IssueID: &issueID,
	})
	if err != nil {
		return fmt.Errorf("record external comment revision: %w", err)
	}
	return nil
}

func (d *Store) EnsureExternalRootLifecycleRequest(
	ctx context.Context,
	params db.ExternalCommentProjectionParams,
) (db.Comment, []db.Event, bool, error) {
	if err := db.ValidateExternalRootLifecycleRequestParams(params); err != nil {
		return db.Comment{}, nil, false, err
	}
	return retryExternalRootWrite3(ctx, d, func() (db.Comment, []db.Event, bool, error) {
		tx, err := d.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
		if err != nil {
			return db.Comment{}, nil, false, err
		}
		defer func() { _ = tx.Rollback() }()
		binding, issue, project, err := externalRootProjectionContext(
			ctx, tx, params.BindingID, params.ClaimToken, params.IntegrationActor,
		)
		if err != nil {
			return db.Comment{}, nil, false, err
		}
		integrationActor, err := effectiveLocalMutationActorTx(
			ctx, tx, issue.ProjectID, params.IntegrationActor,
		)
		if err != nil {
			return db.Comment{}, nil, false, err
		}
		source := db.ExternalRootLifecycleMappingSource(binding)
		mapping, mappingErr := importMappingBySourceTx(ctx, tx, binding.ProjectID, source, "comment", params.ExternalID)
		if mappingErr == nil {
			if mapping.IssueID == nil || *mapping.IssueID != issue.ID || mapping.CommentID == nil {
				return db.Comment{}, nil, false, fmt.Errorf("%w: external lifecycle mapping does not match binding", db.ErrExternalRootValidation)
			}
			comment, err := scanComment(tx.QueryRowContext(ctx,
				commentSelect+` WHERE id=$1 FOR UPDATE`, *mapping.CommentID))
			if err != nil {
				return db.Comment{}, nil, false, err
			}
			if comment.IssueID != issue.ID {
				return db.Comment{}, nil, false, fmt.Errorf("%w: external lifecycle mapping does not match binding", db.ErrExternalRootValidation)
			}
			if err := tx.Commit(); err != nil {
				return db.Comment{}, nil, false, err
			}
			return comment, nil, false, nil
		}
		if !errors.Is(mappingErr, db.ErrNotFound) {
			return db.Comment{}, nil, false, mappingErr
		}
		mutationAt := nowStoredTimestamp()
		if _, err := tx.ExecContext(ctx, `UPDATE external_root_bindings
		   SET last_external_state=$1, last_external_revision=$2, updated_at=$3
		 WHERE id=$4 AND claim_token=$5 AND active=1 AND enabled=1`,
			params.LifecycleState, params.ExternalRevision, mutationAt,
			binding.ID, params.ClaimToken); err != nil {
			return db.Comment{}, nil, false, fmt.Errorf(
				"record external lifecycle transition: %w", mapSQLError(err, nil),
			)
		}

		commentUID, err := katauid.New()
		if err != nil {
			return db.Comment{}, nil, false, fmt.Errorf("generate external lifecycle comment uid: %w", err)
		}
		comment, err := scanComment(tx.QueryRowContext(ctx, `INSERT INTO comments(uid,issue_id,author,body,created_at)
 VALUES($1,$2,$3,$4,$5) RETURNING id,uid,issue_id,author,body,created_at`,
			commentUID, issue.ID, integrationActor, params.Body, formatExternalObservationTime(params.ExternalCreatedAt)))
		if err != nil {
			return db.Comment{}, nil, false, fmt.Errorf("insert external lifecycle comment: %w", err)
		}
		commentID := comment.ID
		issueID := issue.ID
		sourceUpdatedAt := params.ExternalUpdatedAt.UTC()
		if _, err := upsertImportMappingTx(ctx, tx, db.ImportMappingParams{
			Source: source, ExternalID: params.ExternalID, ObjectType: "comment",
			ProjectID: binding.ProjectID, IssueID: &issueID, CommentID: &commentID,
			SourceUpdatedAt: &sourceUpdatedAt,
		}); err != nil {
			return db.Comment{}, nil, false, err
		}
		commentPayload, err := db.MarshalExternalCommentProjectionPayload(binding, comment, params, mutationAt, true)
		if err != nil {
			return db.Comment{}, nil, false, err
		}
		commentEvent, err := d.insertEventTx(ctx, tx,
			issueEventInput(issue, project, "issue.commented", integrationActor, commentPayload))
		if err != nil {
			return db.Comment{}, nil, false, err
		}
		events := []db.Event{commentEvent}
		_, labelErr := scanLabel(tx.QueryRowContext(ctx,
			labelSelect+` WHERE issue_id=$1 AND label=$2 FOR UPDATE`, issue.ID, "needs-review"))
		if labelErr != nil && !errors.Is(labelErr, db.ErrNotFound) {
			return db.Comment{}, nil, false, labelErr
		}
		if errors.Is(labelErr, db.ErrNotFound) {
			if _, err := scanLabel(tx.QueryRowContext(ctx, `INSERT INTO issue_labels(issue_id,label,author)
 VALUES($1,$2,$3) RETURNING issue_id,label,author,created_at`,
				issue.ID, "needs-review", integrationActor)); err != nil {
				return db.Comment{}, nil, false, mapSQLError(err, labelConstraintErrors)
			}
			labelPayload, err := json.Marshal(map[string]string{
				"issue_uid": issue.UID, "label": "needs-review", "updated_at": mutationAt,
			})
			if err != nil {
				return db.Comment{}, nil, false, fmt.Errorf("marshal external review label payload: %w", err)
			}
			labelEvent, err := d.insertEventTx(ctx, tx,
				issueEventInput(issue, project, "issue.labeled", integrationActor, string(labelPayload)))
			if err != nil {
				return db.Comment{}, nil, false, err
			}
			events = append(events, labelEvent)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE issues SET updated_at=$1 WHERE id=$2`, mutationAt, issue.ID); err != nil {
			return db.Comment{}, nil, false, mapSQLError(err, nil)
		}
		if err := tx.Commit(); err != nil {
			return db.Comment{}, nil, false, err
		}
		return comment, events, true, nil
	})
}

func externalRootProjectionContext(
	ctx context.Context,
	tx *sql.Tx,
	bindingID int64,
	claimToken string,
	integrationActor string,
) (db.ExternalRootBinding, db.Issue, db.Project, error) {
	binding, err := scanExternalRootBinding((externalRootTxQueryer{tx}).QueryRowContext(ctx,
		externalRootBindingSelect+` WHERE b.id=? FOR UPDATE`, bindingID))
	if err != nil {
		return db.ExternalRootBinding{}, db.Issue{}, db.Project{}, err
	}
	if binding.ClaimToken != claimToken || !binding.Active || !binding.Enabled {
		return db.ExternalRootBinding{}, db.Issue{}, db.Project{}, db.ErrExternalRootClaimLost
	}
	if integrationActor != "connector:"+binding.ConnectorInstance {
		return db.ExternalRootBinding{}, db.Issue{}, db.Project{}, fmt.Errorf(
			"%w: integration actor does not match connector instance", db.ErrExternalRootValidation,
		)
	}
	// lockedIssueTx applies the same federated-spoke write gate used by native
	// issue mutations before any claimed projection can commit.
	issue, project, err := lockedIssueTx(ctx, tx, binding.IssueID, false)
	return binding, issue, project, err
}

func (d *Store) updateClaimedExternalRoot(
	ctx context.Context,
	bindingID int64,
	claimToken string,
	setClause string,
	args ...any,
) (db.ExternalRootBinding, error) {
	return d.updateClaimedExternalRootWithPolicy(ctx, bindingID, claimToken, false, setClause, args...)
}

func (d *Store) updateClaimedExternalRootWithPolicy(
	ctx context.Context,
	bindingID int64,
	claimToken string,
	allowDisabled bool,
	setClause string,
	args ...any,
) (db.ExternalRootBinding, error) {
	return retryExternalRootWrite1(ctx, d, func() (db.ExternalRootBinding, error) {
		tx, err := d.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
		if err != nil {
			return db.ExternalRootBinding{}, err
		}
		defer func() { _ = tx.Rollback() }()
		current, err := externalRootBindingByIDTx(ctx, tx, bindingID)
		if err != nil {
			return db.ExternalRootBinding{}, err
		}
		if err := lockExternalRootConnectorTx(ctx, tx, current.ConnectorInstance); err != nil {
			return db.ExternalRootBinding{}, err
		}
		queryArgs := append(append([]any(nil), args...), bindingID, claimToken)
		enabledPredicate := " AND enabled=1"
		if allowDisabled {
			enabledPredicate = ""
		}
		result, err := (externalRootTxQueryer{tx}).ExecContext(ctx, `UPDATE external_root_bindings SET `+setClause+`
 WHERE id=? AND claim_token=? AND active=1`+enabledPredicate, queryArgs...)
		if err != nil {
			return db.ExternalRootBinding{}, fmt.Errorf("update claimed external root binding: %w", err)
		}
		if err := requireExternalRootClaimMutation(ctx, tx, bindingID, result); err != nil {
			return db.ExternalRootBinding{}, err
		}
		binding, err := externalRootBindingByIDTx(ctx, tx, bindingID)
		if err != nil {
			return db.ExternalRootBinding{}, err
		}
		if err := tx.Commit(); err != nil {
			return db.ExternalRootBinding{}, err
		}
		return binding, nil
	})
}

func requireExternalRootClaimMutation(
	ctx context.Context,
	tx *sql.Tx,
	bindingID int64,
	result sql.Result,
) error {
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 1 {
		return nil
	}
	if _, err := externalRootBindingByIDTx(ctx, tx, bindingID); err != nil {
		return err
	}
	return db.ErrExternalRootClaimLost
}

func (d *Store) PauseExternalRootBinding(
	ctx context.Context,
	params db.ExternalRootActionParams,
) (db.ExternalRootBinding, db.Event, error) {
	return d.externalRootAction(ctx, params, "paused", "issue.external_root_paused", `
 enabled=0, paused_reason=?, claim_token='', claim_started_at=NULL, updated_at=?`, params.Reason)
}

func (d *Store) ResumeExternalRootBinding(
	ctx context.Context,
	params db.ExternalRootActionParams,
) (db.ExternalRootBinding, db.Event, error) {
	return d.externalRootAction(ctx, params, "resumed", "issue.external_root_resumed", `
 enabled=1, paused_reason='', next_attempt_at=NULL, updated_at=?`)
}

func (d *Store) UnbindExternalRootBinding(
	ctx context.Context,
	params db.ExternalRootActionParams,
) (db.ExternalRootBinding, db.Event, error) {
	return d.externalRootAction(ctx, params, "unbound", "issue.external_root_unbound", `
 active=0, enabled=0, claim_token='', claim_started_at=NULL, unbound_at=?, updated_at=?`)
}

func (d *Store) externalRootAction(
	ctx context.Context,
	params db.ExternalRootActionParams,
	action string,
	eventType string,
	setClause string,
	values ...any,
) (db.ExternalRootBinding, db.Event, error) {
	params.Actor = strings.TrimSpace(params.Actor)
	params.ClaimToken = strings.TrimSpace(params.ClaimToken)
	if params.BindingID <= 0 || params.Actor == "" {
		return db.ExternalRootBinding{}, db.Event{}, fmt.Errorf("%w: binding and actor are required", db.ErrExternalRootValidation)
	}
	return retryExternalRootWrite2(ctx, d, func() (db.ExternalRootBinding, db.Event, error) {
		tx, err := d.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
		if err != nil {
			return db.ExternalRootBinding{}, db.Event{}, err
		}
		defer func() { _ = tx.Rollback() }()
		current, err := externalRootBindingByIDTx(ctx, tx, params.BindingID)
		if err != nil {
			return db.ExternalRootBinding{}, db.Event{}, err
		}
		if action == "unbound" && current.PendingCommentUID != "" {
			return db.ExternalRootBinding{}, db.Event{}, db.ErrExternalCommentPending
		}
		guardOperatorClaim := params.ClaimToken == "" && (action == "paused" || action == "unbound")
		staleBefore := params.StaleBefore
		if staleBefore.IsZero() {
			staleBefore = time.Now().UTC().Add(-db.ExternalRootClaimStaleAfter)
		}
		if guardOperatorClaim && db.ExternalRootClaimIsFresh(current, staleBefore) {
			return db.ExternalRootBinding{}, db.Event{}, db.ErrExternalRootClaimActive
		}
		if params.ClaimToken != "" &&
			(current.ClaimToken != params.ClaimToken || !current.Active || !current.Enabled) {
			return db.ExternalRootBinding{}, db.Event{}, db.ErrExternalRootClaimLost
		}
		issue, projectName, err := externalRootIssueForEvent(ctx, tx, current.IssueID)
		if err != nil {
			return db.ExternalRootBinding{}, db.Event{}, err
		}
		stamp := nowStoredTimestamp()
		args := append([]any{}, values...)
		placeholderCount := strings.Count(setClause, "?")
		for len(args) < placeholderCount {
			args = append(args, stamp)
		}
		args = append(args, params.BindingID)
		where := ` WHERE id=? AND active=1`
		if params.ClaimToken != "" {
			where += ` AND enabled=1 AND claim_token=?`
			args = append(args, params.ClaimToken)
		} else if guardOperatorClaim {
			where += ` AND (claim_token='' OR claim_started_at<?)`
			args = append(args, formatStoredTime(staleBefore))
		}
		result, err := (externalRootTxQueryer{tx}).ExecContext(ctx,
			`UPDATE external_root_bindings SET `+setClause+where, args...)
		if err != nil {
			return db.ExternalRootBinding{}, db.Event{}, fmt.Errorf("%s external root binding: %w", action, err)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return db.ExternalRootBinding{}, db.Event{}, err
		}
		if affected != 1 {
			if guardOperatorClaim {
				latest, readErr := externalRootBindingByIDTx(ctx, tx, params.BindingID)
				if readErr == nil && db.ExternalRootClaimIsFresh(latest, staleBefore) {
					return db.ExternalRootBinding{}, db.Event{}, db.ErrExternalRootClaimActive
				}
			}
			return db.ExternalRootBinding{}, db.Event{}, db.ErrNotFound
		}
		binding, err := externalRootBindingByIDTx(ctx, tx, params.BindingID)
		if err != nil {
			return db.ExternalRootBinding{}, db.Event{}, err
		}
		payload, err := db.MarshalExternalRootAuditPayload(binding, action, params.Actor, "", "")
		if err != nil {
			return db.ExternalRootBinding{}, db.Event{}, err
		}
		eventActor, err := effectiveLocalMutationActorTx(ctx, tx, issue.ProjectID, params.Actor)
		if err != nil {
			return db.ExternalRootBinding{}, db.Event{}, err
		}
		event, err := d.insertEventTx(ctx, tx, eventInsert{
			ProjectID: issue.ProjectID, ProjectUID: issue.ProjectUID, ProjectName: projectName,
			IssueID: &issue.ID, IssueUID: &issue.UID,
			Type: eventType, Actor: eventActor, Payload: payload,
		})
		if err != nil {
			return db.ExternalRootBinding{}, db.Event{}, err
		}
		if err := tx.Commit(); err != nil {
			return db.ExternalRootBinding{}, db.Event{}, err
		}
		return binding, event, nil
	})
}

func (d *Store) nextExternalFieldMappingTimestamp(
	ctx context.Context,
	tx *sql.Tx,
	params db.ExternalFieldMappingParams,
) (string, error) {
	stamp := d.externalRootTimestamp()
	rows, err := tx.QueryContext(ctx, `SELECT created_at FROM external_field_mappings
WHERE connector_instance=$1 AND kata_field=$2`, params.ConnectorInstance, params.KataField)
	if err != nil {
		return "", fmt.Errorf("read latest external field mapping identity: %w", mapSQLError(err, nil))
	}
	defer func() { _ = rows.Close() }()
	stampAt, err := parseStoredTime(stamp)
	if err != nil {
		return "", err
	}
	latestAt := time.Time{}
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return "", fmt.Errorf("read latest external field mapping identity: %w", mapSQLError(err, nil))
		}
		createdAt, err := parseStoredTime(raw)
		if err != nil {
			return "", fmt.Errorf("parse latest external field mapping identity: %w", err)
		}
		if createdAt.After(latestAt) {
			latestAt = createdAt
		}
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("read latest external field mapping identity: %w", mapSQLError(err, nil))
	}
	if !latestAt.IsZero() && !stampAt.After(latestAt) {
		stamp = formatStoredTime(latestAt.Add(time.Millisecond))
	}
	return stamp, nil
}

func (d *Store) SetPendingExternalComment(
	ctx context.Context,
	params db.SetPendingExternalCommentParams,
) (db.ExternalRootBinding, error) {
	if params.BindingID <= 0 || strings.TrimSpace(params.ClaimToken) == "" ||
		strings.TrimSpace(params.CommentUID) == "" || params.At.IsZero() {
		return db.ExternalRootBinding{}, fmt.Errorf("%w: binding, claim, comment, and timestamp are required", db.ErrExternalRootValidation)
	}
	return retryExternalRootWrite1(ctx, d, func() (db.ExternalRootBinding, error) {
		tx, err := d.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
		if err != nil {
			return db.ExternalRootBinding{}, err
		}
		defer func() { _ = tx.Rollback() }()
		current, err := externalRootBindingByIDTx(ctx, tx, params.BindingID)
		if err != nil {
			return db.ExternalRootBinding{}, err
		}
		if current.ClaimToken != params.ClaimToken || !current.Active || !current.Enabled {
			return db.ExternalRootBinding{}, db.ErrExternalRootClaimLost
		}
		if _, err := externalRootCommentByUIDForIssueTx(ctx, tx, current.IssueID, params.CommentUID); err != nil {
			return db.ExternalRootBinding{}, err
		}
		if current.PendingCommentUID != "" {
			if current.PendingCommentUID == params.CommentUID {
				if err := tx.Commit(); err != nil {
					return db.ExternalRootBinding{}, err
				}
				return current, nil
			}
			return db.ExternalRootBinding{}, db.ErrExternalCommentPending
		}
		_, err = (externalRootTxQueryer{tx}).ExecContext(ctx, `UPDATE external_root_bindings
 SET pending_comment_uid=?, pending_comment_started_at=?, updated_at=?
 WHERE id=? AND claim_token=? AND pending_comment_uid=''`,
			params.CommentUID, formatStoredTime(params.At), formatStoredTime(params.At), params.BindingID, params.ClaimToken)
		if err != nil {
			return db.ExternalRootBinding{}, fmt.Errorf("set pending external comment: %w", err)
		}
		binding, err := externalRootBindingByIDTx(ctx, tx, params.BindingID)
		if err != nil {
			return db.ExternalRootBinding{}, err
		}
		if binding.PendingCommentUID != params.CommentUID {
			return db.ExternalRootBinding{}, db.ErrExternalCommentPending
		}
		if err := tx.Commit(); err != nil {
			return db.ExternalRootBinding{}, err
		}
		return binding, nil
	})
}

func (d *Store) ClearPendingExternalComment(
	ctx context.Context,
	params db.ClearPendingExternalCommentParams,
) (db.ExternalRootBinding, db.Event, error) {
	params.Action = strings.TrimSpace(params.Action)
	params.Actor = strings.TrimSpace(params.Actor)
	if params.BindingID <= 0 || strings.TrimSpace(params.ClaimToken) == "" ||
		strings.TrimSpace(params.CommentUID) == "" || params.Actor == "" || params.At.IsZero() {
		return db.ExternalRootBinding{}, db.Event{}, fmt.Errorf("%w: binding, claim, comment, actor, and timestamp are required", db.ErrExternalRootValidation)
	}
	switch params.Action {
	case "published", "adopt", "retry", "skip":
	default:
		return db.ExternalRootBinding{}, db.Event{}, fmt.Errorf("%w: unsupported pending comment action", db.ErrExternalRootValidation)
	}
	return retryExternalRootWrite2(ctx, d, func() (db.ExternalRootBinding, db.Event, error) {
		tx, err := d.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
		if err != nil {
			return db.ExternalRootBinding{}, db.Event{}, err
		}
		defer func() { _ = tx.Rollback() }()
		current, err := externalRootBindingByIDTx(ctx, tx, params.BindingID)
		if err != nil {
			return db.ExternalRootBinding{}, db.Event{}, err
		}
		if current.ClaimToken != params.ClaimToken || !current.Active {
			return db.ExternalRootBinding{}, db.Event{}, db.ErrExternalRootClaimLost
		}
		if current.PendingCommentUID != params.CommentUID {
			return db.ExternalRootBinding{}, db.Event{}, db.ErrExternalCommentPending
		}
		comment, err := externalRootCommentByUIDForIssueTx(ctx, tx, current.IssueID, params.CommentUID)
		if err != nil {
			return db.ExternalRootBinding{}, db.Event{}, err
		}
		requiresMapping := params.Action == "published" || params.Action == "adopt" || params.Action == "skip"
		requiresExpectedBody := params.Action == "published" || params.Action == "adopt"
		requiresExternalRevision := params.Action == "published" || params.Action == "adopt"
		if requiresExpectedBody && (params.ExpectedBody == "" || comment.Body != params.ExpectedBody) {
			return db.ExternalRootBinding{}, db.Event{}, fmt.Errorf(
				"%w: pending comment changed during publication", db.ErrExternalRootValidation,
			)
		}
		if requiresMapping && params.Mapping == nil {
			return db.ExternalRootBinding{}, db.Event{}, fmt.Errorf("%w: pending comment action requires a mapping", db.ErrExternalRootValidation)
		}
		if !requiresMapping && params.Mapping != nil {
			return db.ExternalRootBinding{}, db.Event{}, fmt.Errorf("%w: pending comment action forbids a mapping", db.ErrExternalRootValidation)
		}
		if requiresExternalRevision != (strings.TrimSpace(params.ExternalRevision) != "" && strings.TrimSpace(params.ExternalRevision) == params.ExternalRevision) {
			return db.ExternalRootBinding{}, db.Event{}, fmt.Errorf("%w: pending comment revision does not match action", db.ErrExternalRootValidation)
		}
		if params.Mapping != nil {
			mapping := *params.Mapping
			if err := validateExternalCommentMapping(current, comment, params.Action, mapping); err != nil {
				return db.ExternalRootBinding{}, db.Event{}, err
			}
			if params.Action != "skip" {
				_, err := importMappingBySourceTx(
					ctx, tx, current.ProjectID, db.ExternalRootCommentMappingSource(current), "comment", mapping.ExternalID,
				)
				if err == nil {
					return db.ExternalRootBinding{}, db.Event{}, fmt.Errorf(
						"%w: external comment identity is already mapped inbound", db.ErrExternalRootValidation,
					)
				}
				if !errors.Is(err, db.ErrNotFound) {
					return db.ExternalRootBinding{}, db.Event{}, err
				}
			}
			if err := upsertPendingExternalCommentMappingTx(ctx, tx, mapping); err != nil {
				return db.ExternalRootBinding{}, db.Event{}, err
			}
			if requiresExternalRevision {
				if err := recordExternalCommentRevisionTx(
					ctx, tx, current, mapping.ExternalID, params.ExternalRevision,
				); err != nil {
					return db.ExternalRootBinding{}, db.Event{}, err
				}
			}
		}
		pendingCommentUID := ""
		var pendingCommentStartedAt any
		if params.Action == "retry" {
			pendingCommentUID = params.CommentUID
			pendingCommentStartedAt = formatStoredTime(params.At)
		}
		result, err := (externalRootTxQueryer{tx}).ExecContext(ctx, `UPDATE external_root_bindings
 SET pending_comment_uid=?, pending_comment_started_at=?, updated_at=?
 WHERE id=? AND claim_token=? AND pending_comment_uid=?`,
			pendingCommentUID, pendingCommentStartedAt, formatStoredTime(params.At),
			params.BindingID, params.ClaimToken, params.CommentUID)
		if err != nil {
			return db.ExternalRootBinding{}, db.Event{}, fmt.Errorf("clear pending external comment: %w", err)
		}
		if err := requireExternalRootClaimMutation(ctx, tx, params.BindingID, result); err != nil {
			return db.ExternalRootBinding{}, db.Event{}, err
		}
		binding, err := externalRootBindingByIDTx(ctx, tx, params.BindingID)
		if err != nil {
			return db.ExternalRootBinding{}, db.Event{}, err
		}
		issue, projectName, err := externalRootIssueForEvent(ctx, tx, binding.IssueID)
		if err != nil {
			return db.ExternalRootBinding{}, db.Event{}, err
		}
		payload, err := db.MarshalExternalRootAuditPayload(binding, params.Action, params.Actor, params.CommentUID, "")
		if err != nil {
			return db.ExternalRootBinding{}, db.Event{}, err
		}
		eventActor, err := effectiveLocalMutationActorTx(ctx, tx, issue.ProjectID, params.Actor)
		if err != nil {
			return db.ExternalRootBinding{}, db.Event{}, err
		}
		event, err := d.insertEventTx(ctx, tx, eventInsert{
			ProjectID: issue.ProjectID, ProjectUID: issue.ProjectUID, ProjectName: projectName,
			IssueID: &issue.ID, IssueUID: &issue.UID,
			Type: "issue.external_comment_resolved", Actor: eventActor, Payload: payload,
		})
		if err != nil {
			return db.ExternalRootBinding{}, db.Event{}, err
		}
		if err := tx.Commit(); err != nil {
			return db.ExternalRootBinding{}, db.Event{}, err
		}
		return binding, event, nil
	})
}

func upsertPendingExternalCommentMappingTx(
	ctx context.Context,
	tx *sql.Tx,
	mapping db.ImportMappingParams,
) error {
	var sourceUpdatedAt any
	if mapping.SourceUpdatedAt != nil {
		sourceUpdatedAt = formatExternalObservationTime(*mapping.SourceUpdatedAt)
	}
	_, err := scanImportMapping(tx.QueryRowContext(ctx, `INSERT INTO import_mappings(
  source, external_id, object_type, project_id,
  issue_id, comment_id, link_id, label, source_updated_at
) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)
ON CONFLICT(source, external_id, object_type, project_id) DO UPDATE SET
  issue_id=excluded.issue_id,
  comment_id=excluded.comment_id,
  link_id=excluded.link_id,
  label=excluded.label,
  source_updated_at=excluded.source_updated_at,
  imported_at=to_char(now() AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"')
WHERE import_mappings.issue_id=excluded.issue_id
  AND import_mappings.comment_id=excluded.comment_id
RETURNING id, source, external_id, object_type, project_id,
          issue_id, comment_id, link_id, label, source_updated_at, imported_at`,
		mapping.Source, mapping.ExternalID, mapping.ObjectType, mapping.ProjectID,
		mapping.IssueID, mapping.CommentID, mapping.LinkID, mapping.Label, sourceUpdatedAt))
	if errors.Is(err, db.ErrNotFound) {
		return fmt.Errorf("%w: external comment identity is already mapped", db.ErrExternalRootValidation)
	}
	return err
}

func externalRootCommentByUIDForIssueTx(
	ctx context.Context,
	tx *sql.Tx,
	issueID int64,
	commentUID string,
) (db.Comment, error) {
	return scanComment(tx.QueryRowContext(ctx, commentSelect+`
 WHERE issue_id=$1 AND uid=$2 FOR SHARE`, issueID, commentUID))
}

func validateExternalCommentMapping(
	binding db.ExternalRootBinding,
	comment db.Comment,
	action string,
	mapping db.ImportMappingParams,
) error {
	if mapping.ProjectID != binding.ProjectID || mapping.IssueID == nil || *mapping.IssueID != binding.IssueID ||
		strings.TrimSpace(mapping.ExternalID) == "" || mapping.CommentID == nil || *mapping.CommentID != comment.ID {
		return fmt.Errorf("%w: pending comment mapping does not match binding", db.ErrExternalRootValidation)
	}
	wantSource := db.ExternalRootPublishedCommentMappingSource(binding)
	if action == "skip" {
		wantSource = "connector-skip:" + binding.ConnectorInstance
		if mapping.ExternalID != comment.UID || mapping.LinkID != nil || mapping.Label != nil || mapping.SourceUpdatedAt != nil {
			return fmt.Errorf("%w: pending comment mapping does not match binding", db.ErrExternalRootValidation)
		}
	}
	if mapping.Source != wantSource || mapping.ObjectType != "comment" {
		return fmt.Errorf("%w: pending comment mapping does not match binding", db.ErrExternalRootValidation)
	}
	return nil
}

func (d *Store) ApplyExternalFieldProjection(
	ctx context.Context,
	params db.ExternalFieldProjectionParams,
) (db.Issue, *db.Event, bool, error) {
	if err := db.ValidateExternalFieldProjectionParams(params); err != nil {
		return db.Issue{}, nil, false, err
	}
	for key, raw := range params.Patch {
		if err := metadata.Validate(metadata.IssueRegistry, key, raw); err != nil {
			return db.Issue{}, nil, false, fmt.Errorf("validate %q: %w", key, err)
		}
	}
	return retryExternalRootWrite3(ctx, d, func() (db.Issue, *db.Event, bool, error) {
		tx, err := d.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
		if err != nil {
			return db.Issue{}, nil, false, err
		}
		defer func() { _ = tx.Rollback() }()
		binding, mapping, issue, projectName, err := externalFieldMutationContext(
			ctx, tx, params.BindingID, params.MappingID, params.ClaimToken,
		)
		if err != nil {
			return db.Issue{}, nil, false, err
		}
		if mapping.KataField != params.KataField || params.IntegrationActor != "connector:"+binding.ConnectorInstance {
			return db.Issue{}, nil, false, fmt.Errorf(
				"%w: field projection does not match binding", db.ErrExternalRootValidation,
			)
		}
		if issue.Revision != params.ExpectedIssueRevision {
			return db.Issue{}, nil, false, &db.RevisionConflictError{CurrentRevision: issue.Revision}
		}
		if err := ensureProjectWritableTx(ctx, tx, issue.ProjectID); err != nil {
			return db.Issue{}, nil, false, err
		}
		updated, diff, err := patchedMetadata(issue.Metadata, params.Patch)
		if err != nil {
			return db.Issue{}, nil, false, err
		}
		if len(diff) == 0 {
			if err := tx.Commit(); err != nil {
				return db.Issue{}, nil, false, err
			}
			return issue, nil, false, nil
		}
		newRevision := issue.Revision + 1
		updatedAt := mutationTimestamp()
		if _, err := tx.ExecContext(ctx, `UPDATE issues
 SET metadata=$1, revision=$2, updated_at=$3 WHERE id=$4`, string(updated), newRevision, updatedAt, issue.ID); err != nil {
			return db.Issue{}, nil, false, mapSQLError(err, nil)
		}
		payload, err := json.Marshal(struct {
			Diff        map[string]metadataKeyDiffPayload `json:"diff"`
			RevisionNew int64                             `json:"revision_new"`
			UpdatedAt   string                            `json:"updated_at"`
		}{Diff: diff, RevisionNew: newRevision, UpdatedAt: updatedAt})
		if err != nil {
			return db.Issue{}, nil, false, fmt.Errorf("marshal external field projection event: %w", err)
		}
		eventActor, err := effectiveLocalMutationActorTx(ctx, tx, issue.ProjectID, params.IntegrationActor)
		if err != nil {
			return db.Issue{}, nil, false, err
		}
		event, err := d.insertEventTx(ctx, tx, eventInsert{
			ProjectID: issue.ProjectID, ProjectUID: issue.ProjectUID, ProjectName: projectName,
			IssueID: &issue.ID, IssueUID: &issue.UID,
			Type: "issue.metadata_updated", Actor: eventActor, Payload: string(payload),
		})
		if err != nil {
			return db.Issue{}, nil, false, err
		}
		projected, err := scanIssue(tx.QueryRowContext(ctx, issueSelect+` WHERE i.id=$1`, issue.ID))
		if err != nil {
			return db.Issue{}, nil, false, err
		}
		if err := tx.Commit(); err != nil {
			return db.Issue{}, nil, false, err
		}
		return projected, &event, true, nil
	})
}

func (d *Store) ListExternalFieldMappings(
	ctx context.Context,
	connectorInstance string,
) ([]db.ExternalFieldMapping, error) {
	rows, err := (externalRootStoreQueryer{d}).QueryContext(ctx, externalFieldMappingSelect+`
 WHERE m.connector_instance=? ORDER BY m.id`, strings.TrimSpace(connectorInstance))
	if err != nil {
		return nil, fmt.Errorf("list external field mappings: %w", err)
	}
	defer func() { _ = rows.Close() }()
	mappings := make([]db.ExternalFieldMapping, 0)
	for rows.Next() {
		mapping, err := scanExternalFieldMapping(rows)
		if err != nil {
			return nil, err
		}
		mappings = append(mappings, mapping)
	}
	return mappings, rows.Err()
}

func (d *Store) UpsertExternalFieldMapping(
	ctx context.Context,
	params db.ExternalFieldMappingParams,
) (db.ExternalFieldMapping, error) {
	params, err := db.NormalizeExternalFieldMappingParams(params)
	if err != nil {
		return db.ExternalFieldMapping{}, err
	}
	kinds, err := json.Marshal(params.AcceptedKinds)
	if err != nil {
		return db.ExternalFieldMapping{}, fmt.Errorf("marshal external field accepted kinds: %w", err)
	}
	return retryExternalRootWrite1(ctx, d, func() (db.ExternalFieldMapping, error) {
		tx, err := d.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
		if err != nil {
			return db.ExternalFieldMapping{}, err
		}
		defer func() { _ = tx.Rollback() }()
		if err := lockExternalRootConnectorTx(ctx, tx, params.ConnectorInstance); err != nil {
			return db.ExternalFieldMapping{}, err
		}
		existing, err := scanExternalFieldMapping((externalRootTxQueryer{tx}).QueryRowContext(ctx, externalFieldMappingSelect+`
 WHERE m.connector_instance=? AND m.kata_field=? AND m.active=1`,
			params.ConnectorInstance, params.KataField))
		hasExisting := err == nil
		if err == nil && externalFieldMappingMatches(existing, params) {
			if err := tx.Commit(); err != nil {
				return db.ExternalFieldMapping{}, err
			}
			return existing, nil
		}
		if err != nil && !errors.Is(err, db.ErrNotFound) {
			return db.ExternalFieldMapping{}, err
		}
		if err := lockAndRejectFreshExternalRootClaimsForConnectorTx(
			ctx, tx, params.ConnectorInstance, time.Now().UTC().Add(-db.ExternalRootClaimStaleAfter),
		); err != nil {
			return db.ExternalFieldMapping{}, err
		}
		stamp, err := d.nextExternalFieldMappingTimestamp(ctx, tx, params)
		if err != nil {
			return db.ExternalFieldMapping{}, err
		}
		if hasExisting {
			if _, err := (externalRootTxQueryer{tx}).ExecContext(ctx, `UPDATE external_field_mappings
 SET active=0, updated_at=? WHERE id=?`, stamp, existing.ID); err != nil {
				return db.ExternalFieldMapping{}, fmt.Errorf("deactivate external field mapping: %w", err)
			}
		}
		var mappingID int64
		err = (externalRootTxQueryer{tx}).QueryRowContext(ctx, `INSERT INTO external_field_mappings(
 connector_instance, kata_field, external_field_id, external_field_name,
 accepted_kinds_json, nullable, writable, schema_revision, created_at, updated_at
) VALUES(?,?,?,?,?,?,?,?,?,?) RETURNING id`, params.ConnectorInstance, params.KataField,
			params.ExternalFieldID, params.ExternalFieldName, string(kinds), boolInt(params.Nullable),
			boolInt(params.Writable), params.SchemaRevision, stamp, stamp).Scan(&mappingID)
		if err != nil {
			return db.ExternalFieldMapping{}, fmt.Errorf("insert external field mapping: %w", err)
		}
		mapping, err := externalFieldMappingByIDTx(ctx, tx, mappingID)
		if err != nil {
			return db.ExternalFieldMapping{}, err
		}
		if err := tx.Commit(); err != nil {
			return db.ExternalFieldMapping{}, err
		}
		return mapping, nil
	})
}

func externalFieldMappingMatches(mapping db.ExternalFieldMapping, params db.ExternalFieldMappingParams) bool {
	return mapping.ExternalFieldID == params.ExternalFieldID &&
		mapping.ExternalFieldName == params.ExternalFieldName &&
		mapping.SchemaRevision == params.SchemaRevision &&
		mapping.Nullable == params.Nullable && mapping.Writable == params.Writable &&
		slicesEqual(mapping.AcceptedKinds, params.AcceptedKinds)
}

func slicesEqual(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func externalFieldMappingByIDTx(ctx context.Context, tx *sql.Tx, mappingID int64) (db.ExternalFieldMapping, error) {
	return scanExternalFieldMapping((externalRootTxQueryer{tx}).QueryRowContext(ctx,
		externalFieldMappingSelect+` WHERE m.id=?`, mappingID))
}

func (d *Store) UnmapExternalField(
	ctx context.Context,
	connectorInstance string,
	kataField string,
) (db.ExternalFieldMapping, error) {
	return retryExternalRootWrite1(ctx, d, func() (db.ExternalFieldMapping, error) {
		tx, err := d.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
		if err != nil {
			return db.ExternalFieldMapping{}, err
		}
		defer func() { _ = tx.Rollback() }()
		connectorInstance = strings.TrimSpace(connectorInstance)
		if err := lockAndRejectFreshExternalRootClaimsForConnectorTx(
			ctx, tx, connectorInstance, time.Now().UTC().Add(-db.ExternalRootClaimStaleAfter),
		); err != nil {
			return db.ExternalFieldMapping{}, err
		}
		mapping, err := scanExternalFieldMapping((externalRootTxQueryer{tx}).QueryRowContext(ctx, externalFieldMappingSelect+`
 WHERE m.connector_instance=? AND m.kata_field=? AND m.active=1`,
			connectorInstance, strings.TrimSpace(kataField)))
		if err != nil {
			return db.ExternalFieldMapping{}, err
		}
		stamp := nowStoredTimestamp()
		if _, err := (externalRootTxQueryer{tx}).ExecContext(ctx, `UPDATE external_field_mappings
 SET active=0, updated_at=? WHERE id=?`, stamp, mapping.ID); err != nil {
			return db.ExternalFieldMapping{}, fmt.Errorf("unmap external field: %w", err)
		}
		mapping, err = externalFieldMappingByIDTx(ctx, tx, mapping.ID)
		if err != nil {
			return db.ExternalFieldMapping{}, err
		}
		if err := tx.Commit(); err != nil {
			return db.ExternalFieldMapping{}, err
		}
		return mapping, nil
	})
}

func (d *Store) ExternalFieldStates(ctx context.Context, bindingID int64) ([]db.ExternalFieldState, error) {
	rows, err := (externalRootStoreQueryer{d}).QueryContext(ctx, externalFieldStateSelect+`
 WHERE s.binding_id=? ORDER BY s.mapping_id`, bindingID)
	if err != nil {
		return nil, fmt.Errorf("list external field states: %w", err)
	}
	defer func() { _ = rows.Close() }()
	states := make([]db.ExternalFieldState, 0)
	for rows.Next() {
		state, err := scanExternalFieldState(rows)
		if err != nil {
			return nil, err
		}
		states = append(states, state)
	}
	return states, rows.Err()
}

func (d *Store) UpsertExternalFieldState(
	ctx context.Context,
	params db.ExternalFieldStateParams,
) (db.ExternalFieldState, *db.Event, error) {
	if err := db.ValidateExternalFieldStateParams(params); err != nil {
		return db.ExternalFieldState{}, nil, err
	}
	return retryExternalRootWrite2(ctx, d, func() (db.ExternalFieldState, *db.Event, error) {
		tx, err := d.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
		if err != nil {
			return db.ExternalFieldState{}, nil, err
		}
		defer func() { _ = tx.Rollback() }()
		binding, mapping, issue, projectName, err := externalFieldMutationContext(ctx, tx, params.BindingID, params.MappingID, params.ClaimToken)
		if err != nil {
			return db.ExternalFieldState{}, nil, err
		}
		previous, previousErr := scanExternalFieldState((externalRootTxQueryer{tx}).QueryRowContext(ctx, externalFieldStateSelect+`
 WHERE s.binding_id=? AND s.mapping_id=?`, params.BindingID, params.MappingID))
		if previousErr != nil && !errors.Is(previousErr, db.ErrNotFound) {
			return db.ExternalFieldState{}, nil, previousErr
		}
		if previousErr == nil && previous.Conflicted && !params.Conflicted {
			return db.ExternalFieldState{}, nil, fmt.Errorf("%w: field conflict requires explicit resolution", db.ErrExternalRootValidation)
		}
		var baseline, conflictKata, conflictExternal any
		if len(params.Baseline) > 0 {
			baseline = string(params.Baseline)
		}
		if len(params.ConflictKata) > 0 {
			conflictKata = string(params.ConflictKata)
		}
		if len(params.ConflictExternal) > 0 {
			conflictExternal = string(params.ConflictExternal)
		}
		var conflictAt any
		if params.Conflicted {
			conflictAt = formatStoredTime(params.At)
		}
		_, err = (externalRootTxQueryer{tx}).ExecContext(ctx, `INSERT INTO external_field_states(
 binding_id, mapping_id, baseline_json, conflicted, conflict_kata,
 conflict_external, conflict_at, updated_at
) VALUES(?,?,?,?,?,?,?,?)
ON CONFLICT(binding_id, mapping_id) DO UPDATE SET
 baseline_json=excluded.baseline_json, conflicted=excluded.conflicted,
 conflict_kata=excluded.conflict_kata, conflict_external=excluded.conflict_external,
 conflict_at=excluded.conflict_at, updated_at=excluded.updated_at`,
			params.BindingID, params.MappingID, baseline, boolInt(params.Conflicted),
			conflictKata, conflictExternal, conflictAt, formatStoredTime(params.At))
		if err != nil {
			return db.ExternalFieldState{}, nil, fmt.Errorf("upsert external field state: %w", err)
		}
		state, err := externalFieldStateByKeyTx(ctx, tx, params.BindingID, params.MappingID)
		if err != nil {
			return db.ExternalFieldState{}, nil, err
		}
		var event *db.Event
		if params.Conflicted && (errors.Is(previousErr, db.ErrNotFound) || !previous.Conflicted) {
			payload, err := db.MarshalExternalRootAuditPayload(binding, "conflicted", params.Actor, "", mapping.KataField)
			if err != nil {
				return db.ExternalFieldState{}, nil, err
			}
			eventActor, err := effectiveLocalMutationActorTx(ctx, tx, issue.ProjectID, params.Actor)
			if err != nil {
				return db.ExternalFieldState{}, nil, err
			}
			inserted, err := d.insertEventTx(ctx, tx, eventInsert{
				ProjectID: issue.ProjectID, ProjectUID: issue.ProjectUID, ProjectName: projectName,
				IssueID: &issue.ID, IssueUID: &issue.UID,
				Type: "issue.external_field_conflicted", Actor: eventActor, Payload: payload,
			})
			if err != nil {
				return db.ExternalFieldState{}, nil, err
			}
			event = &inserted
		}
		if err := tx.Commit(); err != nil {
			return db.ExternalFieldState{}, nil, err
		}
		return state, event, nil
	})
}

func (d *Store) ResolveExternalFieldConflict(
	ctx context.Context,
	params db.ResolveExternalFieldConflictParams,
) (db.ExternalFieldState, db.Event, error) {
	if err := db.ValidateResolveExternalFieldConflictParams(params); err != nil {
		return db.ExternalFieldState{}, db.Event{}, err
	}
	return retryExternalRootWrite2(ctx, d, func() (db.ExternalFieldState, db.Event, error) {
		tx, err := d.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
		if err != nil {
			return db.ExternalFieldState{}, db.Event{}, err
		}
		defer func() { _ = tx.Rollback() }()
		binding, mapping, issue, projectName, err := externalFieldMutationContext(ctx, tx, params.BindingID, params.MappingID, params.ClaimToken)
		if err != nil {
			return db.ExternalFieldState{}, db.Event{}, err
		}
		current, err := externalFieldStateByKeyTx(ctx, tx, params.BindingID, params.MappingID)
		if err != nil {
			return db.ExternalFieldState{}, db.Event{}, err
		}
		if !current.Conflicted {
			return db.ExternalFieldState{}, db.Event{}, db.ErrExternalFieldNotConflicted
		}
		var baseline any
		if len(params.Baseline) > 0 {
			baseline = string(params.Baseline)
		}
		_, err = (externalRootTxQueryer{tx}).ExecContext(ctx, `UPDATE external_field_states
 SET baseline_json=?, conflicted=0, conflict_kata=NULL, conflict_external=NULL,
 conflict_at=NULL, updated_at=? WHERE binding_id=? AND mapping_id=?`,
			baseline, formatStoredTime(params.At), params.BindingID, params.MappingID)
		if err != nil {
			return db.ExternalFieldState{}, db.Event{}, fmt.Errorf("resolve external field conflict: %w", err)
		}
		state, err := externalFieldStateByKeyTx(ctx, tx, params.BindingID, params.MappingID)
		if err != nil {
			return db.ExternalFieldState{}, db.Event{}, err
		}
		payload, err := db.MarshalExternalRootAuditPayload(binding, "resolved", params.Actor, "", mapping.KataField)
		if err != nil {
			return db.ExternalFieldState{}, db.Event{}, err
		}
		eventActor, err := effectiveLocalMutationActorTx(ctx, tx, issue.ProjectID, params.Actor)
		if err != nil {
			return db.ExternalFieldState{}, db.Event{}, err
		}
		event, err := d.insertEventTx(ctx, tx, eventInsert{
			ProjectID: issue.ProjectID, ProjectUID: issue.ProjectUID, ProjectName: projectName,
			IssueID: &issue.ID, IssueUID: &issue.UID,
			Type: "issue.external_field_resolved", Actor: eventActor, Payload: payload,
		})
		if err != nil {
			return db.ExternalFieldState{}, db.Event{}, err
		}
		if err := tx.Commit(); err != nil {
			return db.ExternalFieldState{}, db.Event{}, err
		}
		return state, event, nil
	})
}

func externalFieldMutationContext(
	ctx context.Context,
	tx *sql.Tx,
	bindingID int64,
	mappingID int64,
	claimToken string,
) (db.ExternalRootBinding, db.ExternalFieldMapping, db.Issue, string, error) {
	// Mapping reconfiguration fences claims before deactivating mappings. Lock
	// both rows in that order so a worker cannot commit against either stale
	// claim authority or a mapping that was rotated concurrently.
	binding, err := scanExternalRootBinding((externalRootTxQueryer{tx}).QueryRowContext(ctx,
		externalRootBindingSelect+` WHERE b.id=? FOR UPDATE`, bindingID))
	if err != nil {
		return db.ExternalRootBinding{}, db.ExternalFieldMapping{}, db.Issue{}, "", err
	}
	if binding.ClaimToken != claimToken || !binding.Active || !binding.Enabled {
		return db.ExternalRootBinding{}, db.ExternalFieldMapping{}, db.Issue{}, "", db.ErrExternalRootClaimLost
	}
	mapping, err := scanExternalFieldMapping((externalRootTxQueryer{tx}).QueryRowContext(ctx,
		externalFieldMappingSelect+` WHERE m.id=? FOR UPDATE`, mappingID))
	if err != nil {
		return db.ExternalRootBinding{}, db.ExternalFieldMapping{}, db.Issue{}, "", err
	}
	if !mapping.Active || mapping.ConnectorInstance != binding.ConnectorInstance {
		return db.ExternalRootBinding{}, db.ExternalFieldMapping{}, db.Issue{}, "",
			fmt.Errorf("%w: field mapping does not match binding", db.ErrExternalRootValidation)
	}
	issue, projectName, err := externalRootIssueForEvent(ctx, tx, binding.IssueID)
	return binding, mapping, issue, projectName, err
}

func externalRootIssueForEvent(
	ctx context.Context,
	tx *sql.Tx,
	issueID int64,
) (db.Issue, string, error) {
	issue, project, err := lockedIssueTx(ctx, tx, issueID, false)
	if err != nil {
		return db.Issue{}, "", err
	}
	return issue, project.Name, nil
}

func externalFieldStateByKeyTx(ctx context.Context, tx *sql.Tx, bindingID, mappingID int64) (db.ExternalFieldState, error) {
	return scanExternalFieldState((externalRootTxQueryer{tx}).QueryRowContext(ctx, externalFieldStateSelect+`
 WHERE s.binding_id=? AND s.mapping_id=?`, bindingID, mappingID))
}

func scanExternalRootBinding(row rowScanner) (db.ExternalRootBinding, error) {
	var binding db.ExternalRootBinding
	var active, enabled, receiveComments, publishComments, completeExternal int
	var receiveAfter, publishAfter, claimStarted, pendingStarted sql.NullString
	var lastAttempt, lastSuccess, lastErrorAt, nextAttempt, unboundAt sql.NullString
	var createdAt, updatedAt string
	err := row.Scan(
		&binding.ID, &binding.UID, &binding.ProjectID, &binding.IssueID, &binding.RootMappingID,
		&binding.ConnectorInstance, &binding.ExternalRootKey, &binding.ExternalAccountKey,
		&active, &enabled, &binding.PausedReason,
		&receiveComments, &receiveAfter, &publishComments, &publishAfter, &completeExternal,
		&binding.ClaimToken, &claimStarted, &binding.LastExternalState, &binding.LastExternalRevision,
		&binding.PendingCommentUID, &pendingStarted, &lastAttempt, &lastSuccess, &lastErrorAt,
		&binding.LastError, &binding.ConsecutiveFailures, &nextAttempt,
		&createdAt, &updatedAt, &unboundAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return db.ExternalRootBinding{}, db.ErrNotFound
	}
	if err != nil {
		return db.ExternalRootBinding{}, fmt.Errorf("scan external root binding: %w", err)
	}
	binding.Active = active == 1
	binding.Enabled = enabled == 1
	binding.ReceiveComments = receiveComments == 1
	binding.PublishComments = publishComments == 1
	binding.CompleteExternal = completeExternal == 1
	if err := assignExternalRootTime(&binding.ReceiveCommentsAfter, receiveAfter); err != nil {
		return db.ExternalRootBinding{}, err
	}
	if err := assignExternalRootTime(&binding.PublishCommentsAfter, publishAfter); err != nil {
		return db.ExternalRootBinding{}, err
	}
	if err := assignExternalRootTime(&binding.ClaimStartedAt, claimStarted); err != nil {
		return db.ExternalRootBinding{}, err
	}
	if err := assignExternalRootTime(&binding.PendingCommentStartedAt, pendingStarted); err != nil {
		return db.ExternalRootBinding{}, err
	}
	if err := assignExternalRootTime(&binding.LastAttemptAt, lastAttempt); err != nil {
		return db.ExternalRootBinding{}, err
	}
	if err := assignExternalRootTime(&binding.LastSuccessAt, lastSuccess); err != nil {
		return db.ExternalRootBinding{}, err
	}
	if err := assignExternalRootTime(&binding.LastErrorAt, lastErrorAt); err != nil {
		return db.ExternalRootBinding{}, err
	}
	if err := assignExternalRootTime(&binding.NextAttemptAt, nextAttempt); err != nil {
		return db.ExternalRootBinding{}, err
	}
	if err := assignExternalRootTime(&binding.UnboundAt, unboundAt); err != nil {
		return db.ExternalRootBinding{}, err
	}
	binding.CreatedAt, err = parseStoredTime(createdAt)
	if err != nil {
		return db.ExternalRootBinding{}, fmt.Errorf("parse external root binding created_at: %w", err)
	}
	binding.UpdatedAt, err = parseStoredTime(updatedAt)
	if err != nil {
		return db.ExternalRootBinding{}, fmt.Errorf("parse external root binding updated_at: %w", err)
	}
	return binding, nil
}

func scanExternalFieldMapping(row rowScanner) (db.ExternalFieldMapping, error) {
	var mapping db.ExternalFieldMapping
	var kinds string
	var nullable, writable, active int
	var createdAt, updatedAt string
	err := row.Scan(&mapping.ID, &mapping.ConnectorInstance, &mapping.KataField,
		&mapping.ExternalFieldID, &mapping.ExternalFieldName, &kinds,
		&nullable, &writable, &mapping.SchemaRevision, &active,
		&createdAt, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return db.ExternalFieldMapping{}, db.ErrNotFound
	}
	if err != nil {
		return db.ExternalFieldMapping{}, fmt.Errorf("scan external field mapping: %w", err)
	}
	if err := json.Unmarshal([]byte(kinds), &mapping.AcceptedKinds); err != nil {
		return db.ExternalFieldMapping{}, fmt.Errorf("decode external field accepted kinds: %w", err)
	}
	mapping.Nullable = nullable == 1
	mapping.Writable = writable == 1
	mapping.Active = active == 1
	mapping.CreatedAt, err = parseStoredTime(createdAt)
	if err != nil {
		return db.ExternalFieldMapping{}, fmt.Errorf("parse external field mapping created_at: %w", err)
	}
	mapping.UpdatedAt, err = parseStoredTime(updatedAt)
	if err != nil {
		return db.ExternalFieldMapping{}, fmt.Errorf("parse external field mapping updated_at: %w", err)
	}
	return mapping, nil
}

func scanExternalFieldState(row rowScanner) (db.ExternalFieldState, error) {
	var state db.ExternalFieldState
	var baseline, conflictKata, conflictExternal sql.NullString
	var conflicted int
	var conflictAt sql.NullString
	var updatedAt string
	err := row.Scan(&state.BindingID, &state.MappingID, &baseline, &conflicted,
		&conflictKata, &conflictExternal, &conflictAt, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return db.ExternalFieldState{}, db.ErrNotFound
	}
	if err != nil {
		return db.ExternalFieldState{}, fmt.Errorf("scan external field state: %w", err)
	}
	if baseline.Valid {
		state.Baseline = json.RawMessage(baseline.String)
	}
	if conflictKata.Valid {
		state.ConflictKata = json.RawMessage(conflictKata.String)
	}
	if conflictExternal.Valid {
		state.ConflictExternal = json.RawMessage(conflictExternal.String)
	}
	state.Conflicted = conflicted == 1
	if err := assignExternalRootTime(&state.ConflictAt, conflictAt); err != nil {
		return db.ExternalFieldState{}, err
	}
	state.UpdatedAt, err = parseStoredTime(updatedAt)
	if err != nil {
		return db.ExternalFieldState{}, fmt.Errorf("parse external field state updated_at: %w", err)
	}
	return state, nil
}

func assignExternalRootTime(target **time.Time, value sql.NullString) error {
	if !value.Valid {
		return nil
	}
	parsed, err := parseStoredTime(value.String)
	if err != nil {
		return fmt.Errorf("parse external root timestamp: %w", err)
	}
	*target = &parsed
	return nil
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
