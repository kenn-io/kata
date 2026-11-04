package db

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
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
	_, statusPresent := fields[issueStatusScanKey]
	_, checkpointPresent := fields[IssueSyncProviderCheckpointKey]
	if !statusPresent && !checkpointPresent {
		return config, nil
	}
	delete(fields, issueStatusScanKey)
	delete(fields, IssueSyncProviderCheckpointKey)
	return json.Marshal(fields, json.Deterministic(true))
}

// IssueSyncConfigForRestore strips live daemon progress while retaining the
// validated content baseline needed to safely import a published task later.
func IssueSyncConfigForRestore(config jsontext.Value) (jsontext.Value, error) {
	public, err := PublicIssueSyncConfig(config)
	if err != nil {
		return nil, err
	}
	var fields map[string]jsontext.Value
	if err := json.Unmarshal(config, &fields); err != nil || fields == nil {
		return nil, invalidIssueStatusState()
	}
	rawCheckpoint, present := fields[IssueSyncProviderCheckpointKey]
	if !present || len(rawCheckpoint) > 2<<20 {
		return public, nil
	}
	var checkpoint map[string]jsontext.Value
	if err := json.Unmarshal(rawCheckpoint, &checkpoint); err != nil || checkpoint == nil {
		return public, nil
	}
	rawBaselines, present := checkpoint[IssueSyncPublicationContentBaselinesKey]
	if !present {
		return public, nil
	}
	if len(rawBaselines) > 2<<20 {
		return nil, fmt.Errorf("%w: invalid issue publication content baseline", ErrImportValidation)
	}
	var baselines map[string]IssuePublicationContentBaseline
	if err := json.Unmarshal(rawBaselines, &baselines, json.RejectUnknownMembers(true)); err != nil || baselines == nil {
		return nil, fmt.Errorf("%w: invalid issue publication content baseline", ErrImportValidation)
	}
	if len(baselines) == 0 {
		return public, nil
	}
	for externalID, baseline := range baselines {
		if !validIssueSyncPublicationContentBaseline(externalID, baseline) {
			return nil, fmt.Errorf("%w: invalid issue publication content baseline", ErrImportValidation)
		}
		baseline.At = baseline.At.UTC().Truncate(time.Millisecond)
		baselines[externalID] = baseline
	}
	baselineJSON, err := json.Marshal(baselines, json.Deterministic(true))
	if err != nil {
		return nil, err
	}
	checkpointJSON, err := json.Marshal(map[string]jsontext.Value{
		IssueSyncPublicationContentBaselinesKey: baselineJSON,
	}, json.Deterministic(true))
	if err != nil {
		return nil, err
	}
	var restored map[string]jsontext.Value
	if err := json.Unmarshal(public, &restored); err != nil || restored == nil {
		return nil, invalidIssueStatusState()
	}
	restored[IssueSyncProviderCheckpointKey] = checkpointJSON
	return json.Marshal(restored, json.Deterministic(true))
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
	var fields map[string]jsontext.Value
	if err := json.Unmarshal(clean, &fields); err != nil || fields == nil {
		return nil, invalidIssueStatusState()
	}
	for _, key := range []string{issueStatusScanKey, IssueSyncProviderCheckpointKey} {
		if raw, present := old[key]; present {
			if key == issueStatusScanKey {
				if _, err := DecodeIssueStatusScan(previous); err != nil {
					return nil, err
				}
			}
			fields[key] = raw
		}
	}
	return json.Marshal(fields, json.Deterministic(true))
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

// IssueSyncProviderCheckpointKey holds an adapter's private checkpoint inside
// binding config. Exports and operator edits never supply or expose it.
const IssueSyncProviderCheckpointKey = "_provider_checkpoint"

// IssueSyncPublicationContentBaselinesKey stores verified publication content
// snapshots inside the private provider checkpoint.
const IssueSyncPublicationContentBaselinesKey = "publication_content_baselines"

// HasIssueSyncPublicationContentBaseline reports whether the private provider
// checkpoint holds a publication baseline for externalID. Moves use this to
// keep a baseline with the project binding that can consume it.
func HasIssueSyncPublicationContentBaseline(config jsontext.Value, externalID string) (bool, error) {
	invalid := fmt.Errorf("%w: invalid issue sync publication checkpoint", ErrImportValidation)
	if externalID == "" {
		return false, invalid
	}
	var fields map[string]jsontext.Value
	if err := json.Unmarshal(config, &fields); err != nil || fields == nil {
		return false, invalid
	}
	rawCheckpoint, ok := fields[IssueSyncProviderCheckpointKey]
	if !ok {
		return false, nil
	}
	if len(rawCheckpoint) > 2<<20 {
		return false, invalid
	}
	var checkpoint map[string]jsontext.Value
	if err := json.Unmarshal(rawCheckpoint, &checkpoint); err != nil || checkpoint == nil {
		return false, invalid
	}
	rawBaselines, ok := checkpoint[IssueSyncPublicationContentBaselinesKey]
	if !ok {
		return false, nil
	}
	if len(rawBaselines) > 2<<20 {
		return false, invalid
	}
	var baselines map[string]jsontext.Value
	if err := json.Unmarshal(rawBaselines, &baselines); err != nil || baselines == nil {
		return false, invalid
	}
	_, ok = baselines[externalID]
	return ok, nil
}

// WithIssueSyncPublicationContentBaseline records the exact published content
// snapshot without treating it as a provider modification timestamp.
func WithIssueSyncPublicationContentBaseline(
	config jsontext.Value,
	externalID string,
	baseline IssuePublicationContentBaseline,
) (jsontext.Value, error) {
	invalid := fmt.Errorf("%w: invalid issue publication content baseline", ErrImportValidation)
	if !validIssueSyncPublicationContentBaseline(externalID, baseline) {
		return nil, invalid
	}
	baseline.At = baseline.At.UTC().Truncate(time.Millisecond)

	var fields map[string]jsontext.Value
	if err := json.Unmarshal(config, &fields); err != nil || fields == nil {
		return nil, invalid
	}
	checkpoint := map[string]jsontext.Value{}
	if raw, ok := fields[IssueSyncProviderCheckpointKey]; ok {
		if len(raw) > 2<<20 || json.Unmarshal(raw, &checkpoint) != nil || checkpoint == nil {
			return nil, invalid
		}
	}
	baselines := map[string]IssuePublicationContentBaseline{}
	if raw, ok := checkpoint[IssueSyncPublicationContentBaselinesKey]; ok {
		if json.Unmarshal(raw, &baselines) != nil || baselines == nil {
			return nil, invalid
		}
	}
	baselines[externalID] = baseline
	baselineJSON, err := json.Marshal(baselines, json.Deterministic(true))
	if err != nil {
		return nil, err
	}
	checkpoint[IssueSyncPublicationContentBaselinesKey] = baselineJSON
	checkpointJSON, err := json.Marshal(checkpoint, json.Deterministic(true))
	if err != nil {
		return nil, err
	}
	if len(checkpointJSON) > 2<<20 {
		return nil, invalid
	}
	fields[IssueSyncProviderCheckpointKey] = checkpointJSON
	return json.Marshal(fields, json.Deterministic(true))
}

func validIssueSyncPublicationContentBaseline(externalID string, baseline IssuePublicationContentBaseline) bool {
	return externalID != "" && len(externalID) <= 512 && baseline.Hash != "" && len(baseline.Hash) <= 256 &&
		!baseline.At.IsZero() && baseline.At.Year() >= 1 && baseline.At.Year() <= 9999
}

// RefreshIssueSyncConfig stages a provider checkpoint only under a worker claim.
// Normal metadata refresh and enable preserve private state rather than accept it.
func RefreshIssueSyncConfig(previous IssueSyncBinding, p IssueSyncBindingUpdateParams) (jsontext.Value, error) {
	merged, err := PreserveIssueStatusScanConfig(previous.Config, p.Config)
	if err != nil || !p.ReplaceProviderCheckpoint {
		return merged, err
	}
	if p.StartedAt == nil || p.BindingUpdatedAt == nil {
		return nil, fmt.Errorf("%w: provider checkpoint requires a fenced worker claim", ErrImportValidation)
	}
	var next, fields map[string]jsontext.Value
	if err = json.Unmarshal(p.Config, &next); err != nil {
		return nil, err
	}
	raw, ok := next[IssueSyncProviderCheckpointKey]
	if !ok || len(raw) > 2<<20 || len(raw) == 0 || raw[0] != '{' {
		return nil, fmt.Errorf("%w: invalid provider checkpoint", ErrImportValidation)
	}
	if err = json.Unmarshal(merged, &fields); err != nil || fields == nil {
		return nil, invalidIssueStatusState()
	}
	fields[IssueSyncProviderCheckpointKey] = raw
	return json.Marshal(fields, json.Deterministic(true))
}
