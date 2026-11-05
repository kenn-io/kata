package sqlitestore

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"fmt"
	"strings"
	"sync"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kit/search/lexical"
)

// SearchFTS finds terms across issues and their comments and returns the top
// p.Limit issues scoped to the project. Score sums each word's best negated
// document BM25 (higher is better). Soft-deleted
// issues are filtered unless p.IncludeDeleted is set. MatchedIn uses MATCH
// subqueries since FTS5 highlight() returns NULL on contentless tables.
func (d *Store) SearchFTS(ctx context.Context, p db.SearchFTSParams) ([]db.SearchCandidate, error) {
	return d.searchFTS(ctx, searchFTSReq{params: p, mode: searchAll})
}

// SearchFTSAny finds at least one query term in title and body documents only.
// Look-alike checks score title and body overlap, so comments don't contribute
// candidates, scores, or MatchedIn flags. A single OR match sums BM25 relevance
// across query words for each issue.
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

	phrases, err := searchablePhrases(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("search fts: %w", err)
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

	order := "candidates.score DESC, i.id ASC"
	pageOrder := "score DESC, doc_key ASC"
	if r.params.StableOrder {
		order = creationKeySQL("i") + " ASC, i.id ASC"
		pageOrder = "(SELECT " + creationKeySQL("i") + " FROM issues i WHERE i.id = candidates.doc_key) ASC, doc_key ASC"
	}
	var candidates string
	var args []any
	if r.mode == searchAny {
		candidates = fmt.Sprintf(`WITH candidates AS (
  SELECT i.id AS doc_key, -bm25(issues_fts) AS score, 0 AS in_comments
  FROM issues_fts
  CROSS JOIN issues i ON i.id = issues_fts.rowid
  WHERE issues_fts MATCH ? AND i.project_id = ? %s`, rowFilter)
		args = append(args, colPhrase, r.params.ProjectID)
		args = append(args, filterArgs...)
	} else {
		// Match words as rows with a fixed statement shape and one JSON parameter.
		words, err := json.Marshal(phrases)
		if err != nil {
			return nil, fmt.Errorf("search fts: %w", err)
		}
		args = append(args, string(words), r.params.ProjectID)
		args = append(args, filterArgs...)
		args = append(args, r.params.ProjectID)
		args = append(args, filterArgs...)
		args = append(args, len(phrases))
		// Keep per-word aggregation across issue and comment documents for AND search.
		candidates = fmt.Sprintf(`WITH terms AS (
  SELECT value AS word FROM json_each(?)
 ), hits AS MATERIALIZED (
  SELECT issues_fts.rowid AS issue_id, terms.word, -bm25(issues_fts) AS score, 0 AS in_comments
  FROM terms CROSS JOIN issues_fts
  WHERE issues_fts MATCH terms.word AND EXISTS (
   SELECT 1 FROM issues i WHERE i.id = issues_fts.rowid AND i.project_id = ? %s
  )
  UNION ALL
  SELECT c.issue_id, terms.word, -bm25(comments_fts), 1
  FROM terms CROSS JOIN comments_fts
  CROSS JOIN comments c ON c.id = comments_fts.rowid
  WHERE comments_fts MATCH terms.word AND EXISTS (
   SELECT 1 FROM issues i WHERE i.id = c.issue_id AND i.project_id = ? %s
  )
 ), word_hits AS (
  SELECT issue_id, word, max(score) AS score, max(in_comments) AS in_comments
  FROM hits GROUP BY issue_id, word
 ), candidates AS (
  SELECT issue_id AS doc_key, count(*) AS matched_words, sum(score) AS score, max(in_comments) AS in_comments
  FROM word_hits GROUP BY issue_id HAVING count(*) >= ?`, rowFilter, rowFilter)
	}
	query := candidates + fmt.Sprintf(`
 ), page AS MATERIALIZED (
  SELECT * FROM candidates ORDER BY %s LIMIT ?
 )
 SELECT `+issueColumns+`,
        candidates.score,
        (i.id IN (SELECT rowid FROM issues_fts WHERE title MATCH ?)) AS in_title,
        (i.id IN (SELECT rowid FROM issues_fts WHERE body MATCH ?)) AS in_body,
        candidates.in_comments
 FROM page AS candidates
 JOIN issues i ON i.id = candidates.doc_key
 JOIN projects p ON p.id = i.project_id
 ORDER BY %s`, pageOrder, order)
	args = append(args, limit, colPhrase, colPhrase)

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

// The probe shares the index tokenizer without writing to the store, including
// read-only stores. Each transaction owns the single connection until rollback.
var searchProbe = sync.OnceValues(func() (*sql.DB, error) {
	probe, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		return nil, err
	}
	probe.SetMaxOpenConns(1)
	return probe, nil
})

// searchablePhrases deduplicates words and drops those unicode61 leaves empty.
// Its Unicode 6.1 categories differ from Go's Unicode tables.
func searchablePhrases(ctx context.Context, query string) ([]string, error) {
	var words []string
	phrases := make([]string, 0)
	seen := make(map[string]bool)
	analyzer := lexical.Literal()
	for word := range strings.FieldsSeq(query) {
		if seen[word] {
			continue
		}
		seen[word] = true
		term, err := analyzer.PrepareLiteral(word)
		if err != nil {
			return nil, err
		}
		words = append(words, word)
		phrases = append(phrases, term.Match)
	}
	input, err := json.Marshal(words)
	if err != nil {
		return nil, err
	}
	probe, err := searchProbe()
	if err != nil {
		return nil, err
	}
	if _, err := probe.ExecContext(ctx, `CREATE VIRTUAL TABLE IF NOT EXISTS query_fts USING fts5(text, tokenize='unicode61 remove_diacritics 2');
 CREATE VIRTUAL TABLE IF NOT EXISTS query_vocab USING fts5vocab(query_fts, instance)`); err != nil {
		return nil, err
	}
	tx, err := probe.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `INSERT INTO query_fts(rowid,text) SELECT CAST(key AS INTEGER)+1,value FROM json_each(?)`, string(input)); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT doc, group_concat(term, ' ')
 FROM (SELECT doc, term FROM query_vocab ORDER BY doc, offset)
 GROUP BY doc ORDER BY doc`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var searchable []string
	clear(seen)
	for rows.Next() {
		var doc int
		var normalized string
		if err := rows.Scan(&doc, &normalized); err != nil {
			return nil, err
		}
		if seen[normalized] {
			continue
		}
		seen[normalized] = true
		searchable = append(searchable, phrases[doc-1])
	}
	return searchable, rows.Err()
}
