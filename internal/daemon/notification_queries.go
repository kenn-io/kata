package daemon

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strings"

	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
)

// notificationAllowedTx captures current scoped membership in the writing
// transaction. Recipient discovery never trusts a pre-transaction snapshot.
func notificationAllowedTx(ctx context.Context, tx *sql.Tx) (map[int64]bool, error) {
	scope := issueScopeFromContext(ctx)
	if scope == nil {
		return nil, nil
	}
	rows, err := tx.QueryContext(ctx, `WITH RECURSIVE members(id) AS (
 SELECT i.id FROM issues i JOIN projects p ON p.id=i.project_id WHERE i.uid=$1 AND p.uid=$2 AND i.deleted_at IS NULL AND p.deleted_at IS NULL
 UNION SELECT child.id FROM members m JOIN links l ON l.to_issue_id=m.id AND l.type='parent' JOIN issues child ON child.id=l.from_issue_id JOIN projects p ON p.id=child.project_id WHERE p.uid=$2 AND child.deleted_at IS NULL AND p.deleted_at IS NULL
 ) SELECT id FROM members`, scope.RootIssueUID, scope.ProjectUID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	allowed := map[int64]bool{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		allowed[id] = true
	}
	return allowed, rows.Err()
}

func notificationIssueAllowed(allowed map[int64]bool, id int64) bool {
	return allowed == nil || allowed[id]
}

// notificationMutationActorTx resolves the actor used by local mutations on
// a push-enabled spoke while the notification patch transaction is open.
// Event insertion applies the same binding in that transaction.
func notificationMutationActorTx(
	ctx context.Context,
	tx *sql.Tx,
	projectID int64,
	requestedActor string,
) (string, error) {
	var actor string
	err := tx.QueryRowContext(ctx, `SELECT COALESCE(bound_actor, '') FROM federation_bindings
WHERE project_id=$1 AND role=$2 AND enabled=1 AND push_enabled=1 AND relay_config IS NULL`,
		projectID, string(db.FederationRoleSpoke)).Scan(&actor)
	if errors.Is(err, sql.ErrNoRows) {
		return requestedActor, nil
	}
	if err != nil {
		return "", err
	}
	if actor = strings.TrimSpace(actor); actor != "" {
		return actor, nil
	}
	return requestedActor, nil
}

// Pin a selected comment to its currently authorized issue. B's comment
// validation handles link creation; explicit notify uses this check as well.
func notificationCommentTx(ctx context.Context, tx *sql.Tx, issue db.Issue, uid string, allowed map[int64]bool) error {
	var id int64
	err := tx.QueryRowContext(ctx, `SELECT c.issue_id FROM comments c JOIN issues i ON i.id=c.issue_id JOIN projects p ON p.id=i.project_id WHERE c.uid=$1 AND i.project_id=$2 AND i.deleted_at IS NULL AND p.deleted_at IS NULL`, uid, issue.ProjectID).Scan(&id)
	if err == sql.ErrNoRows {
		return db.ErrNotFound
	}
	if err != nil {
		return err
	}
	if !notificationIssueAllowed(allowed, id) {
		return db.ErrNotFound
	}
	db.RecordIssueScopeTarget(ctx, id)
	return nil
}

// Historical pointers follow the comment's current project, even when it is
// different from the notification's source project.
func notificationHistoryVisibleTx(ctx context.Context, tx *sql.Tx, uid string, allowed map[int64]bool) (bool, error) {
	var issueID, projectID int64
	var projectUID string
	err := tx.QueryRowContext(ctx, `SELECT c.issue_id, i.project_id, p.uid FROM comments c JOIN issues i ON i.id=c.issue_id JOIN projects p ON p.id=i.project_id WHERE c.uid=$1 AND i.deleted_at IS NULL AND p.deleted_at IS NULL`, uid).Scan(&issueID, &projectID, &projectUID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !notificationIssueAllowed(allowed, issueID) {
		return false, nil
	}
	readable, err := notificationPeerProjectReadableTx(ctx, tx, projectID, projectUID)
	if err != nil || !readable {
		return false, err
	}
	db.RecordIssueScopeTarget(ctx, issueID)
	db.RecordExposedIssue(ctx, issueID)
	return true, nil
}

// notificationPeerProjectReadableTx authorizes a peer project that discovery
// found inside its write transaction. Access admitted with the request is only
// a snapshot, so the native grant is rechecked live in tx, and a host's peer
// decision must also pass its transaction fence in tx. A fence rejection
// rolls back the whole write, like a rejection of the request's own fence:
// the fence may have written in tx, and the host may need its cleanup.
func notificationPeerProjectReadableTx(ctx context.Context, tx db.Transaction, projectID int64, projectUID string) (bool, error) {
	if err := db.CheckProjectAccessTransaction(ctx, tx, projectUID); err != nil {
		if apiErr, ok := errors.AsType[*api.APIError](err); ok && apiErr.Status == http.StatusNotFound {
			return false, nil
		}
		return false, err
	}
	readable, host, err := notificationPeerAccess(ctx, projectID, projectUID)
	if err != nil || !readable || host == nil {
		return readable, err
	}
	if host.decision.TransactionFence == nil {
		return false, api.NewError(http.StatusServiceUnavailable,
			"access_unavailable", "transaction access decision unavailable", "", nil)
	}
	if err := host.decision.TransactionFence(ctx, tx); err != nil {
		status, code, message := http.StatusServiceUnavailable, "access_unavailable", "transaction access decision unavailable"
		if errors.Is(err, ErrHostAccessDenied) {
			status, code, message = http.StatusNotFound, "not_found", "resource not found"
		}
		return false, &peerFenceRejection{err: errors.Join(api.NewError(status, code, message, "", nil), err)}
	}
	return true, nil
}

// peerFenceRejection carries a peer fence's rejection out of the write
// transaction. Stores roll back on any error but finish host cleanup only for
// their own begin-time fences, so the handler finishes this one.
type peerFenceRejection struct{ err error }

func (e *peerFenceRejection) Error() string { return e.err.Error() }
func (e *peerFenceRejection) Unwrap() error { return e.err }

// finishPeerFenceRejection runs host cleanup for a peer fence rejection after
// the store has rolled back and released the transaction's connection. Other
// errors pass through unchanged.
func finishPeerFenceRejection(ctx context.Context, err error) error {
	rejection, ok := errors.AsType[*peerFenceRejection](err)
	if !ok {
		return err
	}
	return db.FinishTransactionRollback(ctx, rejection.err, nil)
}

// Discovery reads other projects without widening the source mutation's host
// authority. A denied project contributes no recipient identities. Callers
// resolve the project UID themselves: discovery runs inside a write
// transaction, and a pooled lookup could wait on the connection it holds.
func notificationProjectReadable(ctx context.Context, projectID int64, projectUID string) (bool, error) {
	readable, _, err := notificationPeerAccess(ctx, projectID, projectUID)
	return readable, err
}

// notificationPeerAccess also returns the host state that decided the peer,
// or nil when no host governs the request.
func notificationPeerAccess(ctx context.Context, projectID int64, projectUID string) (bool, *hostAccessState, error) {
	if authorizeProjectTarget(ctx, projectUID) != nil {
		return false, nil, nil
	}
	// Native project access is settled above; keep the host check from
	// repeating it with a pooled project read.
	ctx = context.WithValue(ctx, projectAccessContextKey{}, (*ProjectAccessDecision)(nil))
	var host *hostAccessState
	if state, ok := ctx.Value(hostAccessStateContextKey{}).(*hostAccessState); ok {
		isolated := *state
		isolated.authorized = false
		isolated.request.Operation = HostOperation{ID: "showIssue", Method: http.MethodGet,
			Policy: HostOperationPolicy{Kind: hostOperationTaskRead, Capability: hostCapabilityRead}}
		host = &isolated
		ctx = context.WithValue(ctx, hostAccessStateContextKey{}, host)
	}
	_, err := authorizeHostProjectScope(ctx, []int64{projectID}, nil, false)
	if apiErr, ok := errors.AsType[*api.APIError](err); ok && apiErr.Status == http.StatusNotFound {
		return false, nil, nil
	}
	if err != nil {
		return false, nil, err
	}
	return true, host, nil
}
