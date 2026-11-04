package db

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"math"
	"time"
)

func accumulateCronTallies(values map[string]CronTallyValue, selected []pruneRun, now time.Time) (map[string]jsontext.Value, error) {
	changed := map[string]bool{}
	for _, run := range selected {
		value, found := values[run.group]
		if !found {
			value = CronTallyValue{Version: 1, Groups: map[string]CronTallyEntry{}}
		}
		entry, found := value.Groups[run.kind]
		if !found {
			entry = CronTallyEntry{Kind: run.kind, UID: run.group, CoverageStartedAt: now, LastPrunedAt: now, OldestCreatedAt: run.created, NewestCreatedAt: run.created}
		}
		var count *int64
		switch run.status {
		case "succeeded":
			count = &entry.Counts.Succeeded
		case "failed":
			count = &entry.Counts.Failed
		case "cancelled":
			count = &entry.Counts.Cancelled
		case "unknown":
			count = &entry.Counts.Unknown
		default:
			return nil, cronTallyError("cannot prune status %q", run.status)
		}
		if *count == math.MaxInt64 {
			return nil, cronTallyError("%s count overflow for %s %s", run.status, run.kind, run.group)
		}
		*count++
		if now.After(entry.LastPrunedAt) {
			entry.LastPrunedAt = now
		}
		if run.created.Before(entry.OldestCreatedAt) {
			entry.OldestCreatedAt = run.created
		}
		if run.created.After(entry.NewestCreatedAt) {
			entry.NewestCreatedAt = run.created
		}
		value.Groups[run.kind] = entry
		values[run.group] = value
		changed[run.group] = true
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
