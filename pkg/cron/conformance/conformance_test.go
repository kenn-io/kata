package conformance

import (
	"context"
	"encoding/json/v2"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
	_ "time/tzdata"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"hegel.dev/go/hegel"
)

func instant(s string) time.Time {
	v, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		panic(err)
	}
	return v
}
func basePlan() PlanInput {
	return PlanInput{Adapter: "example-adapter", InstalledAt: instant("2029-12-31T00:00:00Z"), Now: instant("2030-01-01T03:00:00Z"), Job: Job{CreatedAt: instant("2030-01-01T00:00:00Z"), DefinitionEventUID: "01ARZ3NDEKTSV4RRFFQ69G5FAB", Definition: []byte(`{"version":1,"kind":"job","enabled":true,"trigger":{"kind":"interval","interval_seconds":3600},"action":{"kind":"execute","prompt":"Inspect"},"issue":{"kind":"per-run","title":"Inspection"},"overlap":"allow","catchup":"all"}`)}}
}

// The reviewed profile is the product contract; frozen literals are the oracle.
func TestReferenceConforms(t *testing.T) {
	scenarios, err := Scenarios()
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(scenarios), 40)
	for _, s := range scenarios {
		t.Run(s.Name, func(t *testing.T) {
			answer, err := Execute(t.Context(), Reference{}, s)
			require.NoError(t, err)
			require.Len(t, answer.Outputs, len(s.Steps))
			for i, step := range s.Steps {
				require.JSONEq(t, string(step.Expect), string(answer.Outputs[i]))
			}
		})
	}
	Run(t, Reference{})
}

func TestLoaderAndCheckerRejectInvalidInputs(t *testing.T) {
	for _, raw := range []string{
		`{"schema":"bad","scenarios":[]}`,
		`{"schema":"kata.cron.adapter-conformance/v1","scenarios":[],"extra":true}`,
		`{"schema":"kata.cron.adapter-conformance/v1","scenarios":[{"name":"x","steps":[]}]}`,
	} {
		_, err := Load([]byte(raw))
		require.Error(t, err)
	}
	scenarios, err := Scenarios()
	require.NoError(t, err)
	raw, err := json.Marshal(struct {
		Schema    string     `json:"schema"`
		Scenarios []Scenario `json:"scenarios"`
	}{Schema, []Scenario{scenarios[0], scenarios[0]}})
	require.NoError(t, err)
	_, err = Load(raw)
	require.ErrorContains(t, err, "duplicate")
	answers := []Answer{}
	for _, s := range scenarios {
		a, err := Execute(t.Context(), Reference{}, s)
		require.NoError(t, err)
		answers = append(answers, a)
	}
	d := Reference{}.Declare()
	report, err := Check(t.Context(), d, answers)
	require.NoError(t, err)
	require.Empty(t, report.Failed)
	require.Equal(t, len(scenarios), report.Passed)
	for name, mutate := range map[string]func([]Answer) []Answer{
		"missing":      func(a []Answer) []Answer { return a[1:] },
		"duplicate":    func(a []Answer) []Answer { return append(a, a[0]) },
		"unknown":      func(a []Answer) []Answer { return append(a, Answer{Name: "unknown"}) },
		"wrong output": func(a []Answer) []Answer { a[0].Outputs = nil; return a },
	} {
		t.Run(name, func(t *testing.T) {
			report, err := Check(t.Context(), d, mutate(slices.Clone(answers)))
			require.NoError(t, err)
			require.NotEmpty(t, report.Failed)
		})
	}
	for _, d := range []Declaration{{}, {Name: "adapter", CatchupLimit: 99, FailurePolicy: "continue"}, {Name: "adapter", ToleranceSeconds: 121, FailurePolicy: "continue"}, {Name: "adapter", CatchupWindowSeconds: 86399, FailurePolicy: "continue"}, {Name: "adapter", FailurePolicy: "invalid"}} {
		_, err := Suite(t.Context(), d)
		require.Error(t, err)
	}
}

func TestPlanProperties(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		// Bound materialized horizons to the reference scan budget.
		hours := hegel.Draw(ht, hegel.Integers(0, int(^uint(0)>>1)))%250 + 1
		active := hegel.Draw(ht, hegel.Integers(0, 2))
		limit := hegel.Draw(ht, hegel.Integers(100, 150))
		in := basePlan()
		in.Now = in.Job.CreatedAt.Add(time.Duration(hours) * time.Hour)
		in.ActiveRuns = active
		var def map[string]any
		require.NoError(ht, json.Unmarshal(in.Job.Definition, &def))
		def["overlap"] = "forbid"
		in.Job.Definition, _ = json.Marshal(def)
		d := Reference{}.Declare()
		d.CatchupLimit = limit
		out, err := (Reference{Policy: d}).Plan(ht.Context(), in)
		require.NoError(ht, err)
		require.False(ht, out.Checkpoint.Through.Before(in.Job.CreatedAt))
		require.Equal(ht, in.Now, out.Checkpoint.Through)
		all := append(slices.Clone(out.Launch), out.Checkpoint.Pending...)
		require.Len(ht, all, min(hours, limit))
		require.True(ht, slices.IsSortedFunc(all, func(a, b time.Time) int { return a.Compare(b) }))
		if active == 0 {
			require.Len(ht, out.Launch, 1)
		} else {
			require.Empty(ht, out.Launch)
		}
		for i, at := range all {
			require.Equal(ht, in.Job.CreatedAt.Add(time.Duration(hours-len(all)+i+1)*time.Hour), at)
		}
		if active > 0 {
			in.Checkpoint = &out.Checkpoint
			in.ActiveRuns = 0
			released, err := (Reference{Policy: d}).Plan(ht.Context(), in)
			require.NoError(ht, err)
			require.Len(ht, released.Launch, 1)
			require.Equal(ht, all[0], released.Launch[0])
		}
	})
}

func TestNativePlannerAgreement(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		minutes := hegel.Draw(ht, hegel.Integers(0, int(^uint(0)>>1)))%200 + 1
		policy := hegel.Draw(ht, hegel.Integers(0, 2))
		interval := hegel.Draw(ht, hegel.Integers(0, 5))
		grace := hegel.Draw(ht, hegel.Integers(1, 300))
		in := basePlan()
		in.Now = in.Job.CreatedAt.Add(time.Duration(minutes) * time.Minute)
		native, err := cron.ParseJob([]byte(`{"version":1,"kind":"job","enabled":true,"trigger":{"kind":"cron","cron":"* * * * *"},"action":{"kind":"notify","recipient":"operator"},"issue":{"kind":"per-run","parent_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAC","title":"Inspect"},"overlap":"allow","catchup":"all"}`))
		require.NoError(ht, err)
		native.Catchup = []string{"all", "latest", "skip"}[policy]
		native.GraceSeconds = int64(grace)
		if interval > 0 {
			native.Trigger = cron.Trigger{Kind: "interval", IntervalSeconds: int64(interval * 60)}
		}
		adapter := native
		adapter.Action = cron.Action{Kind: "execute", Prompt: "Inspect"}
		adapter.Issue = &cron.IssuePolicy{Kind: "per-run", Title: "Inspection"}
		in.Job.Definition, _ = json.Marshal(adapter)
		out, err := (Reference{}).Plan(ht.Context(), in)
		require.NoError(ht, err)
		progress := cron.RecurringProgress{Version: 1, Anchor: in.Job.CreatedAt, Through: in.Job.CreatedAt}
		due := []time.Time{}
		for {
			batch, err := cron.PlanRecurring(native, in.Job.DefinitionEventUID, progress, in.Now)
			require.NoError(ht, err)
			due = append(due, batch.Due...)
			progress = batch.Progress
			if !batch.More {
				break
			}
		}
		require.Equal(ht, due, out.Launch)
	})
}

func TestPolicyBoundaries(t *testing.T) {
	in := basePlan()
	in.Now = in.Job.CreatedAt.Add(201 * time.Minute)
	in.Job.Definition = []byte(strings.ReplaceAll(string(in.Job.Definition), "3600", "60"))
	d := Reference{}.Declare()
	d.CatchupLimit = 100
	out, err := (Reference{Policy: d}).Plan(t.Context(), in)
	require.NoError(t, err)
	require.Len(t, out.Launch, 100)
	require.Equal(t, in.Now.Add(-99*time.Minute), out.Launch[0])
	in.Job.Definition = []byte(strings.ReplaceAll(string(in.Job.Definition), `"all"`, `"skip"`))
	d.ToleranceSeconds = 120
	out, err = (Reference{Policy: d}).Plan(t.Context(), in)
	require.NoError(t, err)
	require.Len(t, out.Launch, 3, "inclusive tolerance boundary")
	d.CatchupWindowSeconds = 86400
	in.Job.Definition = []byte(strings.ReplaceAll(string(in.Job.Definition), `"skip"`, `"all"`))
	in.Now = in.Job.CreatedAt.Add(48 * time.Hour)
	out, err = (Reference{Policy: d}).Plan(t.Context(), in)
	require.NoError(t, err)
	require.Len(t, out.Launch, 100)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = (Reference{}).Plan(ctx, in)
	require.ErrorIs(t, err, context.Canceled)
}

func TestExternalConsumer(t *testing.T) {
	root, err := filepath.Abs("../../..")
	require.NoError(t, err)
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/adapter\n\ngo 1.27.3\n\nrequire go.kenn.io/kata v0.0.0\nreplace go.kenn.io/kata => "+root+"\n"), 0600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "consumer_test.go"), []byte(`package adapter_test
import("context";"testing";_ "time/tzdata";c "go.kenn.io/kata/pkg/cron/conformance")
type adapter struct{c.Reference}
func(a adapter)Plan(ctx context.Context,in c.PlanInput)(c.PlanOutput,error){return a.Reference.Plan(ctx,in)}
func TestConsumer(t *testing.T){ _=c.Job{};_=c.WorkflowStep{};_=c.Attempt{};_=c.Checkpoint{};c.Run(t,adapter{}) }
`), 0600))
	cmd := exec.CommandContext(t.Context(), "go", "test", "-mod=mod", "./...")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=-p=2 -trimpath")
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, string(output))
}

func TestClockRollbackRetainsFuturePending(t *testing.T) {
	in := basePlan()
	in.Now = in.Job.CreatedAt.Add(2 * time.Hour)
	in.Checkpoint = &Checkpoint{Through: in.Job.CreatedAt.Add(3 * time.Hour), DefinitionEventUID: in.Job.DefinitionEventUID, Pending: []time.Time{in.Job.CreatedAt.Add(3 * time.Hour)}}
	out, err := (Reference{}).Plan(t.Context(), in)
	require.NoError(t, err)
	require.Empty(t, out.Launch, "clock rollback must not launch a future pending occurrence")
	require.Equal(t, in.Checkpoint.Pending, out.Checkpoint.Pending)
}

func TestRetryProperties(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		retries := hegel.Draw(ht, hegel.Integers(0, 10))
		unknownAt := hegel.Draw(ht, hegel.Integers(0, retries+1))
		start := instant("2030-01-01T00:00:00Z")
		attempts := []Attempt{}
		for i := 0; i < retries+2; i++ {
			status := "failed"
			if i == unknownAt {
				status = "unknown"
			}
			attempts = append(attempts, Attempt{StartedAt: start.Add(time.Duration(i*2) * time.Second), EndedAt: start.Add(time.Duration(i*2+1) * time.Second), Status: status})
		}
		in := SettleInput{StartedAt: start, Now: start.Add(time.Hour), Steps: []WorkflowStep{{Key: "a", Retries: retries}}, Outcomes: map[string][]Attempt{"a": attempts}}
		out, err := (Reference{}).Settle(ht.Context(), in)
		require.NoError(ht, err)
		require.LessOrEqual(ht, out.Attempts["a"], retries+1)
		if unknownAt <= retries {
			require.Equal(ht, unknownAt+1, out.Attempts["a"])
			require.Equal(ht, "unknown", out.Status)
			require.True(ht, out.Occupied)
		} else {
			require.Equal(ht, "failed", out.Status)
		}
	})
}

func TestAlternativeDeclarationSuite(t *testing.T) {
	d := Reference{}.Declare()
	d.CatchupLimit = 100
	d.CatchupWindowSeconds = 86400
	d.ToleranceSeconds = 120
	d.FailurePolicy = "stop"
	suite, err := Suite(t.Context(), d)
	require.NoError(t, err)
	answers := []Answer{}
	for _, s := range suite.Scenarios {
		a, err := Execute(t.Context(), Reference{Policy: d}, s)
		require.NoError(t, err)
		answers = append(answers, a)
		if s.Name == "retries/independent-continue" {
			var got SettleOutput
			require.NoError(t, json.Unmarshal(a.Outputs[0], &got))
			require.Equal(t, 0, got.Attempts["b"])
			require.Equal(t, []string{"b"}, got.Skipped)
		}
	}
	report, err := Check(t.Context(), d, answers)
	require.NoError(t, err)
	require.Empty(t, report.Failed)
}

func TestPendingMustBeScheduleInstants(t *testing.T) {
	in := basePlan()
	in.Checkpoint = &Checkpoint{Through: in.Now, DefinitionEventUID: in.Job.DefinitionEventUID, Pending: []time.Time{in.Job.CreatedAt.Add(30 * time.Minute)}}
	_, err := (Reference{}).Plan(t.Context(), in)
	require.Error(t, err, "retained pending is required to contain schedule instants")
}

func TestAgeWindowBoundary(t *testing.T) {
	in := basePlan()
	in.Now = in.Job.CreatedAt.Add(48 * time.Hour)
	d := Reference{}.Declare()
	d.CatchupLimit = 0
	d.CatchupWindowSeconds = 86400
	out, err := (Reference{Policy: d}).Plan(t.Context(), in)
	require.NoError(t, err)
	require.Len(t, out.Launch, 25)
	require.Equal(t, in.Now.Add(-24*time.Hour), out.Launch[0])
}

func TestStopPolicyKeepsAlreadyStartedIndependentWork(t *testing.T) {
	start := instant("2030-01-01T00:00:00Z")
	d := Reference{}.Declare()
	d.FailurePolicy = "stop"
	for _, reverse := range []bool{false, true} {
		for _, begins := range []int{1, 5, 7} {
			steps := []WorkflowStep{{Key: "a"}, {Key: "b"}}
			if reverse {
				slices.Reverse(steps)
			}
			in := SettleInput{StartedAt: start, Now: start.Add(20 * time.Second), StoppedWithCertainty: true, Steps: steps, Outcomes: map[string][]Attempt{
				"a": {{StartedAt: start, EndedAt: start.Add(5 * time.Second), Status: "failed"}},
				"b": {{StartedAt: start.Add(time.Duration(begins) * time.Second), EndedAt: start.Add(10 * time.Second), Status: "succeeded"}},
			}}
			out, err := (Reference{Policy: d}).Settle(t.Context(), in)
			require.NoError(t, err)
			if begins < 5 {
				require.Equal(t, 1, out.Attempts["b"])
				require.Empty(t, out.Skipped)
			} else {
				require.Equal(t, 0, out.Attempts["b"])
				require.Equal(t, []string{"b"}, out.Skipped)
			}
		}
	}
}

func TestReviewSettlementBoundaries(t *testing.T) {
	start := instant("2030-01-01T00:00:00Z")
	for _, zeroDuration := range []bool{false, true} {
		d := Reference{}.Declare()
		d.FailurePolicy = "stop"
		in := SettleInput{StartedAt: start, Now: start.Add(10 * time.Second), TimeoutSeconds: 5, Steps: []WorkflowStep{{Key: "a"}}, StoppedWithCertainty: true}
		want := 0
		if zeroDuration {
			in.Outcomes = map[string][]Attempt{"a": {{StartedAt: start, EndedAt: start, Status: "failed"}}}
			want = 1
		}
		out, err := (Reference{Policy: d}).Settle(t.Context(), in)
		require.NoError(t, err)
		require.Equal(t, "failed", out.Status)
		require.Equal(t, want, out.Attempts["a"])
	}
	for _, cancel := range []bool{false, true} {
		in := SettleInput{StartedAt: start, Now: start.Add(10 * time.Second), TimeoutSeconds: 5, Steps: []WorkflowStep{{Key: "a", Retries: 1}}, Outcomes: map[string][]Attempt{"a": {{StartedAt: start, EndedAt: start.Add(time.Second), Status: "failed"}}}}
		want := "failed"
		if cancel {
			at := start.Add(4 * time.Second)
			in.CancelledAt = &at
			want = "cancelled"
		}
		out, err := (Reference{}).Settle(t.Context(), in)
		require.NoError(t, err)
		require.Equal(t, want, out.Status)
		require.False(t, out.Occupied, "completed failed process cannot create uncertainty in a retry gap")
	}
}

func TestCheckerRejectsFractionalCountsAndNormalizesInstants(t *testing.T) {
	scenarios, err := Scenarios()
	require.NoError(t, err)
	answers := []Answer{}
	for _, s := range scenarios {
		a, err := Execute(t.Context(), Reference{}, s)
		require.NoError(t, err)
		answers = append(answers, a)
	}
	for i, s := range scenarios {
		for j, step := range s.Steps {
			if step.Plan != nil {
				var out PlanOutput
				require.NoError(t, json.Unmarshal(answers[i].Outputs[j], &out))
				zone := time.FixedZone("example", 3600)
				out.Checkpoint.Through = out.Checkpoint.Through.In(zone)
				for k := range out.Launch {
					out.Launch[k] = out.Launch[k].In(zone)
				}
				for k := range out.Checkpoint.Pending {
					out.Checkpoint.Pending[k] = out.Checkpoint.Pending[k].In(zone)
				}
				answers[i].Outputs[j], err = json.Marshal(out)
				require.NoError(t, err)
			}
		}
	}
	report, err := Check(t.Context(), Reference{}.Declare(), answers)
	require.NoError(t, err)
	require.Empty(t, report.Failed, "equivalent offsets identify equal instants")
	for i, s := range scenarios {
		if s.Name == "retries/success" {
			answers[i].Outputs[0] = []byte(`{"attempts":{"a":1.0000000000000001},"skipped":[],"status":"succeeded","occupied":false}`)
		}
	}
	report, err = Check(t.Context(), Reference{}.Declare(), answers)
	require.NoError(t, err)
	require.NotEmpty(t, report.Failed, "attempt counts must remain integers")
}

func TestHarnessSelection(t *testing.T) {
	if os.Getenv("KATA_CONFORMANCE_FILTER_CHILD") == "1" {
		Run(t, Reference{})
		return
	}
	for _, pattern := range []string{"^TestHarnessSelection$/^does-not-exist$", "^TestHarnessSelection$/^schedule$/^interval-grid$"} {
		cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run="+pattern)
		cmd.Env = append(os.Environ(), "KATA_CONFORMANCE_FILTER_CHILD=1")
		output, err := cmd.CombinedOutput()
		require.NoError(t, err, string(output))
	}
}

func TestDistantAnchorAndPendingNormalization(t *testing.T) {
	in := basePlan()
	in.Job.CreatedAt = instant("1700-01-01T00:00:00Z")
	in.InstalledAt = instant("2026-11-04T00:00:00Z")
	in.Now = in.InstalledAt.Add(time.Minute)
	in.Job.Definition = []byte(strings.ReplaceAll(string(in.Job.Definition), "3600", "60"))
	out, err := (Reference{}).Plan(t.Context(), in)
	require.NoError(t, err)
	require.Equal(t, []time.Time{in.Now}, out.Launch)
	in = basePlan()
	in.Checkpoint = &Checkpoint{Through: in.Now, DefinitionEventUID: in.Job.DefinitionEventUID, Pending: []time.Time{in.Job.CreatedAt.Add(2 * time.Hour), in.Job.CreatedAt.Add(time.Hour), in.Job.CreatedAt.Add(time.Hour)}}
	out, err = (Reference{}).Plan(t.Context(), in)
	require.NoError(t, err)
	require.Equal(t, []time.Time{in.Job.CreatedAt.Add(time.Hour), in.Job.CreatedAt.Add(2 * time.Hour)}, out.Launch)
}

func TestAllRetentionIncludesFuturePending(t *testing.T) {
	in := basePlan()
	in.Job.Definition = []byte(strings.ReplaceAll(string(in.Job.Definition), "3600", "60"))
	in.Now = in.Job.CreatedAt.Add(100 * time.Minute)
	in.ActiveRuns = 1
	var def map[string]any
	require.NoError(t, json.Unmarshal(in.Job.Definition, &def))
	def["overlap"] = "forbid"
	in.Job.Definition, _ = json.Marshal(def)
	pending := []time.Time{}
	for i := 1; i <= 101; i++ {
		pending = append(pending, in.Job.CreatedAt.Add(time.Duration(i)*time.Minute))
	}
	in.Checkpoint = &Checkpoint{Through: in.Job.CreatedAt.Add(101 * time.Minute), DefinitionEventUID: in.Job.DefinitionEventUID, Pending: pending}
	d := Reference{}.Declare()
	d.CatchupLimit = 100
	out, err := (Reference{Policy: d}).Plan(t.Context(), in)
	require.NoError(t, err)
	require.Equal(t, pending[1:], out.Checkpoint.Pending)
}

func TestPublicEntryPointsRejectInvalidValues(t *testing.T) {
	in := basePlan()
	_, err := (Reference{Policy: Declaration{ToleranceSeconds: 999}}).Plan(t.Context(), in)
	require.Error(t, err)
	scenario := Scenario{Name: "invalid", Steps: []Decision{{Plan: &in, Settle: &SettleInput{}}}}
	_, err = Execute(t.Context(), Reference{}, scenario)
	require.Error(t, err)
}

func TestReviewRetryGap(t *testing.T) {
	start := instant("2030-01-01T00:00:00Z")
	in := SettleInput{StartedAt: start, Now: start.Add(10 * time.Second), TimeoutSeconds: 5, Steps: []WorkflowStep{{Key: "a", Retries: 1}}, Outcomes: map[string][]Attempt{"a": {{StartedAt: start, EndedAt: start.Add(time.Second), Status: "failed"}}}}
	out, err := (Reference{}).Settle(t.Context(), in)
	require.NoError(t, err)
	require.Equal(t, "failed", out.Status)
	require.False(t, out.Occupied)
}
func TestReviewFractionalAttemptCount(t *testing.T) {
	scenarios, err := Scenarios()
	require.NoError(t, err)
	answers := []Answer{}
	for _, s := range scenarios {
		a, err := Execute(t.Context(), Reference{}, s)
		require.NoError(t, err)
		if s.Name == "retries/success" {
			a.Outputs[0] = []byte(`{"attempts":{"a":1.0000000000000001},"skipped":[],"status":"succeeded","occupied":false}`)
		}
		answers = append(answers, a)
	}
	report, err := Check(t.Context(), Reference{}.Declare(), answers)
	require.NoError(t, err)
	require.NotEmpty(t, report.Failed)
}
func TestReviewMixedDecision(t *testing.T) {
	in := basePlan()
	_, err := Execute(t.Context(), Reference{}, Scenario{Name: "invalid", Steps: []Decision{{Plan: &in, Settle: &SettleInput{}}}})
	require.Error(t, err)
}

func TestPendingRevisionProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		mask := hegel.Draw(ht, hegel.Integers(0, 1023))
		changed := hegel.Draw(ht, hegel.Integers(0, 1)) == 1
		in := basePlan()
		in.Job.Definition = []byte(strings.ReplaceAll(string(in.Job.Definition), "3600", "60"))
		in.Now = in.Job.CreatedAt.Add(10 * time.Minute)
		pending := []time.Time{}
		for i := 0; i < 10; i++ {
			if mask&(1<<i) != 0 {
				pending = append(pending, in.Job.CreatedAt.Add(time.Duration(i+1)*time.Minute))
			}
		}
		in.Checkpoint = &Checkpoint{Through: in.Now, DefinitionEventUID: in.Job.DefinitionEventUID, Pending: pending}
		want := pending
		if changed {
			in.Job.DefinitionEventUID = "01ARZ3NDEKTSV4RRFFQ69G5FAA"
			want = []time.Time{}
		}
		out, err := (Reference{}).Plan(ht.Context(), in)
		require.NoError(ht, err)
		require.Equal(ht, want, out.Launch)
		require.Equal(ht, in.Now, out.Checkpoint.Through)
	})
}

func TestSkipBoundaryProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		grace := hegel.Draw(ht, hegel.Integers(0, 86400))
		tolerance := hegel.Draw(ht, hegel.Integers(0, 120))
		delta := hegel.Draw(ht, hegel.Integers(-1, 1))
		in := basePlan()
		at := in.Job.CreatedAt.Add(time.Hour)
		in.Now = at.Add(time.Duration(grace+tolerance)*time.Second + time.Duration(delta))
		var def map[string]any
		require.NoError(ht, json.Unmarshal(in.Job.Definition, &def))
		def["trigger"] = map[string]any{"kind": "once", "at": at.Format(time.RFC3339)}
		def["catchup"] = "skip"
		def["grace_seconds"] = grace
		in.Job.Definition, _ = json.Marshal(def)
		in.Checkpoint = &Checkpoint{Through: at, DefinitionEventUID: in.Job.DefinitionEventUID, Pending: []time.Time{at}}
		d := Reference{}.Declare()
		d.ToleranceSeconds = int64(tolerance)
		out, err := (Reference{Policy: d}).Plan(ht.Context(), in)
		require.NoError(ht, err)
		count := len(out.Launch) + len(out.Checkpoint.Pending)
		if delta <= 0 {
			require.Equal(ht, 1, count)
		} else {
			require.Zero(ht, count)
		}
	})
}

func TestAgeWindowBoundaryProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		window := hegel.Draw(ht, hegel.Integers(86400, 172800))
		delta := hegel.Draw(ht, hegel.Integers(-1, 1))
		in := basePlan()
		at := in.Job.CreatedAt.Add(time.Hour)
		in.Now = at.Add(time.Duration(window)*time.Second + time.Duration(delta))
		var def map[string]any
		require.NoError(ht, json.Unmarshal(in.Job.Definition, &def))
		def["trigger"] = map[string]any{"kind": "once", "at": at.Format(time.RFC3339)}
		in.Job.Definition, _ = json.Marshal(def)
		in.Checkpoint = &Checkpoint{Through: at, DefinitionEventUID: in.Job.DefinitionEventUID, Pending: []time.Time{at}}
		d := Reference{}.Declare()
		d.CatchupLimit = 0
		d.CatchupWindowSeconds = int64(window)
		out, err := (Reference{Policy: d}).Plan(ht.Context(), in)
		require.NoError(ht, err)
		if delta <= 0 {
			require.Equal(ht, []time.Time{at}, out.Launch)
		} else {
			require.Empty(ht, out.Launch)
		}
	})
}

func TestEarliestCutoffProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		timeout := hegel.Draw(ht, hegel.Integers(1, 3600))
		cancel := hegel.Draw(ht, hegel.Integers(1, 3600))
		delta := hegel.Draw(ht, hegel.Integers(-1, 1))
		certain := hegel.Draw(ht, hegel.Integers(0, 1)) == 1
		start := instant("2030-01-01T00:00:00Z")
		cancelledAt := start.Add(time.Duration(cancel) * time.Second)
		in := SettleInput{StartedAt: start, Now: start.Add(time.Duration(max(timeout, cancel)+1) * time.Second), TimeoutSeconds: int64(timeout), CancelledAt: &cancelledAt, StoppedWithCertainty: certain, Steps: []WorkflowStep{{Key: "a"}}, Outcomes: map[string][]Attempt{"a": {{StartedAt: start, EndedAt: start.Add(time.Duration(min(timeout, cancel)+delta) * time.Second), Status: "succeeded"}}}}
		out, err := (Reference{}).Settle(ht.Context(), in)
		require.NoError(ht, err)
		want := "succeeded"
		if delta >= 0 {
			want = "failed"
			if cancel < timeout {
				want = "cancelled"
			}
			if !certain {
				want = "unknown"
			}
		}
		require.Equal(ht, want, out.Status)
		require.Equal(ht, want == "unknown", out.Occupied)
	})
}

func TestRetriedDependencyProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		retries := hegel.Draw(ht, hegel.Integers(0, 5))
		failures := hegel.Draw(ht, hegel.Integers(0, retries))
		unknown := hegel.Draw(ht, hegel.Integers(0, 1)) == 1
		start := instant("2030-01-01T00:00:00Z")
		attempts := []Attempt{}
		for i := 0; i <= failures; i++ {
			status := "failed"
			if i == failures {
				status = "succeeded"
				if unknown {
					status = "unknown"
				}
			}
			attempts = append(attempts, Attempt{StartedAt: start.Add(time.Duration(i*2) * time.Second), EndedAt: start.Add(time.Duration(i*2+1) * time.Second), Status: status})
		}
		in := SettleInput{StartedAt: start, Now: start.Add(time.Hour), Steps: []WorkflowStep{{Key: "a", Retries: retries}, {Key: "b", After: []string{"a"}}}, Outcomes: map[string][]Attempt{"a": attempts, "b": {{StartedAt: start.Add(time.Duration(failures*2+2) * time.Second), EndedAt: start.Add(time.Duration(failures*2+3) * time.Second), Status: "succeeded"}}}}
		out, err := (Reference{}).Settle(ht.Context(), in)
		require.NoError(ht, err)
		require.Equal(ht, failures+1, out.Attempts["a"])
		if unknown {
			require.Equal(ht, "unknown", out.Status)
			require.Equal(ht, 0, out.Attempts["b"])
		} else {
			require.Equal(ht, "succeeded", out.Status)
			require.Equal(ht, 1, out.Attempts["b"])
		}
	})
}
