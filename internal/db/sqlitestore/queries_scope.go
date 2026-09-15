package sqlitestore

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"go.kenn.io/kata/internal/db"
)

func appendAllowedIssueIDsSQLite(query *strings.Builder, args *[]any, issueIDs []int64) {
	if issueIDs == nil {
		return
	}
	if len(issueIDs) == 0 {
		query.WriteString(` AND 1 = 0`)
		return
	}
	encoded, _ := json.Marshal(issueIDs)
	*args = append(*args, string(encoded))
	query.WriteString(` AND i.id IN (SELECT value FROM json_each(?))`)
}

const issueScopeMembersCTE = `WITH RECURSIVE scope_members(id) AS (
 SELECT root.id FROM issues root JOIN projects project ON project.id = root.project_id
 WHERE root.uid = $1 AND project.uid = $2
  AND root.deleted_at IS NULL AND project.deleted_at IS NULL
 UNION
 SELECT child.id FROM scope_members member
 JOIN links parent ON parent.to_issue_id = member.id AND parent.type = 'parent'
 JOIN issues child ON child.id = parent.from_issue_id
 JOIN projects project ON project.id = child.project_id
 WHERE project.uid = $2 AND child.deleted_at IS NULL AND project.deleted_at IS NULL
)
`

// IssueScopedMembers reads active parent containment in one database snapshot.
func (d *Store) IssueScopedMembers(ctx context.Context, scope db.APITokenScope) ([]db.Issue, error) {
	return readIssueScopedMembers(ctx, d, scope)
}

func readIssueScopedMembers(ctx context.Context, query uiQueryer, scope db.APITokenScope) ([]db.Issue, error) {
	rows, err := query.QueryContext(ctx, issueScopeMembersCTE+issueSelect+` WHERE i.id IN (SELECT id FROM scope_members)`, scope.RootIssueUID, scope.ProjectUID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	issues := []db.Issue{}
	for rows.Next() {
		issue, err := scanIssue(rows)
		if err != nil {
			return nil, err
		}
		issues = append(issues, issue)
	}
	return issues, rows.Err()
}

// IssueInScope checks the current ancestry of every target in one query.
func (d *Store) IssueInScope(ctx context.Context, scope db.APITokenScope, issueIDs ...int64) (bool, error) {
	if len(issueIDs) == 0 {
		return true, nil
	}
	var admitted int
	encoded, _ := json.Marshal(issueIDs)
	err := d.QueryRowContext(ctx, issueScopeAncestryCTE+`
 SELECT count(DISTINCT target_id) FROM scope_ancestors WHERE uid = $1`,
		scope.RootIssueUID, scope.ProjectUID, string(encoded)).Scan(&admitted)
	return admitted == len(issueIDs), err
}

const issueScopeAncestryCTE = `WITH RECURSIVE scope_ancestors(target_id, id, uid, project_id) AS (
 SELECT i.id, i.id, i.uid, i.project_id FROM issues i JOIN projects p ON p.id = i.project_id
 WHERE i.id IN (SELECT value FROM json_each($3))
 AND i.deleted_at IS NULL AND p.deleted_at IS NULL AND p.uid = $2
 UNION
 SELECT child.target_id, parent.id, parent.uid, parent.project_id
 FROM scope_ancestors child
 JOIN links link ON link.from_issue_id = child.id AND link.type = 'parent'
 JOIN issues parent ON parent.id = link.to_issue_id AND parent.project_id = child.project_id
 WHERE child.uid <> $1 AND parent.deleted_at IS NULL
)`

func checkIssueScopeTargets(ctx context.Context, tx db.Transaction, scope db.APITokenScope, issueIDs []int64) error {
	if len(issueIDs) == 0 {
		return nil
	}
	var admitted int
	encoded, _ := json.Marshal(issueIDs)
	err := tx.QueryRowContext(ctx, issueScopeAncestryCTE+`
 SELECT count(DISTINCT target_id) FROM scope_ancestors WHERE uid = $1`,
		scope.RootIssueUID, scope.ProjectUID, string(encoded)).Scan(&admitted)
	if err != nil {
		return err
	}
	if admitted != len(issueIDs) {
		return db.ErrNotFound
	}
	return nil
}

func appendIssueScopeSQLite(query *strings.Builder, args *[]any, scope *db.APITokenScope) {
	if scope == nil {
		return
	}
	rootParam := fmt.Sprintf("?%d", len(*args)+1)
	projectParam := fmt.Sprintf("?%d", len(*args)+2)
	cte := strings.NewReplacer("$1", rootParam, "$2", projectParam).Replace(issueScopeMembersCTE)
	query.WriteString(" AND i.id IN (" + cte + " SELECT id FROM scope_members)")
	*args = append(*args, scope.RootIssueUID, scope.ProjectUID)
}

func readIssueScopeIDs(ctx context.Context, query uiQueryer, scope *db.APITokenScope, allowed []int64) ([]int64, error) {
	if scope == nil {
		return allowed, nil
	}
	members, err := readIssueScopedMembers(ctx, query, *scope)
	if err != nil {
		return nil, err
	}
	ids := make([]int64, 0, len(members))
	for _, member := range members {
		ids = append(ids, member.ID)
	}
	return ids, nil
}
