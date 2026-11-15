package sqlitestore

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json/v2"
	"fmt"
	"slices"
	"strings"
	"sync"
	"unicode"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/similarity"
	"go.kenn.io/kit/search/lexical"
)

// SearchFTS finds terms across issues and their comments and returns the top
// p.Limit issues scoped to the project. Score sums each word's best negated
// document BM25 (higher is better). Soft-deleted
// issues are filtered unless p.IncludeDeleted is set. MatchedIn matches the
// returned page text with the index tokenizer.
func (d *Store) SearchFTS(ctx context.Context, p db.SearchFTSParams) ([]db.SearchCandidate, error) {
	return d.searchFTS(ctx, searchFTSReq{params: p, mode: searchAll})
}

// SearchFTSAny finds at least one query term in title and body documents only.
// Look-alike checks score title and body overlap, so comments don't contribute
// candidates, scores, or MatchedIn flags. A single OR match sums BM25 relevance
// across query words for each issue. With TitleQuery on stores of 5k+ issues,
// SQLite can rank eligible titles by similarity score, then ID. When more
// than Limit rows reach the warning threshold, it retains BM25 ranking.
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
	// Cap unbounded callers before tokenizing the returned page text.
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

	var issueCount int
	var eligibleIDs []int64
	var searchQ readQuerier = d
	if r.mode == searchAny && r.params.TitleQuery != "" && strings.IndexFunc(r.params.TitleQuery, func(r rune) bool { return r > unicode.MaxASCII }) < 0 {
		if err := d.QueryRowContext(ctx, `SELECT count(*) FROM (SELECT 1 FROM issues LIMIT 5000)`).Scan(&issueCount); err != nil {
			return nil, err
		}
	}
	if issueCount == 5_000 {
		titleMatch, common, err := d.lookalikeMatch(ctx, r.params.TitleQuery)
		if err != nil {
			return nil, err
		}
		if titleMatch == "" && !common {
			return nil, nil
		}
		if !common {
			tx, err := d.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
			if err != nil {
				return nil, err
			}
			defer func() { _ = tx.Rollback() }()
			searchQ = tx
			// Selection and hydration read the same visibility snapshot.
			titleArgs := append([]any{titleMatch, r.params.ProjectID}, filterArgs...)
			rows, err := tx.QueryContext(ctx, `SELECT i.id, i.title FROM issues_fts
	 CROSS JOIN issues i ON i.id = issues_fts.rowid
	 WHERE issues_fts MATCH ? AND i.project_id = ? `+rowFilter, titleArgs...)
			if err != nil {
				return nil, fmt.Errorf("search titles: %w", err)
			}
			var ids []int64
			titleTokens := similarity.Tokenize(r.params.TitleQuery)
			for rows.Next() {
				var id int64
				var title string
				if err := rows.Scan(&id, &title); err != nil {
					_ = rows.Close()
					return nil, err
				}
				titleScore := similarity.Jaccard(titleTokens, similarity.Tokenize(similarity.TitlePrefix(title)))
				if titleScore >= similarity.MinTitleJaccard {
					ids = append(ids, id)
					if len(ids) > 200 {
						break
					}
				}
			}
			err = rows.Err()
			_ = rows.Close()
			if err != nil {
				return nil, err
			}
			if len(ids) == 0 {
				return nil, nil
			}
			if len(ids) <= 200 {
				encoded, err := json.Marshal(ids)
				if err != nil {
					return nil, err
				}
				hydrationArgs := append([]any{string(encoded), r.params.ProjectID}, filterArgs...)
				out, err := searchCandidates(ctx, tx, `SELECT `+issueColumns+`, 0, 0
 FROM issues i JOIN projects p ON p.id = i.project_id
 WHERE i.id IN (SELECT value FROM json_each(?)) AND i.project_id = ? `+rowFilter, hydrationArgs...)
				if err != nil {
					return nil, err
				}
				warnings := 0
				for i := range out {
					out[i].Score = similarity.Score(r.params.TitleQuery, r.params.TitleBody, out[i].Issue.Title, out[i].Issue.Body)
					if out[i].Score >= similarity.Threshold {
						warnings++
					}
				}
				if warnings <= limit {
					slices.SortFunc(out, func(a, b db.SearchCandidate) int {
						if order := cmp.Compare(b.Score, a.Score); order != 0 {
							return order
						}
						return cmp.Compare(a.Issue.ID, b.Issue.ID)
					})
					if len(out) > limit {
						out = out[:limit]
					}
					if err := pageMatchedIn(ctx, colPhrase, out); err != nil {
						return nil, fmt.Errorf("search fts: %w", err)
					}
					return out, nil
				}
				eligibleIDs = ids
			}
		}
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
		if len(eligibleIDs) > 0 {
			encoded, err := json.Marshal(eligibleIDs)
			if err != nil {
				return nil, err
			}
			// As in AND search, filter the MATCH scan instead of seeking each rowid.
			candidates += " AND issues_fts.rowid + 0 IN (SELECT value FROM json_each(?))"
			args = append(args, string(encoded))
		}
	} else {
		// Match words as rows with a fixed statement shape and one JSON parameter.
		words, err := json.Marshal(phrases)
		if err != nil {
			return nil, fmt.Errorf("search fts: %w", err)
		}
		// Every match contains every word, so the rarest word bounds the issues
		// worth scoring. Scoring walks each posting of a word, while skipping
		// postings outside that set is far cheaper.
		var anchor string
		if len(phrases) > 1 {
			var none bool
			anchor, none, err = d.anchorWord(ctx, words)
			if err != nil {
				return nil, fmt.Errorf("search fts: %w", err)
			}
			if none {
				return nil, nil
			}
		}
		anchorCTE, issueIn, commentIn := "", "", ""
		if anchor != "" {
			anchorCTE = `anchor AS MATERIALIZED (
  SELECT rowid AS id FROM issues_fts WHERE issues_fts MATCH ?1
  UNION
  SELECT c.issue_id FROM comments_fts CROSS JOIN comments c ON c.id = comments_fts.rowid WHERE comments_fts MATCH ?1
 ), `
			issueIn = " AND issues_fts.rowid + 0 IN (SELECT id FROM anchor)"
			commentIn = " AND c.issue_id + 0 IN (SELECT id FROM anchor)"
			args = append(args, anchor)
		}
		args = append(args, string(words), r.params.ProjectID)
		args = append(args, filterArgs...)
		args = append(args, r.params.ProjectID)
		args = append(args, filterArgs...)
		args = append(args, len(phrases))
		// Keep per-word aggregation across issue and comment documents for AND search.
		candidates = fmt.Sprintf(`WITH %sterms AS (
  SELECT value AS word FROM json_each(?)
 ), hits AS MATERIALIZED (
  SELECT issues_fts.rowid AS issue_id, terms.word, -bm25(issues_fts) AS score, 0 AS in_comments
  FROM terms CROSS JOIN issues_fts
  WHERE issues_fts MATCH terms.word%s AND EXISTS (
   SELECT 1 FROM issues i WHERE i.id = issues_fts.rowid AND i.project_id = ? %s
  )
  UNION ALL
  SELECT c.issue_id, terms.word, -bm25(comments_fts), 1
  FROM terms CROSS JOIN comments_fts
  CROSS JOIN comments c ON c.id = comments_fts.rowid
  WHERE comments_fts MATCH terms.word%s AND EXISTS (
   SELECT 1 FROM issues i WHERE i.id = c.issue_id AND i.project_id = ? %s
  )
 ), word_hits AS (
  SELECT issue_id, word, max(score) AS score, max(in_comments) AS in_comments
  FROM hits GROUP BY issue_id, word
 ), candidates AS (
  SELECT issue_id AS doc_key, count(*) AS matched_words, sum(score) AS score, max(in_comments) AS in_comments
  FROM word_hits GROUP BY issue_id HAVING count(*) >= ?`, anchorCTE, issueIn, rowFilter, commentIn, rowFilter)
	}
	query := candidates + fmt.Sprintf(`
 ), page AS MATERIALIZED (
  SELECT * FROM candidates ORDER BY %s LIMIT ?
 )
 SELECT `+issueColumns+`,
        candidates.score,
        candidates.in_comments
 FROM page AS candidates
 JOIN issues i ON i.id = candidates.doc_key
 JOIN projects p ON p.id = i.project_id
 ORDER BY %s`, pageOrder, order)
	args = append(args, limit)

	out, err := searchCandidates(ctx, searchQ, query, args...)
	if err != nil {
		return nil, err
	}
	if err := pageMatchedIn(ctx, colPhrase, out); err != nil {
		return nil, fmt.Errorf("search fts: %w", err)
	}
	return out, nil
}

func searchCandidates(ctx context.Context, q readQuerier, query string, args ...any) ([]db.SearchCandidate, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("search fts: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []db.SearchCandidate
	for rows.Next() {
		var (
			i          db.Issue
			score      float64
			inComments bool
		)
		if err := rows.Scan(&i.ID, &i.UID, &i.ProjectID, &i.ProjectUID, &i.ShortID, &i.Title, &i.Body, &i.Status,
			&i.ClosedReason, &i.Owner, &i.AssignmentExpiresOn, &i.Priority, &i.Author, &i.Metadata, &i.Revision,
			&i.RecurrenceID, &i.OccurrenceKey,
			&i.CreatedAt, &i.UpdatedAt, &i.ClosedAt, &i.DeletedAt, &i.AttributionView,
			&score, &inComments); err != nil {
			return nil, fmt.Errorf("scan search row: %w", err)
		}
		handle, _ := db.IssueTeammate(i.Metadata)
		i.SourceFallback(i.Author, handle)
		db.RecordExposedIssue(ctx, i.ID)
		var matched []string
		if inComments {
			matched = []string{"comments"}
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

// pageMatchedIn matches only the returned page with the index tokenizer,
// avoiding another read of every query word's postings in the store.
// It prepends title and body flags to the existing comments flag.
func pageMatchedIn(ctx context.Context, match string, page []db.SearchCandidate) error {
	if len(page) == 0 {
		return nil
	}
	probe, err := initializedSearchProbe(ctx)
	if err != nil {
		return err
	}
	tx, err := probe.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, c := range page {
		if _, err := tx.ExecContext(ctx, `INSERT INTO page_fts(rowid, title, body) VALUES (?, ?, ?)`, c.Issue.ID, c.Issue.Title, c.Issue.Body); err != nil {
			return err
		}
	}
	matched := make(map[int64][]string, len(page))
	for _, field := range []struct{ column, query string }{
		{"title", `SELECT rowid FROM page_fts WHERE title MATCH ?`},
		{"body", `SELECT rowid FROM page_fts WHERE body MATCH ?`},
	} {
		rows, err := tx.QueryContext(ctx, field.query, match)
		if err != nil {
			return err
		}
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				_ = rows.Close()
				return err
			}
			matched[id] = append(matched[id], field.column)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return err
		}
		if err := rows.Close(); err != nil {
			return err
		}
	}
	for i := range page {
		page[i].MatchedIn = append(matched[page[i].Issue.ID], page[i].MatchedIn...)
	}
	return nil
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

func initializedSearchProbe(ctx context.Context) (*sql.DB, error) {
	probe, err := searchProbe()
	if err != nil {
		return nil, err
	}
	const ftsConfig = "unicode61 remove_diacritics 2"
	if _, err := probe.ExecContext(ctx, `CREATE VIRTUAL TABLE IF NOT EXISTS query_fts USING fts5(text, tokenize='`+ftsConfig+`');
 CREATE VIRTUAL TABLE IF NOT EXISTS query_vocab USING fts5vocab(query_fts, instance);
 CREATE VIRTUAL TABLE IF NOT EXISTS page_fts USING fts5(title, body, tokenize='`+ftsConfig+`')`); err != nil {
		return nil, err
	}
	return probe, nil
}

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
	probe, err := initializedSearchProbe(ctx)
	if err != nil {
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

const anchorCap = 10_000 // postings counted per word and table

// anchorWord returns the query word with the fewest postings in issues and
// comments when that count stays under anchorCap. It returns "" when every word
// is common, and none when a word has no postings, since AND search then
// matches nothing.
func (d *Store) anchorWord(ctx context.Context, words []byte) (anchor string, none bool, err error) {
	counts, err := d.postingCounts(ctx, words, anchorCap, true)
	if err != nil {
		return "", false, err
	}
	phrases := make([]string, 0)
	if err := json.Unmarshal(words, &phrases); err != nil {
		return "", false, err
	}
	best := anchorCap
	for i, n := range counts {
		if n == 0 {
			return "", true, nil
		}
		if n < best {
			anchor, best = phrases[i], n
		}
	}
	return anchor, false, nil
}

func (d *Store) postingCounts(ctx context.Context, words []byte, limit int, comments bool) ([]int, error) {
	count := `(SELECT count(*) FROM (SELECT 1 FROM issues_fts WHERE issues_fts MATCH j.value LIMIT ?1))`
	if comments {
		count += ` + (SELECT count(*) FROM (SELECT 1 FROM comments_fts WHERE comments_fts MATCH j.value LIMIT ?1))`
	}
	rows, err := d.QueryContext(ctx, `SELECT `+count+` FROM json_each(?2) j ORDER BY CAST(j.key AS INTEGER)`, limit, string(words))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var counts []int
	for rows.Next() {
		var n int
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		counts = append(counts, n)
	}
	return counts, rows.Err()
}

func (d *Store) lookalikeMatch(ctx context.Context, titleQuery string) (string, bool, error) {
	titles := similarity.Tokenize(titleQuery)
	slices.Sort(titles)
	titles = slices.Compact(titles)
	if len(titles) == 0 {
		// Stop words and one-letter words leave no title words to filter on,
		// but full-text search still indexes them, so use ordinary retrieval.
		return "", true, nil
	}
	for i, token := range titles {
		titles[i] = `{title} : "` + token + `"*`
	}
	words, err := json.Marshal(titles)
	if err != nil {
		return "", false, err
	}
	counts, err := d.postingCounts(ctx, words, 5_000, false)
	if err != nil {
		return "", false, err
	}
	frequencies := make(map[string]int, len(titles))
	for i, title := range titles {
		frequencies[title] = counts[i]
	}
	slices.SortStableFunc(titles, func(a, b string) int {
		if n := cmp.Compare(frequencies[a], frequencies[b]); n != 0 {
			return n
		}
		// Prefer longer prefixes when the bounded probe can't distinguish them.
		return cmp.Compare(len(b), len(a))
	})
	if frequencies[titles[0]] == 5_000 {
		return "", true, nil
	}
	return strings.Join(titles[:len(titles)/2+1], " OR "), false, nil
}
