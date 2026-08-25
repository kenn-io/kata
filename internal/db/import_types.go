package db

import (
	"time"
)

// ImportOptions controls optional ImportReplay behaviors.
type ImportOptions struct {
	// RequireFreshTarget rejects replay unless the target still contains only
	// its bootstrap metadata and hidden system project. CLI restores use this
	// after observing an uninitialized target so a concurrent writer cannot be
	// cleared without --force.
	RequireFreshTarget bool

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

	// PreserveIssueSyncBindingEnabled keeps imported issue sync bindings in
	// their source enabled/disabled state. This is only for trusted local
	// schema cutover; normal JSONL restore leaves restored bindings disabled
	// until a local operator re-enables them.
	PreserveIssueSyncBindingEnabled bool

	// MergeProject remaps a single project-scoped replay onto fresh numeric IDs
	// and inserts it without clearing existing domain state.
	MergeProject bool

	// PreserveExternalRootBindingsEnabled keeps imported active external-root
	// bindings in their source enabled/paused state. This is only for trusted
	// local schema cutover; normal restore requires local reconfirmation.
	PreserveExternalRootBindingsEnabled bool
}

// ImportRecord is one normalized, current-shape import row. Its
// implementations are the export row structs on pointer receivers (see
// export_types.go), so the discriminator is the record's dynamic type: a
// record cannot disagree with its payload, carry two payloads, or carry
// none. ImportKind returns the frozen NDJSON kind string that internal/jsonl
// writes on the wire. jsonl normalizes every source version to the current
// shape before building these, so ImportReplay never sees a source
// export_version.
type ImportRecord interface {
	ImportKind() string
}

// Import kind discriminators. These mirror the wire Kind strings produced by
// internal/jsonl (jsonl.Kind); db cannot import jsonl (that would be a cycle),
// so the contract is the shared NDJSON kind string, asserted by the roundtrip
// tests.
const (
	ImportKindMeta                 = "meta"
	ImportKindProject              = "project"
	ImportKindProjectAlias         = "project_alias"
	ImportKindIssueSyncBinding     = "issue_sync_binding"
	ImportKindIssueSyncStatus      = "issue_sync_status"
	ImportKindRecurrence           = "recurrence"
	ImportKindIssue                = "issue"
	ImportKindIssueEmbedding       = "issue_embedding"
	ImportKindComment              = "comment"
	ImportKindIssueLabel           = "issue_label"
	ImportKindLink                 = "link"
	ImportKindImportMapping        = "import_mapping"
	ImportKindExternalFieldMapping = "external_field_mapping"
	ImportKindExternalRootBinding  = "external_root_binding"
	ImportKindExternalFieldState   = "external_field_state"
	ImportKindFederationBinding    = "federation_binding"
	ImportKindFederationSyncStatus = "federation_sync_status"
	ImportKindFederationQuarantine = "federation_quarantine"
	ImportKindFederationEnrollment = "federation_enrollment"
	ImportKindIssueClaim           = "issue_claim"
	ImportKindPendingClaimRequest  = "pending_claim_request"
	ImportKindEvent                = "event"
	ImportKindPurgeLog             = "purge_log"
	ImportKindProjectPurgeLog      = "project_purge_log"
	ImportKindSQLiteSequence       = "sqlite_sequence"
)

// ImportBatchParams is the input to ImportBatch: the project receiving the
// import, the source identifier (e.g. "beads"), the actor recorded on emitted
// events, and the normalized issue items to upsert.
type ImportBatchParams struct {
	ProjectID      int64
	Source         string
	Actor          string
	IssueSyncGuard *IssueSyncImportGuard
	// ReconcileLinkTypesForUnchanged asks ImportBatch to reconcile source-managed
	// links of selected types even when the issue row itself is unchanged. Normal
	// imports leave unchanged issues' labels and links alone.
	ReconcileLinkTypesForUnchanged map[string]bool
	// PreserveLocalParentConflicts leaves an existing local parent in place when
	// a source-managed parent insert would create a second parent. Generic
	// imports report ErrParentAlreadySet by default.
	PreserveLocalParentConflicts bool
	Items                        []ImportItem
}

// IssueSyncImportGuard binds an ImportBatch call to a specific claimed
// issue-sync run. Stores that support issue sync verify the binding is still
// enabled, still claimed by StartedAt, and still federation-compatible in the
// same transaction as the import writes.
type IssueSyncImportGuard struct {
	BindingID int64
	Provider  string
	StartedAt time.Time
}

// ImportItem is one normalized issue in an import batch. ExternalID is the
// source-side identifier used for upsert via import_mappings; CreatedAt and
// UpdatedAt drive timestamp fidelity and source-vs-local conflict resolution.
type ImportItem struct {
	ExternalID string
	// LegacyExternalIDs are alternate keys a prior version may have used for
	// this same object. When no mapping exists under ExternalID, import upsert
	// adopts a mapping found under one of these and re-keys it onto ExternalID,
	// so a canonical-key change does not duplicate already-imported issues.
	LegacyExternalIDs      []string
	Title                  string
	Body                   string
	Author                 string
	Owner                  *string
	Priority               *int64
	Status                 string
	ClosedReason           *string
	CreatedAt              time.Time
	UpdatedAt              time.Time
	ClosedAt               *time.Time
	Labels                 []string
	Comments               []ImportComment
	Links                  []ImportLink
	LinkTypesAuthoritative map[string]bool
}

// ImportComment is one normalized comment attached to an ImportItem. ExternalID
// is the source-side comment identifier used for upsert via import_mappings.
type ImportComment struct {
	ExternalID string
	// LegacyExternalIDs mirrors ImportItem.LegacyExternalIDs for comments.
	LegacyExternalIDs []string
	Author            string
	Body              string
	CreatedAt         time.Time
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
