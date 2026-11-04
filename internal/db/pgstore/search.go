package pgstore

import (
	"context"
	"fmt"
	"strings"

	"go.kenn.io/kata/internal/db"
)

// SearchFTS returns issues that contain all normalized query terms.
func (s *Store) SearchFTS(ctx context.Context, p db.SearchFTSParams) ([]db.SearchCandidate, error) {
	return s.searchFTS(ctx, searchFTSRequest{params: p})
}

// SearchFTSAny returns issues that contain at least one normalized query term.
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
	anyQuery := `(SELECT CASE WHEN count(*) = 0 THEN NULL
		ELSE to_tsquery('kata_simple_unaccent', string_agg(quote_literal(term), ' | ')) END
		FROM terms)`
	predicateArgs := []any{queryText}
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
	if !request.any {
		rowFilter.WriteString(` AND NOT EXISTS (
		 SELECT 1 FROM terms WHERE NOT (
		  EXISTS (SELECT 1 FROM issues_search h WHERE h.issue_id = i.id AND h.tsv @@ to_tsquery('kata_simple_unaccent', quote_literal(terms.term)))
		  OR EXISTS (SELECT 1 FROM comment_search h JOIN comments c ON c.id = h.comment_id
		   WHERE c.issue_id = i.id AND h.tsv @@ to_tsquery('kata_simple_unaccent', quote_literal(terms.term)))
		 ))`)
	}
	order := "candidates.score DESC, i.id DESC"
	if request.params.StableOrder {
		order = "i.created_at ASC, i.id ASC"
	}
	query := fmt.Sprintf(`WITH terms AS (
 SELECT unnest(tsvector_to_array(to_tsvector('kata_simple_unaccent', $1))) AS term
), queries AS (
 SELECT %s AS any_query
), issue_hits AS (
 SELECT issue_id, ts_rank_cd(tsv, queries.any_query) AS score
 FROM issues_search CROSS JOIN queries WHERE tsv @@ queries.any_query
), comment_hits AS (
 SELECT c.issue_id, max(ts_rank_cd(h.tsv, queries.any_query)) AS score
 FROM comment_search h JOIN comments c ON c.id = h.comment_id CROSS JOIN queries
 WHERE h.tsv @@ queries.any_query GROUP BY c.issue_id
), hit_ids AS (
 SELECT issue_id FROM issue_hits UNION SELECT issue_id FROM comment_hits
), candidates AS (
 SELECT i.id AS doc_key, coalesce(h.score, 0) + coalesce(c.score, 0) AS score
 FROM hit_ids JOIN issues i ON i.id = hit_ids.issue_id
 LEFT JOIN issue_hits h ON h.issue_id = i.id LEFT JOIN comment_hits c ON c.issue_id = i.id
 WHERE %s
)
SELECT `+issueColumns+`,
       candidates.score,
       to_tsvector('kata_simple_unaccent', i.title) @@ queries.any_query AS in_title,
       to_tsvector('kata_simple_unaccent', i.body) @@ queries.any_query AS in_body,
       EXISTS (SELECT 1 FROM comment_hits h WHERE h.issue_id = i.id) AS in_comments
 FROM candidates
 JOIN issues i ON i.id = candidates.doc_key
 JOIN projects p ON p.id = i.project_id
 CROSS JOIN queries
 ORDER BY %s LIMIT %s`, anyQuery, rowFilter.String(), order, bind(limit))
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
