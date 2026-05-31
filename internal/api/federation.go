package api //nolint:revive // package name "api" is fixed by Plan 1 §4 wire-types layout.

import (
	"encoding/json"
	"time"

	"go.kenn.io/kata/internal/db"
)

// EnableProjectFederationRequest enables pull federation on one hub project.
type EnableProjectFederationRequest struct {
	ProjectID int64 `path:"project_id"`
	Body      struct {
		Actor string `json:"actor,omitempty"`
	}
}

// ProjectFederationRequest reads federation metadata for one project.
type ProjectFederationRequest struct {
	ProjectID int64 `path:"project_id"`
}

// FederationStatusRequest reads status for all locally bound federation
// projects through the normal local/admin daemon auth surface.
type FederationStatusRequest struct{}

// ProjectFederationStatusRequest reads status for one locally bound
// federation project through the normal local/admin daemon auth surface.
type ProjectFederationStatusRequest struct {
	ProjectID int64 `path:"project_id"`
}

// SkipFederationQuarantineRequest skips one quarantined federation batch
// through the local/admin daemon auth surface.
type SkipFederationQuarantineRequest struct {
	ProjectID    int64  `path:"project_id"`
	QuarantineID int64  `path:"quarantine_id"`
	Confirm      string `header:"X-Kata-Confirm"`
	Body         struct {
		Actor  string `json:"actor"`
		Reason string `json:"reason,omitempty"`
	}
}

// FederationProjectMetadataRequest reads federation metadata through the
// enrollment-authenticated transport surface.
type FederationProjectMetadataRequest struct {
	ProjectID     int64  `path:"project_id"`
	Authorization string `header:"Authorization"`
}

// ProjectFederationBody is the hub metadata a trusted spoke needs before it
// begins project-scoped event polling.
type ProjectFederationBody struct {
	ProjectID              int64  `json:"project_id"`
	ProjectUID             string `json:"project_uid"`
	ProjectName            string `json:"project_name"`
	ReplayHorizonEventID   int64  `json:"replay_horizon_event_id"`
	BaselineThroughEventID int64  `json:"baseline_through_event_id"`
}

// ProjectFederationResponse wraps ProjectFederationBody.
type ProjectFederationResponse struct {
	Body ProjectFederationBody
}

// FederationStatusResponse wraps FederationStatusBody.
type FederationStatusResponse struct {
	Body FederationStatusBody
}

// FederationStatusBody is the stable operator-facing federation status
// envelope. A non-federated database returns an empty statuses array.
type FederationStatusBody struct {
	Statuses []FederationProjectStatus `json:"statuses"`
}

// FederationProjectStatus summarizes one local project's federation health.
type FederationProjectStatus struct {
	ProjectID                   int64                         `json:"project_id"`
	ProjectUID                  string                        `json:"project_uid"`
	ProjectName                 string                        `json:"project_name"`
	Role                        string                        `json:"role"`
	Enabled                     bool                          `json:"enabled"`
	PushEnabled                 bool                          `json:"push_enabled"`
	PullCursorEventID           int64                         `json:"pull_cursor_event_id"`
	PushCursorEventID           int64                         `json:"push_cursor_event_id"`
	PendingPushCount            int64                         `json:"pending_push_count"`
	PendingPushHighWaterEventID int64                         `json:"pending_push_high_water_event_id"`
	EnrollmentCount             int64                         `json:"enrollment_count"`
	LiveClaimCount              int64                         `json:"live_claim_count"`
	PendingClaimCount           int64                         `json:"pending_claim_count"`
	ActiveQuarantineCount       int64                         `json:"active_quarantine_count"`
	ActiveQuarantines           []FederationQuarantineSummary `json:"active_quarantines"`
	ResetBlocker                string                        `json:"reset_blocker,omitempty"`
	UnresolvedViolationCount    int64                         `json:"unresolved_violation_count"`
	RecentViolationCount        int64                         `json:"recent_violation_count"`
	RecentViolations            []FederationViolationSummary  `json:"recent_violations"`
	LastSyncAt                  *time.Time                    `json:"last_sync_at,omitempty"`
	LastSuccessfulSyncAt        *time.Time                    `json:"last_successful_sync_at,omitempty"`
	LastPullStartedAt           *time.Time                    `json:"last_pull_started_at,omitempty"`
	LastPullSuccessAt           *time.Time                    `json:"last_pull_success_at,omitempty"`
	LastPushStartedAt           *time.Time                    `json:"last_push_started_at,omitempty"`
	LastPushSuccessAt           *time.Time                    `json:"last_push_success_at,omitempty"`
	LastErrorAt                 *time.Time                    `json:"last_error_at,omitempty"`
	LastError                   *string                       `json:"last_error,omitempty"`
	LastResetAt                 *time.Time                    `json:"last_reset_at,omitempty"`
}

// FederationQuarantineSummary is an unresolved poisoned federation batch shown
// to operators.
type FederationQuarantineSummary struct {
	ID           int64     `json:"id"`
	Direction    string    `json:"direction"`
	FirstEventID int64     `json:"first_event_id"`
	LastEventID  int64     `json:"last_event_id"`
	EventUIDs    []string  `json:"event_uids"`
	Error        string    `json:"error"`
	CreatedAt    time.Time `json:"created_at"`
}

// SkipFederationQuarantineResponse returns the skipped quarantine.
type SkipFederationQuarantineResponse struct {
	Body FederationQuarantineSummary
}

// FederationViolationSummary is an unresolved claim violation shown in
// operator federation status.
type FederationViolationSummary struct {
	EventID                    int64     `json:"event_id"`
	EventUID                   string    `json:"event_uid"`
	IssueUID                   string    `json:"issue_uid"`
	ShortID                    string    `json:"short_id,omitempty"`
	OffendingEventUID          string    `json:"offending_event_uid,omitempty"`
	OffendingEventType         string    `json:"offending_event_type,omitempty"`
	OffendingOriginInstanceUID string    `json:"offending_origin_instance_uid,omitempty"`
	Reason                     string    `json:"reason,omitempty"`
	Actor                      string    `json:"actor,omitempty"`
	At                         time.Time `json:"at"`
}

// CreateFederationEnrollmentRequest creates a hidden hub-side transport
// credential for one spoke.
type CreateFederationEnrollmentRequest struct {
	Body struct {
		SpokeInstanceUID string `json:"spoke_instance_uid"`
		ProjectID        *int64 `json:"project_id"`
		Capabilities     string `json:"capabilities"`
		Token            string `json:"token,omitempty"`
	}
}

// FederationEnrollmentOut is the API-owned enrollment representation. It
// deliberately omits token_hash; Token is populated only on creation.
type FederationEnrollmentOut struct {
	ID               int64      `json:"id"`
	SpokeInstanceUID string     `json:"spoke_instance_uid"`
	ProjectID        *int64     `json:"project_id"`
	Capabilities     string     `json:"capabilities"`
	CreatedAt        time.Time  `json:"created_at"`
	UpdatedAt        time.Time  `json:"updated_at"`
	RevokedAt        *time.Time `json:"revoked_at,omitempty"`
	Token            string     `json:"token,omitempty"`
}

// CreateFederationEnrollmentResponse wraps FederationEnrollmentOut.
type CreateFederationEnrollmentResponse struct {
	Body FederationEnrollmentOut
}

// ListFederationEnrollmentsRequest lists hub-side federation transport grants.
type ListFederationEnrollmentsRequest struct{}

// ListFederationEnrollmentsBody is returned by the hub-side enrollment audit
// endpoint. Tokens are never included.
type ListFederationEnrollmentsBody struct {
	Enrollments []FederationEnrollmentOut `json:"enrollments"`
}

// ListFederationEnrollmentsResponse wraps ListFederationEnrollmentsBody.
type ListFederationEnrollmentsResponse struct {
	Body ListFederationEnrollmentsBody
}

// RevokeFederationEnrollmentRequest revokes a hub-side federation transport
// grant by enrollment ID.
type RevokeFederationEnrollmentRequest struct {
	EnrollmentID int64 `path:"enrollment_id"`
}

// RevokeFederationEnrollmentBody confirms revocation. Revocation is
// idempotent at the DB level; an existing revoked row still reports revoked.
type RevokeFederationEnrollmentBody struct {
	ID      int64 `json:"id"`
	Revoked bool  `json:"revoked"`
}

// RevokeFederationEnrollmentResponse wraps RevokeFederationEnrollmentBody.
type RevokeFederationEnrollmentResponse struct {
	Body RevokeFederationEnrollmentBody
}

// CreateFederationReplicaRequest creates a local spoke project bound to a hub.
type CreateFederationReplicaRequest struct {
	Body struct {
		HubURL                 string `json:"hub_url"`
		HubProjectID           int64  `json:"hub_project_id"`
		HubProjectUID          string `json:"hub_project_uid"`
		ProjectName            string `json:"project_name"`
		ReplayHorizonEventID   int64  `json:"replay_horizon_event_id"`
		BaselineThroughEventID int64  `json:"baseline_through_event_id,omitempty"`
		Token                  string `json:"token,omitempty"`
		Capabilities           string `json:"capabilities,omitempty"`
		PushEnabled            bool   `json:"push_enabled,omitempty"`
		AdoptExisting          bool   `json:"adopt_existing,omitempty"`
	}
}

// CreateFederationReplicaBody is returned after binding a local spoke project.
type CreateFederationReplicaBody struct {
	Project               ProjectOut           `json:"project"`
	Binding               FederationBindingOut `json:"binding"`
	Adopted               bool                 `json:"adopted,omitempty"`
	AdoptionSnapshotCount int64                `json:"adoption_snapshot_count,omitempty"`
}

// CreateFederationReplicaResponse wraps CreateFederationReplicaBody.
type CreateFederationReplicaResponse struct {
	Body CreateFederationReplicaBody
}

// FederationPollEventsRequest is the enrollment-authenticated federation
// transport poll route. It mirrors PollEventsRequest but carries its own bearer
// header because the route bypasses daemon admin bearer auth.
type FederationPollEventsRequest struct {
	ProjectID     int64       `path:"project_id"`
	Authorization string      `header:"Authorization"`
	AfterID       int64       `query:"after_id,omitempty"`
	Limit         OptionalInt `query:"limit,omitempty"`
}

// FederationIngestEventsRequest is the enrollment-authenticated push transport
// route.
type FederationIngestEventsRequest struct {
	ProjectID     int64  `path:"project_id"`
	Authorization string `header:"Authorization"`
	Body          FederationIngestEventsRequestBody
}

// FederationIngestEventsRequestBody carries an all-or-nothing push batch.
type FederationIngestEventsRequestBody struct {
	SchemaVersion int                             `json:"schema_version"`
	Events        []FederationIngestEventEnvelope `json:"events,omitempty"`
}

// FederationIngestEventEnvelope is the portable event shape accepted from a
// spoke. Source EventID is the spoke-local row cursor; local hub IDs and
// display-only short IDs are intentionally excluded.
type FederationIngestEventEnvelope struct {
	EventID           int64           `json:"event_id"`
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

// FederationIngestEventsBody summarizes an accepted push batch. Duplicates are
// same-hash retries and still advance PushCursorEventID.
type FederationIngestEventsBody struct {
	Accepted          int   `json:"accepted"`
	Duplicates        int   `json:"duplicates"`
	PushCursorEventID int64 `json:"push_cursor_event_id"`
}

// FederationIngestEventsResponse wraps FederationIngestEventsBody.
type FederationIngestEventsResponse struct {
	Body FederationIngestEventsBody
}

// ClaimActionRequest carries the shared path/header/body shape for
// claim/renew/release action routes.
type ClaimActionRequest struct {
	ProjectID     int64  `path:"project_id"`
	Ref           string `path:"ref"`
	Authorization string `header:"Authorization"`
	Body          ClaimActionBody
}

// ClaimActionBody describes the caller-controlled pieces of claim mutations.
// holder_instance_uid is intentionally absent; the daemon derives it from the
// resolved local or federation principal.
type ClaimActionBody struct {
	_          struct{} `json:"-" additionalProperties:"true"`
	Holder     string   `json:"holder,omitempty"`
	ClientKind string   `json:"client_kind,omitempty"`
	ClaimKind  string   `json:"claim_kind,omitempty"`
	TTLSeconds int64    `json:"ttl_seconds,omitempty"`
	Purpose    string   `json:"purpose,omitempty"`
	Reason     string   `json:"reason,omitempty"`
	Actor      string   `json:"actor,omitempty"`
}

// ClaimStatusRequest reads the currently live claim for one issue.
type ClaimStatusRequest struct {
	ProjectID     int64  `path:"project_id"`
	Ref           string `path:"ref"`
	Authorization string `header:"Authorization"`
}

// ClaimActionResponse is returned by claim/renew/release/force_release.
type ClaimActionResponse struct {
	Body ClaimActionResponseBody
}

// ClaimActionResponseBody summarizes the arbitration result. Lease is the
// public federation name; Claim is kept as a compatibility alias for the
// internal storage term until this pre-merge branch finishes the storage rename.
type ClaimActionResponseBody struct {
	Granted    bool              `json:"granted"`
	Pending    bool              `json:"pending,omitempty"`
	RequestUID string            `json:"request_uid,omitempty"`
	Holder     ClaimPrincipalOut `json:"holder"`
	Lease      *IssueClaimOut    `json:"lease,omitempty"`
	Claim      *IssueClaimOut    `json:"claim,omitempty"`
	Event      *db.Event         `json:"event,omitempty"`
}

// ClaimStatusResponse wraps ClaimStatusBody.
type ClaimStatusResponse struct {
	Body ClaimStatusBody
}

// ClaimStatusBody is the read-only claim status payload.
type ClaimStatusBody struct {
	Held   bool              `json:"held"`
	Holder ClaimPrincipalOut `json:"holder"`
	Lease  *IssueClaimOut    `json:"lease,omitempty"`
	Claim  *IssueClaimOut    `json:"claim,omitempty"`
	HubNow time.Time         `json:"hub_now"`
}

// FederationBindingOut is the API-owned representation of a local federation
// binding. It avoids leaking Go field names from the storage type into JSON.
type FederationBindingOut struct {
	ProjectID            int64      `json:"project_id"`
	Role                 string     `json:"role"`
	HubURL               string     `json:"hub_url"`
	HubProjectID         int64      `json:"hub_project_id"`
	HubProjectUID        string     `json:"hub_project_uid"`
	ReplayHorizonEventID int64      `json:"replay_horizon_event_id"`
	PullCursorEventID    int64      `json:"pull_cursor_event_id"`
	PushEnabled          bool       `json:"push_enabled"`
	PushCursorEventID    int64      `json:"push_cursor_event_id"`
	Enabled              bool       `json:"enabled"`
	CreatedAt            time.Time  `json:"created_at"`
	UpdatedAt            time.Time  `json:"updated_at"`
	LastSyncAt           *time.Time `json:"last_sync_at,omitempty"`
}
