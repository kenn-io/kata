package db

import (
	"encoding/json"
	"time"
)

// IncludeDeleted controls whether a lookup is allowed to return soft-deleted
// rows. Spec §6: normal read/mutate paths exclude them; the carveout paths
// (restore, idempotent re-delete, purge, idempotency-key collision detection)
// pass IncludeDeletedYes.
type IncludeDeleted int

const (
	// IncludeDeletedNo filters out rows with deleted_at IS NOT NULL.
	IncludeDeletedNo IncludeDeleted = 0
	// IncludeDeletedYes returns soft-deleted rows alongside live ones.
	IncludeDeletedYes IncludeDeleted = 1
)

// Exported param/result structs named in the Storage interface signatures.
// Methods that take/return these live in the impl files.

// InitialLink describes one of the optional links created in the same TX as
// the issue itself. The to_number is resolved within the same project.
//
// Default direction (Incoming=false): the new issue is the link's "from"
// side. Incoming=true reverses for type=blocks so the new issue is the
// "to" side (i.e. it is blocked by ToNumber). Rejected for type=parent
// (no inverse parent direction is exposed); meaningless for type=related
// which is symmetric.
type InitialLink struct {
	Type     string // "parent" | "blocks" | "related"
	ToNumber int64
	Incoming bool
}

// CreateIssueParams carries inputs for CreateIssue.
type CreateIssueParams struct {
	ProjectID int64
	Title     string
	Body      string
	Author    string

	// Optional. When non-empty, CreateIssue uses this ULID instead of
	// generating one. Required to be a valid 26-char ULID; the caller owns
	// uniqueness (the UNIQUE constraint on issues.uid will still surface
	// duplicates as a constraint error). This seam supports deterministic
	// tests and the JSONL replay path; live callers should leave it empty.
	UID string

	// Optional. When non-empty, CreateIssue bypasses assignShortID's
	// auto-extend loop and persists this value verbatim on the new row.
	// The value must satisfy shortid.Valid AND equal the lowercased suffix
	// of UID at its own length — the same invariant the schema CHECK
	// enforces. Used by JSONL import (spec §8.1) to preserve stored
	// short_ids across future cutovers; live callers leave it empty.
	ShortIDOverride string

	// Optional initial state. Plan 2 fields. CreateIssue inserts label/link
	// rows and applies the owner in the same TX, then folds them into the
	// issue.created event payload (no separate labeled/linked/assigned events).
	Labels   []string
	Links    []InitialLink
	Owner    *string
	Priority *int64

	// Optional. When non-empty, both fields are folded into the issue.created
	// event payload so future LookupIdempotency calls can find the row via
	// idx_events_idempotency.
	IdempotencyKey         string
	IdempotencyFingerprint string
}

// ListIssuesParams filters single-project list output.
type ListIssuesParams struct {
	ProjectID     int64
	Status        string   // "open" | "closed" | "" (any)
	Priority      *int64   // nil = no filter; non-nil = exactly this value
	MaxPriority   *int64   // nil = no filter; non-nil = priority <= MaxPriority
	Limit         int      // 0 = no limit
	Unowned       bool     // only issues where owner IS NULL
	Owner         string   // only issues where owner = this value (empty = no filter)
	Labels        []string // issues must have ALL these labels (AND logic)
	ExcludeLabels []string // issues must NOT have any of these labels
}

// ListAllIssuesParams filters cross-project list output. ProjectID==0 means
// "every project"; >0 narrows to a single project. Status="" → all statuses.
// Priority and MaxPriority work the same as ListIssuesParams.
type ListAllIssuesParams struct {
	ProjectID   int64
	Status      string
	Priority    *int64
	MaxPriority *int64
	Limit       int
}

// CreateCommentParams carries inputs for CreateComment.
type CreateCommentParams struct {
	IssueID int64
	Author  string
	Body    string
}

// CloseThrottleReason values for CloseThrottledPayload.Reason. Sibling-burst
// fires when an actor closes too many siblings under one parent in a short
// window; duplicate-message fires when an actor reuses identical close prose
// across sibling issues.
const (
	CloseThrottleReasonSiblingBurst     = "sibling-burst"
	CloseThrottleReasonDuplicateMessage = "duplicate-message"
)

// CloseThrottledPayload is the JSON wire shape persisted on close.throttled
// events. Parent is the user-facing issue number of the shared parent and is
// always populated when a throttle fires (the guards never refuse on an
// unparented issue). Cohort lists the recent sibling closes that triggered
// the burst guard; Prior is the prior matching close for the duplicate-
// message guard. Cohort and Prior are omitted when not relevant to the path
// that fired.
type CloseThrottledPayload struct {
	Reason string   `json:"reason"`
	Parent string   `json:"parent"`
	Cohort []string `json:"cohort,omitempty"`
	Prior  *string  `json:"prior,omitempty"`
}

// EditIssueParams carries the optional fields for an edit. Nil = leave alone.
type EditIssueParams struct {
	IssueID int64
	Title   *string
	Body    *string
	Owner   *string
	Actor   string
}

// ClaimResult contains the result of a ClaimOwner operation.
type ClaimResult struct {
	Issue         Issue
	Event         *Event
	Changed       bool
	PreviousOwner *string
	CurrentOwner  *string // set when ErrAlreadyClaimed is returned
}

// ReadyIssuesFilter holds optional filters for the ready query.
type ReadyIssuesFilter struct {
	Unowned       bool     // only issues where owner IS NULL
	Owner         string   // only issues where owner = this value (empty = no filter)
	Labels        []string // issues must have ALL these labels (AND logic)
	ExcludeLabels []string // issues must NOT have any of these labels
}

// EditIssueAtomicParams carries the full set of mutations to apply to one
// issue in a single transaction. nil/false fields mean "no change."
type EditIssueAtomicParams struct {
	IssueID int64
	Actor   string

	// Field changes (nil = no change).
	Title *string
	Body  *string
	Owner *string

	// Priority. SetPriority != nil means set; ClearPriority means clear.
	// Both nil/false → no change. Mutually exclusive at this layer; the
	// handler enforces that contract before calling in.
	SetPriority   *int64
	ClearPriority bool

	// Parent slot. At most one of SetParent / RemoveParent may be non-nil
	// (the handler enforces). RemoveParent is strict: the asserted number
	// must equal the current parent's number, or ErrParentMismatch is
	// returned and the entire delta rolls back.
	SetParent    *int64
	RemoveParent *int64

	// Multi-valued link ops, framed from the URL issue's POV.
	//   AddBlocks:        URL issue → N (type=blocks)
	//   AddBlockedBy:     N → URL issue (type=blocks)
	//   AddRelated:       URL issue ↔ N (type=related, canonicalized)
	//   Remove* are idempotent — missing links are no-ops.
	AddBlocks       []int64
	AddBlockedBy    []int64
	AddRelated      []int64
	RemoveBlocks    []int64
	RemoveBlockedBy []int64
	RemoveRelated   []int64
}

// ProjectIDFor returns the issue's project ID — a tiny helper so link-op loops
// in the backend read cleanly. Defined on the params type so it travels with
// the contract.
func (EditIssueAtomicParams) ProjectIDFor(i Issue) int64 { return i.ProjectID }

// AtomicEditChanges describes which link mutations actually applied.
// Each entry pairs the peer's short_id (display snapshot) with its UID
// (stable identity) — the aggregated issue.links_changed event payload
// carries both forms so consumers can render display strings and key on
// UIDs for cross-cutover stability.
type AtomicEditChanges struct {
	ParentSet        *string `json:"parent_set,omitempty"`
	ParentSetUID     *string `json:"parent_set_uid,omitempty"`
	ParentRemoved    *string `json:"parent_removed,omitempty"`
	ParentRemovedUID *string `json:"parent_removed_uid,omitempty"`

	BlocksAdded     []string `json:"blocks_added,omitempty"`
	BlocksAddedUIDs []string `json:"blocks_added_uids,omitempty"`

	BlocksRemoved     []string `json:"blocks_removed,omitempty"`
	BlocksRemovedUIDs []string `json:"blocks_removed_uids,omitempty"`

	BlockedByAdded     []string `json:"blocked_by_added,omitempty"`
	BlockedByAddedUIDs []string `json:"blocked_by_added_uids,omitempty"`

	BlockedByRemoved     []string `json:"blocked_by_removed,omitempty"`
	BlockedByRemovedUIDs []string `json:"blocked_by_removed_uids,omitempty"`

	RelatedAdded     []string `json:"related_added,omitempty"`
	RelatedAddedUIDs []string `json:"related_added_uids,omitempty"`

	RelatedRemoved     []string `json:"related_removed,omitempty"`
	RelatedRemovedUIDs []string `json:"related_removed_uids,omitempty"`
}

// EditIssueAtomicResult is what the handler renders into a wire response.
type EditIssueAtomicResult struct {
	Issue     Issue
	Events    []Event           // 0..3: issue.updated, issue.priority_*, issue.links_changed
	Changes   AtomicEditChanges // for the response's "changes" block
	AnyChange bool
}

// CreateLinkParams carries inputs for CreateLink. The caller is responsible
// for canonical ordering of `related` links (from < to) before calling.
type CreateLinkParams struct {
	ProjectID   int64
	FromIssueID int64
	ToIssueID   int64
	Type        string // "parent" | "blocks" | "related"
	Author      string
}

// LinkEventParams describes the event-emission side of a link mutation.
// Payload identifiers are paired: from_short_id is the URL issue's display
// ref, from_uid is its canonical pointer; to_short_id/to_uid identify the
// OTHER endpoint. UIDs survive a short_id cutover or federation merge
// unchanged, so consumers keying on identity should read the UIDs.
type LinkEventParams struct {
	EventType    string // "issue.linked" | "issue.unlinked"
	EventIssueID int64  // the issue whose URL the user posted to
	FromShortID  string // payload field; matches the URL issue's short_id
	FromUID      string // payload field; canonical pointer to the URL issue
	ToShortID    string // payload field; matches the OTHER endpoint's short_id
	ToUID        string // payload field; canonical pointer to the other endpoint
	Actor        string
}

// LabelEventParams describes the event-emission side of a label mutation. The
// DB-layer methods AddLabelAndEvent and RemoveLabelAndEvent split the mutation
// (label insert/delete) from the event metadata so the handler can emit the
// matching issue.labeled / issue.unlabeled event without an extra round trip.
type LabelEventParams struct {
	EventType string // "issue.labeled" | "issue.unlabeled"
	Label     string // the label being added/removed (used for both DB op and event payload)
	Actor     string
}

// AliasRow is the slim view of a project alias the hook resolver needs.
// Field naming is normalized vs. ProjectAlias (which uses Alias-prefixed
// fields) so resolver call sites read cleanly.
type AliasRow struct {
	Identity string
	Kind     string
	RootPath string
}

// EventsAfterParams selects events with id strictly greater than AfterID,
// optionally bounded above by ThroughID and filtered by ProjectID. Limit is
// applied verbatim; callers are responsible for clamping (the polling
// endpoint clamps to [1, 1000]; the SSE drain passes 10001).
type EventsAfterParams struct {
	AfterID   int64
	ProjectID int64 // 0 = cross-project; nonzero adds AND project_id = ?
	ThroughID int64 // 0 = no upper bound; nonzero adds AND id <= ?
	Limit     int
}

// EventsInWindowParams selects events whose created_at lies in the closed
// window [Since, Until]. ProjectID = 0 disables the project filter; an empty
// Actors slice disables actor filtering. Results are ordered by id ASC so the
// digest aggregator can rely on chronological ordering for per-issue
// "actions" sequencing.
//
// Both bounds are inclusive. SQLite stores created_at at millisecond
// precision; an exclusive upper bound silently excludes events emitted in the
// same millisecond as Until, which happens often when Until defaults to
// time.Now() right after a mutation. Inclusive matches what humans typing
// "since 24h" expect anyway.
type EventsInWindowParams struct {
	Since     string // string, inclusive lower bound on created_at
	Until     string // string, inclusive upper bound on created_at
	ProjectID int64  // 0 = cross-project
	Actors    []string
}

// RemoveProjectParams identifies a project to archive (#24). Force overrides
// the open-issue refusal; Actor lands in the project.removed event.
type RemoveProjectParams struct {
	ProjectID int64
	Actor     string
	Force     bool
}

// DetachAliasParams identifies a single alias to drop. ProjectID scopes the
// lookup to a specific project so a stale handler preflight cannot resolve
// to one project_id and then race a reassignment that points alias_id at a
// different project. Force overrides the last-alias refusal.
type DetachAliasParams struct {
	ProjectID int64
	AliasID   int64
	Actor     string
	Force     bool
}

// MergeProjectsParams identifies a source project to fold into a surviving
// target project. The target keeps its id.
type MergeProjectsParams struct {
	SourceProjectID int64
	TargetProjectID int64
	TargetName      *string
}

// ShortIDExtension records a single source-side issue whose short_id was
// extended during merge to break a collision with an existing target-side
// short_id. The UID is stable across the shift; PreMergeShortID is the
// value the issue carried on the source project, PostMergeShortID is the
// value stored after the merge.
type ShortIDExtension struct {
	UID              string
	PreMergeShortID  string
	PostMergeShortID string
}

// ProjectMergeResult summarizes the rows moved by MergeProjects.
type ProjectMergeResult struct {
	Source            Project            `json:"source"`
	Target            Project            `json:"target"`
	IssuesMoved       int64              `json:"issues_moved"`
	AliasesMoved      int64              `json:"aliases_moved"`
	EventsMoved       int64              `json:"events_moved"`
	PurgeLogsMoved    int64              `json:"purge_logs_moved"`
	ShortIDExtensions []ShortIDExtension `json:"short_id_extensions,omitempty"`
}

// MoveIssueProjectIn carries inputs for MoveIssueProject.
type MoveIssueProjectIn struct {
	IssueID       int64
	FromProjectID int64
	ToProjectID   int64
	IfMatchRev    int64
	Actor         string
}

// MoveIssueProjectOut carries results from a successful MoveIssueProject call.
type MoveIssueProjectOut struct {
	Issue       Issue
	EventID     int64
	NewShortID  string
	NewRevision int64
}

// PatchIssueMetadataIn carries inputs for PatchIssueMetadata.
type PatchIssueMetadataIn struct {
	IssueID    int64
	IfMatchRev int64
	Actor      string
	Patch      map[string]json.RawMessage
}

// PatchIssueMetadataOut carries results from a successful PatchIssueMetadata call.
type PatchIssueMetadataOut struct {
	Issue       Issue
	Event       Event
	Changed     bool
	NewRevision int64
}

// PatchProjectMetadataIn carries inputs for PatchProjectMetadata.
type PatchProjectMetadataIn struct {
	ProjectID  int64
	IfMatchRev int64
	Actor      string
	Patch      map[string]json.RawMessage
}

// PatchProjectMetadataOut carries results from a successful PatchProjectMetadata call.
type PatchProjectMetadataOut struct {
	Project     Project
	Event       Event
	Changed     bool
	NewRevision int64
}

// RecurrenceTemplate carries the issue-template fields for a recurrence row.
// Owner and Priority are optional; Labels and Metadata default to empty
// collections when nil.
type RecurrenceTemplate struct {
	Title    string
	Body     string
	Owner    *string
	Priority *int64
	Labels   []string
	Metadata json.RawMessage
}

// CreateRecurrenceIn holds the inputs for CreateRecurrence.
type CreateRecurrenceIn struct {
	ProjectID int64
	Actor     string
	Rule      string
	DTStart   string
	Timezone  string
	Template  RecurrenceTemplate
}

// RecurrenceUpdate holds the optional fields for PatchRecurrence. A nil field
// means "leave unchanged"; a non-nil field means "set to this value".
type RecurrenceUpdate struct {
	Rule             *string
	DTStart          *string
	Timezone         *string
	TemplateTitle    *string
	TemplateBody     *string
	TemplateOwner    *string
	TemplatePriority *int64
	TemplateLabels   *[]string
	TemplateMetadata *json.RawMessage
}

// PatchRecurrenceIn holds the inputs for PatchRecurrence.
type PatchRecurrenceIn struct {
	RecurrenceID int64
	IfMatchRev   int64
	Actor        string
	Update       RecurrenceUpdate
}

// PatchRecurrenceOut carries results from a successful PatchRecurrence call.
type PatchRecurrenceOut struct {
	Recurrence  Recurrence
	NewRevision int64
	Changed     bool
}

// MaterializeNextOut carries the results of a successful MaterializeNext call.
type MaterializeNextOut struct {
	// NewIssueID is the row id of the newly inserted issue (zero when Skipped).
	NewIssueID int64
	// NewIssueUID is the UID of the inserted or already-existing issue.
	NewIssueUID string
	// OccurrenceKey is the occurrence date that was materialized.
	OccurrenceKey string
	// Skipped is true when the occurrence already existed (race with another writer).
	Skipped bool
}

// FederationSyncStatus mirrors federation_sync_status for operator-facing
// federation health and last-attempt state.
type FederationSyncStatus struct {
	ProjectID         int64
	LastPullStartedAt *time.Time
	LastPullSuccessAt *time.Time
	LastPushStartedAt *time.Time
	LastPushSuccessAt *time.Time
	LastErrorAt       *time.Time
	LastError         *string
	LastResetAt       *time.Time
}

// RecordFederationQuarantineParams records a poisoned federation batch.
type RecordFederationQuarantineParams struct {
	ProjectID    int64
	Direction    FederationQuarantineDirection
	FirstEventID int64
	LastEventID  int64
	EventUIDs    []string
	Error        string
	CreatedAt    time.Time
}

// SkipFederationQuarantineParams resolves an active quarantine by advancing the
// relevant cursor past the quarantined batch.
type SkipFederationQuarantineParams struct {
	ID        int64
	ProjectID int64
	Actor     string
	Reason    string
	Now       time.Time
}

// AdoptProjectIntoFederationParams configures adoption of an existing local
// project into a hub federation.
type AdoptProjectIntoFederationParams struct {
	ProjectID            int64
	HubURL               string
	HubProjectID         int64
	HubProjectUID        string
	ReplayHorizonEventID int64
	Actor                string
}

// AdoptProjectIntoFederationResult describes the adopted project, binding, and
// snapshot emission count.
type AdoptProjectIntoFederationResult struct {
	Project               Project
	Binding               FederationBinding
	AdoptionSnapshotCount int64
}

// FederationIngestEvent carries one spoke event plus the spoke-local event row
// id used only for push cursor acknowledgement.
type FederationIngestEvent struct {
	SourceEventID int64
	Event         RemoteEvent
}

// FederationIngestParams is the all-or-nothing DB ingest boundary used by the
// hub transport handler.
type FederationIngestParams struct {
	ProjectID        int64
	SpokeInstanceUID string
	Events           []FederationIngestEvent
}

// FederationIngestResult summarizes an accepted batch. InsertedEventUIDs lists
// only fresh events, including generated claim audit events in insertion order,
// so callers can avoid rebroadcasting response-lost retries.
type FederationIngestResult struct {
	Accepted          int
	Duplicates        int
	PushCursorEventID int64
	InsertedEventUIDs []string
}

// CreateFederationEnrollmentParams carries the plaintext token at creation
// time only. The database stores only its SHA-256 hash.
type CreateFederationEnrollmentParams struct {
	Token            string
	SpokeInstanceUID string
	ProjectID        *int64
	Capabilities     string
}

// CreatedFederationEnrollment returns the created row plus the plaintext token
// so callers can display generated credentials exactly once.
type CreatedFederationEnrollment struct {
	Enrollment FederationEnrollment
	Token      string
}

// Hidden-project + bootstrap constants used by the token-event seam.
const (
	// SystemProjectName is the hidden project used for daemon-global events.
	SystemProjectName = ".kata-system"
	// SystemProjectUID is the stable UID for the hidden system project.
	SystemProjectUID = "00000000000000000000000000"
	// BootstrapActor is the audit actor for bootstrap/admin token operations.
	BootstrapActor = "bootstrap"
)

// APIToken mirrors a row in the api_tokens projection table.
type APIToken struct {
	ID         int64
	TokenHash  string
	Actor      string
	Name       *string
	CreatedAt  time.Time
	LastUsedAt *time.Time
	RevokedAt  *time.Time
}

// CreateAPITokenParams carries the inputs for minting an API token.
type CreateAPITokenParams struct {
	PlaintextToken string
	Actor          string
	Name           *string
	AdminActor     string
}

// ClaimViolationSummary is the display-ready shape for unresolved
// claim.violated audit events.
type ClaimViolationSummary struct {
	EventID                    int64     `json:"event_id"`
	EventUID                   string    `json:"event_uid"`
	IssueUID                   string    `json:"issue_uid"`
	IssueShortID               string    `json:"short_id,omitempty"`
	OffendingEventUID          string    `json:"offending_event_uid,omitempty"`
	OffendingEventType         string    `json:"offending_event_type,omitempty"`
	OffendingOriginInstanceUID string    `json:"offending_origin_instance_uid,omitempty"`
	Actor                      string    `json:"actor,omitempty"`
	Reason                     string    `json:"reason,omitempty"`
	At                         time.Time `json:"at"`
}
