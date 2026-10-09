package pgstore

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
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
	} else if limit > 200 {
		limit = 200
	}
	anyQuery := `(SELECT CASE WHEN count(*) = 0 THEN NULL
		ELSE to_tsquery('kata_simple_unaccent', string_agg(quote_literal(term), ' | ')) END
		FROM unnest(tsvector_to_array(to_tsvector('kata_simple_unaccent', ?))) AS term)`
	matchQuery := "plainto_tsquery('kata_simple_unaccent', ?)"
	if request.any {
		matchQuery = anyQuery
	}
	args := []any{request.params.ProjectID}

	rowFilter := `AND i.deleted_at IS NULL`
	if request.params.IncludeDeleted {
		rowFilter = ""
	}
	if request.params.Status != "" {
		args = append(args, request.params.Status)
		rowFilter += fmt.Sprintf("\n   AND i.status = $%d", len(args))
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
	var scopeFilter strings.Builder
	appendAllowedIssueIDsPostgresBuilder(&scopeFilter, &args, request.params.AllowedIssueIDs)
	scopeFilter.WriteString(" AND " + authorizedIssuePredicate(ctx, "i.project_id", &args))
	appendIssueScopePostgres(&scopeFilter, &args, request.params.IssueScope)
	rowFilter += scopeFilter.String()

	predicate := "EXISTS (SELECT 1 FROM issues i WHERE i.id = d.issue_id AND i.project_id = $1 " + rowFilter + ")"
	var predicateArgs []any
	predicate = regexp.MustCompile(`\$[0-9]+`).ReplaceAllStringFunc(predicate, func(parameter string) string {
		position, _ := strconv.Atoi(parameter[1:])
		predicateArgs = append(predicateArgs, args[position-1])
		return "?"
	})
	relation, err := postgres.BuildLexical(postgres.LexicalRequest{
		Mapping: postgres.LexicalMapping{SourceTable: "issues_search", SourceKey: "issue_id", Vector: "d.tsv"},
		Config:  "kata_simple_unaccent", TSQuery: matchQuery, TSQueryArgs: []any{queryText},
		Rank: postgres.RankCover, CandidateLimit: limit,
		SourcePredicate: sqlquery.Predicate{SQL: predicate, Args: predicateArgs},
	})
	if err != nil {
		return nil, fmt.Errorf("search fts: %w", err)
	}
	relation.SQL = strings.Replace(relation.SQL, "ORDER BY score DESC, doc_key ASC", "ORDER BY score DESC, doc_key DESC", 1)
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
 ORDER BY candidates.score DESC, i.id DESC`, relation.SQL, strings.Replace(anyQuery, "?", "$1", 1))
	args = relation.Args

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
