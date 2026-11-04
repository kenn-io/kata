package conformance

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"time"
)

type recordedPlanOutput struct {
	PlanOutput
	Drops []Drop `json:"drops"`
}

func appendDrop(groups []Drop, reason string, instants []time.Time) []Drop {
	if len(instants) > 0 {
		return append(groups, Drop{Reason: reason, Instants: instants})
	}
	return groups
}

// Inputs are ordered unique sets, and selected is a subset of candidates.
func removedInstants(candidates, selected []time.Time) []time.Time {
	removed := []time.Time{}
	j := 0
	for _, at := range candidates {
		if j < len(selected) && at.Equal(selected[j]) {
			j++
		} else {
			removed = append(removed, at)
		}
	}
	return removed
}

func validateDrops(groups []Drop) error {
	previous := ""
	for _, group := range groups {
		if group.Reason != "catchup_missed" && group.Reason != "overlap_skipped" {
			return fmt.Errorf("unsupported drop reason %q", group.Reason)
		}
		if group.Reason <= previous || len(group.Instants) == 0 || !ordered(group.Instants) {
			return fmt.Errorf("drops require ordered unique reasons and nonempty ordered unique instants")
		}
		previous = group.Reason
	}
	return nil
}

func decodePlanOutput(raw []byte, d Declaration) (PlanOutput, []Drop, error) {
	required := []string{"launch", "checkpoint", "excluded"}
	if d.DropRecords {
		required = append(required, "drops")
	}
	fields, err := requiredOutputFields(raw, required...)
	if err != nil {
		return PlanOutput{}, nil, err
	}
	if _, err := requiredOutputFields(fields["checkpoint"], "through", "definition_event_uid", "pending"); err != nil {
		return PlanOutput{}, nil, fmt.Errorf("checkpoint: %w", err)
	}
	if !d.DropRecords {
		var out PlanOutput
		err := json.Unmarshal(raw, &out, json.RejectUnknownMembers(true))
		return out, nil, err
	}
	var wire struct {
		PlanOutput
		Drops jsontext.Value `json:"drops"`
	}
	if err := json.Unmarshal(raw, &wire, json.RejectUnknownMembers(true)); err != nil {
		return PlanOutput{}, nil, err
	}
	var drops []Drop
	if err := json.Unmarshal(wire.Drops, &drops, json.RejectUnknownMembers(true)); err != nil {
		return PlanOutput{}, nil, err
	}
	if err := validateDrops(drops); err != nil {
		return PlanOutput{}, nil, err
	}
	for i := range drops {
		for j := range drops[i].Instants {
			drops[i].Instants[j] = drops[i].Instants[j].UTC()
		}
	}
	return wire.PlanOutput, drops, nil
}
