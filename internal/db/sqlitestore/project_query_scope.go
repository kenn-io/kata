package sqlitestore

import (
	"context"
	"regexp"
	"strconv"
	"strings"

	"go.kenn.io/kata/internal/db"
)

func authorizedProjectPredicate(ctx context.Context, column string, args *[]any) string {
	return db.AuthorizedProjectPredicate(ctx, column, func(value any) string {
		*args = append(*args, value)
		return "?"
	})
}

func authorizedIssuePredicate(ctx context.Context, column string, args *[]any) string {
	if _, restricted := db.AuthorizedProjects(ctx); !restricted {
		return "1=1"
	}
	return column + " IN (SELECT id FROM projects WHERE " + authorizedProjectPredicate(ctx, "uid", args) + ")"
}

func authorizedLinkPredicate(ctx context.Context, prefix string, args *[]any) string {
	if _, restricted := db.AuthorizedProjects(ctx); !restricted {
		return "1=1"
	}
	from := authorizedProjectPredicate(ctx, "endpoint_project.uid", args)
	to := authorizedProjectPredicate(ctx, "endpoint_project.uid", args)
	return prefix + "from_issue_id IN (SELECT endpoint.id FROM issues endpoint JOIN projects endpoint_project ON endpoint_project.id=endpoint.project_id WHERE " + from + ") AND " + prefix + "to_issue_id IN (SELECT endpoint.id FROM issues endpoint JOIN projects endpoint_project ON endpoint_project.id=endpoint.project_id WHERE " + to + ")"
}

func authorizedEventPredicate(ctx context.Context, args *[]any) string {
	if _, restricted := db.AuthorizedProjects(ctx); !restricted {
		return "1=1"
	}
	source := authorizedProjectPredicate(ctx, "p.uid", args)
	subject := authorizedIssuePredicate(ctx, "endpoint.project_id", args)
	related := authorizedIssuePredicate(ctx, "endpoint.project_id", args)
	predicate := source + " AND (e.issue_uid IS NULL OR EXISTS(SELECT 1 FROM issues endpoint WHERE endpoint.uid=e.issue_uid AND " + subject + "))"
	predicate += " AND (e.related_issue_uid IS NULL OR EXISTS(SELECT 1 FROM issues endpoint WHERE endpoint.uid=e.related_issue_uid AND " + related + "))"
	from := authorizedProjectPredicate(ctx, "json_extract(e.payload,'$.from_project_uid')", args)
	to := authorizedProjectPredicate(ctx, "json_extract(e.payload,'$.to_project_uid')", args)
	peers := authorizedIssuePredicate(ctx, "endpoint.project_id", args)
	predicate += " AND (e.type<>'issue.moved' OR (" + from + " AND " + to + "))"
	predicate += ` AND (e.type<>'issue.links_changed' OR NOT EXISTS(
 SELECT 1 FROM json_each(e.payload) field,
 json_each(CASE WHEN field.type='array' THEN field.value ELSE json_array(field.value) END) ref
 WHERE field.key IN ('parent_set_uid','parent_removed_uid','blocks_added_uids',
 'blocks_removed_uids','blocked_by_added_uids','blocked_by_removed_uids',
 'related_added_uids','related_removed_uids')
 AND ref.type='text' AND NOT EXISTS(SELECT 1 FROM issues endpoint WHERE endpoint.uid=ref.value AND ` + peers + `)))`
	createdPeers := authorizedIssuePredicate(ctx, "endpoint.project_id", args)
	predicate += ` AND (e.type NOT IN ('issue.created','issue.snapshot') OR (
 COALESCE(json_type(e.payload,'$.links'),'array')='array' AND NOT EXISTS(
 SELECT 1 FROM json_each(
   CASE WHEN json_type(e.payload,'$.links')='array' THEN e.payload ELSE '{"links":[]}' END,
   '$.links'
 ) link
 WHERE link.type<>'object'
 OR COALESCE(json_type(CASE WHEN link.type='object' THEN link.value ELSE '{}' END,'$.to_issue_uid'),'')<>'text'
 OR NOT EXISTS(SELECT 1 FROM issues endpoint WHERE endpoint.uid=json_extract(
   CASE WHEN link.type='object' THEN link.value ELSE '{}' END,'$.to_issue_uid') AND ` + createdPeers + `))))`
	closedPeers := authorizedIssuePredicate(ctx, "endpoint.project_id", args)
	predicate += ` AND (e.type<>'issue.closed' OR (
 (COALESCE(json_extract(e.payload,'$.parent_uid'),'')='' AND
  COALESCE(json_extract(e.payload,'$.parent_short_id'),'')='')
 OR (COALESCE(json_type(e.payload,'$.parent_uid'),'')='text' AND
  EXISTS(SELECT 1 FROM issues endpoint WHERE endpoint.uid=json_extract(e.payload,'$.parent_uid') AND ` + closedPeers + `))))`
	return predicate
}

var sqliteNumberedParameter = regexp.MustCompile(`\?[0-9]+`)

// authorizeRelationshipQuery intersects every referenced link endpoint before
// deriving counts, blockers, or relationship facets. The aliases and SQL are
// native query constants; project identities remain bound parameters. A shared
// CTE lets both halves of a relationship UNION reuse the same candidate grant.
func authorizeRelationshipQuery(ctx context.Context, query string, args []any, aliases ...string) (string, []any) {
	if _, restricted := db.AuthorizedProjects(ctx); !restricted {
		return query, args
	}
	var scopeArgs []any
	predicate := authorizedProjectPredicate(ctx, "endpoint_project.uid", &scopeArgs)
	// Prepending the CTE shifts both anonymous and explicitly numbered
	// parameters from native scope builders by the same number of bindings.
	shift := len(scopeArgs)
	query = sqliteNumberedParameter.ReplaceAllStringFunc(query, func(parameter string) string {
		index, _ := strconv.Atoi(parameter[1:])
		return "?" + strconv.Itoa(index+shift)
	})
	scopeArgs = append(scopeArgs, args...)
	for _, alias := range aliases {
		boundary := alias + ".from_issue_id IN (SELECT id FROM authorized_endpoint_issues) AND " + alias + ".to_issue_id IN (SELECT id FROM authorized_endpoint_issues)"
		query = strings.ReplaceAll(query, "WHERE "+alias+".type", "WHERE "+boundary+" AND "+alias+".type")
	}
	return "WITH authorized_endpoint_issues AS (SELECT endpoint.id FROM issues endpoint JOIN projects endpoint_project ON endpoint_project.id=endpoint.project_id WHERE " + predicate + ") " + query, scopeArgs
}
