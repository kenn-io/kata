package conformance

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"os"
	"slices"
	"testing"
	"time"
	_ "time/tzdata"

	"github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"
)

func dropPolicy() Declaration {
	d := Reference{}.Declare()
	d.DropRecords = true
	d.ForbidDeferral = "drop_after_grace"
	d.CatchupWindowSeconds = 86400
	return d
}
func alterPlan(t testing.TB, in PlanInput, fields map[string]any) PlanInput {
	t.Helper()
	var def map[string]any
	require.NoError(t, json.Unmarshal(in.Job.Definition, &def))
	for k, v := range fields {
		def[k] = v
	}
	var err error
	in.Job.Definition, err = json.Marshal(def)
	require.NoError(t, err)
	return in
}

func TestDefaultExportBytesRemainFrozen(t *testing.T) {
	raw, err := os.ReadFile("testdata/default-export-sha256.json")
	require.NoError(t, err)
	var want map[string]string
	require.NoError(t, json.Unmarshal(raw, &want))
	files, err := ExportFiles(t.Context(), Reference{}.Declare())
	require.NoError(t, err)
	got := map[string]string{}
	for name, raw := range files {
		sum := sha256.Sum256(raw)
		got[name] = hex.EncodeToString(sum[:])
	}
	require.Equal(t, want, got)
	scenarios, err := Scenarios()
	require.NoError(t, err)
	require.Len(t, scenarios, 54)
}
func TestDropLiteralVectors(t *testing.T) {
	raw, err := fixtures.ReadFile("testdata/adapter-v1/drops.json")
	require.NoError(t, err)
	scenarios, err := Load(raw)
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(scenarios), 14)
	for _, s := range scenarios {
		t.Run(s.Name, func(t *testing.T) {
			a, err := Execute(t.Context(), Reference{Policy: dropPolicy()}, s)
			require.NoError(t, err)
			for i, step := range s.Steps {
				require.JSONEq(t, string(step.Expect), string(a.Outputs[i]))
			}
		})
	}
}

type plainDropAdapter struct{ ref Reference }

func (a plainDropAdapter) Declare() Declaration { return a.ref.Declare() }
func (a plainDropAdapter) Plan(ctx context.Context, in PlanInput) (PlanOutput, error) {
	return a.ref.Plan(ctx, in)
}
func (a plainDropAdapter) Settle(ctx context.Context, in SettleInput) (SettleOutput, error) {
	return a.ref.Settle(ctx, in)
}

type dropSpy struct {
	plainDropAdapter
	combinedCalls, planCalls int
}

func (a *dropSpy) Plan(ctx context.Context, in PlanInput) (PlanOutput, error) {
	a.planCalls++
	return a.ref.Plan(ctx, in)
}
func (a *dropSpy) PlanWithDrops(ctx context.Context, in PlanInput) (PlanOutput, []Drop, error) {
	a.combinedCalls++
	return a.ref.PlanWithDrops(ctx, in)
}

func TestDropInterfaceIsStrictlyOptIn(t *testing.T) {
	d := dropPolicy()
	suite, err := Suite(t.Context(), d)
	require.NoError(t, err)
	_, err = Execute(t.Context(), plainDropAdapter{Reference{Policy: d}}, suite.Scenarios[0])
	require.ErrorContains(t, err, "DropRecorder")
	spy := &dropSpy{plainDropAdapter: plainDropAdapter{Reference{Policy: d}}}
	_, err = Execute(t.Context(), spy, suite.Scenarios[0])
	require.NoError(t, err)
	require.Equal(t, 1, spy.combinedCalls)
	require.Zero(t, spy.planCalls)
	spy = &dropSpy{plainDropAdapter: plainDropAdapter{Reference{}}}
	base, err := Scenarios()
	require.NoError(t, err)
	_, err = Execute(t.Context(), spy, base[0])
	require.NoError(t, err)
	require.Zero(t, spy.combinedCalls)
	require.Equal(t, 1, spy.planCalls)
	d.DropRecords = false
	d.ForbidDeferral = "invalid"
	_, err = Suite(t.Context(), d)
	require.Error(t, err)
}
func TestDropPolicySwitchesAndRevision(t *testing.T) {
	in := alterPlan(t, basePlan(), map[string]any{"overlap": "forbid", "catchup": "all"})
	in.ActiveRuns = 1
	for _, record := range []bool{false, true} {
		for _, deferral := range []string{"", "queue", "drop_after_grace"} {
			d := Reference{}.Declare()
			d.DropRecords = record
			d.ForbidDeferral = deferral
			out, err := (Reference{Policy: d}).Plan(t.Context(), in)
			require.NoError(t, err)
			if deferral == "drop_after_grace" {
				require.Equal(t, []time.Time{in.Now}, out.Checkpoint.Pending)
			} else {
				require.Len(t, out.Checkpoint.Pending, 3)
			}
		}
	}
	in = alterPlan(t, basePlan(), map[string]any{"catchup": "latest"})
	in.Now = in.Job.CreatedAt.Add(5 * time.Hour)
	in.Checkpoint = &Checkpoint{Through: in.Job.CreatedAt.Add(2 * time.Hour), DefinitionEventUID: "01ARZ3NDEKTSV4RRFFQ69G5FAC", Pending: []time.Time{in.Job.CreatedAt.Add(time.Hour)}}
	out, drops, err := (Reference{Policy: dropPolicy()}).PlanWithDrops(t.Context(), in)
	require.NoError(t, err)
	require.Equal(t, []time.Time{in.Now}, out.Launch)
	require.Equal(t, []Drop{{Reason: "catchup_missed", Instants: []time.Time{in.Job.CreatedAt.Add(3 * time.Hour), in.Job.CreatedAt.Add(4 * time.Hour)}}}, drops)
}

func TestDropCheckerShapes(t *testing.T) {
	d := dropPolicy()
	suite, err := Suite(t.Context(), d)
	require.NoError(t, err)
	answers := []Answer{}
	for _, s := range suite.Scenarios {
		a, err := Execute(t.Context(), Reference{Policy: d}, s)
		require.NoError(t, err)
		answers = append(answers, a)
	}
	report, err := Check(t.Context(), d, answers)
	require.NoError(t, err)
	require.Empty(t, report.Failed)
	for _, kind := range []string{"missing", "null", "wrong-reason", "missing-instant", "duplicate-group", "settle-member"} {
		t.Run(kind, func(t *testing.T) {
			changed := append([]Answer{}, answers...)
			for i, a := range changed {
				if kind == "settle-member" && a.Name != "retries/success" || kind != "settle-member" && a.Name != "drops/latest" {
					continue
				}
				changed[i].Outputs = slices.Clone(a.Outputs)
				var fields map[string]jsontext.Value
				require.NoError(t, json.Unmarshal(a.Outputs[0], &fields))
				switch kind {
				case "missing":
					delete(fields, "drops")
				case "null":
					fields["drops"] = []byte("null")
				case "settle-member":
					fields["drops"] = []byte("[]")
				default:
					var drops []Drop
					require.NoError(t, json.Unmarshal(fields["drops"], &drops))
					switch kind {
					case "wrong-reason":
						drops[0].Reason = "wrong"
					case "missing-instant":
						drops[0].Instants = drops[0].Instants[1:]
					case "duplicate-group":
						drops = append(drops, drops[0])
					}
					fields["drops"], err = json.Marshal(drops)
					require.NoError(t, err)
				}
				changed[i].Outputs[0], err = json.Marshal(fields)
				require.NoError(t, err)
			}
			report, err := Check(t.Context(), d, changed)
			require.NoError(t, err)
			require.NotEmpty(t, report.Failed)
		})
	}
	legacy, err := Scenarios()
	require.NoError(t, err)
	a, err := Execute(t.Context(), Reference{}, legacy[0])
	require.NoError(t, err)
	var fields map[string]jsontext.Value
	require.NoError(t, json.Unmarshal(a.Outputs[0], &fields))
	fields["drops"] = []byte("[]")
	a.Outputs[0], _ = json.Marshal(fields)
	report, err = Check(t.Context(), Reference{}.Declare(), []Answer{a})
	require.NoError(t, err)
	require.Contains(t, report.Failed[0].Diff, "drops")
}

func TestDropConservationProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		hours := hegel.Draw(ht, hegel.Integers(1, 250))
		mask := hegel.Draw(ht, hegel.Integers(0, 1023))
		policy := hegel.Draw(ht, hegel.Integers(0, 2))
		held := hegel.Draw(ht, hegel.Integers(0, 1))
		forbid := hegel.Draw(ht, hegel.Integers(0, 1)) == 1
		changed := hegel.Draw(ht, hegel.Integers(0, 1)) == 1
		in := basePlan()
		in.Job.CreatedAt = in.Job.CreatedAt.Add(time.Duration(hegel.Draw(ht, hegel.Integers(0, 1))) * 500 * time.Millisecond)
		in.Now = in.Job.CreatedAt.Add(time.Duration(hours) * time.Hour)
		in.ActiveRuns = held
		grace := hegel.Draw(ht, hegel.Integers(0, 10800))
		overlap := "allow"
		if forbid {
			overlap = "forbid"
		}
		catchup := []string{"all", "latest", "skip"}[policy]
		in = alterPlan(ht, in, map[string]any{"overlap": overlap, "catchup": catchup, "grace_seconds": grace})
		pending := []time.Time{}
		for i := 1; i <= 10; i++ {
			if mask&(1<<(i-1)) != 0 {
				pending = append(pending, in.Job.CreatedAt.Add(time.Duration(i)*time.Hour))
			}
		}
		if hegel.Draw(ht, hegel.Integers(0, 1)) == 1 {
			slices.Reverse(pending)
		}
		if len(pending) > 0 && hegel.Draw(ht, hegel.Integers(0, 1)) == 1 {
			pending = append(pending, pending[0])
		}
		if hegel.Draw(ht, hegel.Integers(0, 1)) == 1 {
			for i := range pending {
				pending[i] = pending[i].In(time.FixedZone("fixture", 3600))
			}
		}
		in.Checkpoint = &Checkpoint{Through: in.Job.CreatedAt.Add(10 * time.Hour), DefinitionEventUID: in.Job.DefinitionEventUID, Pending: pending}
		if changed {
			in.Job.DefinitionEventUID = "01ARZ3NDEKTSV4RRFFQ69G5FAA"
			pending = []time.Time{}
		}
		candidates := slices.Clone(pending)
		for i := 11; i <= hours; i++ {
			candidates = append(candidates, in.Job.CreatedAt.Add(time.Duration(i)*time.Hour))
		}
		for i := range candidates {
			candidates[i] = candidates[i].UTC()
		}
		slices.SortFunc(candidates, func(a, b time.Time) int { return a.Compare(b) })
		candidates = slices.CompactFunc(candidates, func(a, b time.Time) bool { return a.Equal(b) })
		d := dropPolicy()
		d.ToleranceSeconds = int64(hegel.Draw(ht, hegel.Integers(0, 120)))
		if hegel.Draw(ht, hegel.Integers(0, 1)) == 0 {
			d.CatchupLimit = 100
		} else {
			d.CatchupLimit = 0
		}
		if hegel.Draw(ht, hegel.Integers(0, 1)) == 0 {
			d.ForbidDeferral = "queue"
		}
		out, drops, err := (Reference{Policy: d}).PlanWithDrops(ht.Context(), in)
		require.NoError(ht, err)
		selected := slices.Clone(candidates)
		switch catchup {
		case "latest":
			if len(selected) > 1 {
				selected = selected[len(selected)-1:]
			}
		case "skip":
			cut := in.Now.Add(-time.Duration(grace) * time.Second).Add(-time.Duration(d.ToleranceSeconds) * time.Second)
			selected = slices.DeleteFunc(selected, func(at time.Time) bool { return at.Before(cut) })
		case "all":
			cut := in.Now.Add(-time.Duration(d.CatchupWindowSeconds) * time.Second)
			selected = slices.DeleteFunc(selected, func(at time.Time) bool { return at.Before(cut) })
			if d.CatchupLimit > 0 && len(selected) > d.CatchupLimit {
				selected = selected[len(selected)-d.CatchupLimit:]
			}
		}
		expectedCatchup := []time.Time{}
		for _, at := range candidates {
			if !slices.Contains(selected, at) {
				expectedCatchup = append(expectedCatchup, at)
			}
		}
		seen := map[time.Time]bool{}
		all := []time.Time{}
		add := func(at time.Time) {
			at = at.UTC()
			require.False(ht, seen[at], "dispositions must be disjoint")
			seen[at] = true
			all = append(all, at)
		}
		for _, at := range out.Launch {
			add(at)
		}
		for _, at := range out.Checkpoint.Pending {
			add(at)
		}
		actualCatchup := []time.Time{}
		actualOverlap := []time.Time{}
		expectedOverlap := []time.Time{}
		if forbid && held > 0 && d.ForbidDeferral == "drop_after_grace" {
			cut := in.Now.Add(-time.Duration(grace) * time.Second).Add(-time.Duration(d.ToleranceSeconds) * time.Second)
			for _, at := range selected {
				if at.Before(cut) {
					expectedOverlap = append(expectedOverlap, at)
				}
			}
		}
		for _, drop := range drops {
			require.NotEmpty(ht, drop.Instants)
			for _, at := range drop.Instants {
				add(at)
			}
			if drop.Reason == "catchup_missed" {
				for _, at := range drop.Instants {
					actualCatchup = append(actualCatchup, at.UTC())
				}
			} else {
				for _, at := range drop.Instants {
					actualOverlap = append(actualOverlap, at.UTC())
				}
				require.Equal(ht, "overlap_skipped", drop.Reason)
				require.True(ht, forbid && held > 0 && d.ForbidDeferral == "drop_after_grace")
				for _, at := range drop.Instants {
					require.True(ht, at.Before(in.Now.Add(-time.Duration(grace)*time.Second).Add(-time.Duration(d.ToleranceSeconds)*time.Second)))
				}
			}
		}
		slices.SortFunc(all, func(a, b time.Time) int { return a.Compare(b) })
		require.Equal(ht, candidates, all)
		require.Equal(ht, expectedCatchup, actualCatchup)
		require.Equal(ht, expectedOverlap, actualOverlap)
	})
}
func TestFreeGateDeferralEquivalenceProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		in := basePlan()
		in.Now = in.Job.CreatedAt.Add(time.Duration(hegel.Draw(ht, hegel.Integers(1, 200))) * time.Hour)
		in = alterPlan(ht, in, map[string]any{"overlap": "forbid", "catchup": []string{"all", "latest", "skip"}[hegel.Draw(ht, hegel.Integers(0, 2))]})
		d := dropPolicy()
		a, err := (Reference{Policy: d}).Plan(ht.Context(), in)
		require.NoError(ht, err)
		d.ForbidDeferral = "queue"
		b, err := (Reference{Policy: d}).Plan(ht.Context(), in)
		require.NoError(ht, err)
		require.Equal(ht, b, a)
	})
}
func TestOptInExportsDeterministically(t *testing.T) {
	d := dropPolicy()
	first, err := ExportFiles(t.Context(), d)
	require.NoError(t, err)
	require.Contains(t, first, "drops.json")
	for range 10 {
		next, err := ExportFiles(t.Context(), d)
		require.NoError(t, err)
		require.Equal(t, first, next)
	}
}

func TestSuppliedForbidDeferralMustBeRecognized(t *testing.T) {
	base, err := json.Marshal(Reference{}.Declare())
	require.NoError(t, err)
	for _, value := range []string{`""`, `null`, `"invalid"`, `"queue"`, `"drop_after_grace"`} {
		t.Run(value, func(t *testing.T) {
			var fields map[string]jsontext.Value
			require.NoError(t, json.Unmarshal(base, &fields))
			fields["forbid_deferral"] = []byte(value)
			raw, err := json.Marshal(fields)
			require.NoError(t, err)
			var d Declaration
			err = json.Unmarshal(raw, &d, json.RejectUnknownMembers(true))
			if value == `"queue"` || value == `"drop_after_grace"` {
				require.NoError(t, err)
				require.NoError(t, d.Validate())
			} else {
				require.Error(t, err)
			}
		})
	}
}

func TestDropInstantRepresentationAndPrecision(t *testing.T) {
	d := dropPolicy()
	suite, err := Suite(t.Context(), d)
	require.NoError(t, err)
	answers := []Answer{}
	for _, s := range suite.Scenarios {
		a, err := Execute(t.Context(), Reference{Policy: d}, s)
		require.NoError(t, err)
		if s.Name == "drops/latest" {
			var fields map[string]jsontext.Value
			require.NoError(t, json.Unmarshal(a.Outputs[0], &fields))
			var groups []Drop
			require.NoError(t, json.Unmarshal(fields["drops"], &groups))
			for i := range groups[0].Instants {
				groups[0].Instants[i] = groups[0].Instants[i].In(time.FixedZone("fixture", 3600))
			}
			fields["drops"], _ = json.Marshal(groups)
			a.Outputs[0], _ = json.Marshal(fields)
		}
		answers = append(answers, a)
	}
	report, err := Check(t.Context(), d, answers)
	require.NoError(t, err)
	require.Empty(t, report.Failed)
	for i := range answers {
		if answers[i].Name == "drops/latest" {
			var fields map[string]jsontext.Value
			require.NoError(t, json.Unmarshal(answers[i].Outputs[0], &fields))
			var groups []Drop
			require.NoError(t, json.Unmarshal(fields["drops"], &groups))
			groups[0].Instants[0] = groups[0].Instants[0].Add(time.Nanosecond)
			fields["drops"], _ = json.Marshal(groups)
			answers[i].Outputs[0], _ = json.Marshal(fields)
		}
	}
	report, err = Check(t.Context(), d, answers)
	require.NoError(t, err)
	require.NotEmpty(t, report.Failed)
}
