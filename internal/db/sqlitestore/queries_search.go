package sqlitestore

import (
	"context"
	"fmt"
	"strings"
	"unicode"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kit/search/lexical"
)

// SearchFTS finds terms across issues and their comments and returns the top
// p.Limit issues scoped to the project. Score sums the issue's negated BM25
// and its best matching comment's negated BM25 (higher is better). Soft-deleted
// issues are filtered unless p.IncludeDeleted is set. MatchedIn uses MATCH
// subqueries since FTS5 highlight() returns NULL on contentless tables.
func (d *Store) SearchFTS(ctx context.Context, p db.SearchFTSParams) ([]db.SearchCandidate, error) {
	return d.searchFTS(ctx, searchFTSReq{params: p, mode: searchAll})
}

// SearchFTSAny is like SearchFTS but joins query tokens with FTS5 OR rather
// than implicit AND. The look-alike soft-block uses this so candidate
// retrieval has high recall — similarity.Score is the actual gate, and the
// AND form prematurely filters near-duplicates that differ by one token.
func (d *Store) SearchFTSAny(ctx context.Context, p db.SearchFTSParams) ([]db.SearchCandidate, error) {
	return d.searchFTS(ctx, searchFTSReq{params: p, mode: searchAny})
}

type searchMode int

const (
	searchAll searchMode = iota // implicit AND across query tokens
	searchAny                   // explicit OR across query tokens
)

// searchFTSReq pairs the caller's params with the token-join mode the shared
// searchFTS implementation should use.
type searchFTSReq struct {
	params db.SearchFTSParams
	mode   searchMode
}

func (d *Store) searchFTS(ctx context.Context, r searchFTSReq) ([]db.SearchCandidate, error) {
	q := strings.TrimSpace(r.params.Query)
	if q == "" {
		return nil, nil
	}
	limit := r.params.Limit
	if limit <= 0 {
		limit = 20
	}
	// Cap unbounded callers — the per-column subqueries make a huge limit
	// expensive, and the HTTP layer is the natural enforcer but defending
	// here is cheap.
	if limit > 200 && !r.params.StableOrder {
		limit = 200
	}

	analyzer := lexical.Literal()
	phrases := make([]string, 0)
	for word := range strings.FieldsSeq(q) {
		// unicode61 indexes letters, numbers, and private-use characters.
		if !strings.ContainsFunc(word, func(r rune) bool {
			return unicode.IsLetter(r) || unicode.IsNumber(r) || unicode.Is(unicode.Co, r)
		}) {
			continue
		}
		term, err := analyzer.PrepareLiteral(word)
		if err != nil {
			return nil, fmt.Errorf("search fts: %w", err)
		}
		phrases = append(phrases, term.Match)
	}
	if len(phrases) == 0 {
		return nil, nil
	}
	colPhrase := strings.Join(phrases, " OR ")

	rowFilter := "AND i.deleted_at IS NULL"
	if r.params.IncludeDeleted {
		rowFilter = ""
	}
	// Label predicates mirror ListIssues (AND across Labels, exclusion for
	// ExcludeLabels) and live in the candidate row selection, so they narrow
	// the result set before LIMIT rather than after.
	var filterArgs []any
	if r.params.Status != "" {
		rowFilter += "\n\t\t  AND i.status = ?"
		filterArgs = append(filterArgs, r.params.Status)
	}
	for _, label := range r.params.Labels {
		rowFilter += "\n\t\t  AND EXISTS (SELECT 1 FROM issue_labels il WHERE il.issue_id = i.id AND il.label = ?)"
		filterArgs = append(filterArgs, strings.ToLower(label))
	}
	for _, label := range r.params.ExcludeLabels {
		rowFilter += "\n\t\t  AND NOT EXISTS (SELECT 1 FROM issue_labels il WHERE il.issue_id = i.id AND il.label = ?)"
		filterArgs = append(filterArgs, strings.ToLower(label))
	}
	var scopeFilter strings.Builder
	appendAllowedIssueIDsSQLite(&scopeFilter, &filterArgs, r.params.AllowedIssueIDs)
	if scope := r.params.IssueScope; scope != nil {
		scopeFilter.WriteString(" AND i.id IN (" + strings.NewReplacer("$1", "?", "$2", "?").Replace(issueScopeMembersCTE) + " SELECT id FROM scope_members)")
		filterArgs = append(filterArgs, scope.RootIssueUID, scope.ProjectUID, scope.ProjectUID)
	}
	scopeFilter.WriteString(" AND " + authorizedIssuePredicate(ctx, "i.project_id", &filterArgs))
	rowFilter += scopeFilter.String()
	if r.params.After != nil {
		key := creationKeySQL("i")
		rowFilter += fmt.Sprintf(" AND (%s > ? OR (%s = ? AND i.id > ?))", key, key)
		stamp := r.params.After.CreatedAt.UTC().Format(creationCursorTimeFormat)
		filterArgs = append(filterArgs, stamp, stamp, r.params.After.ID)
	}

	// Match terms as rows so query depth stays fixed as the input grows.
	var termsCTE string
	var args []any
	if r.mode == searchAll && len(phrases) > 1 {
		termsCTE = "terms(phrase) AS (VALUES " + strings.TrimSuffix(strings.Repeat("(?),", len(phrases)), ",") + "), "
		for _, phrase := range phrases {
			args = append(args, phrase)
		}
		rowFilter += ` AND i.id IN (
		 SELECT issue_id FROM (
		  SELECT terms.phrase, issues_fts.rowid AS issue_id
		  FROM terms CROSS JOIN issues_fts WHERE issues_fts MATCH terms.phrase
		  UNION
		  SELECT terms.phrase, c.issue_id
		  FROM terms CROSS JOIN comments_fts JOIN comments c ON c.id = comments_fts.rowid
		  WHERE comments_fts MATCH terms.phrase
		 ) GROUP BY issue_id HAVING count(*) = (SELECT count(DISTINCT phrase) FROM terms)
		)`
	}
	order := "candidates.score DESC, i.id ASC"
	if r.params.StableOrder {
		order = creationKeySQL("i") + " ASC, i.id ASC"
	}
	// Materialize document scores before grouping: FTS5's bm25 must run in
	// the MATCH scan, outside aggregate evaluation.
	query := fmt.Sprintf(`WITH %sissue_hits AS MATERIALIZED (
		SELECT rowid AS issue_id, -bm25(issues_fts) AS score FROM issues_fts WHERE issues_fts MATCH ?
	), comment_scores AS MATERIALIZED (
		SELECT c.issue_id, -bm25(comments_fts) AS score
		FROM comments_fts JOIN comments c ON c.id = comments_fts.rowid WHERE comments_fts MATCH ?
	), comment_hits AS (
		SELECT issue_id, max(score) AS score FROM comment_scores GROUP BY issue_id
	), hit_ids AS (
		SELECT issue_id FROM issue_hits UNION SELECT issue_id FROM comment_hits
	), candidates AS (
		SELECT i.id AS doc_key, coalesce(h.score, 0) + coalesce(c.score, 0) AS score
		FROM hit_ids JOIN issues i ON i.id = hit_ids.issue_id
		LEFT JOIN issue_hits h ON h.issue_id = i.id LEFT JOIN comment_hits c ON c.issue_id = i.id
		WHERE i.project_id = ? %s
	)
		SELECT `+issueColumns+`,
		       candidates.score,
		       (i.id IN (SELECT rowid FROM issues_fts WHERE title MATCH ?)) AS in_title,
		       (i.id IN (SELECT rowid FROM issues_fts WHERE body MATCH ?)) AS in_body,
		       (i.id IN (SELECT issue_id FROM comment_hits)) AS in_comments
		FROM candidates
		JOIN issues i ON i.id = candidates.doc_key
		JOIN projects p ON p.id = i.project_id
		ORDER BY %s LIMIT ?`, termsCTE, rowFilter, order)
	args = append(args, colPhrase, colPhrase, r.params.ProjectID)
	args = append(args, filterArgs...)
	args = append(args, colPhrase, colPhrase, limit)

	rows, err := d.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("search fts: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []db.SearchCandidate
	for rows.Next() {
		var (
			i                           db.Issue
			score                       float64
			inTitle, inBody, inComments bool
		)
		if err := rows.Scan(&i.ID, &i.UID, &i.ProjectID, &i.ProjectUID, &i.ShortID, &i.Title, &i.Body, &i.Status,
			&i.ClosedReason, &i.Owner, &i.AssignmentExpiresOn, &i.Priority, &i.Author, &i.Metadata, &i.Revision,
			&i.RecurrenceID, &i.OccurrenceKey,
			&i.CreatedAt, &i.UpdatedAt, &i.ClosedAt, &i.DeletedAt, &i.AttributionView,
			&score, &inTitle, &inBody, &inComments); err != nil {
			return nil, fmt.Errorf("scan search row: %w", err)
		}
		handle, _ := db.IssueTeammate(i.Metadata)
		i.SourceFallback(i.Author, handle)
		db.RecordExposedIssue(ctx, i.ID)
		matched := make([]string, 0, 3)
		if inTitle {
			matched = append(matched, "title")
		}
		if inBody {
			matched = append(matched, "body")
		}
		if inComments {
			matched = append(matched, "comments")
		}
		out = append(out, db.SearchCandidate{
			Issue:     i,
			Score:     score,
			MatchedIn: matched,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
