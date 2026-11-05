package db

import (
	"encoding/json/v2"
	"fmt"
	"strings"
)

// CronTallyResetClearPayload clears the union of current and retained-history
// keys before a checkpoint's complete metadata state. Old peers recursively
// merge ordinary object values, so omission alone cannot remove a key or kind.
// Source bytes remain untouched; this is an ordinary root metadata event.
func CronTallyResetClearPayload(project Project, records RelayCheckpointRecords) (string, error) {
	current, err := DecodeCronTallies(project.Metadata)
	if err != nil {
		return "", err
	}
	keys := map[string]bool{}
	for groupUID := range current {
		keys[CronTallyPrefix+groupUID] = true
	}
	for _, section := range [][][]byte{records.Sources, records.Entities} {
		for _, raw := range section {
			event, err := DecodeRelaySourceEvent(raw)
			if err != nil {
				return "", err
			}
			if err := ValidateCronTallyEvent(event); err != nil {
				return "", fmt.Errorf("%w: %v", ErrFederationIngestValidation, err)
			}
			for key := range cronTallyEventFields(event) {
				if strings.HasPrefix(key, CronTallyPrefix) {
					keys[key] = true
				}
			}
		}
	}
	if len(keys) == 0 {
		return "", nil
	}
	diff := map[string]struct {
		From any `json:"from"`
		To   any `json:"to"`
	}{}
	for key := range keys {
		diff[key] = struct {
			From any `json:"from"`
			To   any `json:"to"`
		}{}
	}
	raw, err := json.Marshal(struct {
		Diff any `json:"diff"`
	}{diff}, json.Deterministic(true))
	return string(raw), err
}
