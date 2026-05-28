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
	KindRecurrence:           3,
	KindIssue:                4,
	KindComment:              5,
	KindIssueLabel:           6,
	KindLink:                 7,
	KindImportMapping:        8,
	KindFederationBinding:    9,
	KindFederationSyncStatus: 10,
	KindFederationQuarantine: 11,
	KindFederationEnrollment: 12,
	KindIssueClaim:           13,
	KindPendingClaimRequest:  14,
	KindEvent:                15,
	KindPurgeLog:             16,
	KindSQLiteSequence:       17,
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
