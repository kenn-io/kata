package sqlitestore

import (
	"context"
	"fmt"
	"strings"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kit/search/lexical"
	"go.kenn.io/kit/search/sqlitefts"
	"go.kenn.io/kit/search/sqlquery"
)

// SearchFTS runs an FTS5 BM25-ranked query against issues_fts, joins back to
// issues, and returns the top `p.Limit` rows scoped to the given project. When
// p.IncludeDeleted is false, soft-deleted issues are filtered. The returned
// Score is the negated raw BM25 (so higher = better match); MatchedIn is
// derived from per-column MATCH subqueries since FTS5 highlight() returns
// NULL on contentless tables.
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
	if limit > 200 {
		limit = 200
	}

	analyzer := lexical.Literal()
	prepared, err := analyzer.PrepareLiteral(q)
	if err != nil {
		return nil, fmt.Errorf("search fts: %w", err)
	}
	phrases := make([]string, 0)
	for word := range strings.FieldsSeq(q) {
		term, err := analyzer.PrepareLiteral(word)
		if err != nil {
			return nil, fmt.Errorf("search fts: %w", err)
		}
		phrases = append(phrases, term.Match)
	}
	colPhrase := strings.Join(phrases, " OR ")
	topPhrase := prepared.Match
	if r.mode == searchAny {
		topPhrase = colPhrase
	}

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
	rowFilter += scopeFilter.String()
	helper, err := sqlitefts.New(
		sqlitefts.WithIndexTable("issues_fts"), sqlitefts.WithIndexKey("rowid"),
		sqlitefts.WithSourceTable("issues"), sqlitefts.WithSourceKey("id"),
	)
	if err != nil {
		return nil, fmt.Errorf("search fts: %w", err)
	}
	candidates, err := helper.Build(sqlitefts.Request{
		Match: topPhrase, CandidateLimit: limit,
		SourcePredicate: sqlquery.Predicate{
			SQL:  "d.project_id = ? " + strings.ReplaceAll(rowFilter, "i.", "d."),
			Args: append([]any{r.params.ProjectID}, filterArgs...),
		},
	})
	if err != nil {
		return nil, fmt.Errorf("search fts: %w", err)
	}
	query := fmt.Sprintf(`WITH candidates AS (%s)
		SELECT i.id, i.uid, i.project_id, p.uid, i.short_id, i.title, i.body, i.status,
		       i.closed_reason, i.owner, i.assignment_expires_on, i.priority, i.author, i.metadata, i.revision,
		       i.recurrence_id, i.occurrence_key,
		       i.created_at, i.updated_at, i.closed_at, i.deleted_at,
		       candidates.score,
		       (i.id IN (SELECT rowid FROM issues_fts WHERE title MATCH ?)) AS in_title,
		       (i.id IN (SELECT rowid FROM issues_fts WHERE body MATCH ?)) AS in_body,
		       (i.id IN (SELECT rowid FROM issues_fts WHERE comments MATCH ?)) AS in_comments
		FROM candidates
		JOIN issues i ON i.id = candidates.doc_key
		JOIN projects p ON p.id = i.project_id
		ORDER BY candidates.score DESC, i.id ASC`, candidates.SQL)
	args := append(candidates.Args, colPhrase, colPhrase, colPhrase)

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
			&i.CreatedAt, &i.UpdatedAt, &i.ClosedAt, &i.DeletedAt,
			&score, &inTitle, &inBody, &inComments); err != nil {
			return nil, fmt.Errorf("scan search row: %w", err)
		}
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
