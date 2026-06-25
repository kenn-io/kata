// Package jsonl exports and imports kata database state as ordered NDJSON.
package jsonl

import (
	"encoding/json"
	"errors"
)

// Kind is the fixed record kind tag in a JSONL envelope.
type Kind string

// JSONL record kinds. Order matches the export sequence enforced by kindOrder.
const (
	KindMeta                 Kind = "meta"
	KindProject              Kind = "project"
	KindProjectAlias         Kind = "project_alias"
	KindIssueSyncBinding     Kind = "issue_sync_binding"
	KindIssueSyncStatus      Kind = "issue_sync_status"
	KindGitHubSyncBinding    Kind = "github_sync_binding"
	KindGitHubSyncStatus     Kind = "github_sync_status"
	KindRecurrence           Kind = "recurrence"
	KindIssue                Kind = "issue"
	KindComment              Kind = "comment"
	KindIssueLabel           Kind = "issue_label"
	KindLink                 Kind = "link"
	KindImportMapping        Kind = "import_mapping"
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
	KindMeta:                 0,
	KindProject:              1,
	KindProjectAlias:         2,
	KindIssueSyncBinding:     3,
	KindIssueSyncStatus:      4,
	KindGitHubSyncBinding:    3,
	KindGitHubSyncStatus:     4,
	KindRecurrence:           5,
	KindIssue:                6,
	KindComment:              7,
	KindIssueLabel:           8,
	KindLink:                 9,
	KindImportMapping:        10,
	KindFederationBinding:    11,
	KindFederationSyncStatus: 12,
	KindFederationQuarantine: 13,
	KindFederationEnrollment: 14,
	KindIssueClaim:           15,
	KindPendingClaimRequest:  16,
	KindEvent:                17,
	KindPurgeLog:             18,
	KindProjectPurgeLog:      19,
	KindSQLiteSequence:       20,
}

// Envelope is one NDJSON record.
type Envelope struct {
	Kind Kind            `json:"kind"`
	Data json.RawMessage `json:"data"`
}

func kindRank(k Kind) (int, bool) {
	rank, ok := kindOrder[k]
	return rank, ok
}
