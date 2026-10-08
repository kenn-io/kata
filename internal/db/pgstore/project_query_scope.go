package pgstore

import (
	"context"
	"fmt"
	"strings"

	"go.kenn.io/kata/internal/db"
)

func authorizedProjectPredicate(ctx context.Context, column string, args *[]any) string {
	return db.AuthorizedProjectPredicate(ctx, column, func(value any) string {
		*args = append(*args, value)
		return fmt.Sprintf("$%d", len(*args))
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
	from := authorizedProjectPredicate(ctx, "(e.payload::jsonb->>'from_project_uid')", args)
	to := authorizedProjectPredicate(ctx, "(e.payload::jsonb->>'to_project_uid')", args)
	peers := authorizedIssuePredicate(ctx, "endpoint.project_id", args)
	predicate += " AND (e.type<>'issue.moved' OR (" + from + " AND " + to + "))"
	predicate += ` AND (e.type<>'issue.links_changed' OR NOT EXISTS(
 SELECT 1 FROM jsonb_each(CASE WHEN e.type='issue.links_changed' THEN e.payload::jsonb ELSE '{}'::jsonb END) field,
 LATERAL jsonb_array_elements_text(CASE WHEN jsonb_typeof(field.value)='array' THEN field.value ELSE jsonb_build_array(field.value) END) ref(uid)
 WHERE field.key IN ('parent_set_uid','parent_removed_uid','blocks_added_uids',
 'blocks_removed_uids','blocked_by_added_uids','blocked_by_removed_uids',
 'related_added_uids','related_removed_uids')
 AND NOT EXISTS(SELECT 1 FROM issues endpoint WHERE endpoint.uid=ref.uid AND ` + peers + `)))`
	createdPeers := authorizedIssuePredicate(ctx, "endpoint.project_id", args)
	predicate += ` AND (e.type NOT IN ('issue.created','issue.snapshot') OR (
 COALESCE(jsonb_typeof(e.payload::jsonb->'links'),'array')='array' AND NOT EXISTS(
 SELECT 1 FROM jsonb_array_elements(CASE
   WHEN jsonb_typeof(e.payload::jsonb->'links')='array' THEN e.payload::jsonb->'links'
   ELSE '[]'::jsonb END) link
 WHERE jsonb_typeof(link)<>'object'
 OR COALESCE(jsonb_typeof(link->'to_issue_uid'),'')<>'string'
 OR NOT EXISTS(SELECT 1 FROM issues endpoint WHERE endpoint.uid=link->>'to_issue_uid' AND ` + createdPeers + `))))`
	closedPeers := authorizedIssuePredicate(ctx, "endpoint.project_id", args)
	predicate += ` AND (e.type<>'issue.closed' OR (
 (COALESCE(e.payload::jsonb->>'parent_uid','')='' AND
  COALESCE(e.payload::jsonb->>'parent_short_id','')='')
 OR (jsonb_typeof(e.payload::jsonb->'parent_uid')='string' AND
  EXISTS(SELECT 1 FROM issues endpoint WHERE endpoint.uid=e.payload::jsonb->>'parent_uid' AND ` + closedPeers + `))))`
	return predicate
}

// authorizedEventStreamPredicate keeps visible source events whose structured
// peers may be private. EventsAfter replaces those rows with identity-free
// reset markers so polling and SSE invalidate without exposing the peer.
func authorizedEventStreamPredicate(ctx context.Context, args *[]any) string {
	if _, restricted := db.AuthorizedProjects(ctx); !restricted {
		return "1=1"
	}
	source := authorizedProjectPredicate(ctx, "p.uid", args)
	subject := authorizedIssuePredicate(ctx, "endpoint.project_id", args)
	return source + " AND (e.issue_uid IS NULL OR EXISTS(SELECT 1 FROM issues endpoint WHERE endpoint.uid=e.issue_uid AND " + subject + "))"
}

// authorizeRelationshipQuery intersects every referenced link endpoint before
// deriving counts, blockers, or relationship facets. The aliases and SQL are
// native query constants; project identities remain bound parameters. A shared
// CTE lets both halves of a relationship UNION reuse the same candidate grant.
func authorizeRelationshipQuery(ctx context.Context, query string, args []any, aliases ...string) (string, []any) {
	if _, restricted := db.AuthorizedProjects(ctx); !restricted {
		return query, args
	}
	scopeArgs := append([]any(nil), args...)
	predicate := authorizedProjectPredicate(ctx, "endpoint_project.uid", &scopeArgs)
	for _, alias := range aliases {
		boundary := alias + ".from_issue_id IN (SELECT id FROM authorized_endpoint_issues) AND " + alias + ".to_issue_id IN (SELECT id FROM authorized_endpoint_issues)"
		query = strings.ReplaceAll(query, "WHERE "+alias+".type", "WHERE "+boundary+" AND "+alias+".type")
	}
	return "WITH authorized_endpoint_issues AS (SELECT endpoint.id FROM issues endpoint JOIN projects endpoint_project ON endpoint_project.id=endpoint.project_id WHERE " + predicate + ") " + query, scopeArgs
}
