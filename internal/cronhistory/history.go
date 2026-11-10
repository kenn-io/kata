// Package cronhistory enriches retained definition pages with exact field diffs.
package cronhistory

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"slices"
	"time"

	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
)

// ErrChanged means a continuation no longer provides one preceding version.
var ErrChanged = errors.New("definition history changed while reading the previous version; retry history")

// WithDiff compares each version to its predecessor, fetching one older entry
// when needed. It preserves the initial cursor and merges skipped diagnostics.
func WithDiff(ctx context.Context, raw []byte, fetchOlder func(context.Context, string) ([]byte, error)) ([]byte, error) {
	var page db.CronVersionPage
	if err := json.Unmarshal(raw, &page); err != nil {
		return nil, err
	}
	var older *db.CronDefinitionVersion
	if page.NextBeforeEventUID != "" {
		raw, err := fetchOlder(ctx, page.NextBeforeEventUID)
		if err != nil {
			return nil, err
		}
		var lookahead db.CronVersionPage
		if err := json.Unmarshal(raw, &lookahead); err != nil {
			return nil, err
		}
		if len(page.Versions) == 0 || len(lookahead.Versions) != 1 {
			return nil, ErrChanged
		}
		older = &lookahead.Versions[0]
		page.SkippedEventUIDs = historySkippedEvents(page.SkippedEventUIDs, lookahead.SkippedEventUIDs)
	}
	type versionWithChanges struct {
		db.CronDefinitionVersion
		Changes jsontext.Value `json:"changes"`
	}
	out := struct {
		Versions           []versionWithChanges `json:"versions"`
		NextBeforeEventUID string               `json:"next_before_event_uid,omitempty"`
		SkippedEventUIDs   []string             `json:"skipped_event_uids,omitempty"`
	}{Versions: make([]versionWithChanges, 0, len(page.Versions)), NextBeforeEventUID: page.NextBeforeEventUID, SkippedEventUIDs: page.SkippedEventUIDs}
	for i, v := range page.Versions {
		previous := older
		if i+1 < len(page.Versions) {
			previous = &page.Versions[i+1]
		}
		changes := jsontext.Value(`null`)
		if previous != nil {
			before, err := cronHistoryDocument(*previous)
			if err != nil {
				return nil, err
			}
			after, err := cronHistoryDocument(v)
			if err != nil {
				return nil, err
			}
			fields, err := cron.DiffDocuments(before, after)
			if err != nil {
				return nil, err
			}
			changes, err = json.Marshal(fields)
			if err != nil {
				return nil, err
			}
		}
		out.Versions = append(out.Versions, versionWithChanges{CronDefinitionVersion: v, Changes: changes})
	}
	return json.Marshal(out)
}

func cronHistoryDocument(v db.CronDefinitionVersion) (jsontext.Value, error) {
	return json.Marshal(struct {
		Name       string         `json:"name"`
		Definition jsontext.Value `json:"definition"`
		DeletedAt  *time.Time     `json:"deleted_at,omitempty"`
	}{v.Name, v.Definition, v.DeletedAt})
}

func historySkippedEvents(first, second []string) []string {
	values := append(slices.Clone(first), second...)
	slices.Sort(values)
	values = slices.Compact(values)
	return values[:min(20, len(values))]
}
