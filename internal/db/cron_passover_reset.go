package db

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
)

type cronResetChange struct {
	From jsontext.Value `json:"from"`
	To   jsontext.Value `json:"to"`
}

func cronResetChanges(event RemoteEvent) map[string]cronResetChange {
	if event.Type != "project.metadata_updated" {
		return nil
	}
	var payload struct {
		Diff map[string]cronResetChange `json:"diff"`
	}
	if json.Unmarshal(event.Payload, &payload) != nil {
		return nil
	}
	return payload.Diff
}

func cronResetClock(event RemoteEvent) FoldClock {
	return FoldClock{HLCPhysicalMS: event.HLCPhysicalMS, HLCCounter: event.HLCCounter, OriginInstanceUID: event.OriginInstanceUID, EventUID: event.EventUID}
}

// validateCronResetHistory retains superseded legacy bytes only when a later
// root repair names that exact malformed value and current root state is valid.
// Live ingress and current entity records continue to require complete values.
func validateCronResetHistory(history, entities []RemoteEvent, authority string) error {
	current := map[string]bool{}
	for _, event := range entities {
		for key, change := range cronResetChanges(event) {
			if cronPassoverKey(key) && event.OriginInstanceUID == authority && ValidateCronPassoverChange(key, nil, change.To) == nil {
				current[key] = true
			}
		}
	}
	type repairKey struct{ key, old string }
	repairs := map[repairKey]FoldClock{}
	for _, event := range history {
		if event.OriginInstanceUID != authority {
			continue
		}
		for key, change := range cronResetChanges(event) {
			if !cronPassoverKey(key) || len(change.From) == 0 || ValidateCronPassoverChange(key, change.From, change.To) != nil {
				continue
			}
			if _, err := decodeCronPassover(change.From); err == nil {
				continue
			}
			id := repairKey{key, string(canonicalJSON(change.From))}
			clock := cronResetClock(event)
			if before, ok := repairs[id]; !ok || compareClock(before, clock) < 0 {
				repairs[id] = clock
			}
		}
	}
	for _, event := range history {
		if err := ValidateCronControlEvent(event, authority); err != nil {
			changes := cronResetChanges(event)
			if changes == nil || event.OriginInstanceUID != authority {
				return err
			}
			for key, change := range changes {
				if cronControlKey(key) && !cronPassoverKey(key) {
					if controlErr := validateCronControlChange(key, change.From, change.To); controlErr != nil {
						return controlErr
					}
				}
				if !cronPassoverKey(key) || ValidateCronPassoverChange(key, change.From, change.To) == nil {
					continue
				}
				// A valid-but-regressing change is never legacy malformed state.
				if _, decodeErr := decodeCronPassover(change.To); decodeErr == nil {
					return err
				}
				repair, ok := repairs[repairKey{key, string(canonicalJSON(change.To))}]
				if !ok || !current[key] || compareClock(cronResetClock(event), repair) >= 0 {
					return err
				}
			}
		}
	}
	return nil
}
