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
// root repair names that exact malformed value and current root state allows it.
// Live ingress and current entity records continue to require complete values.
func validateCronResetHistory(history, entities []RemoteEvent, authority string) error {
	current, repairs := cronResetCurrent(entities, authority), cronResetRepairs(history, authority)
	for _, event := range history {
		if err := ValidateCronControlEvent(event, authority); err != nil {
			changes := cronResetChanges(event)
			if changes == nil || event.OriginInstanceUID != authority {
				return err
			}
			for key, change := range changes {
				if repairErr := cronResetRepaired(key, change, cronResetClock(event), current, repairs); repairErr != nil {
					return repairErr
				}
			}
		}
	}
	return nil
}

type cronRepairKey struct{ key, old string }

// cronResetCurrent marks keys whose current root value decides a repair: a
// valid passover permits one, while any present other control forbids one.
func cronResetCurrent(entities []RemoteEvent, authority string) map[string]bool {
	current := map[string]bool{}
	for _, event := range entities {
		for key, change := range cronResetChanges(event) {
			if !cronPassoverKey(key) {
				current[key] = current[key] || (cronControlKey(key) && len(change.To) > 0 && change.To.Kind() != jsontext.KindNull)
			} else if event.OriginInstanceUID == authority && ValidateCronPassoverChange(key, nil, change.To) == nil {
				current[key] = true
			}
		}
	}
	return current
}

// cronResetRepairs indexes the latest root repair of each exact old value: a
// valid passover over malformed bytes, or a root deletion of another control.
func cronResetRepairs(history []RemoteEvent, authority string) map[cronRepairKey]FoldClock {
	repairs := map[cronRepairKey]FoldClock{}
	for _, event := range history {
		if event.OriginInstanceUID != authority {
			continue
		}
		for key, change := range cronResetChanges(event) {
			if !cronResetRepair(key, change) {
				continue
			}
			id := cronRepairKey{key, string(canonicalJSON(change.From))}
			clock := cronResetClock(event)
			if before, ok := repairs[id]; !ok || compareClock(before, clock) < 0 {
				repairs[id] = clock
			}
		}
	}
	return repairs
}

func cronResetRepair(key string, change cronResetChange) bool {
	if len(change.From) == 0 || !cronControlKey(key) {
		return false
	}
	if !cronPassoverKey(key) {
		return change.From.Kind() != jsontext.KindNull && change.To.Kind() == jsontext.KindNull
	}
	if _, err := decodeCronPassover(change.From); err == nil {
		return false
	}
	return ValidateCronPassoverChange(key, change.From, change.To) == nil
}

// cronResetRepaired accepts one change of a failed root event when it is valid
// or is legacy bytes an exact later repair names. A passover repair needs a
// valid current value; another control must have been created from absence
// and be absent now, so mutations of complete values never qualify.
func cronResetRepaired(key string, change cronResetChange, clock FoldClock, current map[string]bool, repairs map[cronRepairKey]FoldClock) error {
	err := validateCronControlChange(key, change.From, change.To)
	if err == nil || !cronControlKey(key) {
		return nil
	}
	repair, ok := repairs[cronRepairKey{key, string(canonicalJSON(change.To))}]
	if cronPassoverKey(key) {
		// A valid-but-regressing change is never legacy malformed state.
		_, decodeErr := decodeCronPassover(change.To)
		ok = ok && decodeErr != nil && current[key]
	} else {
		ok = ok && (len(change.From) == 0 || change.From.Kind() == jsontext.KindNull) && !current[key]
	}
	if !ok || compareClock(clock, repair) >= 0 {
		return err
	}
	return nil
}
