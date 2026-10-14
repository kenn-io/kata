package pgstore

import (
	"context"
	"fmt"
	"strings"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kit/search/postgres"
	"go.kenn.io/kit/search/sqlquery"
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
		FROM unnest(tsvector_to_array(to_tsvector('kata_simple_unaccent', ?))) AS term)`
	matchQuery := "plainto_tsquery('kata_simple_unaccent', ?)"
	if request.any {
		matchQuery = anyQuery
	}
	// The kit builder binds the source predicate's anonymous ? placeholders in
	// textual order, so every fragment appends its value as it is rendered.
	var predicateArgs []any
	bind := func(value any) string {
		predicateArgs = append(predicateArgs, value)
		return "?"
	}
	var rowFilter strings.Builder
	rowFilter.WriteString("EXISTS (SELECT 1 FROM issues i WHERE i.id = d.issue_id AND i.project_id = " + bind(request.params.ProjectID))
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
		// The members CTE binds the root once and the project twice, in that
		// textual order.
		rowFilter.WriteString("\n   AND i.id IN (" + strings.NewReplacer("$1", bind(scope.RootIssueUID), "$2", "?").Replace(issueScopeMembersCTE) + " SELECT id FROM scope_members)")
		predicateArgs = append(predicateArgs, scope.ProjectUID, scope.ProjectUID)
	}
	if after := request.params.After; after != nil {
		rowFilter.WriteString("\n   AND (i.created_at,i.id) > (" + bind(formatStoredTime(after.CreatedAt)) + "," + bind(after.ID) + ")")
	}
	rowFilter.WriteString(")")
	predicate := rowFilter.String()
	relation, err := postgres.BuildLexical(postgres.LexicalRequest{
		Mapping: postgres.LexicalMapping{SourceTable: "issues_search", SourceKey: "issue_id", Vector: "d.tsv"},
		Config:  "kata_simple_unaccent", TSQuery: matchQuery, TSQueryArgs: []any{queryText},
		Rank: postgres.RankCover, CandidateLimit: limit,
		SourcePredicate: sqlquery.Predicate{SQL: predicate, Args: predicateArgs},
	})
	if err != nil {
		return nil, fmt.Errorf("search fts: %w", err)
	}
	order := "candidates.score DESC, i.id DESC"
	if request.params.StableOrder {
		relation.SQL = strings.Replace(relation.SQL, "ORDER BY score DESC, doc_key ASC", "ORDER BY (SELECT created_at FROM issues WHERE id = d.issue_id) ASC, doc_key ASC", 1)
		order = "i.created_at ASC, i.id ASC"
	} else {
		relation.SQL = strings.Replace(relation.SQL, "ORDER BY score DESC, doc_key ASC", "ORDER BY score DESC, doc_key DESC", 1)
	}
	query := fmt.Sprintf(`WITH candidates AS (%s), queries AS (
  SELECT %s AS any_query
)
SELECT `+issueColumns+`,
       candidates.score,
       to_tsvector('kata_simple_unaccent', i.title) @@ queries.any_query AS in_title,
       to_tsvector('kata_simple_unaccent', i.body) @@ queries.any_query AS in_body,
       to_tsvector('kata_simple_unaccent', COALESCE((
         SELECT string_agg(c.body, ' ' ORDER BY c.id) FROM comments c WHERE c.issue_id = i.id
       ), '')) @@ queries.any_query AS in_comments
  FROM candidates
  JOIN issues i ON i.id = candidates.doc_key
  JOIN projects p ON p.id = i.project_id
 CROSS JOIN queries
 ORDER BY %s`, relation.SQL, strings.Replace(anyQuery, "?", "$1", 1), order)

	rows, err := s.QueryContext(ctx, query, relation.Args...)
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
