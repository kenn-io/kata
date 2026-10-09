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
WHERE project_id=$1 AND role=$2 AND enabled=1 AND push_enabled=1`,
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

// Discovery reads other projects without widening the source mutation's host
// authority. A denied project contributes no recipient identities.
func notificationProjectReadable(ctx context.Context, projectID int64) (bool, error) {
	if state, ok := ctx.Value(hostAccessStateContextKey{}).(*hostAccessState); ok {
		isolated := *state
		isolated.authorized = false
		isolated.request.Operation = HostOperation{ID: "showIssue", Method: http.MethodGet,
			Policy: HostOperationPolicy{Kind: hostOperationTaskRead, Capability: hostCapabilityRead}}
		ctx = context.WithValue(ctx, hostAccessStateContextKey{}, &isolated)
	}
	_, err := authorizeHostProjectScope(ctx, []int64{projectID}, nil, false)
	if apiErr, ok := errors.AsType[*api.APIError](err); ok && apiErr.Status == http.StatusNotFound {
		return false, nil
	}
	return err == nil, err
}
