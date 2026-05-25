// Package api defines the request/response DTOs for the kata daemon HTTP API.
package api //nolint:revive // package name "api" is fixed by Plan 1 §4 wire-types layout.

import (
	"encoding/json"
	"time"

	"go.kenn.io/kata/internal/db"
)

// PingResponse mirrors the cheapest liveness response.
type PingResponse struct {
	Body struct {
		OK      bool   `json:"ok"`
		Service string `json:"service"`
		Version string `json:"version"`
		PID     int    `json:"pid,omitempty"`
	}
}

// HealthResponse mirrors /api/v1/health.
type HealthResponse struct {
	Body struct {
		OK            bool      `json:"ok"`
		DBPath        string    `json:"db_path"`
		SchemaVersion int       `json:"schema_version"`
		Version       string    `json:"version"`
		Uptime        string    `json:"uptime"`
		StartedAt     time.Time `json:"started_at"`
	}
}

// InstanceResponse mirrors /api/v1/instance. Surfaces the local kata
// installation's stable identifier so a future spoke client can discover the
// peer it is connecting to. Version and SchemaVersion let the spoke decide
// whether it speaks the same wire and storage contracts before issuing
// further calls.
type InstanceResponse struct {
	Body struct {
		InstanceUID   string `json:"instance_uid"`
		Version       string `json:"version"`
		SchemaVersion int64  `json:"schema_version"`
	}
}

// ProjectStatsOut is the per-project aggregate returned by GET
// /api/v1/projects?include=stats. LastEventAt is nil for a project with
// zero events. Spec §7.2.
type ProjectStatsOut struct {
	Open        int        `json:"open"`
	Closed      int        `json:"closed"`
	LastEventAt *time.Time `json:"last_event_at"`
}

// ProjectOut is the API-shape of a project. Metadata carries the project's
// metadata JSON blob verbatim (e.g. {"area":"Personal"}) so consumers can
// read fields like `area` without a per-project follow-up fetch. The type
// is db.JSONBlob, which marshals on the wire as a raw JSON object — not as
// a JSON-encoded string.
type ProjectOut struct {
	ID        int64       `json:"id"`
	UID       string      `json:"uid"`
	Name      string      `json:"name"`
	Metadata  db.JSONBlob `json:"metadata"`
	Revision  int64       `json:"revision"`
	CreatedAt time.Time   `json:"created_at"`
	DeletedAt *time.Time  `json:"deleted_at,omitempty"`

	// Stats is populated only when the request carries ?include=stats.
	// Wired in Task 3.
	Stats *ProjectStatsOut `json:"stats,omitempty"`
}

// ResolveProjectRequest is POST /api/v1/projects/resolve.
//
// Inputs are tried in priority order:
//
//  1. Alias: path-free alias-first lookup. The daemon resolves by
//     alias.identity; on miss it falls back to Name (if supplied) and
//     attaches the alias on first-seen. Resolve is strict — the daemon
//     never creates a project on alias miss, so a git-only client whose
//     alias is unregistered gets a 404 (run "kata init").
//  2. Name (without Alias): strict path-free name lookup.
//  3. StartPath: legacy local-daemon flow that walks the daemon's
//     filesystem from the client-supplied path. Only useful when the
//     client and daemon share a filesystem.
type ResolveProjectRequest struct {
	Body struct {
		Name      string      `json:"name,omitempty" doc:"project name; required for first-seen alias attach"`
		Alias     *AliasInput `json:"alias,omitempty" doc:"client-derived alias metadata; daemon resolves alias first, then falls back to name"`
		StartPath string      `json:"start_path,omitempty" doc:"absolute path to resolve from (daemon-side filesystem); legacy local-only fallback"`
	}
}

// ProjectResolveBody is the JSON body field of a successful resolve response.
type ProjectResolveBody struct {
	Project       ProjectOut      `json:"project"`
	Alias         db.ProjectAlias `json:"alias"`
	WorkspaceRoot string          `json:"workspace_root,omitempty"`
}

// ResolveProjectResponse wraps ProjectResolveBody.
type ResolveProjectResponse struct {
	Body ProjectResolveBody
}

// AliasInput is the alias metadata a remote client can supply during
// path-free init so the daemon attaches/reassigns the alias without
// stat'ing the client's workspace. Mirrors config.AliasInfo on the
// wire.
type AliasInput struct {
	Identity string `json:"identity" doc:"alias identity (normalized git remote or local://<abs>)"`
	Kind     string `json:"kind" doc:"\"git\" or \"local\""`
	RootPath string `json:"root_path" doc:"client-side path the alias roots at; daemon stores but never stats"`
}

// InitProjectRequest is POST /api/v1/projects (used by `kata init`).
//
// Two modes:
//
// Exactly one of start_path or name must be set.
type InitProjectRequest struct {
	Body struct {
		StartPath string      `json:"start_path,omitempty" doc:"absolute path on the daemon's filesystem; omit for path-free init"`
		Name      string      `json:"name,omitempty" doc:"project name; required when start_path is empty"`
		Replace   bool        `json:"replace,omitempty"`
		Reassign  bool        `json:"reassign,omitempty"`
		Alias     *AliasInput `json:"alias,omitempty" doc:"client-derived alias metadata; only honored when start_path is empty"`
	}
}

// InitProjectResponse uses ProjectResolveBody plus a "created" flag.
type InitProjectResponse struct {
	Body struct {
		ProjectResolveBody
		Created bool `json:"created"`
	}
}

// ListProjectsResponse is GET /api/v1/projects.
type ListProjectsResponse struct {
	Body struct {
		Projects []ProjectOut `json:"projects"`
	}
}

// ShowProjectResponse is GET /api/v1/projects/{id}.
type ShowProjectResponse struct {
	Body struct {
		Project ProjectOut        `json:"project"`
		Aliases []db.ProjectAlias `json:"aliases"`
	}
}

// RenameProjectRequest is PATCH /api/v1/projects/{id}.
type RenameProjectRequest struct {
	ProjectID int64 `path:"project_id" required:"true"`
	Body      struct {
		Name string `json:"name" required:"true"`
	}
}

// MergeProjectRequest is POST /api/v1/projects/{id}/merge.
type MergeProjectRequest struct {
	ProjectID int64 `path:"project_id" required:"true"`
	Body      struct {
		SourceProjectID int64  `json:"source_project_id" required:"true"`
		TargetName      string `json:"target_name,omitempty"`
	}
}

// MergeShortIDExtension is the API projection of db.ShortIDExtension: one
// source-side issue whose short_id was auto-extended during merge to break a
// collision with an existing target-side short_id. UID is stable across the
// shift; PreMergeShortID is the value the issue carried on the source
// project, PostMergeShortID is the value it now carries on the target.
type MergeShortIDExtension struct {
	UID              string `json:"uid"`
	PreMergeShortID  string `json:"pre_merge_short_id"`
	PostMergeShortID string `json:"post_merge_short_id"`
}

// MergeProjectResultOut summarizes a completed project merge using
// the API-owned ProjectOut projection. Mirrors db.ProjectMergeResult
// but routes Source and Target through the projection so the wire
// shape doesn't depend on internal db.Project fields.
//
// ShortIDExtensions reports source-side issues whose short_id was extended
// during the merge to break a collision with an existing target-side
// short_id (spec §9.4); omitted when no extensions ran.
type MergeProjectResultOut struct {
	Source            ProjectOut              `json:"source"`
	Target            ProjectOut              `json:"target"`
	IssuesMoved       int64                   `json:"issues_moved"`
	AliasesMoved      int64                   `json:"aliases_moved"`
	EventsMoved       int64                   `json:"events_moved"`
	PurgeLogsMoved    int64                   `json:"purge_logs_moved"`
	ShortIDExtensions []MergeShortIDExtension `json:"short_id_extensions,omitempty"`
}

// MergeProjectResponse summarizes a completed project merge.
type MergeProjectResponse struct {
	Body MergeProjectResultOut
}

// CreateIssueRequest is POST /api/v1/projects/{id}/issues.
//
// IdempotencyKey is read from the Idempotency-Key HTTP header (spec §4.4).
// Body.ForceNew bypasses look-alike soft-block but is overridden by an
// idempotent match (idempotency wins per spec §3.7).
type CreateIssueRequest struct {
	ProjectID      int64  `path:"project_id" required:"true"`
	IdempotencyKey string `header:"Idempotency-Key"`
	Body           struct {
		Actor    string                  `json:"actor" required:"true"`
		Title    string                  `json:"title" required:"true"`
		Body     string                  `json:"body,omitempty"`
		Owner    *string                 `json:"owner,omitempty"`
		Priority *int64                  `json:"priority,omitempty"`
		Labels   []string                `json:"labels,omitempty"`
		Links    []CreateInitialLinkBody `json:"links,omitempty"`
		ForceNew bool                    `json:"force_new,omitempty"`
	}
}

// CreateInitialLinkBody is one entry in CreateIssueRequest.Body.Links.
//
// ToRef is a short_id, qualified short_id ("kata#abc4"), or a 26-char ULID;
// the daemon resolves it to the target issue at request time. Default
// direction: the new issue is the link's "from" side (e.g. for type=blocks
// the new issue blocks ToRef). Setting Incoming=true reverses for
// type=blocks so the link runs from ToRef to the new issue (i.e. the new
// issue is blocked by ToRef). Incoming=true is rejected for type=parent (no
// inverse parent direction is exposed) and is a no-op for type=related
// (which is symmetric).
type CreateInitialLinkBody struct {
	Type     string `json:"type" enum:"parent,blocks,related"`
	ToRef    string `json:"to_ref"`
	Incoming bool   `json:"incoming,omitempty"`
}

// MutationResponse is the standard mutation envelope (§4.5). OriginalEvent is
// non-nil only on idempotent reuse — the issue.created event row of the prior
// creation, so clients can correlate the reuse to the original mutation.
type MutationResponse struct {
	Body struct {
		Issue         db.Issue  `json:"issue"`
		Event         *db.Event `json:"event"`
		OriginalEvent *db.Event `json:"original_event,omitempty"`
		Changed       bool      `json:"changed"`
		Reused        bool      `json:"reused,omitempty"`
	}
}

// ListIssuesRequest is GET /api/v1/projects/{id}/issues. Priority and
// MaxPriority are decoded as strings so the absent-vs-zero distinction
// survives Huma's query parsing (which forbids pointer query types). Empty
// string means no filter; otherwise parsed as 0..4.
type ListIssuesRequest struct {
	ProjectID   int64  `path:"project_id" required:"true"`
	Status      string `query:"status,omitempty" enum:"open,closed,"`
	Priority    string `query:"priority,omitempty" doc:"exact priority filter (0..4); empty = no filter"`
	MaxPriority string `query:"max_priority,omitempty" doc:"include only priority <= this value (0..4); empty = no filter"`
	Limit       int    `query:"limit,omitempty"`
}

// ListAllIssuesRequest is GET /api/v1/issues — the cross-project list. The
// optional project_id query param narrows to a single project for callers
// that want one trip through this surface; omit it for the all-projects feed.
// Priority/MaxPriority are encoded the same way as ListIssuesRequest.
type ListAllIssuesRequest struct {
	ProjectID   int64  `query:"project_id,omitempty"`
	Status      string `query:"status,omitempty" enum:"open,closed,"`
	Priority    string `query:"priority,omitempty" doc:"exact priority filter (0..4); empty = no filter"`
	MaxPriority string `query:"max_priority,omitempty" doc:"include only priority <= this value (0..4); empty = no filter"`
	Limit       int    `query:"limit,omitempty"`

	// Deprecated query params kept on the struct only to surface a 400 if a
	// stale client still sends them — the daemon used to honor view= /
	// area= / offset= and the X-Kata-Client-TZ header for server-computed
	// named views (today/upcoming/inbox/someday/anytime/logbook). Views were
	// removed; consumers now assemble them client-side from the unfiltered
	// /api/v1/issues + /api/v1/projects responses. Silently ignoring these
	// params would let stale clients get plausible-but-wrong results.
	DeprecatedView     string `query:"view,omitempty" hidden:"true" doc:"REMOVED — assemble views client-side"`
	DeprecatedArea     string `query:"area,omitempty" hidden:"true" doc:"REMOVED — filter by project.metadata.area client-side"`
	DeprecatedOffset   string `query:"offset,omitempty" hidden:"true" doc:"REMOVED — view pagination is consumer-side"`
	DeprecatedClientTZ string `header:"X-Kata-Client-TZ" hidden:"true" doc:"REMOVED — compute today_local on the consumer"`
}

// IssueOut is the wire projection of one row in ListIssuesResponse.
// It embeds db.Issue (every persistence column flattens to the top
// level on JSON marshal — including UID and ShortID) and adds row
// metadata the daemon hydrates from relationship tables: labels,
// parent/child summary, outgoing blocker edges, and the rendered
// QualifiedID ("<project>#<short_id>") for human-facing displays.
// Kept separate from db.Issue so the persistence struct stays free of
// wire-only state; rolling labels into db.Issue would force every db
// query path to know whether labels were hydrated, which they aren't
// (LabelsByIssue / LabelsByIssues are explicit calls).
//
// Blocks/BlockedBy/Related carry structured LinkPeer entries (UID +
// short_id) so callers can correlate across short_id cutovers without
// a follow-up join.
//
// omitempty drops the field on rows with no labels so the wire
// payload doesn't carry an empty array per row on label-sparse
// projects.
type IssueOut struct {
	db.Issue
	QualifiedID   string          `json:"qualified_id"`
	Labels        []string        `json:"labels,omitempty"`
	ParentShortID *string         `json:"parent_short_id,omitempty"`
	ChildCounts   *db.ChildCounts `json:"child_counts,omitempty"`
	Blocks        []LinkPeer      `json:"blocks,omitempty"`
	BlockedBy     []LinkPeer      `json:"blocked_by,omitempty"`
	Related       []LinkPeer      `json:"related,omitempty"`
}

// ListIssuesResponse is the list payload. Plan 8 commit 5b: each row
// is now an IssueOut (db.Issue + Labels) so the TUI list view can
// render label chips without an extra fetch per row.
type ListIssuesResponse struct {
	Body struct {
		Issues []IssueOut `json:"issues"`
	}
}

// IssueRef is the compact issue identity used for parent context. UID is
// canonical; ShortID and QualifiedID are display projections rendered at the
// API boundary.
type IssueRef struct {
	UID         string `json:"uid"`
	ShortID     string `json:"short_id"`
	QualifiedID string `json:"qualified_id"`
	Title       string `json:"title"`
	Status      string `json:"status"`
}

// ShowIssueRequest is GET /api/v1/projects/{id}/issues/{ref}. Ref accepts a
// short_id (e.g. "abc4"), a qualified short_id (e.g. "kata#abc4"), or a
// 26-char ULID; the daemon's path resolver picks the matching column.
// IncludeDeleted=true allows fetching soft-deleted issues; default returns 404
// for them.
type ShowIssueRequest struct {
	ProjectID      int64  `path:"project_id" required:"true"`
	Ref            string `path:"ref" required:"true"`
	IncludeDeleted bool   `query:"include_deleted,omitempty"`
}

// ShowIssueByUIDRequest is GET /api/v1/issues/{uid}. UID is globally unique
// across projects, so the route does not need a project path segment.
type ShowIssueByUIDRequest struct {
	UID            string `path:"uid" required:"true"`
	IncludeDeleted bool   `query:"include_deleted,omitempty"`
}

// ShowIssueResponse is the per-issue read payload (Plan 2: + links, + labels).
type ShowIssueResponse struct {
	Body struct {
		Issue    db.Issue        `json:"issue"`
		Comments []db.Comment    `json:"comments"`
		Links    []LinkOut       `json:"links"`
		Labels   []db.IssueLabel `json:"labels"`
		Parent   *IssueRef       `json:"parent,omitempty"`
		Children []IssueOut      `json:"children,omitempty"`
	}
}

// EditIssueRequest is PATCH /api/v1/projects/{id}/issues/{ref}.
type EditIssueRequest struct {
	ProjectID int64  `path:"project_id" required:"true"`
	Ref       string `path:"ref" required:"true"`
	Body      struct {
		Actor         string      `json:"actor" required:"true"`
		Title         *string     `json:"title,omitempty"`
		Body          *string     `json:"body,omitempty"`
		Owner         *string     `json:"owner,omitempty"`
		SetPriority   *int64      `json:"set_priority,omitempty"`
		ClearPriority bool        `json:"clear_priority,omitempty"`
		LinksDelta    *LinksDelta `json:"links_delta,omitempty"`
	}
}

// LinksDelta describes a batched relationship mutation applied as part of
// PATCH /issues/{ref}. Each entry is a target issue ref (short_id, qualified
// short_id, or ULID — same accepted by the path parameter); direction is
// encoded by the field name from the URL issue's POV.
//
//	add_blocks        — URL issue blocks ref
//	add_blocked_by    — ref blocks URL issue
//	add_related       — URL issue related to ref (canonicalized server-side)
//	set_parent        — set URL issue's parent (replaces existing)
//	remove_parent     — strict: must equal current parent
//	remove_blocks/_blocked_by/_related — idempotent
type LinksDelta struct {
	SetParent       *string  `json:"set_parent,omitempty"`
	RemoveParent    *string  `json:"remove_parent,omitempty"`
	AddBlocks       []string `json:"add_blocks,omitempty"`
	AddBlockedBy    []string `json:"add_blocked_by,omitempty"`
	AddRelated      []string `json:"add_related,omitempty"`
	RemoveBlocks    []string `json:"remove_blocks,omitempty"`
	RemoveBlockedBy []string `json:"remove_blocked_by,omitempty"`
	RemoveRelated   []string `json:"remove_related,omitempty"`
}

// LinkChanges reports link mutations actually applied. Every entry carries the
// peer's UID and short_id so callers can correlate without a follow-up
// lookup. Empty fields are omitted; entirely empty LinkChanges means every
// link op was a no-op.
type LinkChanges struct {
	ParentSet        *LinkPeer  `json:"parent_set,omitempty"`
	ParentRemoved    *LinkPeer  `json:"parent_removed,omitempty"`
	BlocksAdded      []LinkPeer `json:"blocks_added,omitempty"`
	BlocksRemoved    []LinkPeer `json:"blocks_removed,omitempty"`
	BlockedByAdded   []LinkPeer `json:"blocked_by_added,omitempty"`
	BlockedByRemoved []LinkPeer `json:"blocked_by_removed,omitempty"`
	RelatedAdded     []LinkPeer `json:"related_added,omitempty"`
	RelatedRemoved   []LinkPeer `json:"related_removed,omitempty"`
}

// EditIssueResponse extends MutationResponse with a Changes block describing
// link mutations actually applied. Field-only edits leave Changes empty.
//
// A single PATCH can emit up to three events (issue.updated for non-priority
// field changes, issue.priority_set/_cleared for priority, issue.links_changed
// for links). Events carries the full ordered slice. Event is retained as a
// compatibility alias holding the FINAL event from that slice — older
// clients that only knew one-event-per-mutation continue to work, while
// new clients can walk the full slice to observe every transition (e.g.
// distinguishing a priority change from a link change).
type EditIssueResponse struct {
	Body struct {
		Issue   db.Issue     `json:"issue"`
		Event   *db.Event    `json:"event"`
		Events  []db.Event   `json:"events,omitempty"`
		Changed bool         `json:"changed"`
		Changes *LinkChanges `json:"changes,omitempty"`
	}
}

// CommentRequest is POST /api/v1/projects/{id}/issues/{ref}/comments.
type CommentRequest struct {
	ProjectID int64  `path:"project_id" required:"true"`
	Ref       string `path:"ref" required:"true"`
	Body      struct {
		Actor string `json:"actor" required:"true"`
		Body  string `json:"body" required:"true"`
	}
}

// CommentResponse mirrors MutationResponse but adds the new comment row.
type CommentResponse struct {
	Body struct {
		Issue   db.Issue   `json:"issue"`
		Comment db.Comment `json:"comment"`
		Event   *db.Event  `json:"event"`
		Changed bool       `json:"changed"`
	}
}

// ActionRequest is POST /api/v1/projects/{id}/issues/{ref}/actions/close|reopen.
// Reason is enforced to the schema's CHECK list so unsupported values surface
// as 400 validation rather than a SQLite constraint failure (500 internal).
// Message, Evidence, and DryRun are close-only inputs (anti-agent-justification);
// reopen ignores them.
type ActionRequest struct {
	ProjectID int64  `path:"project_id" required:"true"`
	Ref       string `path:"ref" required:"true"`
	Body      struct {
		Actor   string `json:"actor" required:"true"`
		Reason  string `json:"reason,omitempty" enum:"done,wontfix,duplicate,superseded,audit-no-change,"`
		Message string `json:"message,omitempty"`
		// Source signals the caller's UI surface. "tui" relaxes the
		// substance / evidence validation so an interactive human close
		// is one keystroke. Structural guards (parent-close, sibling
		// throttle) still apply. Empty string means "agent / CLI" and
		// gets the full validation. Lying about Source is possible and
		// considered acceptable: agents who'd forge this could also
		// forge the evidence payload, so adding a hard signal here would
		// only inconvenience honest callers.
		Source   string     `json:"source,omitempty" enum:"tui,"`
		Evidence []Evidence `json:"evidence,omitempty"`
		DryRun   bool       `json:"dry_run,omitempty"`
	}
}

// CreateLinkRequest is POST /api/v1/projects/{id}/issues/{ref}/links.
type CreateLinkRequest struct {
	ProjectID int64  `path:"project_id" required:"true"`
	Ref       string `path:"ref" required:"true"`
	Body      struct {
		Actor   string `json:"actor" required:"true"`
		Type    string `json:"type" required:"true" enum:"parent,blocks,related"`
		ToRef   string `json:"to_ref" required:"true"`
		Replace bool   `json:"replace,omitempty"` // type=parent only
	}
}

// LinkPeer pairs an issue UID with the rendered short_id. UID is the stable
// reference; short_id is a display snapshot that may shift across a short_id
// cutover or federation merge. Used by every wire shape that carries a link
// endpoint (LinkOut, LinkChanges, IssueOut.Blocks/BlockedBy/Related).
type LinkPeer struct {
	UID     string `json:"uid"`
	ShortID string `json:"short_id"`
}

// LinkOut is the wire projection of a link with both endpoints rendered as
// LinkPeer (UID + short_id) so clients can correlate by either identifier
// without an extra lookup.
type LinkOut struct {
	ID        int64     `json:"id"`
	ProjectID int64     `json:"project_id"`
	From      LinkPeer  `json:"from"`
	To        LinkPeer  `json:"to"`
	Type      string    `json:"type"`
	Author    string    `json:"author"`
	CreatedAt time.Time `json:"created_at"`
}

// CreateLinkResponse extends MutationResponse with the new link's wire
// projection (handlers populate `Link` for both new and no-op cases).
type CreateLinkResponse struct {
	Body struct {
		Issue   db.Issue  `json:"issue"`
		Link    LinkOut   `json:"link"`
		Event   *db.Event `json:"event"`
		Changed bool      `json:"changed"`
	}
}

// DeleteLinkRequest is DELETE /api/v1/projects/{id}/issues/{ref}/links/{link_id}.
// Actor is in the query string because DELETE bodies are non-portable.
type DeleteLinkRequest struct {
	ProjectID int64  `path:"project_id" required:"true"`
	Ref       string `path:"ref" required:"true"`
	LinkID    int64  `path:"link_id" required:"true"`
	Actor     string `query:"actor" required:"true"`
}

// RemoveProjectRequest is DELETE /api/v1/projects/{id} — archives the project
// (#24). Force=true overrides the open-issue refusal.
type RemoveProjectRequest struct {
	ProjectID int64  `path:"project_id" required:"true"`
	Actor     string `query:"actor" required:"true"`
	Force     bool   `query:"force,omitempty"`
}

// RemoveProjectResponse carries the archived project + the project.removed
// event the caller can replay or display.
type RemoveProjectResponse struct {
	Body struct {
		Project ProjectOut `json:"project"`
		Event   *db.Event  `json:"event"`
	}
}

// RestoreProjectRequest is POST /api/v1/projects/{id}/restore. The action is
// idempotent: already-active projects return changed=false with no event.
type RestoreProjectRequest struct {
	ProjectID int64  `path:"project_id" required:"true"`
	Actor     string `query:"actor" required:"true"`
}

// RestoreProjectResponse carries the active project, optional project.restored
// event, and changed=false for retry/no-op restores.
type RestoreProjectResponse struct {
	Body struct {
		Project ProjectOut `json:"project"`
		Event   *db.Event  `json:"event"`
		Changed bool       `json:"changed"`
	}
}

// DetachProjectAliasRequest is DELETE /api/v1/projects/{id}/aliases/{alias_id}.
// Force=true overrides the last-alias refusal.
type DetachProjectAliasRequest struct {
	ProjectID int64  `path:"project_id" required:"true"`
	AliasID   int64  `path:"alias_id" required:"true"`
	Actor     string `query:"actor" required:"true"`
	Force     bool   `query:"force,omitempty"`
}

// DetachProjectAliasResponse carries the dropped alias + the
// project.alias_removed event.
type DetachProjectAliasResponse struct {
	Body struct {
		Alias db.ProjectAlias `json:"alias"`
		Event *db.Event       `json:"event"`
	}
}

// AddLabelRequest is POST /api/v1/projects/{id}/issues/{ref}/labels.
type AddLabelRequest struct {
	ProjectID int64  `path:"project_id" required:"true"`
	Ref       string `path:"ref" required:"true"`
	Body      struct {
		Actor string `json:"actor" required:"true"`
		Label string `json:"label" required:"true"`
	}
}

// AddLabelResponse extends the standard envelope with the new label row.
type AddLabelResponse struct {
	Body struct {
		Issue   db.Issue      `json:"issue"`
		Label   db.IssueLabel `json:"label"`
		Event   *db.Event     `json:"event"`
		Changed bool          `json:"changed"`
	}
}

// RemoveLabelRequest is DELETE /api/v1/projects/{id}/issues/{ref}/labels/{label}.
type RemoveLabelRequest struct {
	ProjectID int64  `path:"project_id" required:"true"`
	Ref       string `path:"ref" required:"true"`
	Label     string `path:"label" required:"true"`
	Actor     string `query:"actor" required:"true"`
}

// AssignRequest is POST /api/v1/projects/{id}/issues/{ref}/actions/assign.
type AssignRequest struct {
	ProjectID int64  `path:"project_id" required:"true"`
	Ref       string `path:"ref" required:"true"`
	Body      struct {
		Actor string `json:"actor" required:"true"`
		Owner string `json:"owner" required:"true"`
	}
}

// PriorityRequest is POST /api/v1/projects/{id}/issues/{ref}/actions/priority.
// Priority is the new value 0..4 (0=highest, 4=lowest); omitting the field or
// passing null clears the issue's priority. The handler emits
// issue.priority_set or issue.priority_cleared depending on the transition,
// or no event when the new value matches the current one.
type PriorityRequest struct {
	ProjectID int64  `path:"project_id" required:"true"`
	Ref       string `path:"ref" required:"true"`
	Body      struct {
		Actor    string `json:"actor" required:"true"`
		Priority *int64 `json:"priority,omitempty"`
	}
}

// UnassignRequest is POST /api/v1/projects/{id}/issues/{ref}/actions/unassign.
// Same shape as AssignRequest minus owner.
type UnassignRequest struct {
	ProjectID int64  `path:"project_id" required:"true"`
	Ref       string `path:"ref" required:"true"`
	Body      struct {
		Actor string `json:"actor" required:"true"`
	}
}

// ReadyRequest is GET /api/v1/projects/{id}/ready.
type ReadyRequest struct {
	ProjectID int64 `path:"project_id" required:"true"`
	Limit     int   `query:"limit,omitempty"`
}

// ReadyResponse is the ready-issue list.
type ReadyResponse struct {
	Body struct {
		Issues []db.Issue `json:"issues"`
	}
}

// LabelsListRequest is GET /api/v1/projects/{id}/labels (counts).
type LabelsListRequest struct {
	ProjectID int64 `path:"project_id" required:"true"`
}

// LabelsListResponse is the per-label aggregate.
type LabelsListResponse struct {
	Body struct {
		Labels []db.LabelCount `json:"labels"`
	}
}

// DestructiveActionRequest is POST /api/v1/projects/{id}/issues/{ref}/actions/delete
// and .../actions/purge. Confirm is read from the X-Kata-Confirm header per
// spec §4.4 and must equal the exact strings "DELETE <project>#<short_id>" /
// "PURGE <project>#<short_id>".
type DestructiveActionRequest struct {
	ProjectID int64  `path:"project_id" required:"true"`
	Ref       string `path:"ref" required:"true"`
	Confirm   string `header:"X-Kata-Confirm"`
	Body      struct {
		Actor  string `json:"actor" required:"true"`
		Reason string `json:"reason,omitempty"` // purge only; lands in purge_log.reason
	}
}

// RestoreRequest is POST /api/v1/projects/{id}/issues/{ref}/actions/restore.
// No confirmation header — restore is reversible and idempotent.
type RestoreRequest struct {
	ProjectID int64  `path:"project_id" required:"true"`
	Ref       string `path:"ref" required:"true"`
	Body      struct {
		Actor string `json:"actor" required:"true"`
	}
}

// PurgeResponse extends the standard envelope with the purge_log row so callers
// see the captured counts and reserved SSE cursor without a follow-up GET.
type PurgeResponse struct {
	Body struct {
		PurgeLog db.PurgeLog `json:"purge_log"`
	}
}

// DigestRequest is GET /api/v1/digest (cross-project) and
// /api/v1/projects/{project_id}/digest (per-project). Since/Until are
// RFC3339 timestamps. Actor is a repeated query param: ?actor=alice&actor=bob.
type DigestRequest struct {
	Since  string   `query:"since" required:"true"`
	Until  string   `query:"until,omitempty"`
	Actors []string `query:"actor,omitempty"`
}

// DigestProjectRequest is the per-project variant. Only the path param differs.
type DigestProjectRequest struct {
	ProjectID int64    `path:"project_id" required:"true"`
	Since     string   `query:"since" required:"true"`
	Until     string   `query:"until,omitempty"`
	Actors    []string `query:"actor,omitempty"`
}

// DigestTotals is the per-actor and grand-total breakdown of mutations the
// digest understands. Categories that do not apply to a window are zero, not
// omitted, so renderers can rely on the field set.
type DigestTotals struct {
	Created         int `json:"created"`
	Closed          int `json:"closed"`
	Reopened        int `json:"reopened"`
	Commented       int `json:"commented"`
	Edited          int `json:"edited"`
	Assigned        int `json:"assigned"`
	Unassigned      int `json:"unassigned"`
	PrioritySet     int `json:"priority_set"`
	PriorityCleared int `json:"priority_cleared"`
	Labeled         int `json:"labeled"`
	Unlabeled       int `json:"unlabeled"`
	Linked          int `json:"linked"`
	Unlinked        int `json:"unlinked"`
	Unblocked       int `json:"unblocked"`
	Deleted         int `json:"deleted"`
	Restored        int `json:"restored"`
	Other           int `json:"other"`
}

// DigestIssueActions is the per-issue summary inside one actor's section.
// IssueShortID/IssueUID identify the issue (UID is canonical; short_id is a
// display snapshot). Actions is a stable, ordered list of human-readable
// action tokens (e.g. "created", "commented:2", "closed:done", "labeled:bug",
// "unblocks kata#abc4"). The aggregator collapses repeated comments into a
// count and joins the close reason / label name into the token so the
// renderer can stay dumb.
type DigestIssueActions struct {
	ProjectID    int64    `json:"project_id"`
	ProjectName  string   `json:"project_name"`
	IssueUID     string   `json:"issue_uid"`
	IssueShortID string   `json:"issue_short_id"`
	Actions      []string `json:"actions"`
}

// DigestActorEntry is one actor's slice of the digest. Issues is sorted by
// issue UID for stable rendering.
type DigestActorEntry struct {
	Actor  string               `json:"actor"`
	Totals DigestTotals         `json:"totals"`
	Issues []DigestIssueActions `json:"issues"`
}

// DigestResponse is the digest payload. ProjectID is 0 for cross-project
// requests, otherwise the requested project. Actors is sorted by actor name.
type DigestResponse struct {
	Body struct {
		Since      time.Time          `json:"since"`
		Until      time.Time          `json:"until"`
		ProjectID  int64              `json:"project_id"`
		EventCount int                `json:"event_count"`
		Totals     DigestTotals       `json:"totals"`
		Actors     []DigestActorEntry `json:"actors"`
	}
}

// SearchRequest is GET /api/v1/projects/{id}/search?q=...&limit=...&include_deleted=...
type SearchRequest struct {
	ProjectID      int64  `path:"project_id" required:"true"`
	Query          string `query:"q" required:"true"`
	Limit          int    `query:"limit,omitempty"`
	IncludeDeleted bool   `query:"include_deleted,omitempty"`
}

// SearchHit is one row in SearchResponse. Score is the negated raw BM25
// (higher = better match), MatchedIn is the FTS columns that contributed.
type SearchHit struct {
	Issue     db.Issue `json:"issue"`
	Score     float64  `json:"score"`
	MatchedIn []string `json:"matched_in"`
}

// SearchResponse mirrors spec §4.10.
type SearchResponse struct {
	Body struct {
		Query   string      `json:"query"`
		Results []SearchHit `json:"results"`
	}
}

// ImportRequest is POST /api/v1/projects/{project_id}/imports. It carries a
// normalized external issue batch that the daemon passes to db.ImportBatch.
type ImportRequest struct {
	ProjectID int64 `path:"project_id" required:"true"`
	Body      struct {
		Actor  string             `json:"actor" required:"true"`
		Source string             `json:"source" required:"true"`
		Items  []ImportIssueInput `json:"items"`
	}
}

// ImportIssueInput is one normalized issue in an import request.
type ImportIssueInput struct {
	ExternalID   string               `json:"external_id" required:"true"`
	Title        string               `json:"title" required:"true"`
	Body         string               `json:"body,omitempty"`
	Author       string               `json:"author" required:"true"`
	Owner        *string              `json:"owner,omitempty"`
	Priority     *int64               `json:"priority,omitempty"`
	Status       string               `json:"status" enum:"open,closed"`
	ClosedReason *string              `json:"closed_reason,omitempty" enum:"done,wontfix,duplicate,superseded,audit-no-change,"`
	CreatedAt    time.Time            `json:"created_at" required:"true"`
	UpdatedAt    time.Time            `json:"updated_at" required:"true"`
	ClosedAt     *time.Time           `json:"closed_at,omitempty"`
	Labels       []string             `json:"labels,omitempty"`
	Comments     []ImportCommentInput `json:"comments,omitempty"`
	Links        []ImportLinkInput    `json:"links,omitempty"`
}

// ImportCommentInput is one normalized external comment.
type ImportCommentInput struct {
	ExternalID string    `json:"external_id" required:"true"`
	Author     string    `json:"author" required:"true"`
	Body       string    `json:"body" required:"true"`
	CreatedAt  time.Time `json:"created_at" required:"true"`
}

// ImportLinkInput is one normalized external relationship. TargetExternalID
// resolves against issues from the same source and project.
type ImportLinkInput struct {
	Type             string `json:"type" required:"true" enum:"parent,blocks,related"`
	TargetExternalID string `json:"target_external_id" required:"true"`
}

// ImportResponse returns db.ImportBatchResult at the response body top level.
type ImportResponse struct {
	Body db.ImportBatchResult
}

// AuditClosesRequest is GET /api/v1/audit/closes. The window defaults to
// (zero, now). Filters compose via AND; all are optional. NoEvidence
// narrows to closes whose `Flags` includes "no-evidence".
type AuditClosesRequest struct {
	ProjectID  int64  `query:"project_id" required:"true"`
	Since      string `query:"since,omitempty" doc:"RFC3339 timestamp (default: zero time)"`
	Until      string `query:"until,omitempty" doc:"RFC3339 timestamp (default: now)"`
	Actor      string `query:"actor,omitempty"`
	Parent     string `query:"parent,omitempty" doc:"filter to closes of children of parent <ref>"`
	Reason     string `query:"reason,omitempty"`
	NoEvidence bool   `query:"no_evidence,omitempty"`
}

// AuditCloseRow is one row in AuditClosesResponse. Flags includes
// computed markers — "no-evidence" when an evidence-required close
// carried no items, "throttled" when this actor previously tripped a
// throttle on this issue (sibling-burst or duplicate-message) before
// the close eventually succeeded. EvidenceTypes lists the typed
// evidence items from the close event payload (e.g. "commit", "pr").
// Message is the close message verbatim.
type AuditCloseRow struct {
	Time          string   `json:"time"`
	Actor         string   `json:"actor"`
	Issue         string   `json:"issue"`
	Parent        string   `json:"parent,omitempty"`
	Reason        string   `json:"reason"`
	EvidenceTypes []string `json:"evidence_types,omitempty"`
	Flags         []string `json:"flags,omitempty"`
	Message       string   `json:"message,omitempty"`
}

// AuditClosesResponse wraps the AuditCloseRow list. Rows is never nil
// in JSON output (handler emits an empty slice when no rows match).
type AuditClosesResponse struct {
	Body struct {
		Rows []AuditCloseRow `json:"rows"`
	}
}

// RecurrenceTemplateInput is the JSON wire shape for the template fields
// embedded in a CreateRecurrenceRequest. Mirrors db.RecurrenceTemplate but
// stays in the api package so the public surface doesn't leak db-package
// types into request bodies. Labels are accepted as a JSON array of strings;
// metadata is an opaque JSON object.
type RecurrenceTemplateInput struct {
	Title    string          `json:"title" required:"true"`
	Body     string          `json:"body,omitempty"`
	Owner    *string         `json:"owner,omitempty"`
	Priority *int64          `json:"priority,omitempty"`
	Labels   []string        `json:"labels,omitempty"`
	Metadata json.RawMessage `json:"metadata,omitempty"`
}

// CreateRecurrenceRequest is POST /api/v1/projects/{project_id}/recurrences.
type CreateRecurrenceRequest struct {
	ProjectID int64 `path:"project_id" required:"true"`
	Body      struct {
		Actor    string                  `json:"actor" required:"true"`
		RRule    string                  `json:"rrule" required:"true"`
		DTStart  string                  `json:"dtstart" required:"true"`
		Timezone string                  `json:"timezone" required:"true"`
		Template RecurrenceTemplateInput `json:"template"`
	}
}

// CreateRecurrenceResponse returns the new recurrence row.
type CreateRecurrenceResponse struct {
	Body struct {
		Recurrence db.Recurrence `json:"recurrence"`
	}
}

// ListRecurrencesRequest is GET /api/v1/projects/{project_id}/recurrences.
type ListRecurrencesRequest struct {
	ProjectID int64 `path:"project_id" required:"true"`
}

// ListRecurrencesResponse wraps the recurrence list.
type ListRecurrencesResponse struct {
	Body struct {
		Recurrences []db.Recurrence `json:"recurrences"`
	}
}

// ShowRecurrenceRequest is GET /api/v1/projects/{project_id}/recurrences/{recurrence_uid}.
type ShowRecurrenceRequest struct {
	ProjectID     int64  `path:"project_id" required:"true"`
	RecurrenceUID string `path:"recurrence_uid" required:"true"`
}

// ShowRecurrenceResponse returns a single recurrence row.
type ShowRecurrenceResponse struct {
	Body struct {
		Recurrence db.Recurrence `json:"recurrence"`
	}
}

// RecurrenceTemplateUpdateInput carries the partial-update shape for the
// recurrence template. All fields are optional pointers; nil means "no change".
type RecurrenceTemplateUpdateInput struct {
	Title    *string          `json:"title,omitempty"`
	Body     *string          `json:"body,omitempty"`
	Owner    *string          `json:"owner,omitempty"`
	Priority *int64           `json:"priority,omitempty"`
	Labels   *[]string        `json:"labels,omitempty"`
	Metadata *json.RawMessage `json:"metadata,omitempty"`
}

// PatchRecurrenceRequest is PATCH /api/v1/projects/{project_id}/recurrences/{recurrence_uid}.
// If-Match is required and carries the current "rev-N" ETag for optimistic concurrency.
type PatchRecurrenceRequest struct {
	ProjectID     int64  `path:"project_id" required:"true"`
	RecurrenceUID string `path:"recurrence_uid" required:"true"`
	IfMatch       string `header:"If-Match"`
	Body          struct {
		Actor    string                         `json:"actor" required:"true"`
		RRule    *string                        `json:"rrule,omitempty"`
		DTStart  *string                        `json:"dtstart,omitempty"`
		Timezone *string                        `json:"timezone,omitempty"`
		Template *RecurrenceTemplateUpdateInput `json:"template,omitempty"`
	}
}

// PatchRecurrenceResponse returns the patched recurrence and the new ETag.
type PatchRecurrenceResponse struct {
	ETag string `header:"ETag"`
	Body struct {
		Recurrence db.Recurrence `json:"recurrence"`
		Changed    bool          `json:"changed"`
	}
}

// DeleteRecurrenceRequest is DELETE /api/v1/projects/{project_id}/recurrences/{recurrence_uid}.
type DeleteRecurrenceRequest struct {
	ProjectID     int64  `path:"project_id" required:"true"`
	RecurrenceUID string `path:"recurrence_uid" required:"true"`
	Actor         string `query:"actor" required:"true"`
}

// DeleteRecurrenceResponse is the 204 No Content envelope for soft-delete.
// The 204 status is set via DefaultStatus in the huma.Operation; no body is returned.
type DeleteRecurrenceResponse struct{}

// PatchIssueMetadataRequest is POST /api/v1/projects/{project_id}/issues/{ref}/metadata.
type PatchIssueMetadataRequest struct {
	ProjectID int64  `path:"project_id" required:"true"`
	Ref       string `path:"ref" required:"true"`
	IfMatch   string `header:"If-Match"`
	Body      struct {
		Actor string                     `json:"actor" required:"true"`
		Patch map[string]json.RawMessage `json:"patch"`
	}
}

// PatchIssueMetadataResponse is the response for POST .../issues/{ref}/metadata.
type PatchIssueMetadataResponse struct {
	ETag string `header:"ETag"`
	Body struct {
		Issue   db.Issue  `json:"issue"`
		Event   *db.Event `json:"event,omitempty"`
		Changed bool      `json:"changed"`
	}
}

// PatchProjectMetadataRequest is POST /api/v1/projects/{project_id}/metadata.
type PatchProjectMetadataRequest struct {
	ProjectID int64  `path:"project_id" required:"true"`
	IfMatch   string `header:"If-Match"`
	Body      struct {
		Actor string                     `json:"actor" required:"true"`
		Patch map[string]json.RawMessage `json:"patch"`
	}
}

// PatchProjectMetadataResponse is the response for POST .../projects/{project_id}/metadata.
type PatchProjectMetadataResponse struct {
	ETag string `header:"ETag"`
	Body struct {
		Project db.Project `json:"project"`
		Event   *db.Event  `json:"event,omitempty"`
		Changed bool       `json:"changed"`
	}
}

// MoveIssueRequest is POST /api/v1/projects/{project_id}/issues/{ref}/actions/move.
// project_id names the source project; the target project is identified by
// its stable UID in the body. The If-Match header carries the issue's
// expected revision in the standard `"rev-N"` form.
type MoveIssueRequest struct {
	ProjectID int64  `path:"project_id" required:"true"`
	Ref       string `path:"ref" required:"true"`
	IfMatch   string `header:"If-Match"`
	Body      struct {
		Actor        string `json:"actor" required:"true"`
		ToProjectUID string `json:"to_project_uid" required:"true"`
	}
}

// MoveIssueResponse is the response for the move action. ETag carries the
// new revision in the standard `"rev-N"` form. NewShortID surfaces the
// short_id freshly allocated in the target project (which may differ from
// the issue's previous short_id when the two projects collide on
// numbering).
type MoveIssueResponse struct {
	ETag string `header:"ETag"`
	Body struct {
		Issue      db.Issue `json:"issue"`
		EventID    int64    `json:"event_id"`
		NewShortID string   `json:"new_short_id"`
		Changed    bool     `json:"changed"`
	}
}

// Server-reserved issue metadata keys (mirrors internal/metadata.IssueRegistry).
// Keys outside this set are accepted opaquely by the daemon.
const (
	MetadataKeyScheduledOn = "scheduled_on"
	MetadataKeyDeadlineOn  = "deadline_on"
	MetadataKeySomeday     = "someday"
	MetadataKeyChecklist   = "checklist"
	MetadataKeyTimezone    = "timezone"
)

// Server-reserved project metadata keys (mirrors internal/metadata.ProjectRegistry).
// Keys outside this set are accepted opaquely by the daemon.
const (
	ProjectMetadataKeyArea = "area"
)
