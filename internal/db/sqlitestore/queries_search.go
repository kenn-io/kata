package sqlitestore

import (
	"context"
	"fmt"
	"strings"

	"go.kenn.io/kata/internal/db"
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

	// Split the user query on whitespace, quote each whitespace-delimited
	// segment as an FTS5 phrase. This keeps every segment opaque to FTS5's
	// special characters (`:`, `*`, parens, `OR`/`AND`/`NOT` as bare words);
	// embedded double quotes are doubled per FTS5 quoting rules. The top-level
	// phrase joins quoted tokens by mode (space → implicit AND, " OR " →
	// explicit OR).
	var quoted []string
	for _, w := range strings.Fields(q) {
		quoted = append(quoted, `"`+strings.ReplaceAll(w, `"`, `""`)+`"`)
	}
	if len(quoted) == 0 {
		return nil, nil
	}
	var topPhrase string
	switch r.mode {
	case searchAny:
		topPhrase = strings.Join(quoted, " OR ")
	default:
		topPhrase = strings.Join(quoted, " ")
	}
	// Per-column MATCH always uses OR-of-tokens regardless of the top-level
	// mode: matched_in answers "which columns contributed at least one term?"
	// — under implicit-AND a cross-column match (e.g. title="login",
	// body="Safari" for "login Safari") is a valid hit but no single column
	// holds all the tokens, so an AND per-column subquery would mark every
	// column as not-matched and matched_in would be empty.
	colPhrase := topPhrase
	if r.mode == searchAll && len(quoted) > 1 {
		colPhrase = strings.Join(quoted, " OR ")
	}

	rowFilter := "AND i.deleted_at IS NULL"
	if r.params.IncludeDeleted {
		rowFilter = ""
	}
	// Label predicates mirror ListIssues (AND across Labels, exclusion for
	// ExcludeLabels) and live in the candidate row selection, so they narrow
	// the result set before LIMIT rather than after.
	var labelArgs []any
	for _, label := range r.params.Labels {
		rowFilter += "\n\t\t  AND EXISTS (SELECT 1 FROM issue_labels il WHERE il.issue_id = i.id AND il.label = ?)"
		labelArgs = append(labelArgs, strings.ToLower(label))
	}
	for _, label := range r.params.ExcludeLabels {
		rowFilter += "\n\t\t  AND NOT EXISTS (SELECT 1 FROM issue_labels il WHERE il.issue_id = i.id AND il.label = ?)"
		labelArgs = append(labelArgs, strings.ToLower(label))
	}
	// Per-column MATCH subqueries replace highlight() because issues_fts is
	// declared content='' (contentless), and highlight() returns NULL for every
	// column on contentless tables. Each subquery returns 1 if the row's
	// title/body/comments column matches the per-column phrase, 0 otherwise.
	query := fmt.Sprintf(`
		SELECT i.id, i.uid, i.project_id, p.uid, i.short_id, i.title, i.body, i.status,
		       i.closed_reason, i.owner, i.priority, i.author, i.metadata, i.revision,
		       i.recurrence_id, i.occurrence_key,
		       i.created_at, i.updated_at, i.closed_at, i.deleted_at,
		       bm25(issues_fts),
		       (issues_fts.rowid IN (SELECT rowid FROM issues_fts WHERE title    MATCH ?)) AS in_title,
		       (issues_fts.rowid IN (SELECT rowid FROM issues_fts WHERE body     MATCH ?)) AS in_body,
		       (issues_fts.rowid IN (SELECT rowid FROM issues_fts WHERE comments MATCH ?)) AS in_comments
		FROM issues_fts
		JOIN issues i ON i.id = issues_fts.rowid
		JOIN projects p ON p.id = i.project_id
		WHERE issues_fts MATCH ?
		  AND i.project_id = ?
		  %s
		ORDER BY bm25(issues_fts) ASC
		LIMIT %d`, rowFilter, limit)

	// Bind order: colPhrase (×3 — title MATCH, body MATCH, comments MATCH),
	// then topPhrase (top-level MATCH), then projectID, then one bind per
	// label predicate in rowFilter. Reordering the SELECT/WHERE clauses
	// without updating the bind list will silently transpose binds.
	args := append([]any{colPhrase, colPhrase, colPhrase, topPhrase, r.params.ProjectID}, labelArgs...)
	rows, err := d.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("search fts: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []db.SearchCandidate
	for rows.Next() {
		var (
			i                           db.Issue
			rawScore                    float64
			inTitle, inBody, inComments bool
		)
		if err := rows.Scan(&i.ID, &i.UID, &i.ProjectID, &i.ProjectUID, &i.ShortID, &i.Title, &i.Body, &i.Status,
			&i.ClosedReason, &i.Owner, &i.Priority, &i.Author, &i.Metadata, &i.Revision,
			&i.RecurrenceID, &i.OccurrenceKey,
			&i.CreatedAt, &i.UpdatedAt, &i.ClosedAt, &i.DeletedAt,
			&rawScore, &inTitle, &inBody, &inComments); err != nil {
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
		// FTS5 BM25 returns negative numbers; invert so callers compare with
		// "higher = better" semantics.
		out = append(out, db.SearchCandidate{
			Issue:     i,
			Score:     -rawScore,
			MatchedIn: matched,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
