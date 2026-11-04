package daemon

import (
	"errors"
	"fmt"
	"maps"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"hegel.dev/go/hegel"
)

// Beat is test-only admission for registry fixtures. Production normalizes and
// authorizes requests in the HTTP handler before calling beatNormalized.
func (r *CronRuntimeRegistry) Beat(projectID int64, id, actor string, b cron.RuntimeBeat) (cron.Runtime, error) {
	b, err := cron.NormalizeRuntimeBeat(b)
	if err != nil {
		return cron.Runtime{}, err
	}
	return r.beatNormalized(projectID, id, actor, b, "")
}

func TestCronRuntimeRegistryRetentionAndIsolation(t *testing.T) {
	now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	start := now
	reg := NewCronRuntimeRegistry(start, func() time.Time { return now })
	beat := cron.RuntimeBeat{Label: "worker", Adapter: "example", IntervalSeconds: 10, Features: []string{"cron_v1"}}
	r, err := reg.Beat(1, fmt.Sprintf("%026d", 1), "operator", beat)
	require.NoError(t, err)
	r.Features[0] = "returned-mutation"
	list := reg.List(1, nil, "dev")
	require.Equal(t, []string{"cron_v1"}, list.Runtimes[0].Features)
	list.Runtimes[0].Features[0] = "list-mutation"
	require.Equal(t, []string{"cron_v1"}, reg.List(1, nil, "dev").Runtimes[0].Features)
	now = now.Add(30 * time.Second)
	require.Equal(t, "expired", reg.List(1, nil, "dev").Runtimes[0].Status)
	now = now.Add(24*time.Hour - time.Nanosecond)
	require.Len(t, reg.List(1, nil, "dev").Runtimes, 1)
	now = now.Add(time.Nanosecond)
	out := reg.List(1, nil, "dev")
	require.Empty(t, out.Runtimes)
	require.Equal(t, "unknown", out.Inventory.State)
}
func TestCronRuntimeRegistryCapacity(t *testing.T) {
	now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	reg := NewCronRuntimeRegistry(now, func() time.Time { return now })
	beat := cron.RuntimeBeat{Label: "worker", Adapter: "example", IntervalSeconds: 10}
	for i := range 256 {
		_, err := reg.Beat(1, fmt.Sprintf("%026d", i), "operator", beat)
		require.NoError(t, err)
	}
	_, err := reg.Beat(1, fmt.Sprintf("%026d", 256), "operator", beat)
	require.ErrorIs(t, err, errCronRuntimeLimit, "project limit applies below daemon capacity")
	for p := int64(2); p <= 16; p++ {
		for i := range 256 {
			_, err := reg.Beat(p, fmt.Sprintf("%026d", i), "operator", beat)
			require.NoError(t, err)
		}
	}
	_, err = reg.Beat(17, fmt.Sprintf("%026d", 256), "operator", beat)
	require.ErrorIs(t, err, errCronRuntimeLimit)
	now = now.Add(30 * time.Second)
	_, err = reg.Beat(1, fmt.Sprintf("%026d", 0), "operator", beat)
	require.NoError(t, err, "retained identities may refresh at capacity")
	_, err = reg.Beat(17, fmt.Sprintf("%026d", 256), "operator", beat)
	require.ErrorIs(t, err, errCronRuntimeLimit, "expiry alone does not release retention")
	now = now.Add(24 * time.Hour)
	_, err = reg.Beat(17, fmt.Sprintf("%026d", 256), "operator", beat)
	require.NoError(t, err, "reclaim across untouched projects")
}
func TestCronRuntimeRegistrySequences(t *testing.T) {
	check := func(h interface {
		Fatal(...any)
		Fatalf(string, ...any)
	}, steps []int) {
		now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
		reg := NewCronRuntimeRegistry(now, func() time.Time { return now })
		model := map[string]time.Time{}
		if len(steps) > 100 {
			steps = steps[:100]
		}
		for _, step := range steps {
			key := fmt.Sprintf("%026d", step%10)
			operation := step / 10 % 3
			switch operation {
			case 0:
				now = now.Add(time.Duration(step) * time.Second)
			case 1:
				// Rejected writes must preserve the last accepted UID/expiry inventory.
				b := cron.RuntimeBeat{Label: key, Adapter: "example", IntervalSeconds: 9}
				if _, err := reg.Beat(1, key, "operator", b); !errors.Is(err, cron.ErrInvalid) {
					h.Fatalf("invalid beat returned %v", err)
				}
			default:
				b := cron.RuntimeBeat{Label: key, Adapter: "example", IntervalSeconds: 10}
				_, err := reg.Beat(1, key, "operator", b)
				if err != nil {
					h.Fatal(err)
				}
				model[key] = now.Add(30 * time.Second)
			}
			out := reg.List(1, nil, "dev")
			for key, expires := range model {
				if !now.Before(expires.Add(24 * time.Hour)) {
					delete(model, key)
				}
			}
			observed := map[string]time.Time{}
			for _, runtime := range out.Runtimes {
				observed[runtime.UID] = runtime.ExpiresAt
			}
			if len(out.Runtimes) != len(model) || !maps.Equal(observed, model) {
				h.Fatalf("retained UID/expiry inventory %v want %v", observed, model)
			}
		}
	}
	t.Run("two_runtime_inventory", func(t *testing.T) { check(t, []int{20, 21}) })
	t.Run("model", func(t *testing.T) {
		hegel.Test(t, func(h *hegel.T) {
			check(h, hegel.Draw(h, hegel.Lists(hegel.Integers(0, 10000))))
		}, hegel.WithTestCases(1000), hegel.WithDatabase(t.TempDir()))
	})
}

func TestCronRuntimeRegistryRefresh(t *testing.T) {
	hegel.Test(t, func(h *hegel.T) {
		now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
		reg := NewCronRuntimeRegistry(now, func() time.Time { return now })
		first := now
		interval := hegel.Draw(h, hegel.Integers(10, 3600))
		b := cron.RuntimeBeat{Label: "worker", Adapter: "example", IntervalSeconds: interval, State: "stopping"}
		stopped, err := reg.Beat(1, fmt.Sprintf("%026d", 1), "first-actor", b)
		if err != nil {
			h.Fatal(err)
		}
		if stopped.Status != "expired" || !stopped.ExpiresAt.Equal(now) {
			h.Fatal("stopping not immediate")
		}
		// UTC wall-clock readings can move backwards; stopping stays expired.
		now = now.Add(-time.Second)
		observed := reg.List(1, nil, "dev").Runtimes[0]
		if observed.Status != "expired" || !observed.ExpiresAt.Equal(first) {
			h.Fatal("clock rollback revived a stopping runtime")
		}
		now = first.Add(time.Second)
		b.State = "active"
		active, err := reg.Beat(1, fmt.Sprintf("%026d", 1), "second-actor", b)
		if err != nil {
			h.Fatal(err)
		}
		if !active.FirstSeenAt.Equal(first) || !active.LastSeenAt.Equal(now) || !active.ExpiresAt.Equal(now.Add(time.Duration(interval)*3*time.Second)) || active.Actor != "second-actor" || active.Status != "live" {
			h.Fatal("refresh did not replace current evidence and preserve retained history")
		}
		_, err = reg.Beat(2, fmt.Sprintf("%026d", 1), "other-project", b)
		if err != nil {
			h.Fatal(err)
		}
		if reg.List(1, nil, "dev").Runtimes[0].Actor != "second-actor" {
			h.Fatal("same UID crossed project boundary")
		}
	}, hegel.WithTestCases(1000), hegel.WithDatabase(t.TempDir()))
}
