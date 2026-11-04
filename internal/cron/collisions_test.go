package cron

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"slices"
	"sort"
	"testing"
	"time"
	_ "time/tzdata"

	"github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"
)

func collisionJob(t testing.TB, id, expression string) CollisionJob {
	t.Helper()
	job := adapterJob(t, Trigger{Kind: "cron", Cron: expression})
	job.Catchup = "latest"
	return CollisionJob{UID: id, Name: id, Def: job}
}

// The collision window includes from, excludes until, and keeps the stored
// delivered watermark exclusive. Core next-fire previews remain strictly after.
func TestCronCollisionsWindow(t *testing.T) {
	from := time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC)
	candidate := collisionJob(t, "candidate", "0 9 * * *")
	peer := collisionJob(t, "peer", "0 9 * * *")
	got := collisionReport(t, candidate, []CollisionJob{peer}, from, CollisionStepBudget)
	require.False(t, got.Truncated)
	require.Equal(t, from.Add(7*24*time.Hour), got.Until)
	require.Len(t, got.Items, 1)
	require.Equal(t, from, got.Items[0].At)
	require.Equal(t, 7, got.Items[0].Occurrences)
	candidate.Def.Trigger = Trigger{Kind: "once", At: from.Format(time.RFC3339)}
	got = collisionReport(t, candidate, []CollisionJob{peer}, from, CollisionStepBudget)
	require.Equal(t, 1, got.Items[0].Occurrences)
	candidate.Def = nativeCollisionDefinition(candidate.Def)
	candidate.Def.Trigger = Trigger{Kind: "cron", Cron: "0 9 * * *"}
	peer.Def = candidate.Def
	peer.Progress = &RecurringProgress{Version: 1, Anchor: from.Add(-24 * time.Hour), Through: from.Add(-time.Hour)}
	got = collisionReport(t, candidate, []CollisionJob{peer}, from, CollisionStepBudget)
	require.Equal(t, from, got.Items[0].At, "fresh cron has no delivered watermark")
	candidate.Progress = &RecurringProgress{Version: 1, Anchor: from.Add(-24 * time.Hour), Through: from}
	got = collisionReport(t, candidate, []CollisionJob{peer}, from, CollisionStepBudget)
	require.Equal(t, from.Add(24*time.Hour), got.Items[0].At)
	candidate.Progress = nil
	candidate.Def.Trigger = Trigger{Kind: "interval", IntervalSeconds: 60}
	fires, truncated, err := occurrencesWithin(candidate.Def, nil, from, from.Add(2*time.Minute), 10080, nil)
	require.NoError(t, err)
	require.False(t, truncated)
	require.Equal(t, []time.Time{from.Add(time.Minute)}, fires)
}

func nativeCollisionDefinition(job JobDefinition) JobDefinition {
	job.Action = Action{Kind: "notify", Recipient: "current-owner-or-author"}
	job.Issue.ParentUID = "01ARZ3NDEKTSV4RRFFQ69G5FAA"
	job.Overlap = "allow"
	job.Options = nil
	return job
}

func TestCronCollisionsEligibilityAndPins(t *testing.T) {
	from := time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)
	candidate := collisionJob(t, "candidate", "0 9 * * *")
	candidate.Def.Options = jsontext.Value(`{"adapter":"runner-a","executor":"group-a"}`)
	peers := []CollisionJob{candidate}
	for i, options := range []string{`{}`, `{"adapter":"runner-a"}`, `{"adapter":42,"executor":null}`, `{"executor":"group-b"}`, `{"adapter":"runner-b"}`} {
		peer := collisionJob(t, fmt.Sprint(i), "0 9 * * *")
		peer.Def.Options = jsontext.Value(options)
		peers = append(peers, peer)
	}
	disabled := collisionJob(t, "disabled", "0 9 * * *")
	disabled.Def.Enabled = false
	native := collisionJob(t, "native", "0 9 * * *")
	native.Def = nativeCollisionDefinition(native.Def)
	peers = append(peers, disabled, native)
	got := collisionReport(t, candidate, peers, from, CollisionStepBudget)
	require.Len(t, got.Items, 1)
	require.Equal(t, 3, got.Items[0].PeerCount)
	require.Equal(t, []CollisionPeer{{UID: "0", Name: "0"}, {UID: "1", Name: "1"}, {UID: "2", Name: "2"}}, got.Items[0].Peers)
	require.Zero(t, got.Skipped)
	candidate.Def.Options = nil
	peer := collisionJob(t, "unpreviewable", "@every 1h")
	missingProgress := collisionJob(t, "missing-progress", "0 9 * * *")
	missingProgress.Def = nativeCollisionDefinition(missingProgress.Def)
	candidate.Def = nativeCollisionDefinition(candidate.Def)
	got = collisionReport(t, candidate, []CollisionJob{missingProgress}, from, CollisionStepBudget)
	require.Equal(t, 1, got.Skipped)
	candidate.Def = previewJob(t)
	got = collisionReport(t, candidate, []CollisionJob{peer}, from, CollisionStepBudget)
	require.Equal(t, 1, got.Skipped)
}

func TestCronCollisionsBoundsAndPartialPeers(t *testing.T) {
	from := time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)
	candidate := collisionJob(t, "candidate", "0 * * * *")
	b, c := collisionJob(t, "b", "0 * * * *"), collisionJob(t, "c", "0 * * * *")
	// Candidate and b each take 169 calls including their terminal probe;
	// c then contributes two known occurrences before its walk is cut short.
	partial := collisionReport(t, candidate, []CollisionJob{b, c}, from, 340)
	require.True(t, partial.Truncated)
	require.Len(t, partial.Items, 2)
	require.Equal(t, 2, partial.Items[0].PeerCount)
	require.Equal(t, 2, partial.Items[0].Occurrences)
	require.Equal(t, 1, partial.Items[1].PeerCount)
	require.Equal(t, 166, partial.Items[1].Occurrences)
	peers := []CollisionJob{}
	for i := range 25 {
		peer := collisionJob(t, fmt.Sprintf("peer-%02d", i), "0 * * * *")
		peer.Def.Trigger = Trigger{Kind: "once", At: from.Add(time.Duration(i) * time.Hour).Format(time.RFC3339)}
		peers = append(peers, peer)
	}
	got := collisionReport(t, candidate, peers, from, CollisionStepBudget)
	require.Len(t, got.Items, 20)
	require.True(t, got.Truncated)
	for i := range peers {
		peers[i].Def = b.Def
	}
	got = collisionReport(t, candidate, peers, from, CollisionStepBudget)
	require.Len(t, got.Items, 1)
	require.Equal(t, 25, got.Items[0].PeerCount)
	require.Len(t, got.Items[0].Peers, 10)
	require.True(t, got.Truncated)
	_, truncated, err := occurrencesWithin(b.Def, nil, from, from.Add(time.Hour), 1, nil)
	require.NoError(t, err)
	require.False(t, truncated, "an exactly full output is complete after its terminal probe")
	_, truncated, err = occurrencesWithin(b.Def, nil, from, from.Add(2*time.Hour), 1, nil)
	require.NoError(t, err)
	require.True(t, truncated)
}

func TestCronCollisionsWireBoundaries(t *testing.T) {
	for _, from := range []time.Time{{}, time.Date(9999, 12, 30, 0, 0, 0, 0, time.UTC)} {
		candidate := collisionJob(t, "candidate", "0 * * * *")
		candidate.Def = nativeCollisionDefinition(candidate.Def)
		peer := candidate
		peer.UID = "peer"
		peer.Progress = &RecurringProgress{Version: 1, Anchor: from.Add(-time.Hour), Through: from.Add(-time.Minute)}
		got := collisionReport(t, candidate, []CollisionJob{peer}, from, CollisionStepBudget)
		_, err := json.Marshal(got)
		require.NoError(t, err)
		require.Equal(t, from.Year() == 9999, got.Truncated)
	}
	candidate := collisionJob(t, "candidate", "0 * * * *")
	candidate.Def.Trigger = Trigger{Kind: "once", At: "9999-12-31T23:30:00Z", Timezone: "Pacific/Kiritimati"}
	peer := candidate
	peer.UID = "peer"
	got := collisionReport(t, candidate, []CollisionJob{peer}, time.Date(9999, 12, 31, 0, 0, 0, 0, time.UTC), CollisionStepBudget)
	require.Empty(t, got.Items)
	require.True(t, got.Truncated)
}

// This oracle uses the pre-existing count-based recurrence entry point, then
// windows and groups independently of the bounded collision implementation.
func referenceCollisionTimes(t require.TestingT, job CollisionJob, from time.Time) []time.Time {
	until := from.Add(7 * 24 * time.Hour)
	if until.Year() > 9999 {
		until = time.Date(9999, 12, 31, 23, 59, 59, 999999999, time.UTC)
	}
	p := RecurringProgress{Version: 1, Anchor: from, Through: from.Add(-time.Nanosecond)}
	if job.Progress != nil {
		p = *job.Progress
	}
	job.Def.Enabled = true
	var times []time.Time
	for cursor := from.Add(-time.Nanosecond); ; {
		next, err := RecurringOccurrences(job.Def, p, cursor, 1)
		require.NoError(t, err)
		if len(next) == 0 || !next[0].Before(until) {
			break
		}
		times = append(times, next[0])
		cursor = next[0]
	}
	return times
}

func referenceCollisions(t require.TestingT, candidate CollisionJob, peers []CollisionJob, from time.Time) []CollisionItem {
	walk := func(job CollisionJob) []time.Time { return referenceCollisionTimes(t, job, from) }
	candidateTimes := walk(candidate)
	peerTimes := map[string][]time.Time{}
	for _, peer := range peers {
		peerTimes[peer.UID] = walk(peer)
	}
	sort.Slice(peers, func(i, j int) bool {
		if peers[i].Name != peers[j].Name {
			return peers[i].Name < peers[j].Name
		}
		return peers[i].UID < peers[j].UID
	})
	bySet := map[string]*CollisionItem{}
	var order []string
	for _, at := range candidateTimes {
		if _, err := at.In(TriggerLocation(candidate.Def.Trigger)).MarshalText(); err != nil {
			continue
		}
		var matches []CollisionPeer
		for _, peer := range peers {
			if peer.UID == candidate.UID || !peer.Def.Enabled || IsRecurringIssueJob(peer.Def) != IsRecurringIssueJob(candidate.Def) {
				continue
			}
			// Fixture options are independently inspected, never pinsDisjoint.
			var a, b map[string]any
			_ = json.Unmarshal(candidate.Def.Options, &a)
			_ = json.Unmarshal(peer.Def.Options, &b)
			disjoint := false
			for _, key := range []string{"adapter", "executor"} {
				x, xOK := a[key].(string)
				y, yOK := b[key].(string)
				if xOK && yOK && x != "" && y != "" && x != y {
					disjoint = true
				}
			}
			if disjoint {
				continue
			}
			if slices.ContainsFunc(peerTimes[peer.UID], func(other time.Time) bool { return at.Equal(other) }) {
				matches = append(matches, CollisionPeer{UID: peer.UID, Name: peer.Name})
			}
		}
		if len(matches) == 0 {
			continue
		}
		key := ""
		for _, peer := range matches {
			key += peer.UID + "\x00"
		}
		item := bySet[key]
		if item == nil {
			item = &CollisionItem{At: at, Local: at.In(TriggerLocation(candidate.Def.Trigger)).Format(time.RFC3339Nano), PeerCount: len(matches), Peers: matches}
			bySet[key] = item
			order = append(order, key)
		}
		item.Occurrences++
	}
	out := []CollisionItem{}
	for _, key := range order {
		out = append(out, *bySet[key])
	}
	return out
}

func drawCollisionFleet(ht *hegel.T) (CollisionJob, []CollisionJob, time.Time) {
	year := hegel.Draw(ht, hegel.Integers(0, 9999))
	month := time.Month(hegel.Draw(ht, hegel.Integers(1, 12)))
	day := hegel.Draw(ht, hegel.Integers(1, time.Date(year, month+1, 0, 0, 0, 0, 0, time.UTC).Day()))
	from := time.Date(year, month, day, hegel.Draw(ht, hegel.Integers(0, 23)), hegel.Draw(ht, hegel.Integers(0, 59)), hegel.Draw(ht, hegel.Integers(0, 59)), hegel.Draw(ht, hegel.Integers(0, 999999999)), time.UTC)
	if hegel.Draw(ht, hegel.Booleans()) {
		from = hegel.Draw(ht, hegel.SampledFrom([]time.Time{{}, time.Date(0, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(9999, 12, 30, 0, 0, 0, 0, time.UTC), time.Date(2026, 3, 8, 0, 0, 0, 0, time.UTC), time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)}))
	}
	native := hegel.Draw(ht, hegel.Booleans())
	makeJob := func(i int) CollisionJob {
		job := collisionJob(ht, fmt.Sprint(i), hegel.Draw(ht, hegel.SampledFrom([]string{"0 * * * *", "0 9 * * *", "30 1 * * *", "30 2 * * *", "0 0 * * 1"})))
		job.Name = hegel.Draw(ht, hegel.Text())
		job.Def.Trigger.Timezone = hegel.Draw(ht, hegel.SampledFrom([]string{"UTC", "America/New_York", "Australia/Lord_Howe", "Pacific/Kiritimati"}))
		if native {
			job.Def = nativeCollisionDefinition(job.Def)
			anchor, through := from.Add(-24*time.Hour), from.Add(-time.Hour)
			minimum := time.Date(0, 1, 1, 0, 0, 0, 0, time.UTC)
			if anchor.Before(minimum) {
				anchor = minimum
			}
			if anchor.IsZero() {
				anchor = anchor.Add(-time.Nanosecond)
			}
			if through.Before(anchor) {
				through = anchor
			}
			job.Progress = &RecurringProgress{Version: 1, Anchor: anchor, Through: through}
			if hegel.Draw(ht, hegel.Booleans()) {
				job.Def.Trigger = Trigger{Kind: "interval", IntervalSeconds: hegel.Draw(ht, hegel.OneOf(hegel.Integers(int64(60), maxDurationSeconds), hegel.SampledFrom([]int64{60, 3600, 604800})))}
			}
		} else {
			if hegel.Draw(ht, hegel.Booleans()) {
				at := from.Add(time.Duration(hegel.Draw(ht, hegel.Integers(0, 604800))) * time.Second)
				if at.Year() > 9999 {
					at = time.Date(9999, 12, 31, 23, 59, 59, 999999999, time.UTC)
				}
				job.Def.Trigger = Trigger{Kind: "once", At: at.Format(time.RFC3339Nano)}
			}
			job.Def.Options = jsontext.Value(fmt.Sprintf(`{"adapter":%q,"executor":%q}`, hegel.Draw(ht, hegel.SampledFrom([]string{"", "a", "b"})), hegel.Draw(ht, hegel.SampledFrom([]string{"", "a", "b"}))))
		}
		return job
	}
	candidate := makeJob(-1)
	// Cap materialized fleets, not the drawn size domain. Small walk budgets
	// independently exercise incomplete fleets without allocating huge slices.
	size := hegel.Draw(ht, hegel.Integers(0, int(^uint(0)>>1)))
	peers := make([]CollisionJob, min(size, 12))
	for i := range peers {
		peers[i] = makeJob(i)
		peers[i].Def.Enabled = hegel.Draw(ht, hegel.Booleans())
	}
	return candidate, peers, from
}

func TestCronCollisionsReferenceProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		candidate, peers, from := drawCollisionFleet(ht)
		got := collisionReport(t, candidate, peers, from, CollisionStepBudget)
		want := referenceCollisions(ht, candidate, slices.Clone(peers), from)
		limited := len(want) > 20 || from.Add(7*24*time.Hour).Year() > 9999
		for _, at := range referenceCollisionTimes(ht, candidate, from) {
			if _, err := at.In(TriggerLocation(candidate.Def.Trigger)).MarshalText(); err != nil {
				limited = true
			}
		}
		if len(want) > 20 {
			want = want[:20]
		}
		for i := range want {
			if len(want[i].Peers) > 10 {
				want[i].Peers = want[i].Peers[:10]
				limited = true
			}
		}
		require.Equal(ht, limited, got.Truncated)
		require.Equal(ht, want, got.Items)
	}, hegel.WithTestCases(60), hegel.WithDatabase(t.TempDir()))
	t.Run("symmetry", func(t *testing.T) {
		hegel.Test(t, func(ht *hegel.T) {
			a, _, from := drawCollisionFleet(ht)
			b, _, _ := drawCollisionFleet(ht)
			b.UID = "other"
			a.Def.Enabled = true
			b.Def.Enabled = true
			ab := collisionReport(t, a, []CollisionJob{b}, from, CollisionStepBudget)
			ba := collisionReport(t, b, []CollisionJob{a}, from, CollisionStepBudget)
			// Clipping can omit different local instants for the two candidates.
			if ab.Truncated || ba.Truncated {
				return
			}
			require.Equal(ht, len(ab.Items), len(ba.Items))
			if len(ab.Items) > 0 {
				require.Equal(ht, ab.Items[0].At, ba.Items[0].At)
				require.Equal(ht, ab.Items[0].Occurrences, ba.Items[0].Occurrences)
			}
		}, hegel.WithTestCases(40), hegel.WithDatabase(t.TempDir()))
	})
}

func TestCronCollisionsOrderProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		candidate, peers, from := drawCollisionFleet(ht)
		budget := hegel.Draw(ht, hegel.Integers(0, 200000))
		want := collisionReport(t, candidate, peers, from, budget)
		slices.Reverse(peers)
		require.Equal(ht, want, collisionReport(t, candidate, peers, from, budget))
	}, hegel.WithTestCases(60), hegel.WithDatabase(t.TempDir()))
}

func TestCronCollisionsPausedProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		candidate, peers, from := drawCollisionFleet(ht)
		want := collisionReport(t, candidate, peers, from, CollisionStepBudget)
		candidate.Def.Enabled = false
		require.Equal(ht, want, collisionReport(t, candidate, peers, from, CollisionStepBudget))
	}, hegel.WithTestCases(60), hegel.WithDatabase(t.TempDir()))
}

func TestCronCollisionsZeroInstant(t *testing.T) {
	from := time.Time{}
	for _, trigger := range []Trigger{{Kind: "cron", Cron: "0 * * * *"}, {Kind: "once", At: "0001-01-01T00:00:00Z"}} {
		candidate := collisionJob(t, "candidate", "0 * * * *")
		candidate.Def.Trigger = trigger
		peer := candidate
		peer.UID = "peer"
		got := collisionReport(t, candidate, []CollisionJob{peer}, from, CollisionStepBudget)
		require.NotEmpty(t, got.Items)
		require.Equal(t, from, got.Items[0].At)
	}
	impossible := collisionJob(t, "candidate", "0 0 31 2 *")
	peer := impossible
	peer.UID = "peer"
	got := collisionReport(t, impossible, []CollisionJob{peer}, from, CollisionStepBudget)
	require.Empty(t, got.Items, "a scheduler zero sentinel is not a real occurrence")

	t.Run("expired once before zero", func(t *testing.T) {
		candidate := collisionJob(t, "candidate", "0 * * * *")
		candidate.Def.Trigger = Trigger{Kind: "once", At: "0000-12-29T00:00:00Z"}
		peer := candidate
		peer.UID = "peer"
		got := collisionReport(t, candidate, []CollisionJob{peer}, time.Time{}.Add(-48*time.Hour), CollisionStepBudget)
		require.Empty(t, got.Items)
	})
	t.Run("zero horizon is finite", func(t *testing.T) {
		candidate := collisionJob(t, "candidate", "0 * * * *")
		peer := candidate
		peer.UID = "peer"
		got := collisionReport(t, candidate, []CollisionJob{peer}, time.Time{}.Add(-7*24*time.Hour), CollisionStepBudget)
		require.False(t, got.Truncated)
		require.Equal(t, 168, got.Items[0].Occurrences)
	})
}

func TestCronCollisionsBoundsProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		candidate, peers, from := drawCollisionFleet(ht)
		budget := hegel.Draw(ht, hegel.Integers(0, CollisionStepBudget))
		got := collisionReport(t, candidate, peers, from, budget)
		require.LessOrEqual(ht, len(got.Items), 20)
		total := 0
		for _, item := range got.Items {
			require.LessOrEqual(ht, len(item.Peers), 10)
			require.GreaterOrEqual(ht, item.Occurrences, 1)
			total += item.Occurrences
		}
		count := len(referenceCollisionTimes(ht, candidate, from))
		require.LessOrEqual(ht, total, count)
		if budget <= count {
			require.True(ht, got.Truncated, "candidate exhausted the budget before its terminal probe")
		}
	}, hegel.WithTestCases(60), hegel.WithDatabase(t.TempDir()))
}

func TestCronCollisionsSoundnessProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		candidate, peers, from := drawCollisionFleet(ht)
		got := collisionReport(t, candidate, peers, from, hegel.Draw(ht, hegel.Integers(0, CollisionStepBudget)))
		candidateTimes := referenceCollisionTimes(ht, candidate, from)
		for _, item := range got.Items {
			require.False(ht, item.At.Before(from))
			require.True(ht, item.At.Before(got.Until))
			require.Contains(ht, candidateTimes, item.At)
			for _, member := range item.Peers {
				index := slices.IndexFunc(peers, func(peer CollisionJob) bool { return peer.UID == member.UID })
				require.GreaterOrEqual(ht, index, 0)
				peer := peers[index]
				require.True(ht, peer.Def.Enabled)
				require.NotEqual(ht, candidate.UID, peer.UID)
				require.Equal(ht, IsRecurringIssueJob(candidate.Def), IsRecurringIssueJob(peer.Def))
				require.Contains(ht, referenceCollisionTimes(ht, peer, from), item.At)
			}
		}
	}, hegel.WithTestCases(60), hegel.WithDatabase(t.TempDir()))
}

func TestCronCollisionsLocalPrecision(t *testing.T) {
	from := time.Date(2026, 11, 3, 0, 0, 0, 123456789, time.UTC)
	candidate := collisionJob(t, "candidate", "0 * * * *")
	candidate.Def = nativeCollisionDefinition(candidate.Def)
	candidate.Def.Trigger = Trigger{Kind: "interval", IntervalSeconds: 60}
	candidate.Progress = &RecurringProgress{Version: 1, Anchor: from.Add(-time.Hour), Through: from.Add(-time.Minute)}
	peer := candidate
	peer.UID = "peer"
	got := collisionReport(t, candidate, []CollisionJob{peer}, from, CollisionStepBudget)
	require.Equal(t, from, got.Items[0].At)
	require.Equal(t, from.Format(time.RFC3339Nano), got.Items[0].Local)
}

func TestCronCollisionsCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := PreviewCollisions(ctx, collisionJob(t, "candidate", "0 * * * *"), nil, time.Now().UTC(), CollisionStepBudget)
	require.ErrorIs(t, err, context.Canceled)
}

func TestCronCollisionsDormantDescriptors(t *testing.T) {
	from := time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)
	for _, native := range []bool{false, true} {
		for _, expression := range []string{"@none", "@manual", "@triggered", "CRON_TZ=UTC @none", "TZ=UTC @manual"} {
			t.Run(fmt.Sprintf("native=%t/%s", native, expression), func(t *testing.T) {
				candidate := collisionJob(t, "candidate", "0 9 * * *")
				peer := collisionJob(t, "peer", expression)
				if native {
					candidate.Def = nativeCollisionDefinition(candidate.Def)
					peer.Def = nativeCollisionDefinition(peer.Def)
					peer.Progress = &RecurringProgress{Version: 1, Anchor: from.Add(-time.Hour), Through: from.Add(-time.Minute)}
				}
				require.NoError(t, peer.Def.Validate())
				got := collisionReport(t, candidate, []CollisionJob{peer}, from, CollisionStepBudget)
				require.Equal(t, 1, got.Skipped)
				require.Empty(t, got.Items)
				peer.Def.Trigger = Trigger{Kind: "cron", Cron: "0 0 1 1 *"}
				require.Zero(t, collisionReport(t, candidate, []CollisionJob{peer}, from, CollisionStepBudget).Skipped,
					"a calendar schedule outside the window is still previewable")
			})
		}
	}
}

func TestCronCollisionsFullSetsSurviveDisplayClipping(t *testing.T) {
	from := time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)
	candidate := collisionJob(t, "candidate", "0 * * * *")
	peers := make([]CollisionJob, 0, 12)
	for i := range 10 {
		peers = append(peers, collisionJob(t, fmt.Sprintf("common-%02d", i), "0 * * * *"))
	}
	for i := range 2 {
		peer := collisionJob(t, fmt.Sprintf("zz-extra-%d", i), "0 * * * *")
		peer.Def.Trigger = Trigger{Kind: "once", At: from.Add(time.Duration(i) * time.Hour).Format(time.RFC3339)}
		peers = append(peers, peer)
	}
	got := collisionReport(t, candidate, peers, from, CollisionStepBudget)
	require.True(t, got.Truncated)
	require.Len(t, got.Items, 3)
	require.Equal(t, got.Items[0].Peers, got.Items[1].Peers, "displayed prefixes match")
	require.Equal(t, 11, got.Items[0].PeerCount)
	require.Equal(t, 11, got.Items[1].PeerCount)
	require.Equal(t, 1, got.Items[0].Occurrences)
	require.Equal(t, 1, got.Items[1].Occurrences)
	require.Equal(t, from, got.Items[0].At)
	require.Equal(t, from.Add(time.Hour), got.Items[1].At)
}

func TestCronCollisionsOnceTerminalBudget(t *testing.T) {
	from := time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)
	job := collisionJob(t, "candidate", "0 * * * *").Def
	job.Trigger = Trigger{Kind: "once", At: from.Format(time.RFC3339)}
	for initial := range 3 {
		budget := initial
		times, limited, err := occurrencesWithin(job, nil, from, from.Add(time.Hour), 1, &budget)
		require.NoError(t, err)
		require.Equal(t, min(initial, 1), len(times))
		require.Equal(t, initial < 2, limited, "completion needs the terminal probe")
		require.Zero(t, budget)
	}
}

func collisionReport(t testing.TB, candidate CollisionJob, peers []CollisionJob, from time.Time, budget int) ValidationCollisions {
	t.Helper()
	out, err := PreviewCollisions(t.Context(), candidate, peers, from, budget)
	require.NoError(t, err)
	return out
}
