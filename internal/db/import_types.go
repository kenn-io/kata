package db

import (
	"fmt"
	"strings"
	"time"
)

// ImportOptions controls optional ImportReplay behaviors.
type ImportOptions struct {
	// NewInstance keeps the target's existing meta.instance_uid (the value
	// db.Open wrote on first open) instead of applying the source's. The
	// imported events/purge_log origin_instance_uid columns are NOT rewritten:
	// they keep the original origins so a future federation loop-detector can
	// tell which rows came from the cloned-from instance.
	NewInstance bool

	// DedupeLegacyActivePendingClaims tells ImportReplay to skip a pending
	// claim request whose (issue_uid, holder_instance_uid, holder,
	// client_kind) tuple already has an active (not rejected, not resolved)
	// row. Used when the source pre-dates v12 — that schema lacked the
	// uniqueness constraint and could carry duplicates. Current-version
	// streams set this false; the constraint is already enforced upstream.
	DedupeLegacyActivePendingClaims bool

	// RecomputeEventContentHash tells ImportReplay to replace event hashes
	// after resolving replay-only fields such as issue_uid. Used for legacy
	// JSONL streams whose source schema lacked the final portable event fields.
	// Current streams leave this false so a mismatched supplied hash is refused.
	RecomputeEventContentHash bool
}

// ImportRecord is one normalized, current-shape import row: a Kind discriminator
// plus exactly one payload pointer reusing the 1c-export row structs. jsonl
// normalizes every source version to the current shape before building these,
// so ImportReplay never sees a source export_version.
type ImportRecord struct {
	Kind                 string
	Meta                 *MetaKV
	Project              *ProjectExport
	Alias                *AliasExport
	Recurrence           *RecurrenceExport
	Issue                *IssueExport
	Comment              *CommentExport
	Label                *IssueLabelExport
	Link                 *LinkExport
	ImportMapping        *ImportMappingExport
	FederationBinding    *FederationBindingExport
	FederationSyncStatus *FederationSyncStatusExport
	FederationQuarantine *FederationQuarantineExport
	FederationEnrollment *FederationEnrollmentExport
	IssueClaim           *IssueClaimExport
	PendingClaimRequest  *PendingClaimRequestExport
	Event                *EventExport
	PurgeLog             *PurgeLogExport
	Sequence             *SequenceExport
}

// Import kind discriminators. These mirror the wire Kind strings produced by
// internal/jsonl (jsonl.Kind); db cannot import jsonl (that would be a cycle),
// so the contract is the shared NDJSON kind string, asserted by the roundtrip
// tests.
const (
	ImportKindMeta                 = "meta"
	ImportKindProject              = "project"
	ImportKindProjectAlias         = "project_alias"
	ImportKindRecurrence           = "recurrence"
	ImportKindIssue                = "issue"
	ImportKindComment              = "comment"
	ImportKindIssueLabel           = "issue_label"
	ImportKindLink                 = "link"
	ImportKindImportMapping        = "import_mapping"
	ImportKindFederationBinding    = "federation_binding"
	ImportKindFederationSyncStatus = "federation_sync_status"
	ImportKindFederationQuarantine = "federation_quarantine"
	ImportKindFederationEnrollment = "federation_enrollment"
	ImportKindIssueClaim           = "issue_claim"
	ImportKindPendingClaimRequest  = "pending_claim_request"
	ImportKindEvent                = "event"
	ImportKindPurgeLog             = "purge_log"
	ImportKindSQLiteSequence       = "sqlite_sequence"
)

// Validate enforces the tagged-union invariant: Kind is recognized and exactly
// the one matching payload pointer is set. It returns a clear error naming the
// offending Kind; ImportReplay adds the slice ordinal.
func (r ImportRecord) Validate() error {
	payloads := []struct {
		kind string
		set  bool
	}{
		{ImportKindMeta, r.Meta != nil},
		{ImportKindProject, r.Project != nil},
		{ImportKindProjectAlias, r.Alias != nil},
		{ImportKindRecurrence, r.Recurrence != nil},
		{ImportKindIssue, r.Issue != nil},
		{ImportKindComment, r.Comment != nil},
		{ImportKindIssueLabel, r.Label != nil},
		{ImportKindLink, r.Link != nil},
		{ImportKindImportMapping, r.ImportMapping != nil},
		{ImportKindFederationBinding, r.FederationBinding != nil},
		{ImportKindFederationSyncStatus, r.FederationSyncStatus != nil},
		{ImportKindFederationQuarantine, r.FederationQuarantine != nil},
		{ImportKindFederationEnrollment, r.FederationEnrollment != nil},
		{ImportKindIssueClaim, r.IssueClaim != nil},
		{ImportKindPendingClaimRequest, r.PendingClaimRequest != nil},
		{ImportKindEvent, r.Event != nil},
		{ImportKindPurgeLog, r.PurgeLog != nil},
		{ImportKindSQLiteSequence, r.Sequence != nil},
	}
	known := false
	var set []string
	for _, p := range payloads {
		if p.kind == r.Kind {
			known = true
		}
		if p.set {
			set = append(set, p.kind)
		}
	}
	if !known {
		return fmt.Errorf("unknown kind %q", r.Kind)
	}
	if len(set) == 0 {
		return fmt.Errorf("kind %q: no payload set", r.Kind)
	}
	if len(set) > 1 {
		return fmt.Errorf("kind %q: multiple payloads set (%s)", r.Kind, strings.Join(set, ", "))
	}
	if set[0] != r.Kind {
		return fmt.Errorf("kind %q: payload does not match (got %s)", r.Kind, set[0])
	}
	return nil
}

// ImportBatchParams is the input to ImportBatch: the project receiving the
// import, the source identifier (e.g. "beads"), the actor recorded on emitted
// events, and the normalized issue items to upsert.
type ImportBatchParams struct {
	ProjectID int64
	Source    string
	Actor     string
	Items     []ImportItem
}

// ImportItem is one normalized issue in an import batch. ExternalID is the
// source-side identifier used for upsert via import_mappings; CreatedAt and
// UpdatedAt drive timestamp fidelity and source-vs-local conflict resolution.
type ImportItem struct {
	ExternalID   string
	Title        string
	Body         string
	Author       string
	Owner        *string
	Priority     *int64
	Status       string
	ClosedReason *string
	CreatedAt    time.Time
	UpdatedAt    time.Time
	ClosedAt     *time.Time
	Labels       []string
	Comments     []ImportComment
	Links        []ImportLink
}

// ImportComment is one normalized comment attached to an ImportItem. ExternalID
// is the source-side comment identifier used for upsert via import_mappings.
type ImportComment struct {
	ExternalID string
	Author     string
	Body       string
	CreatedAt  time.Time
}

// ImportLink is one normalized outgoing link from an ImportItem. TargetExternalID
// references another item's ExternalID in the same batch (or an existing mapped
// item); the daemon resolves it to a kata issue number.
type ImportLink struct {
	Type             string
	TargetExternalID string
}

// ImportBatchResult summarizes a completed import batch: per-status counts and
// a per-item breakdown the CLI uses for human and JSON output.
type ImportBatchResult struct {
	Source    string             `json:"source"`
	Created   int                `json:"created"`
	Updated   int                `json:"updated"`
	Unchanged int                `json:"unchanged"`
	Comments  int                `json:"comments"`
	Links     int                `json:"links"`
	Items     []ImportItemResult `json:"items"`
	Errors    []string           `json:"errors"`
}

// ImportItemResult is the per-item entry in ImportBatchResult.Items. Status is
// "created", "updated", or "unchanged"; Reason carries an optional rationale
// (e.g. "local newer").
type ImportItemResult struct {
	ExternalID   string `json:"external_id"`
	IssueShortID string `json:"issue_short_id"`
	Status       string `json:"status"`
	Reason       string `json:"reason,omitempty"`
}

// ImportMapping mirrors a row in import_mappings.
type ImportMapping struct {
	ID              int64      `json:"id"`
	Source          string     `json:"source"`
	ExternalID      string     `json:"external_id"`
	ObjectType      string     `json:"object_type"`
	ProjectID       int64      `json:"project_id"`
	IssueID         *int64     `json:"issue_id,omitempty"`
	CommentID       *int64     `json:"comment_id,omitempty"`
	LinkID          *int64     `json:"link_id,omitempty"`
	Label           *string    `json:"label,omitempty"`
	SourceUpdatedAt *time.Time `json:"source_updated_at,omitempty"`
	ImportedAt      time.Time  `json:"imported_at"`
}

// ImportMappingParams carries values for inserting or updating a source
// identity mapping.
type ImportMappingParams struct {
	Source          string
	ExternalID      string
	ObjectType      string
	ProjectID       int64
	IssueID         *int64
	CommentID       *int64
	LinkID          *int64
	Label           *string
	SourceUpdatedAt *time.Time
}
