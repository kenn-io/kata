package pgstore

import (
	"context"
	"database/sql"
	"fmt"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/shortid"
)

type eventQueryRower interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// eventIssueLookup memoizes the event reference lookups below for one read.
func eventIssueLookup(ctx context.Context, query eventQueryRower) *db.EventIssueLookup {
	return db.NewEventIssueLookup(
		func(uid string) (db.Issue, error) { return eventIssueByUID(ctx, query, uid) },
		func(projectUID, ref string) (db.Issue, error) {
			return eventIssueByRef(ctx, query, projectUID, ref)
		},
	)
}

// eventIssueByUID and eventIssueByRef resolve the issues an event refers to.
// A reference the caller may not see resolves as not found, so the feed
// replaces the event with its reset marker without exposing the hidden issue.
func eventIssueByUID(ctx context.Context, query eventQueryRower, uid string) (db.Issue, error) {
	args := []any{uid}
	return scanIssue(ctx, query.QueryRowContext(ctx,
		issueSelect+` WHERE i.uid = $1 AND `+authorizedIssuePredicate(ctx, "i.project_id", &args), args...))
}

func eventIssueByRef(ctx context.Context, query eventQueryRower, projectUID, ref string) (db.Issue, error) {
	parsed, err := shortid.Parse(ref)
	if err != nil {
		return db.Issue{}, db.ErrNotFound
	}
	if parsed.ULID != "" {
		return eventIssueByUID(ctx, query, parsed.ULID)
	}
	if parsed.Project == "" && parsed.ShortID == "" {
		return db.Issue{}, db.ErrNotFound
	}
	projectColumn, projectValue := "p.uid", projectUID
	if parsed.Project != "" {
		projectColumn, projectValue = "p.name", parsed.Project
	}
	args := []any{projectValue, parsed.ShortID}
	return scanIssue(ctx, query.QueryRowContext(ctx,
		issueSelect+fmt.Sprintf(` WHERE %s = $1 AND i.short_id = $2 AND `, projectColumn)+
			authorizedIssuePredicate(ctx, "i.project_id", &args), args...))
}

// numberedArg appends each bound value to args and returns its $N placeholder.
func numberedArg(args *[]any) func(any) string {
	return func(value any) string {
		*args = append(*args, value)
		return fmt.Sprintf("$%d", len(*args))
	}
}

func authorizedProjectPredicate(ctx context.Context, column string, args *[]any) string {
	return db.AuthorizedProjectPredicate(ctx, column, numberedArg(args))
}

func authorizedIssuePredicate(ctx context.Context, column string, args *[]any) string {
	return authorizedIssuePredicateWith(ctx, column, numberedArg(args))
}

// authorizedIssuePredicateWith renders the project boundary for an issue
// column with the caller's placeholder style, so a statement that a builder
// later renumbers (such as search) composes it with anonymous placeholders
// instead of rewriting numbered ones.
func authorizedIssuePredicateWith(ctx context.Context, column string, bind func(any) string) string {
	if _, restricted := db.AuthorizedProjects(ctx); !restricted {
		return "1=1"
	}
	return column + " IN (SELECT id FROM projects WHERE " + db.AuthorizedProjectPredicate(ctx, "uid", bind) + ")"
}

// authorizedLinkPredicate bounds one links table reference (prefix is its
// alias followed by a dot, or empty) to rows whose endpoints both lie in the
// caller's authorized projects. Every query that reads links composes it into
// that reference's WHERE clause at construction time, after every earlier
// binding has been appended to args; a query without it has no endpoint
// authorization.
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
	closedEvidencePeers := authorizedIssuePredicate(ctx, "endpoint.project_id", args)
	predicate += ` AND (e.type<>'issue.closed' OR (
	((COALESCE(e.payload::jsonb->>'parent_uid','')='' AND
	  COALESCE(e.payload::jsonb->>'parent_short_id','')='')
	 OR (jsonb_typeof(e.payload::jsonb->'parent_uid')='string' AND
	  EXISTS(SELECT 1 FROM issues endpoint WHERE endpoint.uid=e.payload::jsonb->>'parent_uid' AND ` + closedPeers + `))
	)
 AND COALESCE(jsonb_typeof(e.payload::jsonb->'evidence'),'array')='array'
 AND NOT EXISTS(
   SELECT 1 FROM jsonb_array_elements(CASE
     WHEN jsonb_typeof(e.payload::jsonb->'evidence')='array' THEN e.payload::jsonb->'evidence'
     ELSE '[]'::jsonb END) evidence(value)
   WHERE jsonb_typeof(evidence.value)<>'object'
      OR COALESCE(jsonb_typeof(evidence.value->'type'),'')<>'string'
      OR ((evidence.value->>'type') IN ('duplicate-of','superseded-by') AND (
          COALESCE(jsonb_typeof(evidence.value->'issue_ref'),'')<>'string'
          OR COALESCE(evidence.value->>'issue_ref','')=''
          OR NOT EXISTS(SELECT 1 FROM issues endpoint
                        JOIN projects endpoint_project ON endpoint_project.id=endpoint.project_id
                        WHERE (endpoint.uid=upper(evidence.value->>'issue_ref')
                           OR (endpoint.short_id=CASE
                                 WHEN position('#' in evidence.value->>'issue_ref')=0
                                 THEN evidence.value->>'issue_ref'
                                 ELSE substring(evidence.value->>'issue_ref' from position('#' in evidence.value->>'issue_ref')+1)
                               END
                               AND endpoint_project.uid=CASE
                                 WHEN position('#' in evidence.value->>'issue_ref')=0 THEN p.uid
                                 ELSE (SELECT ref_project.uid FROM projects ref_project
                                       WHERE ref_project.name=substring(evidence.value->>'issue_ref' from 1 for position('#' in evidence.value->>'issue_ref')-1))
                               END))
                          AND ` + closedEvidencePeers + `))))))`
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
	visibleSubject := source + " AND (e.issue_uid IS NULL OR EXISTS(SELECT 1 FROM issues endpoint WHERE endpoint.uid=e.issue_uid AND " + subject + "))"
	departure := authorizedProjectPredicate(ctx, "(e.payload::jsonb->>'from_project_uid')", args)
	return "((" + visibleSubject + ") OR (e.type='issue.moved' AND e.issue_uid IS NOT NULL AND " + departure + "))"
}
