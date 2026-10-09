// Package jsonl exports and imports kata database state as ordered NDJSON.
package jsonl

import (
	"encoding/json/jsontext"
	"errors"
)

// Kind is the fixed record kind tag in a JSONL envelope.
type Kind string

// JSONL record kinds. Order matches the export sequence enforced by kindOrder.
const (
	KindEmbeddingArtifact    Kind = "federation_embedding_artifact"
	KindRelayOutbox          Kind = "federation_relay_outbox"
	KindRelayInbox           Kind = "federation_relay_inbox"
	KindRelayCursors         Kind = "federation_relay_cursors"
	KindRootKey              Kind = "federation_root_key"
	KindEventProvenance      Kind = "federation_event_provenance"
	KindEntityProvenance     Kind = "federation_entity_provenance"
	KindTeam                 Kind = "team"
	KindTeamMembership       Kind = "team_membership"
	KindProjectAccessPolicy  Kind = "project_access_policy"
	KindMeta                 Kind = "meta"
	KindProject              Kind = "project"
	KindProjectAlias         Kind = "project_alias"
	KindIssueSyncBinding     Kind = "issue_sync_binding"
	KindIssueSyncStatus      Kind = "issue_sync_status"
	KindGitHubSyncBinding    Kind = "github_sync_binding"
	KindGitHubSyncStatus     Kind = "github_sync_status"
	KindRecurrence           Kind = "recurrence"
	KindIssue                Kind = "issue"
	KindIssueEmbedding       Kind = "issue_embedding"
	KindComment              Kind = "comment"
	KindIssueLabel           Kind = "issue_label"
	KindLink                 Kind = "link"
	KindImportMapping        Kind = "import_mapping"
	KindExternalFieldMapping Kind = "external_field_mapping"
	KindExternalRootBinding  Kind = "external_root_binding"
	KindExternalFieldState   Kind = "external_field_state"
	KindFederationBinding    Kind = "federation_binding"
	KindFederationSyncStatus Kind = "federation_sync_status"
	KindFederationQuarantine Kind = "federation_quarantine"
	KindFederationEnrollment Kind = "federation_enrollment"
	KindIssueClaim           Kind = "issue_claim"
	KindPendingClaimRequest  Kind = "pending_claim_request"
	KindEvent                Kind = "event"
	KindPurgeLog             Kind = "purge_log"
	KindProjectPurgeLog      Kind = "project_purge_log"
	KindSQLiteSequence       Kind = "sqlite_sequence"
)

// Sentinel errors returned by the decoder for malformed or out-of-order envelopes.
var (
	ErrMissingExportVersion = errors.New("missing export_version")
	ErrUnknownKind          = errors.New("unknown kind")
	ErrKindOrderViolation   = errors.New("kind order violation")
)

var kindOrder = map[Kind]int{
	KindEmbeddingArtifact: 34,
	KindRelayOutbox:       31, KindRelayInbox: 32, KindRelayCursors: 33,
	KindTeam: 25, KindTeamMembership: 26, KindProjectAccessPolicy: 27,
	KindRootKey: 28, KindEventProvenance: 29, KindEntityProvenance: 30,
	KindMeta:                 0,
	KindProject:              1,
	KindProjectAlias:         2,
	KindIssueSyncBinding:     3,
	KindIssueSyncStatus:      4,
	KindGitHubSyncBinding:    3,
	KindGitHubSyncStatus:     4,
	KindRecurrence:           5,
	KindIssue:                6,
	KindIssueEmbedding:       7,
	KindComment:              8,
	KindIssueLabel:           9,
	KindLink:                 10,
	KindImportMapping:        11,
	KindExternalFieldMapping: 12,
	KindExternalRootBinding:  13,
	KindExternalFieldState:   14,
	KindFederationBinding:    15,
	KindFederationSyncStatus: 16,
	KindFederationQuarantine: 17,
	KindFederationEnrollment: 18,
	KindIssueClaim:           19,
	KindPendingClaimRequest:  20,
	KindEvent:                21,
	KindPurgeLog:             22,
	KindProjectPurgeLog:      23,
	KindSQLiteSequence:       24,
}

// Envelope is one NDJSON record.
type Envelope struct {
	Kind Kind           `json:"kind"`
	Data jsontext.Value `json:"data"`
}

func kindRank(k Kind) (int, bool) {
	rank, ok := kindOrder[k]
	return rank, ok
}
