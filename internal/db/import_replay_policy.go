package db

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"

	"go.kenn.io/kata/internal/config"
)

// ReplayEventProjectName selects the durable name covered by an event's
// content hash. Current-version exports preserve the name recorded when the
// event was emitted; compatibility cutovers deliberately substitute the
// imported projection's current name before recomputing the hash.
func ReplayEventProjectName(event *EventExport, currentName string, recomputeHash bool) (string, error) {
	name := event.ProjectName
	if name == "" || recomputeHash {
		name = currentName
	}
	if err := config.ValidateProjectName(name); err != nil {
		return "", fmt.Errorf("event %d project_name: %w", event.ID, err)
	}
	return name, nil
}

// ValidateImportRecords checks the normalized replay union before a backend
// opens a transaction. A malformed envelope therefore cannot partially mutate
// either storage implementation.
func ValidateImportRecords(records []ImportRecord) error {
	for i, record := range records {
		if err := record.Validate(); err != nil {
			return fmt.Errorf("import record %d: %w", i, err)
		}
	}
	return nil
}

// OrderImportRecords returns a stable dependency order for replay. JSONL
// exports already use this order, but accepting normalized records in any
// order keeps the Storage contract backend-neutral without requiring every
// Postgres foreign key to be globally deferrable.
func OrderImportRecords(records []ImportRecord) []ImportRecord {
	ordered := append([]ImportRecord(nil), records...)
	sort.SliceStable(ordered, func(i, j int) bool {
		return importReplayRank(ordered[i].Kind) < importReplayRank(ordered[j].Kind)
	})
	return ordered
}

func importReplayRank(kind string) int {
	switch kind {
	case ImportKindMeta:
		return 0
	case ImportKindProject:
		return 1
	case ImportKindProjectAlias, ImportKindIssueSyncBinding, ImportKindRecurrence:
		return 2
	case ImportKindIssueSyncStatus, ImportKindIssue:
		return 3
	case ImportKindComment, ImportKindIssueLabel, ImportKindLink,
		ImportKindFederationBinding, ImportKindFederationEnrollment,
		ImportKindIssueClaim, ImportKindPendingClaimRequest:
		return 4
	case ImportKindImportMapping, ImportKindFederationSyncStatus,
		ImportKindFederationQuarantine:
		return 5
	case ImportKindEvent:
		return 6
	case ImportKindPurgeLog, ImportKindProjectPurgeLog:
		return 7
	case ImportKindIssueEmbedding:
		return 8
	case ImportKindSQLiteSequence:
		return 9
	default:
		return 10
	}
}

// PrepareReplayEvent validates the durable replay identity and either checks
// or regenerates the content hash after backend lookups have filled missing
// issue UIDs.
func PrepareReplayEvent(
	event *EventExport,
	projectUID string,
	projectName string,
	recomputeHash bool,
) error {
	if event.HLCPhysicalMS <= 0 {
		return fmt.Errorf("event %d missing hlc_physical_ms", event.ID)
	}
	if event.HLCCounter < 0 {
		return fmt.Errorf("event %d has negative hlc_counter", event.ID)
	}
	if !recomputeHash && !ValidReplayContentHash(event.ContentHash) {
		return fmt.Errorf("event %d invalid content_hash %q", event.ID, event.ContentHash)
	}
	recomputed, err := EventContentHash(EventHashInput{
		UID:               event.UID,
		OriginInstanceUID: event.OriginInstanceUID,
		ProjectUID:        projectUID,
		ProjectName:       projectName,
		IssueUID:          event.IssueUID,
		RelatedIssueUID:   event.RelatedIssueUID,
		Type:              event.Type,
		Actor:             event.Actor,
		HLCPhysicalMS:     event.HLCPhysicalMS,
		HLCCounter:        event.HLCCounter,
		CreatedAt:         event.CreatedAt,
		Payload:           event.Payload,
	})
	if err != nil {
		return fmt.Errorf("event %d content_hash: %w", event.ID, err)
	}
	if recomputeHash {
		event.ContentHash = recomputed
		return nil
	}
	if recomputed != event.ContentHash {
		return fmt.Errorf("event %d content_hash mismatch (supplied %s, recomputed %s)",
			event.ID, event.ContentHash, recomputed)
	}
	return nil
}

// ValidReplayContentHash reports whether value is a canonical lowercase
// SHA-256 digest.
func ValidReplayContentHash(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, r := range value {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// ReplayTokenCreated is the durable token.created event payload used to
// rebuild the API-token projection during replay.
type ReplayTokenCreated struct {
	TokenID     int64   `json:"token_id"`
	TokenHash   string  `json:"token_hash"`
	TargetActor string  `json:"target_actor"`
	Name        *string `json:"name,omitempty"`
}

// ReplayTokenRevoked is the durable token.revoked projection payload.
type ReplayTokenRevoked struct {
	TokenID int64 `json:"token_id"`
}

// DecodeReplayTokenCreated validates a token.created payload before either
// backend writes it to the derived projection.
func DecodeReplayTokenCreated(payload []byte) (ReplayTokenCreated, error) {
	var record ReplayTokenCreated
	if err := json.Unmarshal(payload, &record); err != nil {
		return ReplayTokenCreated{}, fmt.Errorf("decode token.created payload: %w", err)
	}
	if record.TokenID == 0 || record.TokenHash == "" || record.TargetActor == "" {
		return ReplayTokenCreated{}, fmt.Errorf("decode token.created payload: missing required field")
	}
	if len(record.TokenHash) != 64 {
		return ReplayTokenCreated{}, fmt.Errorf(
			"decode token.created payload: token_hash must be 64 hex characters",
		)
	}
	if _, err := hex.DecodeString(record.TokenHash); err != nil {
		return ReplayTokenCreated{}, fmt.Errorf(
			"decode token.created payload: token_hash must be 64 hex characters: %w", err,
		)
	}
	if err := ValidateTokenActor(record.TargetActor); err != nil {
		return ReplayTokenCreated{}, fmt.Errorf("decode token.created payload: %w", err)
	}
	return record, nil
}

// DecodeReplayTokenRevoked validates a token.revoked payload.
func DecodeReplayTokenRevoked(payload []byte) (ReplayTokenRevoked, error) {
	var record ReplayTokenRevoked
	if err := json.Unmarshal(payload, &record); err != nil {
		return ReplayTokenRevoked{}, fmt.Errorf("decode token.revoked payload: %w", err)
	}
	if record.TokenID == 0 {
		return ReplayTokenRevoked{}, fmt.Errorf("decode token.revoked payload: missing token_id")
	}
	return record, nil
}
