package db

import (
	"context"
	"encoding/json/jsontext"
	"fmt"
	"time"
)

// IssuePublication is daemon-local recovery state, never portable issue metadata.
// Its payload is the frozen provider request, without credentials.
type IssuePublication struct {
	Remote          *IssuePublicationRemote `json:"remote,omitempty"`
	Version         int                     `json:"version"`
	AttemptUID      string                  `json:"attempt_uid"`
	InstanceUID     string                  `json:"instance_uid"`
	ProjectUID      string                  `json:"project_uid"`
	IssueUID        string                  `json:"issue_uid"`
	BindingID       int64                   `json:"binding_id"`
	Provider        string                  `json:"provider"`
	SourceKey       string                  `json:"source_key"`
	ConfigDigest    string                  `json:"config_digest"`
	Revision        int64                   `json:"revision"`
	PendingEventUID string                  `json:"pending_event_uid,omitempty"`
	SnapshotEventID int64                   `json:"snapshot_event_id"`
	Actor           string                  `json:"actor"`
	CreatedAt       time.Time               `json:"created_at"`
	UpdatedAt       time.Time               `json:"updated_at"`
	Phase           string                  `json:"phase"`
	Payload         jsontext.Value          `json:"payload"`
	PayloadDigest   string                  `json:"payload_digest"`
}

// IssuePublicationRemote is a validated provider observation. SourceUpdatedAt
// is absent when that provider has no genuine content modification clock.
type IssuePublicationRemote struct {
	ExternalID        string     `json:"external_id"`
	LegacyExternalIDs []string   `json:"legacy_external_ids,omitempty"`
	URL               string     `json:"url,omitempty"`
	Locator           string     `json:"locator,omitempty"`
	Status            string     `json:"status"`
	RawStatus         *string    `json:"raw_status,omitempty"`
	ClosedReason      string     `json:"closed_reason,omitempty"`
	Version           time.Time  `json:"version"`
	SourceUpdatedAt   *time.Time `json:"source_updated_at,omitempty"`
}

// IssuePublicationResolve records an attributed attestation that an attempt created no remote object.
type IssuePublicationResolve struct {
	Guard      IssuePublicationGuard
	AttemptUID string
	Actor      string
	Message    string
}

// IssuePublicationAttach binds a verified remote identity to the guarded native issue.
type IssuePublicationAttach struct {
	Guard           IssuePublicationGuard
	AttemptUID      string
	Remote          IssuePublicationRemote
	RestoreAdoption bool
}

// IssuePublicationGuard fences an operation to the exact source and claim.
type IssuePublicationGuard struct {
	// ContentRevision belongs to the claimed snapshot, not the durable journal.
	ContentRevision  int64
	Recovery         bool
	ProjectID        int64
	ProjectUID       string
	IssueID          int64
	IssueUID         string
	BindingID        int64
	Provider         string
	SourceKey        string
	StartedAt        time.Time
	BindingUpdatedAt time.Time
}

// IssuePublicationClaim holds the guarded issue, binding, and existing publication state.
type IssuePublicationClaim struct {
	Binding IssueSyncBinding
	Issue   Issue
	Guard   IssuePublicationGuard
	Pending *IssuePublication
	Mapping *ImportMapping
}

// IssuePublicationBegin persists a frozen request under its current publication claim.
type IssuePublicationBegin struct {
	Guard       IssuePublicationGuard
	Publication IssuePublication
}

// IssuePublicationStore is optional; regular read adapters need not publish.
type IssuePublicationStore interface {
	ResolveIssuePublication(context.Context, IssuePublicationResolve) ([]Event, error)
	RecordIssuePublicationCreated(context.Context, IssuePublicationGuard, string, IssuePublicationRemote) error
	AttachIssuePublication(context.Context, IssuePublicationAttach) (ImportMapping, []Event, error)
	ReleaseIssuePublication(context.Context, IssuePublicationGuard, bool) error
	ClaimIssuePublication(context.Context, int64, int64, string, time.Time, time.Time, bool) (IssuePublicationClaim, error)
	BeginIssuePublication(context.Context, IssuePublicationBegin) (IssuePublication, error)
	DispatchIssuePublication(context.Context, IssuePublicationGuard, string) error
	PendingIssuePublication(context.Context, int64) (*IssuePublication, error)
}

// IssuePublicationMetaPrefix namespaces daemon-local recovery records by project UID.
const IssuePublicationMetaPrefix = "issue_publish.pending.v1:"

// IssuePublicationPendingError exposes recovery identity without saved content.
type IssuePublicationPendingError struct {
	AttemptUID string
	Provider   string
	IssueUID   string
	IssueRef   string
	Phase      string
}

func (e *IssuePublicationPendingError) Error() string {
	issueRef := e.IssueRef
	if issueRef == "" {
		issueRef = e.IssueUID
	}
	return fmt.Sprintf("unresolved publication %s (%s); recover with kata sync %s publish %s --adopt <remote-id> or --resolve-not-created --attempt %s --message '<inspection evidence>'", e.AttemptUID, e.Phase, e.Provider, issueRef, e.AttemptUID)
}
