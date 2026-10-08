package sqlitestore

import (
	"context"
	"database/sql"
	"regexp"
	"strconv"
	"strings"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/shortid"
)

type eventQueryRower interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func eventIssueByUID(ctx context.Context, query eventQueryRower, uid string) (db.Issue, error) {
	return scanIssue(query.QueryRowContext(ctx, issueSelect+` WHERE i.uid = ?`, uid))
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
	return scanIssue(query.QueryRowContext(ctx,
		issueSelect+` WHERE `+projectColumn+` = ? AND i.short_id = ?`, projectValue, parsed.ShortID))
}

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
	closedEvidencePeers := authorizedIssuePredicate(ctx, "endpoint.project_id", args)
	predicate += ` AND (e.type<>'issue.closed' OR (
	((COALESCE(json_extract(e.payload,'$.parent_uid'),'')='' AND
	  COALESCE(json_extract(e.payload,'$.parent_short_id'),'')='')
	 OR (COALESCE(json_type(e.payload,'$.parent_uid'),'')='text' AND
	  EXISTS(SELECT 1 FROM issues endpoint WHERE endpoint.uid=json_extract(e.payload,'$.parent_uid') AND ` + closedPeers + `))
	)
 AND COALESCE(json_type(e.payload,'$.evidence'),'array')='array'
 AND NOT EXISTS(
   SELECT 1 FROM (
     SELECT item.type AS item_type,
            CASE WHEN item.type='object' THEN json_type(item.value,'$.type') END AS type_type,
            CASE WHEN item.type='object' THEN json_extract(item.value,'$.type') END AS evidence_type,
            CASE WHEN item.type='object' THEN json_type(item.value,'$.issue_ref') END AS ref_type,
            CASE WHEN item.type='object' THEN json_extract(item.value,'$.issue_ref') END AS issue_ref
     FROM json_each(
       CASE WHEN json_type(e.payload,'$.evidence')='array' THEN e.payload ELSE '{"evidence":[]}' END,
       '$.evidence'
     ) item
   ) evidence
   WHERE evidence.item_type<>'object'
      OR COALESCE(evidence.type_type,'')<>'text'
      OR (evidence.evidence_type IN ('duplicate-of','superseded-by') AND (
          COALESCE(evidence.ref_type,'')<>'text'
          OR COALESCE(evidence.issue_ref,'')=''
          OR NOT EXISTS(SELECT 1 FROM issues endpoint
                        JOIN projects endpoint_project ON endpoint_project.id=endpoint.project_id
                        WHERE (endpoint.uid=upper(evidence.issue_ref)
                           OR (endpoint.short_id=CASE
                                 WHEN instr(evidence.issue_ref,'#')=0 THEN evidence.issue_ref
                                 ELSE substr(evidence.issue_ref,instr(evidence.issue_ref,'#')+1)
                               END
                               AND endpoint_project.uid=CASE
                                 WHEN instr(evidence.issue_ref,'#')=0 THEN p.uid
                                 ELSE (SELECT ref_project.uid FROM projects ref_project
                                       WHERE ref_project.name=substr(evidence.issue_ref,1,instr(evidence.issue_ref,'#')-1))
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
	departure := authorizedProjectPredicate(ctx, "json_extract(e.payload,'$.from_project_uid')", args)
	return "((" + visibleSubject + ") OR (e.type='issue.moved' AND e.issue_uid IS NOT NULL AND " + departure + "))"
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
