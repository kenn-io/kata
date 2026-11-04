package db

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"time"

	"go.kenn.io/kata/internal/uid"
)

// SQLiteCommentReplyTargetUIDExpr must match the expression in idx_events_reply_to.
const SQLiteCommentReplyTargetUIDExpr = `json_extract(e.payload,'$.reply_to_uid')`

// CommentGraphQuery scopes a coherent comment read to one active project.
type CommentGraphQuery struct {
	ProjectID                   int64
	AllowedIssueIDs             []int64
	IssueScope                  *APITokenScope
	IncludeDeletedSourceIssueID int64
	// SelectedIssueID limits bodies and endpoint evidence to one issue and its
	// direct neighbors. Zero retains the project graph used by selectors.
	SelectedIssueID int64
	// Reference reads at most two matching identities, without graph hydration.
	Reference *CommentGraphReference
}

// CommentGraphReference selects a full UID or an issue-qualified suffix.
type CommentGraphReference struct {
	UID          string
	Suffix       string
	IssueID      int64
	IssueShortID string
}

// CommentGraphRecord retains the comment's current owning issue and project.
type CommentGraphRecord struct {
	Comment      Comment
	IssueUID     string
	IssueShortID string
	ProjectID    int64
	ProjectUID   string
	ProjectName  string
}

// CommentGraphTarget retains lookup and lineage evidence for unavailable endpoints.
type CommentGraphTarget struct {
	Status string
	Record *CommentGraphRecord
	// ProjectID retains authority for soft-deleted targets without exposing
	// their content. Purged and pending targets have no current project.
	ProjectID int64
}

// CommentGraphData is captured wholly inside one backend read transaction.
type CommentGraphData struct {
	// CommentUIDsByProject holds each project's UIDs sharing a six-character
	// suffix with the selected issue's comments, their reply targets, or their
	// incoming replies, enough to derive the same handles as the full project.
	CommentUIDsByProject map[int64][]string
	Comments             []CommentGraphRecord
	Targets              map[string]CommentGraphTarget
}

// ReadCommentGraphTx reads graph inputs using the backend's placeholder and
// timestamp conventions. The caller resolves issue scope in the same
// transaction before calling this helper.
func ReadCommentGraphTx(
	ctx context.Context,
	tx *sql.Tx,
	query CommentGraphQuery,
	bind func(int) string,
	parseTime func(string) (time.Time, error),
	replyTargetUIDExpr string,
	attributionExpr string,
	displayDialect CommentDisplaySQLDialect,
) (CommentGraphData, error) {
	data := CommentGraphData{
		Comments:             []CommentGraphRecord{},
		Targets:              map[string]CommentGraphTarget{},
		CommentUIDsByProject: map[int64][]string{},
	}
	if query.AllowedIssueIDs != nil && len(query.AllowedIssueIDs) == 0 {
		return data, nil
	}

	projectPredicate := func(args *[]any) string {
		return AuthorizedProjectPredicate(ctx, "p.uid", func(value any) string {
			*args = append(*args, value)
			return bind(len(*args))
		})
	}
	commentGraphSelect := `SELECT c.id,c.uid,c.issue_id,c.author,c.body,c.created_at,c.teammate,c.reply_to_uid,c.reply_kind,c.edited_at,` + attributionExpr + `,i.uid,i.short_id,i.project_id,p.uid,p.name` + commentGraphFrom
	args := []any{query.ProjectID}
	where := " WHERE i.project_id=" + bind(1) + " AND p.deleted_at IS NULL AND i.deleted_at IS NULL"
	if query.IncludeDeletedSourceIssueID > 0 {
		args = append(args, query.IncludeDeletedSourceIssueID)
		where = " WHERE i.project_id=" + bind(1) + " AND p.deleted_at IS NULL AND (i.deleted_at IS NULL OR i.id=" + bind(2) + ")"
	}
	if query.AllowedIssueIDs != nil {
		marks := make([]string, len(query.AllowedIssueIDs))
		for index, id := range query.AllowedIssueIDs {
			args = append(args, id)
			marks[index] = bind(len(args))
		}
		where += " AND i.id IN (" + strings.Join(marks, ",") + ")"
	}
	where += " AND " + projectPredicate(&args)
	if query.Reference != nil {
		ref := query.Reference
		if ref.UID != "" {
			where += " AND " + CommentUIDLookupPredicate("c.uid", ref.UID, &args, bind, displayDialect)
			// The suffix equality enables the handle index; the identity predicate enforces ASCII-only case folding.
			args = append(args, strings.ToUpper(ref.UID))
			where += " AND lower(substr(c.uid,21))=lower(substr(" + bind(len(args)) + ",21))"
		} else {
			args = append(args, "%"+ref.Suffix)
			where += " AND LOWER(c.uid) LIKE " + bind(len(args))
			if ref.IssueShortID != "" {
				args = append(args, ref.IssueShortID)
				where += " AND i.short_id=" + bind(len(args))
			} else {
				args = append(args, ref.IssueID)
				where += " AND c.issue_id=" + bind(len(args))
			}
		}
		rows, err := tx.QueryContext(ctx, `SELECT c.uid,c.issue_id,i.uid,i.short_id,i.project_id,p.uid,p.name`+commentGraphFrom+where+" LIMIT 2", args...) // #nosec G202 -- Predicates are static SQL; reference values are bound arguments.
		if err != nil {
			return data, err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var record CommentGraphRecord
			if err := rows.Scan(&record.Comment.UID, &record.Comment.IssueID, &record.IssueUID,
				&record.IssueShortID, &record.ProjectID, &record.ProjectUID, &record.ProjectName); err != nil {
				return data, err
			}
			RecordExposedIssue(ctx, record.Comment.IssueID)
			data.Comments = append(data.Comments, record)
		}
		return data, rows.Err()
	}
	statement := commentGraphSelect + where // #nosec G202 -- Only backend placeholders and static predicates are concatenated.
	statementArgs := args
	if query.SelectedIssueID > 0 {
		statementArgs = append(slices.Clone(args), query.SelectedIssueID)
		// #nosec G202 -- Only backend placeholders and static predicates are concatenated.
		statement = `WITH visible AS (SELECT c.uid,c.issue_id,c.reply_to_uid` + commentGraphFrom + where + `),
selected AS (SELECT uid,reply_to_uid FROM visible WHERE issue_id=` + bind(len(statementArgs)) + `)
` + commentGraphSelect + ` WHERE c.uid IN (
 SELECT uid FROM selected
 UNION SELECT v.uid FROM selected s CROSS JOIN visible v WHERE v.reply_to_uid=s.uid
 UNION SELECT v.uid FROM visible v JOIN selected s ON ` + CommentUIDEqualSQL("v.reply_to_uid", "s.uid", displayDialect) + `
 UNION SELECT v.uid FROM visible v JOIN selected s ON ` + CommentUIDEqualSQL("v.uid", "s.reply_to_uid", displayDialect) + `
)`
	}
	rows, err := tx.QueryContext(ctx, statement, statementArgs...)
	if err != nil {
		return data, err
	}
	for rows.Next() {
		record, err := scanCommentGraphRecord(rows, parseTime)
		if err != nil {
			_ = rows.Close()
			return data, err
		}
		RecordExposedIssue(ctx, record.Comment.IssueID)
		data.Comments = append(data.Comments, record)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return data, err
	}
	if query.SelectedIssueID > 0 && len(data.Comments) > 0 {
		uids, err := readCommentGraphUIDs(ctx, tx, where, args, query.SelectedIssueID, bind, displayDialect)
		if err != nil {
			return data, err
		}
		data.CommentUIDsByProject[query.ProjectID] = uids
	}

	visible := make(map[string]bool, len(data.Comments))
	for _, record := range data.Comments {
		visible[CanonicalCommentUID(record.Comment.UID)] = true
	}
	resolvedTargets := make(map[string]CommentGraphTarget)
	for index, record := range data.Comments {
		targetUID := record.Comment.ReplyToUID
		if targetUID == "" || visible[CanonicalCommentUID(targetUID)] {
			continue
		}
		if query.AllowedIssueIDs != nil {
			data.Comments[index].Comment.ReplyToUID = ""
			data.Comments[index].Comment.ReplyKind = ""
			continue
		}
		if query.SelectedIssueID > 0 && record.Comment.IssueID != query.SelectedIssueID {
			continue
		}
		identity := CanonicalCommentUID(targetUID)
		if state, alreadyResolved := resolvedTargets[identity]; alreadyResolved {
			data.Targets[targetUID] = state
			continue
		}

		targetArgs := []any{}
		targetUIDPredicate := CommentUIDLookupPredicate("c.uid", targetUID, &targetArgs, bind, displayDialect)
		targetWhere := " WHERE " + targetUIDPredicate + " AND i.deleted_at IS NULL AND p.deleted_at IS NULL AND " + projectPredicate(&targetArgs)
		target, err := findCommentGraphRecord(ctx, tx,
			commentGraphSelect+targetWhere+" ORDER BY c.uid", targetArgs, targetUID, parseTime)
		if err != nil {
			return data, err
		}
		if target != nil {
			targetWhere := " WHERE i.project_id=" + bind(1) + " AND i.deleted_at IS NULL AND p.deleted_at IS NULL"
			targetArgs := []any{target.ProjectID}
			if _, loaded := data.CommentUIDsByProject[target.ProjectID]; !loaded {
				uids, err := readCommentGraphUIDs(ctx, tx, targetWhere, targetArgs, query.SelectedIssueID, bind, displayDialect)
				if err != nil {
					return data, err
				}
				data.CommentUIDsByProject[target.ProjectID] = uids
			}
			RecordExposedIssue(ctx, target.Comment.IssueID)
			state := CommentGraphTarget{Status: "moved", Record: target}
			data.Targets[targetUID] = state
			resolvedTargets[identity] = state
			continue
		}

		statusArgs := []any{}
		visibleProject := projectPredicate(&statusArgs)
		targetUIDPredicate = CommentUIDLookupPredicate("c.uid", targetUID, &statusArgs, bind, displayDialect)
		status, projectID, err := findCommentGraphTargetStatus(ctx, tx,
			`SELECT CASE WHEN p.deleted_at IS NOT NULL OR NOT (`+visibleProject+`) THEN 'hidden' WHEN i.deleted_at IS NOT NULL THEN 'removed' ELSE 'pending' END, i.project_id,c.uid`+commentGraphFrom+` WHERE `+targetUIDPredicate+" ORDER BY c.uid",
			statusArgs, targetUID)
		if err != nil {
			return data, err
		}
		if status == "" {
			removed, err := commentGraphPurgedTargetEvidence(ctx, tx, targetUID, bind, replyTargetUIDExpr, displayDialect)
			if err != nil {
				return data, err
			}
			if removed {
				status = "removed"
			} else {
				status = "pending"
			}
		}
		state := CommentGraphTarget{Status: status}
		if status == "removed" {
			state.ProjectID = projectID
		}
		data.Targets[targetUID] = state
		resolvedTargets[identity] = state
	}
	comments := make([]*Comment, 0, len(data.Comments)+len(data.Targets))
	for index := range data.Comments {
		comments = append(comments, &data.Comments[index].Comment)
	}
	for _, target := range data.Targets {
		if target.Record != nil {
			comments = append(comments, &target.Record.Comment)
		}
	}
	if err := HydrateCommentDisplayTx(ctx, tx, comments, bind, displayDialect); err != nil {
		return data, err
	}
	return data, nil
}

// CommentUIDLookupPredicate matches a valid ULID by ASCII case while keeping
// malformed and opaque identifiers exact. Column must be a static SQL expression.
func CommentUIDLookupPredicate(column, value string, args *[]any, bind func(int) string, dialect CommentDisplaySQLDialect) string {
	identity := CanonicalCommentUID(value)
	if uid.Valid(identity) {
		*args = append(*args, identity)
		exact := bind(len(*args))
		*args = append(*args, identity)
		caseInsensitive := bind(len(*args))
		return "(" + column + "=" + exact + " OR (" + commentUIDValidSQL(column, dialect) + " AND UPPER(" + column + ")=" + caseInsensitive + "))"
	}
	*args = append(*args, value)
	return column + "=" + bind(len(*args))
}

// CommentUIDEqualSQL compares two comment identity expressions. Column
// expressions must be static SQL controlled by the caller.
func CommentUIDEqualSQL(left, right string, dialect CommentDisplaySQLDialect) string {
	return "(" + left + "=" + right + " OR (" + commentUIDValidSQL(left, dialect) + " AND " + commentUIDValidSQL(right, dialect) + " AND UPPER(" + left + ")=UPPER(" + right + ")))"
}

func commentUIDValidSQL(column string, dialect CommentDisplaySQLDialect) string {
	switch dialect {
	case CommentDisplaySQLite:
		return "(length(" + column + ")=26 AND substr(" + column + ",1,1) GLOB '[0-7]' AND " + column + " NOT GLOB '*[^0123456789ABCDEFGHJKMNPQRSTVWXYZabcdefghjkmnpqrstvwxyz]*')"
	case CommentDisplayPostgres:
		return "(char_length(" + column + ")=26 AND " + column + " ~ '^[0-7][0-9A-HJKMNP-TV-Za-hjkmnp-tv-z]{25}$')"
	default:
		return "0"
	}
}

func findCommentGraphRecord(
	ctx context.Context,
	tx *sql.Tx,
	query string,
	args []any,
	targetUID string,
	parseTime func(string) (time.Time, error),
) (*CommentGraphRecord, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	identity := CanonicalCommentUID(targetUID)
	var match *CommentGraphRecord
	for rows.Next() {
		record, err := scanCommentGraphRecord(rows, parseTime)
		if err != nil {
			return nil, err
		}
		if CanonicalCommentUID(record.Comment.UID) != identity {
			continue
		}
		if record.Comment.UID == targetUID {
			return &record, nil
		}
		if match != nil && match.Comment.UID != record.Comment.UID {
			return nil, fmt.Errorf("comment UID %q has multiple case variants", targetUID)
		}
		copiedRecord := record
		match = &copiedRecord
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return match, nil
}

func findCommentGraphTargetStatus(ctx context.Context, tx *sql.Tx, query string, args []any, targetUID string) (string, int64, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = rows.Close() }()
	identity := CanonicalCommentUID(targetUID)
	var matchUID string
	var matchStatus string
	var matchProjectID int64
	for rows.Next() {
		var storedUID, status string
		var projectID int64
		if err := rows.Scan(&status, &projectID, &storedUID); err != nil {
			return "", 0, err
		}
		if CanonicalCommentUID(storedUID) != identity {
			continue
		}
		if storedUID == targetUID {
			return status, projectID, nil
		}
		if matchUID != "" && matchUID != storedUID {
			return "", 0, fmt.Errorf("comment UID %q has multiple case variants", targetUID)
		}
		matchUID, matchStatus, matchProjectID = storedUID, status, projectID
	}
	if err := rows.Err(); err != nil {
		return "", 0, err
	}
	return matchStatus, matchProjectID, nil
}

func readCommentGraphUIDs(ctx context.Context, tx *sql.Tx, where string, args []any, selectedIssueID int64, bind func(int) string, displayDialect CommentDisplaySQLDialect) ([]string, error) {
	from := commentGraphFrom
	if selectedIssueID > 0 {
		args = append(slices.Clone(args), selectedIssueID, selectedIssueID, selectedIssueID)
		n := len(args)
		// CROSS JOIN keeps SQLite on the suffix index without planner statistics.
		from = ` FROM comments c CROSS JOIN issues i CROSS JOIN projects p`
		where += ` AND i.id=c.issue_id AND p.id=i.project_id AND lower(substr(c.uid,21)) IN (
 SELECT lower(substr(uid,21)) FROM comments WHERE issue_id=` + bind(n-2) + `
 UNION SELECT lower(substr(reply_to_uid,21)) FROM comments WHERE issue_id=` + bind(n-1) + ` AND reply_to_uid IS NOT NULL
 UNION SELECT lower(substr(r.uid,21)) FROM comments s JOIN comments r ON ` + CommentUIDEqualSQL("r.reply_to_uid", "s.uid", displayDialect) + ` WHERE s.issue_id=` + bind(n) + `)`
	}
	rows, err := tx.QueryContext(ctx, `SELECT c.uid`+from+where, args...) // #nosec G202 -- Callers supply static predicates and bind every value.
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	uids := []string{}
	for rows.Next() {
		var uid string
		if err := rows.Scan(&uid); err != nil {
			return nil, err
		}
		uids = append(uids, uid)
	}
	return uids, rows.Err()
}

// commentGraphPurgedTargetEvidenceSQL builds the indexed missing-target probe.
func commentGraphPurgedTargetEvidenceSQL(bind func(int) string, replyTargetUIDExpr string) string {
	return `SELECT EXISTS(SELECT 1 FROM events e JOIN purge_log p ON p.issue_uid=e.related_issue_uid WHERE e.type='issue.commented' AND ` + replyTargetUIDExpr + "=" + bind(1) + ")"
}

func commentGraphPurgedTargetEvidence(
	ctx context.Context,
	tx *sql.Tx,
	targetCommentUID string,
	bind func(int) string,
	replyTargetUIDExpr string,
	dialect CommentDisplaySQLDialect,
) (bool, error) {
	var removed bool
	args := []any{}
	uidPredicate := CommentUIDLookupPredicate(replyTargetUIDExpr, targetCommentUID, &args, bind, dialect)
	err := tx.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM events e JOIN purge_log p ON p.issue_uid=e.related_issue_uid WHERE e.type='issue.commented' AND `+uidPredicate+")",
		args...,
	).Scan(&removed)
	return removed, err
}

type commentGraphScanner interface{ Scan(...any) error }

const commentGraphFrom = ` FROM comments c JOIN issues i ON i.id=c.issue_id JOIN projects p ON p.id=i.project_id`

func scanCommentGraphRecord(row commentGraphScanner, parseTime func(string) (time.Time, error)) (CommentGraphRecord, error) {
	var record CommentGraphRecord
	var created string
	var teammate, replyTo, replyKind, edited sql.NullString
	comment := &record.Comment
	err := row.Scan(&comment.ID, &comment.UID, &comment.IssueID, &comment.Author, &comment.Body,
		&created, &teammate, &replyTo, &replyKind, &edited, &comment.AttributionView, &record.IssueUID,
		&record.IssueShortID, &record.ProjectID, &record.ProjectUID, &record.ProjectName)
	if err != nil {
		return record, err
	}
	comment.Teammate, comment.ReplyToUID, comment.ReplyKind = teammate.String, replyTo.String, replyKind.String
	comment.SourceFallback(comment.Author, comment.Teammate)
	comment.CreatedAt, err = parseTime(created)
	if err != nil {
		return record, fmt.Errorf("parse graph comment time: %w", err)
	}
	if edited.Valid {
		value, err := parseTime(edited.String)
		if err != nil {
			return record, err
		}
		comment.EditedAt = &value
	}
	return record, nil
}

// CommentGraphContainsID reports whether ids contains target.
func CommentGraphContainsID(ids []int64, target int64) bool {
	return slices.Contains(ids, target)
}

// CommentGraphIntersectIDs intersects scoped issue IDs with an optional
// caller-provided allowlist. A nil allowlist preserves all scoped IDs.
func CommentGraphIntersectIDs(scopeIDs, allowedIDs []int64) []int64 {
	if allowedIDs == nil {
		return scopeIDs
	}
	intersection := make([]int64, 0, len(scopeIDs))
	for _, id := range scopeIDs {
		if CommentGraphContainsID(allowedIDs, id) {
			intersection = append(intersection, id)
		}
	}
	return intersection
}

// CommentGraphAddID appends id once, preserving the existing order.
func CommentGraphAddID(ids []int64, id int64) []int64 {
	if CommentGraphContainsID(ids, id) {
		return ids
	}
	return append(ids, id)
}

// CommentGraphDeletedSourceInScope checks the selected deleted issue's current
// parent ancestry in the same snapshot as comment and target reads.
func CommentGraphDeletedSourceInScope(ctx context.Context, tx *sql.Tx, sourceID, projectID int64, scope APITokenScope, bind func(int) string) (bool, error) {
	query := `WITH RECURSIVE ancestors(id, uid, project_id) AS (
 SELECT i.id, i.uid, i.project_id FROM issues i JOIN projects p ON p.id=i.project_id
 WHERE i.id=` + bind(1) + ` AND i.project_id=` + bind(2) + ` AND i.deleted_at IS NOT NULL AND p.deleted_at IS NULL
 UNION
 SELECT parent.id, parent.uid, parent.project_id FROM ancestors child
 JOIN links edge ON edge.from_issue_id=child.id AND edge.type='parent'
 JOIN issues parent ON parent.id=edge.to_issue_id AND parent.project_id=child.project_id
 JOIN projects p ON p.id=parent.project_id
 WHERE parent.deleted_at IS NULL AND p.deleted_at IS NULL
)
SELECT EXISTS(
 SELECT 1 FROM ancestors a
 JOIN issues root ON root.id=a.id AND root.uid=` + bind(3) + ` AND root.deleted_at IS NULL
 JOIN projects p ON p.id=root.project_id AND p.uid=` + bind(4) + ` AND p.deleted_at IS NULL
 WHERE a.project_id=p.id
)`
	var included bool
	err := tx.QueryRowContext(ctx, query, sourceID, projectID, scope.RootIssueUID, scope.ProjectUID).Scan(&included)
	return included, err
}
