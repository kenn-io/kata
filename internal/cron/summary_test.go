package cron

import (
	"encoding/json/v2"
	"fmt"
	"math"
	"regexp"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"
)

// Contract: v2 is additive evidence; the document number and old canonical bytes stay v1.
func TestSummaryV2(t *testing.T) {
	raw := `{"version":1,"reason":"agent_quota","message":"quota reached","input_tokens":12,"output_tokens":3,"usage":{"model":"model-large","cache_read_tokens":9,"cache_write_tokens":4,"cost_micros":0,"currency":"USD"},"steps":[{"key":"review","status":"failed","attempts":3,"exit_code":0,"reason":"agent_quota","started_at":"2026-11-03T09:00:00Z","ended_at":"2026-11-03T09:04:10Z","message":"429"}],"log_ref":"https://logs.daemon.example/run/1"}`
	summary, err := ParseSummary([]byte(raw))
	require.NoError(t, err)
	encoded, err := json.Marshal(summary)
	require.NoError(t, err)
	require.Equal(t, raw, string(encoded))
	for _, old := range []string{`{"version":1}`, `{"version":1,"message":"reason: timeout","input_tokens":1,"output_tokens":2}`} {
		summary, err := ParseSummary([]byte(old))
		require.NoError(t, err)
		encoded, err := json.Marshal(summary)
		require.NoError(t, err)
		require.Equal(t, old, string(encoded))
	}
}

func TestSummaryInvalidFields(t *testing.T) {
	for _, fields := range []string{
		`"reason":null`, `"reason":""`, `"reason":"A"`, `"reason":"two words"`, `"reason":"` + strings.Repeat("a", 65) + `"`,
		`"usage":null`, `"usage":{}`, `"usage":{"model":""}`, `"usage":{"model":"bad\nmodel"}`, `"usage":{"model":"` + strings.Repeat("a", 129) + `"}`,
		`"usage":{"model":"m","cache_read_tokens":-1}`, `"usage":{"model":"m","cache_write_tokens":-1}`, `"usage":{"model":"m","cost_micros":-1,"currency":"USD"}`,
		`"usage":{"model":"m","cost_micros":1}`, `"usage":{"model":"m","currency":"USD"}`, `"usage":{"model":"m","cost_micros":0,"currency":"usd"}`,
		`"usage":{"model":"m","cost_micros":1.5,"currency":"USD"}`, `"usage":{"model":"m","other":1}`,
		`"steps":null`, `"steps":[{"key":"a","status":"failed"},{"key":"a","status":"failed"}]`,
		`"steps":[{"key":"../a","status":"failed"}]`, `"steps":[{"key":"a","status":"invalid"}]`, `"steps":[{"key":"a","status":"failed","attempts":1001}]`,
		`"steps":[{"key":"a","status":"failed","attempts":-1}]`, `"steps":[{"key":"a","status":"failed","exit_code":2147483648}]`,
		`"steps":[{"key":"a","status":"failed","reason":"BAD"}]`, `"steps":[{"key":"a","status":"failed","started_at":"yesterday"}]`,
		`"steps":[{"key":"a","status":"failed","started_at":"2026-11-03T09:00:00Z","ended_at":"2026-11-03T08:00:00Z"}]`,
		`"steps":[{"key":"a","status":"failed","message":"` + strings.Repeat("x", 4097) + `"}]`,
		`"log_ref":null`, `"log_ref":""`, `"log_ref":"/logs/run"`, `"log_ref":"file:///logs/run"`, `"log_ref":"FILE:///logs/run"`,
		`"log_ref":"C:/logs/run"`, `"log_ref":"C:relative"`, `"log_ref":"C:\\logs\\run"`, `"log_ref":"\\\\host\\run"`,
		`"log_ref":"https:relative"`, `"log_ref":"https:///logs/run"`, `"log_ref":"https://user:pass@daemon.example/run"`,
		`"log_ref":"https://daemon.example/a b"`, `"log_ref":"` + strings.Repeat("x", 513) + `"`, `"other":1`,
	} {
		t.Run(fields[:min(len(fields), 90)], func(t *testing.T) {
			_, err := ParseSummary([]byte(`{"version":1,` + fields + `}`))
			require.ErrorIs(t, err, ErrInvalid)
		})
	}
}

func TestSummaryPresenceAndBounds(t *testing.T) {
	for _, fields := range []string{`"steps":[]`, `"usage":{"model":"m"}`, `"log_ref":"urn:example:run"`} {
		raw := `{"version":1,` + fields + `}`
		summary, err := ParseSummary([]byte(raw))
		require.NoError(t, err)
		encoded, err := json.Marshal(summary)
		require.NoError(t, err)
		require.Equal(t, raw, string(encoded))
	}
	steps := make([]string, 65)
	for i := range steps {
		steps[i] = fmt.Sprintf(`{"key":"step-%d","status":"skipped","attempts":1000}`, i)
	}
	_, err := ParseSummary([]byte(`{"version":1,"steps":[` + strings.Join(steps[:64], ",") + `]}`))
	require.NoError(t, err)
	_, err = ParseSummary([]byte(`{"version":1,"steps":[` + strings.Join(steps, ",") + `]}`))
	require.ErrorIs(t, err, ErrInvalid)
	_, err = ParseSummary([]byte(`{"version":1,"message":"` + strings.Repeat("x", SummaryLimit) + `"}`))
	require.ErrorIs(t, err, ErrInvalid)
}

func FuzzSummaryCanonical(f *testing.F) {
	for _, seed := range []string{`{"version":1}`, `{"version":1,"steps":[]}`, `{"version":1,"reason":"failed"}`, `{"version":1,"usage":{"model":"m","cost_micros":0,"currency":"USD"}}`} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, input []byte) {
		summary, err := ParseSummary(input)
		if err != nil {
			return
		}
		encoded, err := json.Marshal(summary)
		require.NoError(t, err)
		again, err := ParseSummary(encoded)
		require.NoError(t, err)
		require.Equal(t, summary, again)
	})
}

// The generator materializes bounded valid documents; full-domain invalid
// fields are exercised independently below and by the byte-oriented fuzzer.
func summaryPropertyText(ht *hegel.T, maxBytes int) string {
	raw := hegel.Draw(ht, hegel.Text())
	for len(raw) > maxBytes {
		_, n := utf8.DecodeLastRuneInString(raw)
		raw = raw[:len(raw)-n]
	}
	return raw
}

func summaryPropertyReason(ht *hegel.T) string {
	code := hegel.Draw(ht, hegel.Text().Alphabet("abcdefghijklmnopqrstuvwxyz0123456789_.-"))
	if code == "" {
		code = "failed"
	}
	return code[:min(len(code), 64)]
}

func TestSummaryRoundTripProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		s := Summary{Version: 1, Message: summaryPropertyText(ht, 4096), InputTokens: hegel.Draw(ht, hegel.Integers[int64](0, math.MaxInt64)), OutputTokens: hegel.Draw(ht, hegel.Integers[int64](0, math.MaxInt64))}
		if hegel.Draw(ht, hegel.Booleans()) {
			s.Reason = summaryPropertyReason(ht)
		}
		if hegel.Draw(ht, hegel.Booleans()) {
			model := strings.Map(func(r rune) rune {
				if unicode.IsControl(r) {
					return 'm'
				}
				return r
			}, summaryPropertyText(ht, 128))
			if model == "" {
				model = "m"
			}
			s.Usage = &SummaryUsage{Model: model, CacheReadTokens: hegel.Draw(ht, hegel.Integers[int64](0, math.MaxInt64)), CacheWriteTokens: hegel.Draw(ht, hegel.Integers[int64](0, math.MaxInt64))}
			if hegel.Draw(ht, hegel.Booleans()) {
				cost := hegel.Draw(ht, hegel.Integers[int64](0, math.MaxInt64))
				s.Usage.CostMicros = &cost
				s.Usage.Currency = hegel.Draw(ht, hegel.Text().Alphabet("ABCDEFGHIJKLMNOPQRSTUVWXYZ").MinSize(3).MaxSize(3))
			}
		}
		if hegel.Draw(ht, hegel.Booleans()) {
			count := min(hegel.Draw(ht, hegel.Integers[uint64](0, math.MaxUint64)), 64)
			s.Steps = make([]SummaryStep, int(count))
			for i := range s.Steps {
				step := SummaryStep{Key: fmt.Sprintf("step-%d", i), Status: hegel.Draw(ht, hegel.SampledFrom([]string{"running", "succeeded", "failed", "cancelled", "unknown", "skipped"})), Attempts: hegel.Draw(ht, hegel.Integers[int](0, 1000)), Message: summaryPropertyText(ht, 256)}
				if hegel.Draw(ht, hegel.Booleans()) {
					code := hegel.Draw(ht, hegel.Integers[int32](math.MinInt32, math.MaxInt32))
					step.ExitCode = &code
				}
				if hegel.Draw(ht, hegel.Booleans()) {
					step.Reason = summaryPropertyReason(ht)
				}
				// Equivalent instants retain their spelling, including offsets and zeros.
				step.StartedAt = hegel.Draw(ht, hegel.SampledFrom([]string{"", "2026-11-03T09:00:00Z", "2026-11-03T01:00:00.000-08:00"}))
				step.EndedAt = hegel.Draw(ht, hegel.SampledFrom([]string{"", "2026-11-03T09:00:00Z", "2026-11-03T10:00:00+01:00", "2026-11-03T09:01:00.123456789Z"}))
				s.Steps[i] = step
			}
		}
		if hegel.Draw(ht, hegel.Booleans()) {
			s.LogRef = hegel.Draw(ht, hegel.SampledFrom([]string{"urn:example:run", "https://logs.daemon.example/run/1"}))
		}
		encoded, err := json.Marshal(s)
		require.NoError(ht, err)
		parsed, err := ParseSummary(encoded)
		require.NoError(ht, err)
		require.Equal(ht, s, parsed)
		again, err := json.Marshal(parsed)
		require.NoError(ht, err)
		require.Equal(ht, encoded, again)
	}, hegel.WithTestCases(2000), hegel.WithDatabase(t.TempDir()))
}

func TestSummaryV1CompatibilityProperty(t *testing.T) {
	type oldSummary struct {
		Version      int    `json:"version"`
		Message      string `json:"message,omitempty"`
		InputTokens  int64  `json:"input_tokens,omitzero"`
		OutputTokens int64  `json:"output_tokens,omitzero"`
	}
	hegel.Test(t, func(ht *hegel.T) {
		old := oldSummary{1, summaryPropertyText(ht, 8192), hegel.Draw(ht, hegel.Integers[int64](0, math.MaxInt64)), hegel.Draw(ht, hegel.Integers[int64](0, math.MaxInt64))}
		raw, err := json.Marshal(old)
		require.NoError(ht, err)
		s, err := ParseSummary(raw)
		require.NoError(ht, err)
		require.False(ht, s.UsesV2())
		encoded, err := json.Marshal(s)
		require.NoError(ht, err)
		require.Equal(ht, raw, encoded)
	}, hegel.WithTestCases(2000), hegel.WithDatabase(t.TempDir()))
}

func TestSummaryModelValidityProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		model := hegel.Draw(ht, hegel.OneOf(hegel.Text(), hegel.SampledFrom([]string{strings.Repeat("a", 128), strings.Repeat("é", 64), strings.Repeat("é", 65), "\x00", "\x7f"})))
		raw, err := json.Marshal(map[string]any{"version": 1, "usage": map[string]any{"model": model}})
		require.NoError(ht, err)
		_, err = ParseSummary(raw)
		valid := len(model) > 0 && len(model) <= 128
		for _, r := range model {
			if unicode.IsControl(r) {
				valid = false
			}
		}
		if valid {
			require.NoError(ht, err)
		} else {
			require.ErrorIs(ht, err, ErrInvalid)
		}
	}, hegel.WithTestCases(2000), hegel.WithDatabase(t.TempDir()))
}

func TestSummaryReasonProperty(t *testing.T) {
	grammar := regexp.MustCompile(`^[a-z0-9_.-]{1,64}$`)
	hegel.Test(t, func(ht *hegel.T) {
		reason := hegel.Draw(ht, hegel.OneOf(hegel.Text(), hegel.SampledFrom([]string{"failed", "timeout", "kata.stale", "needs-human", "", "bad\r", "long code"})))
		message := hegel.Draw(ht, hegel.OneOf(hegel.Text(), hegel.SampledFrom([]string{"reason: failed", "reason: timeout\nmore", "reason: timeout\r\nmore", "prefix\nreason: failed"})))
		s := Summary{Version: 1, Reason: reason, Message: message}
		want := reason
		valid := grammar.MatchString(want)
		if reason == "" {
			line, _, _ := strings.Cut(message, "\n")
			code, prefix := strings.CutPrefix(line, "reason: ")
			want, valid = code, prefix && grammar.MatchString(code)
		}
		got, ok := RunReason(s)
		require.Equal(ht, valid, ok)
		if ok {
			require.Equal(ht, want, got)
		}
	}, hegel.WithTestCases(2000), hegel.WithDatabase(t.TempDir()))
}

func TestSummaryUsesV2Presence(t *testing.T) {
	for _, raw := range []string{`{"version":1,"steps":[]}`, `{"version":1,"log_ref":"urn:example:run"}`, `{"version":1,"usage":{"model":"m"}}`} {
		s, err := ParseSummary([]byte(raw))
		require.NoError(t, err)
		require.True(t, s.UsesV2())
	}
	require.False(t, (Summary{Version: 1, Message: "reason: failed"}).UsesV2())
}

func TestSummaryReasonGrammarProperty(t *testing.T) {
	grammar := regexp.MustCompile(`^[a-z0-9_.-]{1,64}$`)
	hegel.Test(t, func(ht *hegel.T) {
		code := hegel.Draw(ht, hegel.OneOf(hegel.Text(), hegel.Text().Alphabet("abcdefghijklmnopqrstuvwxyz0123456789_.-")))
		require.Equal(ht, grammar.MatchString(code), ValidReason(code))
	}, hegel.WithTestCases(2000), hegel.WithDatabase(t.TempDir()))
}

func TestSummaryRejectsNegativeUsageProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		negative := hegel.Draw(ht, hegel.Integers[int64](math.MinInt64, -1))
		field := hegel.Draw(ht, hegel.SampledFrom([]string{"cache_read_tokens", "cache_write_tokens", "cost_micros"}))
		usage := map[string]any{"model": "model-large", field: negative}
		if field == "cost_micros" {
			usage["currency"] = "USD"
		}
		raw, err := json.Marshal(map[string]any{"version": 1, "usage": usage})
		require.NoError(ht, err)
		_, err = ParseSummary(raw)
		require.ErrorIs(ht, err, ErrInvalid)
	}, hegel.WithTestCases(2000), hegel.WithDatabase(t.TempDir()))
}
