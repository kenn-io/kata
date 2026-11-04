package conformance

import (
	"bytes"
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

// Both inputs are ordered unique sets; selected is a subset of candidates.
func removedInstants(candidates, selected []time.Time) []time.Time {
	removed := []time.Time{}
	j := 0
	for _, at := range candidates {
		if j < len(selected) && at.Equal(selected[j]) {
			j++
			continue
		}
		removed = append(removed, at)
	}
	return removed
}
func validateDrops(drops []Drop) error {
	previous := ""
	for _, drop := range drops {
		if drop.Reason != "catchup_missed" && drop.Reason != "overlap_skipped" {
			return fmt.Errorf("unsupported drop reason %q", drop.Reason)
		}
		if drop.Reason <= previous || len(drop.Instants) == 0 || !ordered(drop.Instants) {
			return fmt.Errorf("drops require ordered unique reasons and nonempty ordered unique instants")
		}
		previous = drop.Reason
	}
	return nil
}

func decodePlanOutput(raw []byte, declaration Declaration) (PlanOutput, []Drop, error) {
	if !declaration.DropRecords {
		var out PlanOutput
		err := json.Unmarshal(raw, &out, json.RejectUnknownMembers(true))
		return out, nil, err
	}
	var encoded struct {
		PlanOutput
		Drops jsontext.Value `json:"drops"`
	}
	if err := json.Unmarshal(raw, &encoded, json.RejectUnknownMembers(true)); err != nil {
		return PlanOutput{}, nil, err
	}
	if len(encoded.Drops) == 0 || bytes.Equal(bytes.TrimSpace(encoded.Drops), []byte("null")) {
		return PlanOutput{}, nil, fmt.Errorf("recorded plan requires a drops array")
	}
	var drops []Drop
	if err := json.Unmarshal(encoded.Drops, &drops, json.RejectUnknownMembers(true)); err != nil {
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
	return encoded.PlanOutput, drops, nil
}
