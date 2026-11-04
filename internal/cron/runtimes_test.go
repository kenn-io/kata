package cron

import (
	"encoding/json/v2"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"
)

func runtimeFixture() Runtime {
	at := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	return Runtime{Label: "worker", Adapter: "example", AdapterVersion: "1.0.0", KataVersion: "0.31.0", IntervalSeconds: 60, State: "active", Features: []string{}, UID: "01ARZ3NDEKTSV4RRFFQ69G5FAV", Actor: "operator", FirstSeenAt: at, LastSeenAt: at, ExpiresAt: at.Add(180 * time.Second), ReportedVia: "direct"}
}

func runtimeJobFixture(t *testing.T, options []byte) RuntimeJob {
	t.Helper()
	definition := adapterJob(t, Trigger{Kind: "manual"})
	definition.Options = options
	require.NoError(t, definition.Validate())
	return RuntimeJob{UID: "01ARZ3NDEKTSV4RRFFQ69G5FAY", Definition: definition}
}

func runtimeTestUID(index int) string {
	return fmt.Sprintf("%026d", index)
}

// Contract: liveness is a half-open interval.
func TestRuntimeStatusBoundaries(t *testing.T) {
	r := runtimeFixture()
	require.Equal(t, "live", RuntimeStatus(r, r.ExpiresAt.Add(-time.Nanosecond)))
	require.Equal(t, "expired", RuntimeStatus(r, r.ExpiresAt))
}
func TestRuntimeInventoryState(t *testing.T) {
	r := runtimeFixture()
	start := r.LastSeenAt
	now := start.Add(3 * time.Hour)
	require.Equal(t, "unknown", EvaluateRuntimes(nil, nil, now, "0.31.0", start).Inventory.State)
	require.Equal(t, "warming", EvaluateRuntimes([]Runtime{r}, nil, now.Add(-time.Nanosecond), "0.31.0", start).Inventory.State)
	require.Equal(t, "ready", EvaluateRuntimes([]Runtime{r}, nil, now, "0.31.0", start).Inventory.State)
	slow := r
	slow.UID = "01ARZ3NDEKTSV4RRFFQ69G5FAW"
	slow.IntervalSeconds = 3600
	require.Equal(t, "ready", EvaluateRuntimes([]Runtime{r, slow}, nil, now, "0.31.0", start).Inventory.State)
}
func runtimeWarningCodes(out RuntimeInventory) []string {
	result := []string{}
	for _, w := range out.Warnings {
		result = append(result, w.Code)
	}
	return result
}
func TestRuntimePermutationInvariant(t *testing.T) {
	hegel.Test(t, func(h *hegel.T) {
		entries := []Runtime{}
		n := hegel.Draw(h, hegel.Integers(0, 30))
		for i := range n {
			r := runtimeFixture()
			r.UID = runtimeTestUID(i)
			r.Label = hegel.Draw(h, hegel.SampledFrom([]string{"worker", "another"}))
			r.Adapter = hegel.Draw(h, hegel.SampledFrom([]string{"alpha", "beta"}))
			r.AdapterVersion = hegel.Draw(h, hegel.SampledFrom([]string{"", "1.0.0", "2.0.0"}))
			entries = append(entries, r)
		}
		now := runtimeFixture().LastSeenAt
		start := now.Add(-4 * time.Hour)
		one := EvaluateRuntimes(entries, nil, now, "0.32.0", start)
		slices.Reverse(entries)
		two := EvaluateRuntimes(entries, nil, now, "0.32.0", start)
		a, _ := json.Marshal(one)
		b, _ := json.Marshal(two)
		if string(a) != string(b) {
			h.Fatalf("order-dependent output: %s vs %s", a, b)
		}
	}, hegel.WithTestCases(2000), hegel.WithDatabase(t.TempDir()))
}
func TestRuntimeValidationDomains(t *testing.T) {
	hegel.Test(t, func(h *hegel.T) {
		b := runtimeFixture().RuntimeBeat
		b.Label = hegel.Draw(h, hegel.Text())
		n := hegel.Draw(h, hegel.Integers(-100, 4000))
		b.IntervalSeconds = n
		_, err := NormalizeRuntimeBeat(b)
		want := len(b.Label) > 0 && len(b.Label) <= 256 && strings.TrimSpace(b.Label) != "" && (n == 0 || n >= 10 && n <= 3600)
		if (err == nil) != want {
			h.Fatalf("validation %v want valid=%v label=%q interval=%d", err, want, b.Label, n)
		}
	}, hegel.WithTestCases(2000), hegel.WithDatabase(t.TempDir()))
	for _, bad := range []RuntimeBeat{{Label: "worker", Adapter: "example", Features: []string{"same", "same"}}, {Label: string([]byte{255}), Adapter: "example"}, {Label: "worker", Adapter: "example", State: "idle"}} {
		_, err := NormalizeRuntimeBeat(bad)
		require.Error(t, err)
	}
	b, err := NormalizeRuntimeBeat(RuntimeBeat{Label: "worker", Adapter: "example"})
	require.NoError(t, err)
	require.Equal(t, 60, b.IntervalSeconds)
	require.Equal(t, "active", b.State)
	require.NotNil(t, b.Features)
}

// Contract: all selector components match conjunctively and labels stay exact.
func TestRuntimeSelectorsConjunctive(t *testing.T) {
	hegel.Test(t, func(h *hegel.T) {
		r := runtimeFixture()
		r.Label = fmt.Sprintf("worker-%d", hegel.Draw(h, hegel.Integers[int64](0, 1<<40)))
		r.Adapter = fmt.Sprintf("adapter-%d", hegel.Draw(h, hegel.Integers[int64](0, 1<<40)))
		// Padded selectors remain exact mismatches, rather than trimmed matches.
		pinMode := hegel.Draw(h, hegel.Integers(0, 4))
		familyMode := hegel.Draw(h, hegel.Integers(0, 4))
		pin := []string{"", " ", r.Label, r.Label + "-other", " " + r.Label + " "}[pinMode]
		family := []string{"", " ", r.Adapter, r.Adapter + "-other", " " + r.Adapter + " "}[familyMode]
		ready := hegel.Draw(h, hegel.Booleans())
		live := hegel.Draw(h, hegel.Booleans())
		now := r.LastSeenAt
		start := now
		if ready {
			start = now.Add(-4 * time.Hour)
		}
		if !live {
			r.ExpiresAt = now
		}
		raw, err := json.Marshal(map[string]string{"executor": pin, "adapter": family})
		if err != nil {
			h.Fatal(err)
		}
		job := runtimeJobFixture(t, raw)
		job.Definition.Enabled = hegel.Draw(h, hegel.Booleans())
		out := EvaluateRuntimes([]Runtime{r}, []RuntimeJob{job}, now, "dev", start)
		want := []string{}
		if ready && job.Definition.Enabled {
			unmatched := !live || pinMode >= 3 || familyMode >= 3
			if pinMode >= 2 && unmatched {
				want = append(want, "pin_unmatched")
			} else if pinMode < 2 && familyMode >= 2 && unmatched {
				want = append(want, "adapter_unmatched")
			}
			if !live {
				want = append(want, "no_live_runtime")
			}
		}
		slices.Sort(want)
		if got := runtimeWarningCodes(out); !slices.Equal(got, want) {
			h.Fatalf("warning codes %v want %v, pin mode=%d adapter mode=%d ready=%v live=%v enabled=%v", got, want, pinMode, familyMode, ready, live, job.Definition.Enabled)
		}
		for _, warning := range out.Warnings {
			if warning.Code != "no_live_runtime" && warning.JobUID != job.UID {
				h.Fatal("selector warning lost its job identity")
			}
		}
	}, hegel.WithTestCases(2000), hegel.WithDatabase(t.TempDir()))
}
func TestRuntimeVersionSkewReportedLiveOnly(t *testing.T) {
	hegel.Test(t, func(h *hegel.T) {
		a, b := runtimeFixture(), runtimeFixture()
		a.UID, b.UID = runtimeTestUID(1), runtimeTestUID(2)
		version := fmt.Sprintf("%d.%d.%d", hegel.Draw(h, hegel.Integers(0, 1000)), hegel.Draw(h, hegel.Integers(0, 1000)), hegel.Draw(h, hegel.Integers(0, 1000)))
		a.AdapterVersion = version
		mode := hegel.Draw(h, hegel.Integers(0, 2))
		b.AdapterVersion = []string{"", version, version + "+other"}[mode]
		live := hegel.Draw(h, hegel.Booleans())
		sameAdapter := hegel.Draw(h, hegel.Booleans())
		if !sameAdapter {
			b.Adapter = "another"
		}
		now := a.LastSeenAt
		if !live {
			b.ExpiresAt = now
		}
		out := EvaluateRuntimes([]Runtime{b, a}, nil, now, "dev", now)
		want := live && sameAdapter && mode == 2
		got := slices.Contains(runtimeWarningCodes(out), "adapter_version_skew")
		if got != want {
			h.Fatalf("skew=%v want %v, version mode=%d live=%v same adapter=%v", got, want, mode, live, sameAdapter)
		}
		if got {
			if !slices.Equal([]string{version, version + "+other"}, out.Warnings[0].Versions) {
				h.Fatal("skew versions not sorted and complete")
			}
		}
	}, hegel.WithTestCases(2000), hegel.WithDatabase(t.TempDir()))
}
func TestRuntimeNonStringAndBlankSelectorsIgnored(t *testing.T) {
	r := runtimeFixture()
	now := r.LastSeenAt
	for _, raw := range []string{`{"executor":42,"adapter":true}`, `{"executor":" ","adapter":null}`} {
		job := runtimeJobFixture(t, []byte(raw))
		require.Empty(t, EvaluateRuntimes([]Runtime{r}, []RuntimeJob{job}, now, "dev", now.Add(-4*time.Hour)).Warnings)
	}
}

func TestRuntimeNormalizationOwnsSlices(t *testing.T) {
	hegel.Test(t, func(h *hegel.T) {
		b := runtimeFixture().RuntimeBeat
		b.Features = hegel.Draw(h, hegel.UniqueLists(hegel.Text().Alphabet("abcdefghijklmnopqrstuvwxyz0123456789_.-").MinSize(1).MaxSize(64)).MaxSize(32))
		before := slices.Clone(b.Features)
		normalized, err := NormalizeRuntimeBeat(b)
		if !slices.Equal(b.Features, before) {
			h.Fatal("normalization mutated caller")
		}
		if err != nil {
			h.Fatal(err)
		}
		twice, err := NormalizeRuntimeBeat(normalized)
		if err != nil {
			h.Fatal(err)
		}
		one, _ := json.Marshal(normalized)
		two, _ := json.Marshal(twice)
		if string(one) != string(two) {
			h.Fatal("normalization is not idempotent")
		}
		if len(normalized.Features) > 0 {
			normalized.Features[0] = "changed"
			if !slices.Equal(b.Features, before) {
				h.Fatal("normalized result aliases caller")
			}
		}
	}, hegel.WithTestCases(2000), hegel.WithDatabase(t.TempDir()))
}
func TestRuntimeValidationTokenAndVersionDomains(t *testing.T) {
	hegel.Test(t, func(h *hegel.T) {
		b := runtimeFixture().RuntimeBeat
		token := hegel.Draw(h, hegel.Text())
		b.Adapter = token
		valid := len(token) >= 1 && len(token) <= 64
		for _, c := range []byte(token) {
			if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '_' && c != '.' && c != '-' {
				valid = false
			}
		}
		_, err := NormalizeRuntimeBeat(b)
		if (err == nil) != valid {
			h.Fatal("adapter token domain mismatch")
		}
	}, hegel.WithTestCases(2000), hegel.WithDatabase(t.TempDir()))
	hegel.Test(t, func(h *hegel.T) {
		b := runtimeFixture().RuntimeBeat
		version := hegel.Draw(h, hegel.Text())
		b.AdapterVersion = version
		valid := len(version) <= 64
		for _, c := range []byte(version) {
			if c < 32 || c > 126 {
				valid = false
			}
		}
		_, err := NormalizeRuntimeBeat(b)
		if (err == nil) != valid {
			h.Fatal("version domain mismatch")
		}
	}, hegel.WithTestCases(2000), hegel.WithDatabase(t.TempDir()))
}
func TestRuntimeKataVersionComparison(t *testing.T) {
	now := runtimeFixture().LastSeenAt
	for _, tc := range []struct {
		daemon, runtime string
		skew            bool
	}{{"v0.31.1", "0.31.9", false}, {"0.31.1+build", "v0.31.2", false}, {"0.31.1", "0.32.0", true}, {"1.0.0", "0.31.0", true}, {"dev", "0.31.0", false}, {"0.31.0", "0.32.0-dev", false}, {"0.31.0", "garbage", false}, {"0.31.0", "", false}, {"0.31.0", "1", false}, {"0.31.0", "1.2", false}, {"0.31.0", "1.2.0+build.with.dots", true}} {
		r := runtimeFixture()
		r.KataVersion = tc.runtime
		out := EvaluateRuntimes([]Runtime{r}, nil, now, tc.daemon, now)
		require.Equal(t, tc.skew, slices.Contains(runtimeWarningCodes(out), "kata_version_skew"), "%+v", tc)
	}
}
