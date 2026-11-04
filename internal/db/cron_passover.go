package db

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"strings"
	"time"

	"go.kenn.io/kata/internal/cron"
)

// CronPassoverMetadataKey stores the root-owned project cutoff.
const CronPassoverMetadataKey = "cron.passover"

// CronPassoverMetadataPrefix reserves root-owned per-job cutoffs.
const CronPassoverMetadataPrefix = "cron.passover."

// CronPassover is a complete root-owned scheduled-delivery cutoff.
type CronPassover struct {
	Version  int        `json:"version"`
	Through  time.Time  `json:"through"`
	By       string     `json:"by"`
	PausedAt *time.Time `json:"paused_at,omitempty"`
}

// CronPassoverState is derived outside the portable job definition. Invalid
// applicable metadata blocks scheduling rather than silently losing a cutoff.
type CronPassoverState struct {
	Through time.Time `json:"through,omitzero"`
	Scope   string    `json:"scope,omitempty"`
	Invalid bool      `json:"invalid,omitempty"`
}

func cronPassoverKey(key string) bool {
	return key == CronPassoverMetadataKey || strings.HasPrefix(key, CronPassoverMetadataPrefix)
}
func validCronPassoverTime(t time.Time) bool {
	return !t.IsZero() && cronUTCOffset(t) == 0 && t.Year() >= 1 && t.Year() <= 9999
}

// cronUTCOffset lets RFC3339 "+00:00" name UTC like "Z"; other offsets fail.
func cronUTCOffset(t time.Time) int {
	_, offset := t.Zone()
	return offset
}
func decodeCronPassover(raw jsontext.Value) (CronPassover, error) {
	invalid := func() (CronPassover, error) {
		return CronPassover{}, fmt.Errorf("%w: invalid cron passover value", cron.ErrInvalid)
	}
	var fields map[string]jsontext.Value
	var value CronPassover
	if err := json.Unmarshal(raw, &fields); err != nil || (len(fields) != 3 && len(fields) != 4) {
		return invalid()
	}
	if fields["version"].Kind() != jsontext.KindNumber || fields["through"].Kind() != jsontext.KindString || fields["by"].Kind() != jsontext.KindString {
		return invalid()
	}
	if paused, ok := fields["paused_at"]; ok && paused.Kind() != jsontext.KindString {
		return invalid()
	}
	if err := json.Unmarshal(raw, &value, json.RejectUnknownMembers(true)); err != nil {
		return invalid()
	}
	if value.Version != 1 || !validCronPassoverTime(value.Through) || strings.TrimSpace(value.By) == "" || len(value.By) > 256 || (value.PausedAt != nil && !validCronPassoverTime(*value.PausedAt)) {
		return invalid()
	}
	return value, nil
}

// CronPassoverFromMetadata selects the later applicable cutoff. A job cutoff
// wins an equal instant's scope; an invalid applicable value always blocks.
func CronPassoverFromMetadata(blob JSONBlob, jobUID string) *CronPassoverState {
	if blob == "" {
		return nil
	}
	var values map[string]jsontext.Value
	if err := json.Unmarshal([]byte(blob), &values); err != nil {
		return &CronPassoverState{Invalid: true}
	}
	var state *CronPassoverState
	for _, item := range []struct{ key, scope string }{{CronPassoverMetadataKey, "project"}, {CronPassoverMetadataPrefix + jobUID, "job"}} {
		raw, ok := values[item.key]
		if !ok || raw.Kind() == jsontext.KindNull {
			continue
		}
		v, e := decodeCronPassover(raw)
		if e != nil {
			return &CronPassoverState{Invalid: true}
		}
		if state == nil || !v.Through.Before(state.Through) {
			state = &CronPassoverState{Through: v.Through.UTC(), Scope: item.scope}
		}
	}
	return state
}

// ValidateCronPassoverChange enforces complete, nondecreasing values without a
// receiver clock. Missing/malformed old values allow root baseline and repair.
func ValidateCronPassoverChange(key string, from, to jsontext.Value) error {
	if !cronPassoverKey(key) || (key != CronPassoverMetadataKey && !cronUID(strings.TrimPrefix(key, CronPassoverMetadataPrefix))) {
		return fmt.Errorf("%w: invalid cron passover key", cron.ErrInvalid)
	}
	next, err := decodeCronPassover(to)
	if err != nil {
		return err
	}
	previous, err := decodeCronPassover(from)
	if err == nil && next.Through.Before(previous.Through) {
		return fmt.Errorf("%w: cron passover cannot decrease", cron.ErrInvalid)
	}
	return nil
}

// FoldCronPassover selects the complete maximum value, with canonical JSON
// byte order breaking equal-time ties. Attribution never merges recursively.
func FoldCronPassover(current, next jsontext.Value) (jsontext.Value, error) {
	n, err := decodeCronPassover(next)
	if err != nil {
		return nil, err
	}
	p, err := decodeCronPassover(current)
	if err != nil {
		return canonicalJSON(next), nil
	}
	old, newValue := canonicalJSON(current), canonicalJSON(next)
	if p.Through.After(n.Through) || (p.Through.Equal(n.Through) && bytes.Compare(old, newValue) >= 0) {
		return old, nil
	}
	return newValue, nil
}

func applyCronProjectMetadataDiff(current, diff jsontext.Value) jsontext.Value {
	// Ordinary keys keep their established recursive merge behavior.
	next := applyMetadataDiff(current, diff)
	var before, after map[string]jsontext.Value
	_ = json.Unmarshal(current, &before)
	_ = json.Unmarshal(next, &after)
	if after == nil {
		after = map[string]jsontext.Value{}
	}
	var changes map[string]struct {
		To jsontext.Value `json:"to"`
	}
	_ = json.Unmarshal(diff, &changes)
	for key, change := range changes {
		if !cronPassoverKey(key) {
			continue
		}
		value, err := FoldCronPassover(before[key], change.To)
		if err != nil {
			// Old malformed state must remain visibly blocked. Rejected deletions
			// cannot erase a valid retained cutoff during replay of legacy history.
			if len(change.To) == 0 || change.To.Kind() == jsontext.KindNull {
				value = before[key]
				if len(value) == 0 {
					delete(after, key)
					continue
				}
			} else {
				value = canonicalJSON(change.To)
			}
		}
		after[key] = value
	}
	raw, err := json.Marshal(after, json.Deterministic(true))
	if err != nil {
		return next
	}
	return raw
}

func applyCronProjectMetadataBaseline(current, baseline jsontext.Value) jsontext.Value {
	var before, next map[string]jsontext.Value
	_ = json.Unmarshal(current, &before)
	_ = json.Unmarshal(baseline, &next)
	if next == nil {
		next = map[string]jsontext.Value{}
	}
	for key, value := range before {
		if !cronPassoverKey(key) {
			continue
		}
		if candidate, ok := next[key]; ok {
			if folded, err := FoldCronPassover(value, candidate); err == nil {
				next[key] = folded
			}
		} else {
			next[key] = value
		}
	}
	raw, err := json.Marshal(next, json.Deterministic(true))
	if err != nil {
		return canonicalJSON(baseline)
	}
	return raw
}
