package db

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"time"
)

const issueStatusScanKey = "_status_sync"

// NextIssueSyncBindingUpdatedAt advances the binding timestamp enough to fence
// operator edits, even within one millisecond or when the clock moves backward.
func NextIssueSyncBindingUpdatedAt(previous, now time.Time) time.Time {
	now = now.UTC().Truncate(time.Millisecond)
	minimum := previous.UTC().Truncate(time.Millisecond).Add(time.Millisecond)
	if now.Before(minimum) {
		return minimum
	}
	return now
}

// IssueStatusScanCursor stores one bounded private status-scan interval.
type IssueStatusScanCursor struct {
	After   int64 `json:"after"`
	Through int64 `json:"through"`
}

// IssueStatusScanState stores daemon-owned scan progress outside the content
// cursor, without a per-object retry schedule or provider metadata.
type IssueStatusScanState struct {
	Pending     IssueStatusScanCursor `json:"pending"`
	Sweep       IssueStatusScanCursor `json:"sweep"`
	LocatorPage int                   `json:"locator_page,omitzero"`
}

// IssueStatusScanStore persists the fenced scan checkpoint for a binding.
type IssueStatusScanStore interface {
	UpdateIssueStatusScan(context.Context, IssueSyncImportGuard, IssueStatusScanState) (IssueSyncBinding, error)
}

// DecodeIssueStatusScan reads and validates daemon-owned scan progress.
func DecodeIssueStatusScan(config jsontext.Value) (IssueStatusScanState, error) {
	var fields map[string]jsontext.Value
	if err := json.Unmarshal(config, &fields); err != nil || fields == nil {
		return IssueStatusScanState{}, invalidIssueStatusState()
	}
	var state IssueStatusScanState
	if raw, ok := fields[issueStatusScanKey]; ok {
		if err := json.Unmarshal(raw, &state, json.RejectUnknownMembers(true)); err != nil || string(raw) == "null" {
			return state, invalidIssueStatusState()
		}
	}
	if err := validateStatusScan(state); err != nil {
		return state, err
	}
	return state, nil
}

func validateStatusScan(state IssueStatusScanState) error {
	for _, cursor := range []IssueStatusScanCursor{state.Pending, state.Sweep} {
		if cursor.After < 0 || cursor.Through < 0 || cursor.After > cursor.Through {
			return invalidIssueStatusState()
		}
	}
	if state.LocatorPage < 0 {
		return invalidIssueStatusState()
	}
	return nil
}

// SetIssueStatusScanConfig replaces only the private scan checkpoint.
func SetIssueStatusScanConfig(config jsontext.Value, state IssueStatusScanState) (jsontext.Value, error) {
	if err := validateStatusScan(state); err != nil {
		return nil, err
	}
	var fields map[string]jsontext.Value
	if err := json.Unmarshal(config, &fields); err != nil || fields == nil {
		return nil, invalidIssueStatusState()
	}
	raw, err := json.Marshal(state)
	if err != nil {
		return nil, err
	}
	fields[issueStatusScanKey] = raw
	return json.Marshal(fields, json.Deterministic(true))
}

// PublicIssueSyncConfig excludes daemon-owned scan checkpoints from typed
// provider validation and ordinary restores; user configuration remains intact.
func PublicIssueSyncConfig(config jsontext.Value) (jsontext.Value, error) {
	var fields map[string]jsontext.Value
	if err := json.Unmarshal(config, &fields); err != nil || fields == nil {
		return nil, invalidIssueStatusState()
	}
	if _, present := fields[issueStatusScanKey]; !present {
		return config, nil
	}
	delete(fields, issueStatusScanKey)
	return json.Marshal(fields, json.Deterministic(true))
}

// IssueSyncConfigMatches compares user-selected fields without allowing daemon
// scan progress or top-level object ordering to invalidate an operator snapshot.
func IssueSyncConfigMatches(left, right jsontext.Value) (bool, error) {
	return issueSyncConfigMatches(left, right, nil)
}

// issueStatusSettingKeys select status sync mode and write targets. Content
// imports never read them.
var issueStatusSettingKeys = []string{"status_sync", "todo_group_id", "closed_status_id", "open_status_id", "closed_state_id", "open_state_id", "open_status"}

// IssueSyncContentConfigMatches reports whether two configs produce the same
// content imports, so a change between them keeps the content cursor.
func IssueSyncContentConfigMatches(left, right jsontext.Value) (bool, error) {
	return issueSyncConfigMatches(left, right, issueStatusSettingKeys)
}

func issueSyncConfigMatches(left, right jsontext.Value, ignored []string) (bool, error) {
	var normalized [2][]byte
	for i, raw := range []jsontext.Value{left, right} {
		public, err := PublicIssueSyncConfig(raw)
		if err != nil {
			return false, err
		}
		var fields map[string]jsontext.Value
		if err := json.Unmarshal(public, &fields); err != nil {
			return false, err
		}
		for _, key := range ignored {
			delete(fields, key)
		}
		normalized[i], err = json.Marshal(fields, json.Deterministic(true))
		if err != nil {
			return false, err
		}
	}
	return string(normalized[0]) == string(normalized[1]), nil
}

// PreserveIssueStatusScanConfig prevents a config edit or metadata refresh from
// supplying or losing private progress. Only the fenced scan method changes it.
func PreserveIssueStatusScanConfig(previous, next jsontext.Value) (jsontext.Value, error) {
	clean, err := PublicIssueSyncConfig(next)
	if err != nil {
		return nil, err
	}
	var old map[string]jsontext.Value
	if err := json.Unmarshal(previous, &old); err != nil || old == nil {
		return nil, invalidIssueStatusState()
	}
	if raw, present := old[issueStatusScanKey]; present {
		if _, err := DecodeIssueStatusScan(previous); err != nil {
			return nil, err
		}
		var fields map[string]jsontext.Value
		if err := json.Unmarshal(clean, &fields); err != nil {
			return nil, err
		}
		if fields == nil {
			return nil, invalidIssueStatusState()
		}
		fields[issueStatusScanKey] = raw
		return json.Marshal(fields, json.Deterministic(true))
	}
	return clean, nil
}

// IssueStatusMode returns the configured mode, defaulting to one-way sync.
func IssueStatusMode(config jsontext.Value) (string, error) {
	var fields map[string]jsontext.Value
	if err := json.Unmarshal(config, &fields); err != nil || fields == nil {
		return "", invalidIssueStatusState()
	}
	mode := "one-way"
	if raw, present := fields["status_sync"]; present {
		if err := json.Unmarshal(raw, &mode); err != nil || (mode != "one-way" && mode != "two-way") {
			return "", invalidIssueStatusState()
		}
	}
	return mode, nil
}
