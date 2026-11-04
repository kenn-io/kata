package pgstore

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"strings"

	"go.kenn.io/kata/internal/db"
)

// SearchFTS returns issues that contain all normalized query terms.
func (s *Store) SearchFTS(ctx context.Context, p db.SearchFTSParams) ([]db.SearchCandidate, error) {
	return s.searchFTS(ctx, searchFTSRequest{params: p})
}

// SearchFTSAny finds at least one normalized query term in title and body
// documents only. Comments don't contribute candidates, scores, or MatchedIn flags.
func (s *Store) SearchFTSAny(ctx context.Context, p db.SearchFTSParams) ([]db.SearchCandidate, error) {
	return s.searchFTS(ctx, searchFTSRequest{params: p, any: true})
}

type searchFTSRequest struct {
	params db.SearchFTSParams
	any    bool
}

func (s *Store) searchFTS(ctx context.Context, request searchFTSRequest) ([]db.SearchCandidate, error) {
	queryText := strings.TrimSpace(request.params.Query)
	if queryText == "" {
		return nil, nil
	}
	limit := request.params.Limit
	if limit <= 0 {
		limit = 20
	} else if limit > 200 && !request.params.StableOrder {
		limit = 200
	}
	words, err := json.Marshal(strings.Fields(queryText))
	if err != nil {
		return nil, fmt.Errorf("search fts: %w", err)
	}
	predicateArgs := []any{string(words)}
	bind := numberedArg(&predicateArgs)
	var rowFilter strings.Builder
	rowFilter.WriteString("i.project_id = " + bind(request.params.ProjectID))
	if !request.params.IncludeDeleted {
		rowFilter.WriteString("\n   AND i.deleted_at IS NULL")
	}
	if request.params.Status != "" {
		rowFilter.WriteString("\n   AND i.status = " + bind(request.params.Status))
	}
	// Label predicates mirror ListIssues (AND across Labels, exclusion for
	// ExcludeLabels) and live in the candidate row selection, so they narrow
	// the result set before LIMIT rather than after.
	for _, label := range request.params.Labels {
		rowFilter.WriteString("\n   AND EXISTS (SELECT 1 FROM issue_labels il WHERE il.issue_id = i.id AND il.label = " + bind(strings.ToLower(label)) + ")")
	}
	for _, label := range request.params.ExcludeLabels {
		rowFilter.WriteString("\n   AND NOT EXISTS (SELECT 1 FROM issue_labels il WHERE il.issue_id = i.id AND il.label = " + bind(strings.ToLower(label)) + ")")
	}
	if allowed, ok := allowedIssueIDsPredicate(request.params.AllowedIssueIDs, bind); ok {
		rowFilter.WriteString("\n   AND " + allowed)
	}
	rowFilter.WriteString("\n   AND " + authorizedIssuePredicateWith(ctx, "i.project_id", bind))
	if scope := request.params.IssueScope; scope != nil {
		// Reuse the project parameter wherever the members CTE needs it.
		rowFilter.WriteString("\n   AND i.id IN (" + strings.NewReplacer("$1", bind(scope.RootIssueUID), "$2", bind(scope.ProjectUID)).Replace(issueScopeMembersCTE) + " SELECT id FROM scope_members)")
	}
	if after := request.params.After; after != nil {
		rowFilter.WriteString("\n   AND (i.created_at,i.id) > (" + bind(formatStoredTime(after.CreatedAt)) + "," + bind(after.ID) + ")")
	}
	minimum := "(SELECT count(*) FROM terms)"
	if request.any {
		minimum = "1"
	}
	commentHits := ""
	if !request.any {
		commentHits = fmt.Sprintf(`
 UNION ALL
 SELECT i.id, terms.word, ts_rank_cd(to_tsvector('kata_simple_unaccent', c.body), terms.q), true
 FROM terms CROSS JOIN comments c JOIN issues i ON i.id = c.issue_id
 WHERE to_tsvector('kata_simple_unaccent', c.body) @@ terms.q AND %s`, rowFilter.String())
	}

	order := "candidates.score DESC, i.id DESC"
	if request.params.StableOrder {
		order = "i.created_at ASC, i.id ASC"
	}
	// Quote normalized lexemes directly so punctuation and URLs survive without
	// a second parse through the text-search dictionary.
	query := fmt.Sprintf(`WITH words AS (
 SELECT DISTINCT unnest(tsvector_to_array(to_tsvector('kata_simple_unaccent', value))) AS word
 FROM jsonb_array_elements_text($1::jsonb)
), terms AS (
 SELECT word, quote_literal(word)::tsquery AS q FROM words
), hits AS (
 SELECT i.id AS issue_id, terms.word, ts_rank_cd(h.tsv, terms.q) AS score, false AS in_comments
 FROM terms CROSS JOIN issues_search h JOIN issues i ON i.id = h.issue_id
 WHERE h.tsv @@ terms.q AND %s%s
), word_hits AS (
 SELECT issue_id, word, max(score) AS score, bool_or(in_comments) AS in_comments
 FROM hits GROUP BY issue_id, word
), candidates AS (
 SELECT issue_id AS doc_key, count(*) AS matched_words, sum(score) AS score, bool_or(in_comments) AS in_comments
 FROM word_hits GROUP BY issue_id HAVING count(*) >= %s
)
SELECT `+issueColumns+`,
       candidates.score,
       EXISTS (SELECT 1 FROM terms WHERE to_tsvector('kata_simple_unaccent', i.title) @@ terms.q) AS in_title,
       EXISTS (SELECT 1 FROM terms WHERE to_tsvector('kata_simple_unaccent', i.body) @@ terms.q) AS in_body,
       candidates.in_comments
 FROM candidates
 JOIN issues i ON i.id = candidates.doc_key
 JOIN projects p ON p.id = i.project_id
 ORDER BY %s LIMIT %s`, rowFilter.String(), commentHits, minimum, order, bind(limit))
	args := predicateArgs

	rows, err := s.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("search fts: %w", mapSQLError(err, nil))
	}
	defer func() { _ = rows.Close() }()

	var candidates []db.SearchCandidate
	for rows.Next() {
		var issue db.Issue
		var assignmentExpiresOn, closedAt, deletedAt storedNullTime
		var score float64
		var inTitle, inBody, inComments bool
		destinations := append(issueDestinations(&issue, &assignmentExpiresOn, &closedAt, &deletedAt),
			&score, &inTitle, &inBody, &inComments)
		if err := rows.Scan(destinations...); err != nil {
			return nil, fmt.Errorf("scan search candidate: %w", mapSQLError(err, nil))
		}
		issue.AssignmentExpiresOn = assignmentExpiresOn.Time
		issue.ClosedAt = closedAt.Time
		issue.DeletedAt = deletedAt.Time
		handle, _ := db.IssueTeammate(issue.Metadata)
		issue.SourceFallback(issue.Author, handle)
		db.RecordExposedIssue(ctx, issue.ID)
		matchedIn := make([]string, 0, 3)
		if inTitle {
			matchedIn = append(matchedIn, "title")
		}
		if inBody {
			matchedIn = append(matchedIn, "body")
		}
		if inComments {
			matchedIn = append(matchedIn, "comments")
		}
		candidates = append(candidates, db.SearchCandidate{
			Issue: issue, Score: score, MatchedIn: matchedIn,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate search candidates: %w", mapSQLError(err, nil))
	}
	return candidates, nil
}
