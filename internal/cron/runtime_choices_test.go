package cron

import (
	"encoding/json/v2"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"
)

// Contract: choice values are exact strings in a bounded, closed domain.
func TestRuntimeChoiceDomain(t *testing.T) {
	hegel.Test(t, func(h *hegel.T) {
		value := hegel.Draw(h, hegel.Text())
		key := hegel.Draw(h, hegel.SampledFrom([]string{"checkout_key", "options.agent", "options.model", "other"}))
		want := key != "other" && len(value) > 0 && len(value) <= 128 && utf8.ValidString(value) && strings.TrimSpace(value) != ""
		for _, r := range value {
			if unicode.IsControl(r) {
				want = false
			}
			if key == "checkout_key" && !unicode.IsLetter(r) && !unicode.IsDigit(r) && !strings.ContainsRune("-_.", r) {
				want = false
			}
		}
		if key == "checkout_key" && (value == "." || value == "..") {
			want = false
		}
		b := runtimeFixture().RuntimeBeat
		b.Choices = map[string][]string{key: {value}}
		_, err := NormalizeRuntimeBeat(b)
		if (err == nil) != want {
			h.Fatalf("%q %q valid=%v err=%v", key, value, want, err)
		}
	}, hegel.WithTestCases(2000), hegel.WithDatabase(t.TempDir()))
	for _, choices := range []map[string][]string{{"options.agent": nil}, {"options.agent": {"a", "a"}}, {"options.agent": make([]string, 33)}, {"checkout_key": {"/bad"}}} {
		b := runtimeFixture().RuntimeBeat
		b.Choices = choices
		_, err := NormalizeRuntimeBeat(b)
		require.Error(t, err)
	}
}
func TestRuntimeChoicesOracle(t *testing.T) {
	for _, property := range []string{"union", "warning", "permutation"} {
		t.Run(property, func(t *testing.T) {
			hegel.Test(t, func(h *hegel.T) {
				key := hegel.Draw(h, hegel.SampledFrom(runtimeChoiceKeys))
				n := hegel.Draw(h, hegel.Integers(0, 15))
				entries := []Runtime{}
				want := map[string][]string{}
				ready := hegel.Draw(h, hegel.Booleans())
				enabled := hegel.Draw(h, hegel.Booleans())
				execute := hegel.Draw(h, hegel.Booleans())
				def := JobDefinition{Enabled: enabled, Action: Action{Kind: "execute"}, CheckoutKey: "missing", Options: []byte(`{"executor":"worker","adapter":"example","agent":"missing","model":"missing"}`)}
				if !execute {
					def.Action.Kind = "notify"
				}
				for i := range n {
					r := runtimeFixture()
					r.UID = fmt.Sprintf("%02d", i)
					r.Status = hegel.Draw(h, hegel.SampledFrom([]string{"live", "expired"}))
					r.Label = hegel.Draw(h, hegel.SampledFrom([]string{"worker", "other"}))
					r.Adapter = hegel.Draw(h, hegel.SampledFrom([]string{"example", "another"}))
					if r.Status == "expired" {
						r.ExpiresAt = r.LastSeenAt
					}
					if hegel.Draw(h, hegel.Booleans()) {
						r.Choices = map[string][]string{key: {}}
						if hegel.Draw(h, hegel.Booleans()) {
							r.Choices[key] = []string{hegel.Draw(h, hegel.SampledFrom([]string{"present", "missing"}))}
						}
						if r.Status == "live" && r.Label == "worker" && r.Adapter == "example" {
							if _, ok := want[key]; !ok {
								want[key] = []string{}
							}
							want[key] = append(want[key], r.Choices[key]...)
						}
					}
					entries = append(entries, r)
				}
				for k, v := range want {
					slices.Sort(v)
					want[k] = slices.Compact(v)
				}
				got := RuntimeChoices(entries, def)
				if property == "union" && !reflect.DeepEqual(want, got) {
					h.Fatalf("union got=%v want=%v", got, want)
				}
				now := runtimeFixture().LastSeenAt
				start := now
				if ready {
					start = now.Add(-4 * time.Hour)
				}
				inv := EvaluateRuntimes(entries, []RuntimeJob{{UID: "job", Definition: def}}, now, "dev", start)
				values, known := want[key]
				expected := ready && len(entries) > 0 && enabled && execute && known && !slices.Contains(values, "missing")
				actual := false
				for _, w := range inv.Warnings {
					if w.Code == "choice_unknown" {
						actual = true
						if w.Choice != key || w.Value != "missing" || w.JobUID != "job" {
							h.Fatal("incorrect choice diagnostic")
						}
					}
				}
				if property == "warning" && actual != expected {
					h.Fatalf("warning got=%v want=%v", actual, expected)
				}
				slices.Reverse(entries)
				other := EvaluateRuntimes(entries, []RuntimeJob{{UID: "job", Definition: def}}, now, "dev", start)
				a, _ := json.Marshal(inv)
				b, _ := json.Marshal(other)
				if property == "permutation" && string(a) != string(b) {
					h.Fatal("permutation changes result")
				}
			}, hegel.WithTestCases(1500), hegel.WithDatabase(t.TempDir()))
		})
	}
}
func TestRuntimeChoicesOwnershipAndExactness(t *testing.T) {
	b := runtimeFixture().RuntimeBeat
	b.Choices = map[string][]string{"options.agent": {"z", "a"}, "checkout_key": {}}
	normalized, err := NormalizeRuntimeBeat(b)
	require.NoError(t, err)
	b.Choices["options.agent"][0] = "mutated"
	delete(b.Choices, "checkout_key")
	require.Equal(t, []string{"a", "z"}, normalized.Choices["options.agent"])
	require.Contains(t, normalized.Choices, "checkout_key")
	r := runtimeFixture()
	r.RuntimeBeat = normalized
	r.Status = "live"
	inv := EvaluateRuntimes([]Runtime{r}, nil, r.LastSeenAt, "dev", r.LastSeenAt)
	inv.Runtimes[0].Choices["options.agent"][0] = "mutated"
	require.Equal(t, []string{"a", "z"}, r.Choices["options.agent"])
	union := RuntimeChoices([]Runtime{r}, JobDefinition{})
	union["options.agent"][0] = "mutated"
	require.Equal(t, []string{"a", "z"}, r.Choices["options.agent"])
	require.True(t, RuntimeMatches(r, "worker", "example"))
	require.False(t, RuntimeMatches(r, " worker", "example"))
	require.False(t, RuntimeMatches(r, "worker", "other"))
}

func TestRuntimeChoicesNormalizeOrder(t *testing.T) {
	hegel.Test(t, func(h *hegel.T) {
		b := runtimeFixture().RuntimeBeat
		n := hegel.Draw(h, hegel.Integers(0, 32))
		values := []string{}
		for i := range n {
			values = append(values, strings.Repeat("ä", hegel.Draw(h, hegel.Integers(1, 60)))+fmt.Sprintf("%02d", i))
		}
		key := hegel.Draw(h, hegel.SampledFrom(runtimeChoiceKeys))
		b.Choices = map[string][]string{key: values}
		first, err := NormalizeRuntimeBeat(b)
		if err != nil {
			h.Fatal(err)
		}
		slices.Reverse(values)
		second, err := NormalizeRuntimeBeat(b)
		if err != nil || !reflect.DeepEqual(first, second) {
			h.Fatal("normalization depends on input order")
		}
		again, err := NormalizeRuntimeBeat(first)
		if err != nil || !reflect.DeepEqual(first, again) {
			h.Fatal("normalization not idempotent")
		}
	}, hegel.WithTestCases(1000), hegel.WithDatabase(t.TempDir()))
	for _, value := range []string{strings.Repeat("x", 128), strings.Repeat("ä", 64), "日本語"} {
		b := runtimeFixture().RuntimeBeat
		b.Choices = map[string][]string{"checkout_key": {value}}
		_, err := NormalizeRuntimeBeat(b)
		require.NoError(t, err)
	}
	for _, value := range []string{strings.Repeat("x", 129), string([]byte{255}), "x\x7fy", "x\u0085y"} {
		b := runtimeFixture().RuntimeBeat
		b.Choices = map[string][]string{"options.agent": {value}}
		_, err := NormalizeRuntimeBeat(b)
		require.Error(t, err)
	}
}

func TestRuntimeChoiceWarningValueDomain(t *testing.T) {
	hegel.Test(t, func(h *hegel.T) {
		text := hegel.Draw(h, hegel.Text())
		kind := hegel.Draw(h, hegel.Integers(0, 5))
		var value any = text
		switch kind {
		case 1:
			value = nil
		case 2:
			value = 42
		case 3:
			value = true
		case 4:
			value = []string{text}
		case 5:
			value = map[string]string{"nested": text}
		}
		raw, err := json.Marshal(map[string]any{"agent": value})
		if err != nil {
			h.Fatal(err)
		}
		r := runtimeFixture()
		r.Choices = map[string][]string{"options.agent": {"present"}}
		job := RuntimeJob{UID: "job", Definition: JobDefinition{Enabled: true, Action: Action{Kind: "execute"}, Options: raw}}
		inv := EvaluateRuntimes([]Runtime{r}, []RuntimeJob{job}, r.LastSeenAt, "dev", r.LastSeenAt.Add(-4*time.Hour))
		want := kind == 0 && strings.TrimSpace(text) != "" && text != "present"
		if slices.Contains(runtimeWarningCodes(inv), "choice_unknown") != want {
			h.Fatal("choice warning coerced or lost a job value")
		}
	}, hegel.WithTestCases(1500), hegel.WithDatabase(t.TempDir()))
}
