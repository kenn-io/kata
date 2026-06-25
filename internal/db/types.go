package db

import (
	"encoding/json"
	"time"
)

// JSONBlob is the storage type for TEXT columns that hold a JSON value
// (objects for metadata, arrays for labels). Underlying string lets the
// database/sql driver scan TEXT into it directly with no `(*[]byte)(&…)`
// cast and lets handlers pass it back into INSERT/UPDATE as a regular
// string parameter. Custom MarshalJSON / UnmarshalJSON make it round-trip
// on the wire as the raw JSON value rather than as a JSON-encoded string:
// {"area":"Personal"} instead of "{\"area\":\"Personal\"}".
//
// The empty value marshals as JSON null so a struct missing a default
// doesn't produce invalid JSON. CHECK constraints in the schema
// (json_valid + json_type) enforce that any persisted value is the
// expected shape — this type intentionally trusts those constraints and
// does no shape validation on marshal.
type JSONBlob string

// MarshalJSON emits the stored bytes verbatim (a JSON object or array,
// per schema). Empty JSONBlob marshals as JSON null.
func (j JSONBlob) MarshalJSON() ([]byte, error) {
	if j == "" {
		return []byte("null"), nil
	}
	return []byte(j), nil
}

// UnmarshalJSON copies the raw input bytes verbatim. JSON null is stored
// as the empty string so a round-trip through unmarshal/marshal produces
// the same value.
func (j *JSONBlob) UnmarshalJSON(b []byte) error {
	if string(b) == "null" {
		*j = ""
		return nil
	}
	*j = JSONBlob(b)
	return nil
}

// Project mirrors a row in projects. DeletedAt is set when the project has
// been archived via kata projects remove (#24); the row stays in the table so
// events/issues keep referring to a valid FK target, but read paths filter it
// out. Name is the user-facing unique project key.
type Project struct {
	ID        int64      `json:"id"`
	UID       string     `json:"uid"`
	Name      string     `json:"name"`
	Metadata  JSONBlob   `json:"metadata"`
	Revision  int64      `json:"revision"`
	CreatedAt time.Time  `json:"created_at"`
	DeletedAt *time.Time `json:"deleted_at,omitempty"`
}

// ProjectStats is the per-project aggregate returned by BatchProjectStats.
// Used by GET /api/v1/projects?include=stats. LastEventAt is nil for a
// project with zero events; tests pin this so the TUI's "—" rendering
// is exercised.
type ProjectStats struct {
	Open        int
	Closed      int
	LastEventAt *time.Time
}

// FederationRole describes this daemon's relationship to a federated project.
type FederationRole string

const (
	// FederationRoleHub marks a local project as the federation source.
	FederationRoleHub FederationRole = "hub"
	// FederationRoleSpoke marks a local project as a read-only hub replica.
	FederationRoleSpoke FederationRole = "spoke"
)

// FederationBinding mirrors federation_bindings. HubURL and HubProjectID are
// meaningful for spoke bindings; hub bindings keep the local project identity
// and replay horizon.
type FederationBinding struct {
	ProjectID            int64
	Role                 FederationRole
	HubURL               string
	HubProjectID         int64
	HubProjectUID        string
	ReplayHorizonEventID int64
	PullCursorEventID    int64
	PushEnabled          bool
	PushCursorEventID    int64
	Actor                string
	// AllowInsecure records the join-time plaintext-HTTP transport opt-in on
	// the binding itself so leave can rebuild the hub client even when the
	// credential file (the only other holder of the flag) is gone.
	AllowInsecure bool
	Enabled       bool
	CreatedAt     time.Time
	UpdatedAt     time.Time
	LastSyncAt    *time.Time
}

// FederationQuarantineDirection identifies which federation stream is blocked.
type FederationQuarantineDirection string

const (
	// FederationQuarantineDirectionPush blocks outgoing spoke-to-hub batches.
	FederationQuarantineDirectionPush FederationQuarantineDirection = "push"
	// FederationQuarantineDirectionPull blocks incoming hub-to-spoke batches.
	FederationQuarantineDirectionPull FederationQuarantineDirection = "pull"
)

// FederationQuarantine records a poisoned federation batch that requires an
// explicit operator decision before the stream can continue.
type FederationQuarantine struct {
	ID           int64
	ProjectID    int64
	Direction    FederationQuarantineDirection
	FirstEventID int64
	LastEventID  int64
	EventUIDs    []string
	Error        string
	CreatedAt    time.Time
	SkippedAt    *time.Time
	SkippedBy    *string
	SkipReason   *string
}

// FederationEnrollment mirrors federation_enrollments. TokenHash stores only
// the hash of an enrollment token; plaintext tokens are never persisted.
type FederationEnrollment struct {
	ID                           int64
	TokenHash                    string
	SpokeInstanceUID             string
	ProjectID                    *int64
	Capabilities                 string
	Actor                        string
	AllowAdoptionSnapshotAuthors bool
	CreatedAt                    time.Time
	UpdatedAt                    time.Time
	RevokedAt                    *time.Time
}

// IssueClaim mirrors issue_claims. Active claims have ReleasedAt == nil.
type IssueClaim struct {
	ID                int64
	ClaimUID          string
	ProjectID         int64
	IssueID           int64
	IssueUID          string
	Holder            string
	HolderInstanceUID string
	ClientKind        string
	Purpose           string
	ClaimKind         string
	AcquiredAt        time.Time
	ExpiresAt         *time.Time
	ReleasedAt        *time.Time
	ReleaseReason     *string
	Revision          int64
	UpdatedAt         time.Time
}

// PendingClaimRequest mirrors pending_claim_requests for offline claim
// requests awaiting hub resolution.
type PendingClaimRequest struct {
	ID                int64
	RequestUID        string
	ProjectID         int64
	IssueID           int64
	IssueUID          string
	Holder            string
	HolderInstanceUID string
	ClientKind        string
	ClaimKind         string
	TTLSeconds        *int64
	Purpose           string
	RequestedAt       time.Time
	LastAttemptAt     *time.Time
	LastError         *string
	RejectedAt        *time.Time
	ResolvedAt        *time.Time
}

// ClaimStatusRefreshError records the most recent failed spoke show-status
// refresh for one issue.
type ClaimStatusRefreshError struct {
	ProjectID     int64
	IssueUID      string
	StatusCode    int
	LastAttemptAt time.Time
	LastError     string
}

// PendingClaimParams records an offline spoke claim request awaiting hub
// resolution.
type PendingClaimParams struct {
	ProjectID int64
	IssueRef  string
	Principal ClaimPrincipal
	ClaimKind string
	TTL       time.Duration
	Purpose   string
	Now       time.Time
}

// ClaimPrincipal identifies the federation client tuple that owns a live
// issue claim.
type ClaimPrincipal struct {
	HolderInstanceUID string
	Holder            string
	ClientKind        string
}

// AcquireClaimParams requests a hard or timed claim on a project-scoped issue
// reference.
type AcquireClaimParams struct {
	ProjectID int64
	IssueRef  string
	Principal ClaimPrincipal
	ClaimKind string
	TTL       time.Duration
	Purpose   string
	Now       time.Time
}

// RenewClaimParams extends a live timed claim held by the same principal.
type RenewClaimParams struct {
	ProjectID int64
	IssueRef  string
	Principal ClaimPrincipal
	TTL       time.Duration
	Now       time.Time
}

// ReleaseClaimParams releases a live claim held by the same principal.
type ReleaseClaimParams struct {
	ProjectID int64
	IssueRef  string
	Principal ClaimPrincipal
	Reason    string
	Now       time.Time
}

// ForceReleaseClaimParams releases a live claim regardless of holder. The DB
// layer records the actor/reason but does not enforce daemon auth policy.
type ForceReleaseClaimParams struct {
	ProjectID int64
	IssueRef  string
	Actor     string
	Reason    string
	Now       time.Time
}

// LeaseResult is returned by acquire, renew, and release arbitration helpers
// for federation write leases. Event is nil for idempotent/no-event outcomes;
// Events carries auxiliary events, such as opportunistic claim expirations,
// that committed in the same transaction and should be delivered before Event.
// (`ClaimResult` is reserved for the simple owner-claim flow in queries.go.)
type LeaseResult struct {
	Granted bool
	Holder  ClaimPrincipal
	Claim   *IssueClaim
	Event   *Event
	Events  []Event
}

// ClaimStatus describes the currently live claim for an issue, if any.
type ClaimStatus struct {
	Held   bool
	Holder ClaimPrincipal
	Claim  *IssueClaim
	HubNow time.Time
	Events []Event
}

// ClaimGateParams checks whether a local mutation can proceed using the
// cached authoritative claim state available to this database.
type ClaimGateParams struct {
	ProjectID int64
	IssueRef  string
	Principal ClaimPrincipal
	Now       time.Time
}

// ProjectAlias mirrors a row in project_aliases.
type ProjectAlias struct {
	ID            int64     `json:"id"`
	ProjectID     int64     `json:"project_id"`
	AliasIdentity string    `json:"alias_identity"`
	AliasKind     string    `json:"alias_kind"`
	CreatedAt     time.Time `json:"created_at"`
}

// IssueQualifier is the minimal identity needed to render an issue
// reference: the owning project (id + name) and the issue's current
// short_id. It lets a caller render a ref bare for a same-project issue
// and qualified ("project#short_id") for a foreign one.
type IssueQualifier struct {
	ProjectID   int64
	ProjectName string
	ShortID     string
}

// Issue mirrors a row in issues. Priority is 0..4 with 0 = highest priority
// and 4 = lowest; nil means no priority is set.
type Issue struct {
	ID            int64      `json:"id"`
	UID           string     `json:"uid"`
	ProjectID     int64      `json:"project_id"`
	ProjectUID    string     `json:"project_uid,omitempty"`
	ShortID       string     `json:"short_id"`
	Title         string     `json:"title"`
	Body          string     `json:"body"`
	Status        string     `json:"status"`
	ClosedReason  *string    `json:"closed_reason,omitempty"`
	Owner         *string    `json:"owner,omitempty"`
	Priority      *int64     `json:"priority,omitempty"`
	Author        string     `json:"author"`
	Metadata      JSONBlob   `json:"metadata"`
	Revision      int64      `json:"revision"`
	RecurrenceID  *int64     `json:"recurrence_id,omitempty"`
	OccurrenceKey *string    `json:"occurrence_key,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
	ClosedAt      *time.Time `json:"closed_at,omitempty"`
	DeletedAt     *time.Time `json:"deleted_at,omitempty"`
}

// ReadyGlobalIssue is an Issue paired with its project's canonical name. Used
// only by ReadyIssuesGlobal so the cross-project ready view can render
// qualified refs (`<project>#<short_id>`) without a follow-up project lookup.
type ReadyGlobalIssue struct {
	Issue
	ProjectName string `json:"project_name"`
}

// Comment mirrors a row in comments.
type Comment struct {
	ID        int64     `json:"id"`
	UID       string    `json:"uid"`
	IssueID   int64     `json:"issue_id"`
	Author    string    `json:"author"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"created_at"`
}

// Event mirrors a row in events. IssueShortID and RelatedIssueShortID are
// not columns on events; they are joined from issues.short_id at read time
// so old events render correctly across short_id-shifting events (project
// merge, federation merge) — UIDs remain canonical, short_ids are display
// snapshots resolved on every read.
type Event struct {
	ID                  int64     `json:"id"`
	UID                 string    `json:"uid"`
	OriginInstanceUID   string    `json:"origin_instance_uid"`
	ProjectID           int64     `json:"project_id"`
	ProjectUID          string    `json:"project_uid"`
	ProjectName         string    `json:"project_name"`
	IssueID             *int64    `json:"issue_id,omitempty"`
	IssueUID            *string   `json:"issue_uid,omitempty"`
	IssueShortID        *string   `json:"issue_short_id,omitempty"`
	RelatedIssueID      *int64    `json:"related_issue_id,omitempty"`
	RelatedIssueUID     *string   `json:"related_issue_uid,omitempty"`
	RelatedIssueShortID *string   `json:"related_issue_short_id,omitempty"`
	Type                string    `json:"type"`
	Actor               string    `json:"actor"`
	Payload             string    `json:"payload"`
	HLCPhysicalMS       int64     `json:"hlc_physical_ms"`
	HLCCounter          int64     `json:"hlc_counter"`
	ContentHash         string    `json:"content_hash"`
	CreatedAt           time.Time `json:"created_at"`
}

// RemoteEvent is the portable event shape accepted from a federation hub.
// Local SQLite row IDs and display-only short IDs are intentionally excluded.
type RemoteEvent struct {
	EventUID          string          `json:"event_uid"`
	OriginInstanceUID string          `json:"origin_instance_uid"`
	ProjectUID        string          `json:"project_uid"`
	ProjectName       string          `json:"project_name"`
	IssueUID          *string         `json:"issue_uid,omitempty"`
	RelatedIssueUID   *string         `json:"related_issue_uid,omitempty"`
	Type              string          `json:"type"`
	Actor             string          `json:"actor"`
	HLCPhysicalMS     int64           `json:"hlc_physical_ms"`
	HLCCounter        int64           `json:"hlc_counter"`
	ContentHash       string          `json:"content_hash"`
	Payload           json.RawMessage `json:"payload,omitempty"`
	CreatedAt         time.Time       `json:"created_at"`
}

// Link mirrors a row in links.
type Link struct {
	ID           int64     `json:"id"`
	FromIssueID  int64     `json:"from_issue_id"`
	FromIssueUID string    `json:"from_issue_uid"`
	ToIssueID    int64     `json:"to_issue_id"`
	ToIssueUID   string    `json:"to_issue_uid"`
	Type         string    `json:"type"`
	Author       string    `json:"author"`
	CreatedAt    time.Time `json:"created_at"`
}

// IssueLabel mirrors a row in issue_labels.
type IssueLabel struct {
	IssueID   int64     `json:"issue_id"`
	Label     string    `json:"label"`
	Author    string    `json:"author"`
	CreatedAt time.Time `json:"created_at"`
}

// Recurrence mirrors a row in recurrences. Each row carries the RRULE plus
// the issue template fields (title, body, owner, priority, labels, metadata)
// used when materializing each occurrence into a concrete issues row. The
// link is closed by issues.recurrence_id + issues.occurrence_key, whose
// partial unique index guarantees one materialized issue per occurrence.
type Recurrence struct {
	ID                  int64      `json:"id"`
	UID                 string     `json:"uid"`
	ProjectID           int64      `json:"project_id"`
	RRule               string     `json:"rrule"`
	DTStart             string     `json:"dtstart"`
	Timezone            string     `json:"timezone"`
	TemplateTitle       string     `json:"template_title"`
	TemplateBody        string     `json:"template_body"`
	TemplateOwner       *string    `json:"template_owner,omitempty"`
	TemplatePriority    *int64     `json:"template_priority,omitempty"`
	TemplateLabels      JSONBlob   `json:"template_labels"`
	TemplateMetadata    JSONBlob   `json:"template_metadata"`
	NextOccurrenceKey   *string    `json:"next_occurrence_key,omitempty"`
	LastMaterializedUID *string    `json:"last_materialized_uid,omitempty"`
	Author              string     `json:"author"`
	Revision            int64      `json:"revision"`
	CreatedAt           time.Time  `json:"created_at"`
	UpdatedAt           time.Time  `json:"updated_at"`
	DeletedAt           *time.Time `json:"deleted_at,omitempty"`
}

// LabelCount is the per-label aggregate returned by LabelCounts.
type LabelCount struct {
	Label string `json:"label"`
	Count int64  `json:"count"`
}

// ChildCounts is the direct-child aggregate for one parent issue.
type ChildCounts struct {
	Open  int `json:"open"`
	Total int `json:"total"`
}

// SearchCandidate is one row from SearchFTS: an issue, a BM25 score (lower is
// better in raw form; we negate so higher = better), and the columns where
// the query matched. MatchedIn is the basis for the wire response's matched_in.
type SearchCandidate struct {
	Issue     Issue    `json:"issue"`
	Score     float64  `json:"score"` // BM25, negated; higher = better match
	MatchedIn []string `json:"matched_in"`
}

// IdempotencyMatch is the payload returned by LookupIdempotency. The Event row
// is included so the handler can populate `original_event` in the reuse-case
// MutationResponse without a second query.
type IdempotencyMatch struct {
	IssueID      int64
	IssueShortID string
	Fingerprint  string
	Event        Event
}

// Evidence is the typed-union element persisted on issue.closed event
// payloads (anti-agent-justification spec §3.3). It mirrors api.Evidence
// field-for-field; the daemon handler does a 1:1 conversion at the
// boundary so the db package stays free of the api dependency that
// would create an import cycle (internal/api already imports
// internal/db). Per-reason validation lives in the daemon, not here —
// the db layer treats this as opaque payload data and simply persists
// the marshaled JSON onto the event row.
type Evidence struct {
	Type string `json:"type"`

	SHA       string   `json:"sha,omitempty"`       // commit
	URL       string   `json:"url,omitempty"`       // pr
	Command   string   `json:"command,omitempty"`   // test
	Paths     []string `json:"paths,omitempty"`     // reviewed-paths
	Rationale string   `json:"rationale,omitempty"` // no-change-audit
	IssueRef  string   `json:"issue_ref,omitempty"` // duplicate-of, superseded-by
}

// PurgeLog mirrors a row in purge_log. Snapshots the issue identity at purge
// time so audits survive any future project rename. EventsDeletedMinID/MaxID
// and PurgeResetAfterEventID are nullable: NULL when no events were attached
// to the purged issue.
type PurgeLog struct {
	ID                     int64     `json:"id"`
	UID                    string    `json:"uid"`
	OriginInstanceUID      string    `json:"origin_instance_uid"`
	ProjectID              int64     `json:"project_id"`
	PurgedIssueID          int64     `json:"purged_issue_id"`
	IssueUID               *string   `json:"issue_uid,omitempty"`
	ProjectUID             *string   `json:"project_uid,omitempty"`
	ProjectName            string    `json:"project_name"`
	ShortID                *string   `json:"short_id,omitempty"`
	IssueTitle             string    `json:"issue_title"`
	IssueAuthor            string    `json:"issue_author"`
	CommentCount           int64     `json:"comment_count"`
	LinkCount              int64     `json:"link_count"`
	LabelCount             int64     `json:"label_count"`
	EventCount             int64     `json:"event_count"`
	EventsDeletedMinID     *int64    `json:"events_deleted_min_id,omitempty"`
	EventsDeletedMaxID     *int64    `json:"events_deleted_max_id,omitempty"`
	PurgeResetAfterEventID *int64    `json:"purge_reset_after_event_id,omitempty"`
	Actor                  string    `json:"actor"`
	Reason                 *string   `json:"reason,omitempty"`
	PurgedAt               time.Time `json:"purged_at"`
}

// ProjectPurgeLog is the durable audit tombstone written when a project is
// permanently purged. No FK to projects: the row outlives the deleted project.
type ProjectPurgeLog struct {
	ID                       int64     `json:"id"`
	UID                      string    `json:"uid"`
	OriginInstanceUID        string    `json:"origin_instance_uid"`
	ProjectID                int64     `json:"project_id"`
	ProjectUID               *string   `json:"project_uid,omitempty"`
	ProjectName              string    `json:"project_name"`
	IssueCount               int64     `json:"issue_count"`
	EventCount               int64     `json:"event_count"`
	AliasCount               int64     `json:"alias_count"`
	CommentCount             int64     `json:"comment_count"`
	LinkCount                int64     `json:"link_count"`
	LabelCount               int64     `json:"label_count"`
	ClaimCount               int64     `json:"claim_count"`
	PendingClaimRequestCount int64     `json:"pending_claim_request_count"`
	EventsDeletedMinID       *int64    `json:"events_deleted_min_id,omitempty"`
	EventsDeletedMaxID       *int64    `json:"events_deleted_max_id,omitempty"`
	PurgeResetAfterEventID   *int64    `json:"purge_reset_after_event_id,omitempty"`
	Actor                    string    `json:"actor"`
	Reason                   *string   `json:"reason,omitempty"`
	PurgedAt                 time.Time `json:"purged_at"`
}
