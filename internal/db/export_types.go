package db

import "encoding/json"

// ExportFilter scopes a JSONL export. The zero value exports every project's
// live (non-deleted) rows.
type ExportFilter struct {
	ProjectID      *int64 // nil = all projects
	IncludeDeleted bool
}

// MetaKV is one meta key/value row.
type MetaKV struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// IssueExport is one issue row in export shape (recurrence_uid resolved via join).
type IssueExport struct {
	ID            int64           `json:"id"`
	UID           string          `json:"uid"`
	ProjectID     int64           `json:"project_id"`
	ShortID       string          `json:"short_id"`
	Title         string          `json:"title"`
	Body          string          `json:"body"`
	Status        string          `json:"status"`
	ClosedReason  *string         `json:"closed_reason"`
	Owner         *string         `json:"owner"`
	Priority      *int64          `json:"priority,omitempty"`
	Author        string          `json:"author"`
	CreatedAt     string          `json:"created_at"`
	UpdatedAt     string          `json:"updated_at"`
	ClosedAt      *string         `json:"closed_at"`
	DeletedAt     *string         `json:"deleted_at"`
	Metadata      json.RawMessage `json:"metadata"`
	Revision      int64           `json:"revision"`
	RecurrenceID  *int64          `json:"recurrence_id,omitempty"`
	RecurrenceUID *string         `json:"recurrence_uid,omitempty"`
	OccurrenceKey *string         `json:"occurrence_key,omitempty"`
}

// RecurrenceExport is one recurrence row in export shape.
type RecurrenceExport struct {
	ID                  int64           `json:"id"`
	UID                 string          `json:"uid"`
	ProjectID           int64           `json:"project_id"`
	RRule               string          `json:"rrule"`
	DTStart             string          `json:"dtstart"`
	Timezone            string          `json:"timezone"`
	TemplateTitle       string          `json:"template_title"`
	TemplateBody        string          `json:"template_body"`
	TemplateOwner       *string         `json:"template_owner,omitempty"`
	TemplatePriority    *int64          `json:"template_priority,omitempty"`
	TemplateLabels      json.RawMessage `json:"template_labels"`
	TemplateMetadata    json.RawMessage `json:"template_metadata"`
	NextOccurrenceKey   *string         `json:"next_occurrence_key,omitempty"`
	LastMaterializedUID *string         `json:"last_materialized_uid,omitempty"`
	Author              string          `json:"author"`
	Revision            int64           `json:"revision"`
	CreatedAt           string          `json:"created_at"`
	UpdatedAt           string          `json:"updated_at"`
	DeletedAt           *string         `json:"deleted_at,omitempty"`
}

// LinkExport is one link row in export shape, with both endpoint UIDs resolved
// via the standard FROM links JOIN issues AS from_issues JOIN issues AS
// to_issues query.
type LinkExport struct {
	ID           int64  `json:"id"`
	ProjectID    int64  `json:"project_id"`
	FromIssueID  int64  `json:"from_issue_id"`
	FromIssueUID string `json:"from_issue_uid"`
	ToIssueID    int64  `json:"to_issue_id"`
	ToIssueUID   string `json:"to_issue_uid"`
	Type         string `json:"type"`
	Author       string `json:"author"`
	CreatedAt    string `json:"created_at"`
}

// AliasExport is one project_aliases row in export shape.
type AliasExport struct {
	ID            int64  `json:"id"`
	ProjectID     int64  `json:"project_id"`
	AliasIdentity string `json:"alias_identity"`
	AliasKind     string `json:"alias_kind"`
	RootPath      string `json:"root_path"`
	CreatedAt     string `json:"created_at"`
	LastSeenAt    string `json:"last_seen_at"`
}

// CommentExport is one comment row in export shape.
type CommentExport struct {
	ID        int64  `json:"id"`
	UID       string `json:"uid"`
	IssueID   int64  `json:"issue_id"`
	Author    string `json:"author"`
	Body      string `json:"body"`
	CreatedAt string `json:"created_at"`
}

// IssueLabelExport is one issue_labels row in export shape.
type IssueLabelExport struct {
	IssueID   int64  `json:"issue_id"`
	Label     string `json:"label"`
	Author    string `json:"author"`
	CreatedAt string `json:"created_at"`
}

// ImportMappingExport is one import_mappings row in export shape.
type ImportMappingExport struct {
	ID              int64   `json:"id"`
	Source          string  `json:"source"`
	ExternalID      string  `json:"external_id"`
	ObjectType      string  `json:"object_type"`
	ProjectID       int64   `json:"project_id"`
	IssueID         *int64  `json:"issue_id,omitempty"`
	CommentID       *int64  `json:"comment_id,omitempty"`
	LinkID          *int64  `json:"link_id,omitempty"`
	Label           *string `json:"label,omitempty"`
	SourceUpdatedAt *string `json:"source_updated_at,omitempty"`
	ImportedAt      string  `json:"imported_at"`
}

// FederationBindingExport is one federation_bindings row in export shape.
type FederationBindingExport struct {
	ProjectID            int64   `json:"project_id"`
	Role                 string  `json:"role"`
	HubURL               string  `json:"hub_url"`
	HubProjectID         int64   `json:"hub_project_id"`
	HubProjectUID        string  `json:"hub_project_uid"`
	ReplayHorizonEventID int64   `json:"replay_horizon_event_id"`
	PullCursorEventID    int64   `json:"pull_cursor_event_id"`
	PushEnabled          bool    `json:"push_enabled"`
	PushCursorEventID    int64   `json:"push_cursor_event_id"`
	Enabled              bool    `json:"enabled"`
	CreatedAt            string  `json:"created_at"`
	UpdatedAt            string  `json:"updated_at"`
	LastSyncAt           *string `json:"last_sync_at,omitempty"`
}

// FederationSyncStatusExport is one federation_sync_status row in export shape.
type FederationSyncStatusExport struct {
	ProjectID         int64   `json:"project_id"`
	LastPullStartedAt *string `json:"last_pull_started_at,omitempty"`
	LastPullSuccessAt *string `json:"last_pull_success_at,omitempty"`
	LastPushStartedAt *string `json:"last_push_started_at,omitempty"`
	LastPushSuccessAt *string `json:"last_push_success_at,omitempty"`
	LastErrorAt       *string `json:"last_error_at,omitempty"`
	LastError         *string `json:"last_error,omitempty"`
	LastResetAt       *string `json:"last_reset_at,omitempty"`
}

// FederationQuarantineExport is one federation_quarantine row in export shape.
type FederationQuarantineExport struct {
	ID           int64           `json:"id"`
	ProjectID    int64           `json:"project_id"`
	Direction    string          `json:"direction"`
	FirstEventID int64           `json:"first_event_id"`
	LastEventID  int64           `json:"last_event_id"`
	EventUIDs    json.RawMessage `json:"event_uids"`
	Error        string          `json:"error"`
	CreatedAt    string          `json:"created_at"`
	SkippedAt    *string         `json:"skipped_at,omitempty"`
	SkippedBy    *string         `json:"skipped_by,omitempty"`
	SkipReason   *string         `json:"skip_reason,omitempty"`
}

// FederationEnrollmentExport is one federation_enrollments row in export shape.
type FederationEnrollmentExport struct {
	ID               int64   `json:"id"`
	TokenHash        string  `json:"token_hash"`
	SpokeInstanceUID string  `json:"spoke_instance_uid"`
	ProjectID        *int64  `json:"project_id,omitempty"`
	Capabilities     string  `json:"capabilities"`
	CreatedAt        string  `json:"created_at"`
	UpdatedAt        string  `json:"updated_at"`
	RevokedAt        *string `json:"revoked_at,omitempty"`
}

// IssueClaimExport is one issue_claims row in export shape.
type IssueClaimExport struct {
	ID                int64   `json:"id"`
	ClaimUID          string  `json:"claim_uid"`
	ProjectID         int64   `json:"project_id"`
	IssueID           int64   `json:"issue_id"`
	IssueUID          string  `json:"issue_uid"`
	Holder            string  `json:"holder"`
	HolderInstanceUID string  `json:"holder_instance_uid"`
	ClientKind        string  `json:"client_kind"`
	Purpose           string  `json:"purpose"`
	ClaimKind         string  `json:"claim_kind"`
	AcquiredAt        string  `json:"acquired_at"`
	ExpiresAt         *string `json:"expires_at,omitempty"`
	ReleasedAt        *string `json:"released_at,omitempty"`
	ReleaseReason     *string `json:"release_reason,omitempty"`
	Revision          int64   `json:"revision"`
	UpdatedAt         string  `json:"updated_at"`
}

// PendingClaimRequestExport is one pending_claim_requests row in export shape.
type PendingClaimRequestExport struct {
	ID                int64   `json:"id"`
	RequestUID        string  `json:"request_uid"`
	ProjectID         int64   `json:"project_id"`
	IssueID           int64   `json:"issue_id"`
	IssueUID          string  `json:"issue_uid"`
	Holder            string  `json:"holder"`
	HolderInstanceUID string  `json:"holder_instance_uid"`
	ClientKind        string  `json:"client_kind"`
	ClaimKind         string  `json:"claim_kind"`
	TTLSeconds        *int64  `json:"ttl_seconds,omitempty"`
	Purpose           string  `json:"purpose"`
	RequestedAt       string  `json:"requested_at"`
	LastAttemptAt     *string `json:"last_attempt_at,omitempty"`
	LastError         *string `json:"last_error,omitempty"`
	RejectedAt        *string `json:"rejected_at,omitempty"`
	ResolvedAt        *string `json:"resolved_at,omitempty"`
}

// SequenceExport is one sqlite_sequence row. SQLite-only; future backends yield nothing.
type SequenceExport struct {
	Name string `json:"name"`
	Seq  int64  `json:"seq"`
}

// PurgeLogExport is one purge_log row in export shape. project_name is the
// denormalized purge_log.project_name column at v10+ (no join).
type PurgeLogExport struct {
	ID                     int64   `json:"id"`
	UID                    string  `json:"uid"`
	OriginInstanceUID      string  `json:"origin_instance_uid"`
	ProjectID              int64   `json:"project_id"`
	PurgedIssueID          int64   `json:"purged_issue_id"`
	IssueUID               *string `json:"issue_uid"`
	ProjectUID             *string `json:"project_uid"`
	ProjectName            string  `json:"project_name"`
	ShortID                *string `json:"short_id,omitempty"`
	IssueTitle             string  `json:"issue_title"`
	IssueAuthor            string  `json:"issue_author"`
	CommentCount           int64   `json:"comment_count"`
	LinkCount              int64   `json:"link_count"`
	LabelCount             int64   `json:"label_count"`
	EventCount             int64   `json:"event_count"`
	EventsDeletedMinID     *int64  `json:"events_deleted_min_id"`
	EventsDeletedMaxID     *int64  `json:"events_deleted_max_id"`
	PurgeResetAfterEventID *int64  `json:"purge_reset_after_event_id"`
	Actor                  string  `json:"actor"`
	Reason                 *string `json:"reason"`
	PurgedAt               string  `json:"purged_at"`
}

// EventExport is one event row in export shape. ProjectUID is marshaled as
// `json:"-"` because it is used only to compute the content hash and is not
// part of the wire envelope. related_issue_id/uid are scrubbed to NULL when
// the peer row is missing (any type) or, on live-only export, when an
// issue.links_changed peer is soft-deleted.
type EventExport struct {
	ID                int64           `json:"id"`
	UID               string          `json:"uid"`
	OriginInstanceUID string          `json:"origin_instance_uid"`
	ProjectID         int64           `json:"project_id"`
	ProjectUID        string          `json:"-"`
	ProjectName       string          `json:"project_name"`
	IssueID           *int64          `json:"issue_id"`
	IssueUID          *string         `json:"issue_uid"`
	RelatedIssueID    *int64          `json:"related_issue_id"`
	RelatedIssueUID   *string         `json:"related_issue_uid"`
	Type              string          `json:"type"`
	Actor             string          `json:"actor"`
	Payload           json.RawMessage `json:"payload"`
	HLCPhysicalMS     int64           `json:"hlc_physical_ms"`
	HLCCounter        int64           `json:"hlc_counter"`
	ContentHash       string          `json:"content_hash"`
	CreatedAt         string          `json:"created_at"`
}

// ProjectExport is one project row in export shape.
type ProjectExport struct {
	ID        int64           `json:"id"`
	UID       string          `json:"uid"`
	Name      string          `json:"name"`
	CreatedAt string          `json:"created_at"`
	DeletedAt *string         `json:"deleted_at,omitempty"`
	Metadata  json.RawMessage `json:"metadata"`
	Revision  int64           `json:"revision"`
}
