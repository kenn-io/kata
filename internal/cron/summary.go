package cron

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"net/url"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// HLC identifies the winning definition independently of any later baseline.
type HLC struct {
	Version           int    `json:"version"`
	PhysicalMS        int64  `json:"physical_ms"`
	Counter           int64  `json:"counter"`
	OriginInstanceUID string `json:"origin_instance_uid"`
}

// Validate checks the portable clock version, counters and origin identity.
func (h HLC) Validate() error {
	if h.Version != 1 || h.PhysicalMS < 0 || h.Counter < 0 || !validUID(h.OriginInstanceUID) {
		return invalid("invalid definition HLC")
	}
	return nil
}

// Summary is bounded shared evidence, excluding local paths and raw logs.
type Summary struct {
	Version      int           `json:"version"`
	Reason       string        `json:"reason,omitempty"`
	Trigger      string        `json:"trigger,omitempty"`
	Message      string        `json:"message,omitempty"`
	InputTokens  int64         `json:"input_tokens,omitzero"`
	OutputTokens int64         `json:"output_tokens,omitzero"`
	Usage        *SummaryUsage `json:"usage,omitempty"`
	Steps        []SummaryStep `json:"steps,omitzero"`
	LogRef       string        `json:"log_ref,omitempty"`
}

// SummaryUsage records adapter-reported usage; nil cost means unknown, not free.
type SummaryUsage struct {
	Model            string `json:"model"`
	CacheReadTokens  int64  `json:"cache_read_tokens,omitzero"`
	CacheWriteTokens int64  `json:"cache_write_tokens,omitzero"`
	CostMicros       *int64 `json:"cost_micros,omitempty"`
	Currency         string `json:"currency,omitempty"`
}

// SummaryStep is ordered run evidence, independent of the current workflow definition.
// Timestamps remain strings so timezone spelling survives replay unchanged.
type SummaryStep struct {
	Key       string `json:"key"`
	Status    string `json:"status"`
	Attempts  int    `json:"attempts,omitzero"`
	ExitCode  *int32 `json:"exit_code,omitempty"`
	Reason    string `json:"reason,omitempty"`
	StartedAt string `json:"started_at,omitempty"`
	EndedAt   string `json:"ended_at,omitempty"`
	Message   string `json:"message,omitempty"`
}

// UsesV2 identifies the optional structured capability, not a document version.
func (s Summary) UsesV2() bool {
	return s.Trigger != "" || s.Reason != "" || s.Usage != nil || s.Steps != nil || s.LogRef != ""
}

// ValidReason accepts adapter-defined codes and the reserved kata.* namespace.
func ValidReason(code string) bool {
	if len(code) < 1 || len(code) > 64 {
		return false
	}
	for _, c := range code {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || strings.ContainsRune("_.-", c)) {
			return false
		}
	}
	return true
}

// RunReason prefers structured evidence, then an exact LF-delimited legacy first line.
func RunReason(s Summary) (string, bool) {
	if s.Reason != "" {
		return s.Reason, ValidReason(s.Reason)
	}
	line, _, _ := strings.Cut(s.Message, "\n")
	code, ok := strings.CutPrefix(line, "reason: ")
	return code, ok && ValidReason(code)
}

// UnmarshalJSON enforces the same contract for API, import and federation decoding.
// Alias decoding avoids recursion and preserves all v1 acceptance/encoding rules.
func (s *Summary) UnmarshalJSON(input []byte) error {
	type wire Summary
	var value wire
	if err := decode(input, &value, SummaryLimit); err != nil {
		return err
	}
	var fields map[string]jsontext.Value
	if err := json.Unmarshal(input, &fields); err != nil {
		return invalid("invalid run summary")
	}
	for _, key := range []string{"reason", "trigger", "usage", "steps", "log_ref"} {
		if raw, exists := fields[key]; exists && (string(raw) == "null" || (key == "reason" && value.Reason == "") || (key == "trigger" && value.Trigger == "") || (key == "log_ref" && value.LogRef == "")) {
			return invalid("structured summary fields must be non-null and nonempty")
		}
	}
	result := Summary(value)
	if err := result.validate(); err != nil {
		return err
	}
	*s = result
	return nil
}

// ParseSummary decodes and validates one bounded observation summary.
func ParseSummary(input []byte) (Summary, error) {
	var value Summary
	if err := decode(input, &value, SummaryLimit); err != nil {
		return value, err
	}
	return value, nil
}

func (s Summary) validate() error {
	if s.Version != 1 || s.InputTokens < 0 || s.OutputTokens < 0 {
		return invalid("invalid run summary")
	}
	if s.Reason != "" && !ValidReason(s.Reason) {
		return invalid("invalid summary reason")
	}
	switch s.Trigger {
	case "", "scheduled", "catchup", "request":
	default:
		return invalid("invalid summary trigger")
	}
	if u := s.Usage; u != nil {
		if len(u.Model) == 0 || len(u.Model) > 128 || !utf8.ValidString(u.Model) || strings.ContainsFunc(u.Model, unicode.IsControl) || u.CacheReadTokens < 0 || u.CacheWriteTokens < 0 {
			return invalid("invalid summary usage")
		}
		if (u.CostMicros == nil) != (u.Currency == "") || u.CostMicros != nil && (*u.CostMicros < 0 || len(u.Currency) != 3 || strings.ContainsFunc(u.Currency, func(r rune) bool { return r < 'A' || r > 'Z' })) {
			return invalid("cost requires nonnegative micros and a three-letter uppercase currency")
		}
	}
	if len(s.Steps) > 64 {
		return invalid("summary exceeds 64 steps")
	}
	seen := map[string]bool{}
	for _, step := range s.Steps {
		if !portableKey(step.Key) || seen[step.Key] || step.Attempts < 0 || step.Attempts > 1000 || len(step.Message) > 4096 || !utf8.ValidString(step.Message) || step.Reason != "" && !ValidReason(step.Reason) {
			return invalid("invalid summary step")
		}
		seen[step.Key] = true
		switch step.Status {
		case "running", "succeeded", "failed", "cancelled", "unknown", "skipped":
		default:
			return invalid("invalid step status")
		}
		var start, end time.Time
		for i, raw := range []string{step.StartedAt, step.EndedAt} {
			if raw == "" {
				continue
			}
			at, err := time.Parse(time.RFC3339Nano, raw)
			if err != nil {
				return invalid("invalid step timestamp")
			}
			if i == 0 {
				start = at
			} else {
				end = at
			}
		}
		if step.StartedAt != "" && step.EndedAt != "" && end.Before(start) {
			return invalid("step end precedes start")
		}
	}
	if s.LogRef != "" && !validLogRef(s.LogRef) {
		return invalid("log_ref must be an absolute non-file URI without credentials or local paths")
	}
	return nil
}

func validLogRef(value string) bool {
	if len(value) > 512 || !utf8.ValidString(value) || strings.ContainsRune(value, '\\') || strings.ContainsFunc(value, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) {
		return false
	}
	// net/url treats Windows drive paths as one-letter schemes, on every OS.
	if len(value) >= 2 && value[1] == ':' && (value[0] >= 'a' && value[0] <= 'z' || value[0] >= 'A' && value[0] <= 'Z') {
		return false
	}
	u, err := url.Parse(value)
	if err != nil || !u.IsAbs() || strings.EqualFold(u.Scheme, "file") || u.User != nil {
		return false
	}
	return !(strings.EqualFold(u.Scheme, "http") || strings.EqualFold(u.Scheme, "https")) || u.Hostname() != ""
}

// Shared drop reasons describe an occurrence that was not launched. The adapter
// chooses the run status and whether one observation groups several drops.
const (
	ReasonOverlapSkipped   = "overlap_skipped"
	ReasonCatchupMissed    = "catchup_missed"
	ReasonPausedPassedOver = "paused_passed_over"
)

// DropReason reports exact membership in the shared drop reason registry.
func DropReason(code string) bool {
	switch code {
	case ReasonOverlapSkipped, ReasonCatchupMissed, ReasonPausedPassedOver:
		return true
	default:
		return false
	}
}

// RunTrigger prefers explicit evidence. Derived values are display hints, not
// schedule eligibility: lateness beyond two minutes is shown as catchup.
func RunTrigger(s Summary, occurrenceKey *string, startedAt *time.Time) (trigger string, derived bool) {
	if s.Trigger != "" {
		return s.Trigger, false
	}
	if occurrenceKey == nil {
		return "", false
	}
	for _, prefix := range []string{"request:", "manual:"} {
		if rest, ok := strings.CutPrefix(*occurrenceKey, prefix); ok && rest != "" {
			return "request", true
		}
	}
	if startedAt != nil {
		if at, err := time.Parse(time.RFC3339Nano, *occurrenceKey); err == nil {
			if startedAt.Sub(at) > 120*time.Second {
				return "catchup", true
			}
			return "scheduled", true
		}
	}
	return "", false
}
