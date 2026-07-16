package pgstore

import (
	"context"
	"fmt"
	"strings"

	"go.kenn.io/kata/internal/db"
)

// SearchFTS returns issues that contain all normalized query terms.
func (s *Store) SearchFTS(
	ctx context.Context,
	projectID int64,
	query string,
	limit int,
	includeDeleted bool,
) ([]db.SearchCandidate, error) {
	return s.searchFTS(ctx, searchFTSRequest{
		projectID: projectID, query: query, limit: limit, includeDeleted: includeDeleted,
	})
}

// SearchFTSAny returns issues that contain at least one normalized query term.
func (s *Store) SearchFTSAny(
	ctx context.Context,
	projectID int64,
	query string,
	limit int,
	includeDeleted bool,
) ([]db.SearchCandidate, error) {
	return s.searchFTS(ctx, searchFTSRequest{
		projectID: projectID, query: query, limit: limit, includeDeleted: includeDeleted, any: true,
	})
}

type searchFTSRequest struct {
	projectID      int64
	query          string
	limit          int
	includeDeleted bool
	any            bool
}

func (s *Store) searchFTS(ctx context.Context, request searchFTSRequest) ([]db.SearchCandidate, error) {
	queryText := strings.TrimSpace(request.query)
	if queryText == "" {
		return nil, nil
	}
	limit := request.limit
	if limit <= 0 {
		limit = 20
	} else if limit > 200 {
		limit = 200
	}
	matchQuery := "queries.all_query"
	if request.any {
		matchQuery = "queries.any_query"
	}
	deletedFilter := `AND i.deleted_at IS NULL`
	if request.includeDeleted {
		deletedFilter = ""
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
 LIMIT $3`, matchQuery, deletedFilter)
	rows, err := s.QueryContext(ctx, query, request.projectID, queryText, limit)
	if err != nil {
		return nil, fmt.Errorf("search fts: %w", mapSQLError(err, nil))
	}
	defer func() { _ = rows.Close() }()

	var candidates []db.SearchCandidate
	for rows.Next() {
		var buffer issueScanBuffer
		var score float64
		var inTitle, inBody, inComments bool
		destinations := append(buffer.destinations(), &score, &inTitle, &inBody, &inComments)
		if err := rows.Scan(destinations...); err != nil {
			return nil, fmt.Errorf("scan search candidate: %w", mapSQLError(err, nil))
		}
		issue, err := buffer.value()
		if err != nil {
			return nil, err
		}
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
