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
	} else if limit > 200 {
		limit = 200
	}
	matchQuery := "queries.all_query"
	if request.any {
		matchQuery = "queries.any_query"
	}
	args := []any{request.params.ProjectID, queryText, limit}
	rowFilter := `AND i.deleted_at IS NULL`
	if request.params.IncludeDeleted {
		rowFilter = ""
	}
	// Label predicates mirror ListIssues (AND across Labels, exclusion for
	// ExcludeLabels) and live in the candidate row selection, so they narrow
	// the result set before LIMIT rather than after.
	addLabelFilter := func(predicate, label string) {
		args = append(args, strings.ToLower(label))
		rowFilter += "\n   " + fmt.Sprintf(predicate, len(args))
	}
	for _, label := range request.params.Labels {
		addLabelFilter("AND EXISTS (SELECT 1 FROM issue_labels il WHERE il.issue_id = i.id AND il.label = $%d)", label)
	}
	for _, label := range request.params.ExcludeLabels {
		addLabelFilter("AND NOT EXISTS (SELECT 1 FROM issue_labels il WHERE il.issue_id = i.id AND il.label = $%d)", label)
	}

	// plainto_tsquery provides the all-terms form without interpreting user
	// input as tsquery syntax. The any-terms form is rebuilt from the normalized
	// lexemes so it has the same tokenizer and dictionary behavior.
	query := fmt.Sprintf(`WITH queries AS (
  SELECT plainto_tsquery('kata_simple_unaccent', $2) AS all_query,
         (
           SELECT CASE WHEN count(*) = 0 THEN NULL
                       ELSE to_tsquery('kata_simple_unaccent', string_agg(quote_literal(term), ' | '))
                  END
             FROM unnest(tsvector_to_array(to_tsvector('kata_simple_unaccent', $2))) AS term
         ) AS any_query
)
SELECT i.id, i.uid, i.project_id, p.uid, i.short_id, i.title, i.body, i.status,
       i.closed_reason, i.owner, i.priority, i.author, i.metadata, i.revision, i.recurrence_id,
       i.occurrence_key, i.created_at, i.updated_at, i.closed_at, i.deleted_at,
       ts_rank_cd(search.tsv, %[1]s) AS score,
       to_tsvector('kata_simple_unaccent', i.title) @@ queries.any_query AS in_title,
       to_tsvector('kata_simple_unaccent', i.body) @@ queries.any_query AS in_body,
       to_tsvector('kata_simple_unaccent', COALESCE((
         SELECT string_agg(c.body, ' ' ORDER BY c.id) FROM comments c WHERE c.issue_id = i.id
       ), '')) @@ queries.any_query AS in_comments
  FROM issues_search search
  JOIN issues i ON i.id = search.issue_id
  JOIN projects p ON p.id = i.project_id
 CROSS JOIN queries
 WHERE i.project_id = $1
   AND %[1]s IS NOT NULL
   AND search.tsv @@ %[1]s
   %[2]s
 ORDER BY score DESC, i.id DESC
 LIMIT $3`, matchQuery, rowFilter)
	rows, err := s.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("search fts: %w", mapSQLError(err, nil))
	}
	defer func() { _ = rows.Close() }()

	var candidates []db.SearchCandidate
	for rows.Next() {
		var issue db.Issue
		var closedAt, deletedAt storedNullTime
		var score float64
		var inTitle, inBody, inComments bool
		destinations := append(issueDestinations(&issue, &closedAt, &deletedAt),
			&score, &inTitle, &inBody, &inComments)
		if err := rows.Scan(destinations...); err != nil {
			return nil, fmt.Errorf("scan search candidate: %w", mapSQLError(err, nil))
		}
		issue.ClosedAt = closedAt.Time
		issue.DeletedAt = deletedAt.Time
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
