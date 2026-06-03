package tui

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/kata/internal/api"
)

// Issue is a strict subset of the daemon's wire shape. Labels rides on
// list-row decode (the daemon embeds them per row) and on a manual
// copy from showIssue's body.labels for detail open; the omitempty tag
// keeps absence on a show response from blanking a previously-populated
// slice. The deleted bool is derived from DeletedAt being non-nil.
//
// UID is the canonical reference; ShortID is the per-project display
// snapshot (4+ chars of the ULID suffix, per spec §4); QualifiedID is
// the human-facing "<project>#<short_id>" form populated on list rows
// (api.IssueOut.QualifiedID).
type Issue struct {
	ID            int64        `json:"id"`
	UID           string       `json:"uid,omitempty"`
	ProjectID     int64        `json:"project_id"`
	ProjectUID    string       `json:"project_uid,omitempty"`
	ShortID       string       `json:"short_id"`
	QualifiedID   string       `json:"qualified_id,omitempty"`
	Title         string       `json:"title"`
	Body          string       `json:"body"`
	Status        string       `json:"status"`
	ClosedReason  *string      `json:"closed_reason,omitempty"`
	Owner         *string      `json:"owner,omitempty"`
	Author        string       `json:"author"`
	CreatedAt     time.Time    `json:"created_at"`
	UpdatedAt     time.Time    `json:"updated_at"`
	ClosedAt      *time.Time   `json:"closed_at,omitempty"`
	DeletedAt     *time.Time   `json:"deleted_at,omitempty"`
	Labels        []string     `json:"labels,omitempty"`
	ParentShortID *string      `json:"parent_short_id,omitempty"`
	ChildCounts   *ChildCounts `json:"child_counts,omitempty"`
	Blocks        []LinkPeer   `json:"blocks,omitempty"`
	BlockedBy     []LinkPeer   `json:"blocked_by,omitempty"`
	Related       []LinkPeer   `json:"related,omitempty"`
	Priority      *int64       `json:"priority,omitempty"`
}

// LinkPeer mirrors api.LinkPeer: the canonical UID plus the rendered
// short_id snapshot for one end of a link.
type LinkPeer struct {
	UID     string `json:"uid"`
	ShortID string `json:"short_id"`
}

// ChildCounts is the direct-child aggregate attached to queue/detail rows.
type ChildCounts struct {
	Open  int `json:"open"`
	Total int `json:"total"`
}

// IssueRef is the compact parent issue projection on detail responses.
// Mirrors api.IssueRef: UID is canonical, ShortID and QualifiedID are
// display projections rendered at the API boundary.
type IssueRef struct {
	UID         string `json:"uid"`
	ShortID     string `json:"short_id"`
	QualifiedID string `json:"qualified_id"`
	Title       string `json:"title"`
	Status      string `json:"status"`
}

// ListFilter is the union of filters used by list views. Limit is sent
// on the wire for capped working-set fetches. Status/Owner/Author/
// Labels/Search are applied client-side after the daemon returns results.
//
// IncludeDeleted is deliberately absent: the daemon's list endpoint
// hard-codes deleted_at IS NULL (internal/db/queries.go::ListIssues) and
// has no include_deleted query param, so a client-side flag would
// promise something the wire cannot deliver. Re-introducing it requires
// daemon work and is deferred to a future task.
type ListFilter struct {
	Status, Owner, Author, Search string
	Labels                        []string
	Limit                         int
}

// values returns the query params the daemon honors for the TUI queue
// fetch path.
func (f ListFilter) values() url.Values {
	v := url.Values{}
	if f.Status != "" {
		v.Set("status", f.Status)
	}
	if f.Limit > 0 {
		v.Set("limit", strconv.Itoa(f.Limit))
	}
	return v
}

// CreateInitialLinkBody requests a link created atomically with a new
// issue. ToRef is a short_id, qualified short_id ("kata#abc4"), or a
// 26-char ULID; the daemon resolves it to the target issue at request
// time.
//
// Incoming reverses direction for type=blocks: when true, the new issue
// is the link's "to" side (i.e. the new issue is BLOCKED BY the named
// target — the `kata create --blocked-by N` shape). Default (false)
// leaves the new issue as the link's "from" side (`--blocks N`).
// Rejected by the daemon for type=parent (no inverse parent direction
// is exposed) and meaningless for type=related (which is symmetric).
type CreateInitialLinkBody struct {
	Type     string `json:"type"`
	ToRef    string `json:"to_ref"`
	Incoming bool   `json:"incoming,omitempty"`
}

// CreateIssueBody is the input to CreateIssue. IdempotencyKey rides the
// Idempotency-Key header per spec §4.4 instead of the JSON body.
//
// Owner and Labels are populated by the new-issue form (Plan 8 commit 4)
// when the user fills in the optional fields; they are omitted from the
// payload when zero so an inline-row commit does not promise the daemon
// fields it has no value for.
type CreateIssueBody struct {
	Title          string                  `json:"title"`
	Body           string                  `json:"body,omitempty"`
	Actor          string                  `json:"actor"`
	Owner          *string                 `json:"owner,omitempty"`
	Labels         []string                `json:"labels,omitempty"`
	Links          []CreateInitialLinkBody `json:"links,omitempty"`
	ForceNew       bool                    `json:"force_new,omitempty"`
	IdempotencyKey string                  `json:"-"`
}

// LinkBody is the request projection for POST /links. ToRef accepts any
// shape the daemon's resolver understands (short_id, qualified, or
// 26-char ULID).
type LinkBody struct {
	Type  string `json:"type"`
	ToRef string `json:"to_ref"`
}

// MutationResp mirrors the §4.5 mutation envelope.
type MutationResp struct {
	Issue   *Issue         `json:"issue"`
	Event   *EventEnvelope `json:"event,omitempty"`
	Changed bool           `json:"changed"`
	Reused  bool           `json:"reused,omitempty"`
}

// EventEnvelope is the minimal event projection embedded in mutation
// responses. The richer poll/SSE shape uses EventLogEntry.
type EventEnvelope struct {
	ID              int64     `json:"id"`
	Type            string    `json:"type"`
	ProjectUID      string    `json:"project_uid,omitempty"`
	IssueUID        string    `json:"issue_uid,omitempty"`
	IssueShortID    *string   `json:"issue_short_id,omitempty"`
	RelatedIssueUID string    `json:"related_issue_uid,omitempty"`
	CreatedAt       time.Time `json:"created_at"`
}

// ResolveResp is the body of POST /projects/resolve.
type ResolveResp struct {
	Project struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
	} `json:"project"`
	WorkspaceRoot string `json:"workspace_root"`
}

// ProjectSummary is one row of GET /projects.
type ProjectSummary struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

// InstanceInfo is the daemon instance identity returned by /api/v1/instance.
type InstanceInfo struct {
	InstanceUID   string   `json:"instance_uid"`
	Version       string   `json:"version"`
	SchemaVersion int64    `json:"schema_version"`
	Auth          AuthInfo `json:"auth"`
}

// AuthInfo is redacted request-auth metadata returned by the daemon.
type AuthInfo = api.AuthInfoOut

// FederationStatusBody is the daemon federation status response.
type FederationStatusBody = api.FederationStatusBody

// FederationProjectStatus is one project row from the federation status API.
type FederationProjectStatus = api.FederationProjectStatus

// FederationEnrollment is the hub-created temporary enrollment token response.
type FederationEnrollment = api.FederationEnrollmentOut

// ProjectFederationMetadata is hub metadata needed to join a spoke replica.
type ProjectFederationMetadata = api.ProjectFederationBody

// FederationReplicaResult is the spoke join/adoption result response.
type FederationReplicaResult = api.CreateFederationReplicaBody

// CreateFederationReplicaInput is the spoke join/adoption request body.
type CreateFederationReplicaInput struct {
	HubURL                 string `json:"hub_url"`
	HubProjectID           int64  `json:"hub_project_id"`
	HubProjectUID          string `json:"hub_project_uid"`
	ProjectName            string `json:"project_name"`
	ReplayHorizonEventID   int64  `json:"replay_horizon_event_id"`
	BaselineThroughEventID int64  `json:"baseline_through_event_id,omitempty"`
	Token                  string `json:"token,omitempty"`
	Capabilities           string `json:"capabilities,omitempty"`
	Actor                  string `json:"actor,omitempty"`
	AllowInsecure          bool   `json:"allow_insecure,omitempty"`
	PushEnabled            bool   `json:"push_enabled,omitempty"`
	AdoptExisting          bool   `json:"adopt_existing,omitempty"`
}

// CreateFederationEnrollmentInput is the hub enrollment creation request body.
type CreateFederationEnrollmentInput struct {
	SpokeInstanceUID             string `json:"spoke_instance_uid"`
	ProjectID                    *int64 `json:"project_id"`
	Capabilities                 string `json:"capabilities"`
	Token                        string `json:"token,omitempty"`
	Actor                        string `json:"actor,omitempty"`
	AllowAdoptionSnapshotAuthors bool   `json:"allow_adoption_snapshot_authors,omitempty"`
}

// ProjectStatsSummary is the per-project aggregate carried by
// /api/v1/projects?include=stats. LastEventAt is nil for a project with
// zero events. Spec §7.2.
type ProjectStatsSummary struct {
	Open        int        `json:"open"`
	Closed      int        `json:"closed"`
	LastEventAt *time.Time `json:"last_event_at"`
}

// ProjectSummaryWithStats extends ProjectSummary with the stats triple.
// The boot project-name cache uses ProjectSummary; viewProjects uses
// this shape.
type ProjectSummaryWithStats struct {
	ProjectSummary
	Stats *ProjectStatsSummary `json:"stats,omitempty"`
}

// CommentEntry is the per-comment projection rendered in the comments tab.
type CommentEntry struct {
	ID        int64     `json:"id"`
	Author    string    `json:"author"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"created_at"`
}

// EventLogEntry is the per-event projection used by the events tab.
// IssueShortID is the project-scoped display ref for the subject issue;
// IssueUID is the canonical reference and survives short_id cutovers.
type EventLogEntry struct {
	ID                  int64          `json:"event_id"`
	Type                string         `json:"type"`
	Actor               string         `json:"actor"`
	ProjectUID          string         `json:"project_uid,omitempty"`
	IssueUID            string         `json:"issue_uid,omitempty"`
	IssueShortID        *string        `json:"issue_short_id,omitempty"`
	RelatedIssueUID     string         `json:"related_issue_uid,omitempty"`
	RelatedIssueShortID *string        `json:"related_issue_short_id,omitempty"`
	CreatedAt           time.Time      `json:"created_at"`
	Payload             map[string]any `json:"payload,omitempty"`
}

// LinkEntry mirrors api.LinkOut: each endpoint is a LinkPeer (UID +
// short_id) so the TUI can correlate either by canonical ID or by
// display snapshot.
type LinkEntry struct {
	ID        int64     `json:"id"`
	Type      string    `json:"type"`
	From      LinkPeer  `json:"from"`
	To        LinkPeer  `json:"to"`
	Author    string    `json:"author"`
	CreatedAt time.Time `json:"created_at"`
}

// APIError is the structured form of the §4.6 error envelope.
type APIError struct {
	Method, Path string
	Status       int
	Code         string
	Message      string
	Hint         string
}

// Error returns "code: message[: hint: ...]" when the daemon supplied a
// structured envelope. When Code and Message are both empty (a 404 with
// no body, a 502 from a proxy, etc.) it falls back to a method+path+status
// summary so toasts stay actionable.
func (e *APIError) Error() string {
	if e.Code == "" && e.Message == "" {
		return fmt.Sprintf("%s %s: HTTP %d", e.Method, e.Path, e.Status)
	}
	parts := []string{e.Code, e.Message}
	if e.Hint != "" {
		parts = append(parts, "hint: "+e.Hint)
	}
	return strings.Join(parts, ": ")
}

// showIssueBody mirrors the daemon's GET /issues/{ref} envelope.
// The daemon ships labels as a sibling slice (one IssueLabel per row);
// showIssueLabel keeps decode tight to the fields the TUI needs.
type showIssueBody struct {
	Issue    Issue            `json:"issue"`
	Comments []CommentEntry   `json:"comments"`
	Links    []LinkEntry      `json:"links"`
	Labels   []showIssueLabel `json:"labels"`
	Parent   *IssueRef        `json:"parent,omitempty"`
	Children []Issue          `json:"children,omitempty"`
}

// IssueDetail is the hydrated detail payload used by the TUI detail view.
type IssueDetail struct {
	Issue    *Issue
	Parent   *IssueRef
	Children []Issue
}

// showIssueLabel is the per-label projection from showIssue's labels
// slice. The wire shape is db.IssueLabel (issue_id, label, author,
// created_at) — only label is used for rendering.
type showIssueLabel struct {
	Label string `json:"label"`
}
