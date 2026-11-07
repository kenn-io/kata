package db

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"math"
	"time"
)

func accumulateCronTallies(values map[string]CronTallyValue, groups []CronRunPruneGroup, now time.Time) (map[string]jsontext.Value, error) {
	changed := map[string]bool{}
	for _, group := range groups {
		kind, groupUID := "job", group.JobUID
		if groupUID == nil {
			kind, groupUID = "workflow", group.WorkflowUID
		}
		value, found := values[*groupUID]
		if !found {
			value = CronTallyValue{Version: 1, Groups: map[string]CronTallyEntry{}}
		}
		entry, found := value.Groups[kind]
		if !found {
			entry = CronTallyEntry{Kind: kind, UID: *groupUID, CoverageStartedAt: now, LastPrunedAt: now, OldestCreatedAt: group.OldestCreatedAt, NewestCreatedAt: group.NewestCreatedAt}
		}
		for status, count := range map[string]*int64{"succeeded": &entry.Counts.Succeeded, "failed": &entry.Counts.Failed, "cancelled": &entry.Counts.Cancelled, "unknown": &entry.Counts.Unknown} {
			n := int64(group.Counts[status])
			if n > math.MaxInt64-*count {
				return nil, cronTallyError("%s count overflow for %s %s", status, kind, *groupUID)
			}
			*count += n
		}
		if now.After(entry.LastPrunedAt) {
			entry.LastPrunedAt = now
		}
		if group.OldestCreatedAt.Before(entry.OldestCreatedAt) {
			entry.OldestCreatedAt = group.OldestCreatedAt
		}
		if group.NewestCreatedAt.After(entry.NewestCreatedAt) {
			entry.NewestCreatedAt = group.NewestCreatedAt
		}
		value.Groups[kind] = entry
		values[*groupUID] = value
		changed[*groupUID] = true
	}
	patch := make(map[string]jsontext.Value, len(changed))
	for groupUID := range changed {
		raw, err := json.Marshal(values[groupUID], json.Deterministic(true))
		if err != nil {
			return nil, err
		}
		patch[CronTallyPrefix+groupUID] = raw
	}
	return patch, nil
}
