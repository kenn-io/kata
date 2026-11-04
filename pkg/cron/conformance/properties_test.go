package conformance

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"golang.org/x/mod/modfile"
	"hegel.dev/go/hegel"
)

func TestPlanningSetProperties(t *testing.T) {
	base := testPlan(t)
	hegel.Test(t, func(ht *hegel.T) {
		// Draw the full count domain; bound the concrete trace, not its generator.
		drawnCount := hegel.Draw(ht, hegel.Integers(0, int(^uint(0)>>1)))
		n := drawnCount%500 + 1
		drawnStep := hegel.Draw(ht, hegel.Integers[int64](1, maxSeconds))
		// Keep this materialized trace within time.Duration without narrowing
		// the schedule interval domain. One-item traces reach the full bound.
		step := 1 + (drawnStep-1)%(maxSeconds/int64(n))
		grace := hegel.Draw(ht, hegel.Integers[int64](0, maxSeconds))
		tolerance := hegel.Draw(ht, hegel.Integers(0, 120))
		limit := hegel.Draw(ht, hegel.OneOf(hegel.SampledFrom([]int{0}), hegel.Integers(100, 100000)))
		window := hegel.Draw(ht, hegel.OneOf(hegel.SampledFrom([]int64{0}), hegel.Integers[int64](86400, maxSeconds)))
		activeRuns := hegel.Draw(ht, hegel.Integers(0, int(^uint(0)>>1)))
		active := activeRuns > 0
		policy := hegel.Draw(ht, hegel.SampledFrom([]string{"all", "latest", "skip"}))
		overlap := hegel.Draw(ht, hegel.SampledFrom([]string{"allow", "forbid"}))
		deferral := hegel.Draw(ht, hegel.SampledFrom([]string{"queue", "drop_after_grace"}))
		d := Reference{}.Declare()
		d.DropRecords = true // This property checks the opt-in disposition contract.
		d.ForbidDeferral = deferral
		d.CatchupLimit = limit
		d.CatchupWindowSeconds = window
		d.ToleranceSeconds = int64(tolerance)
		in := base
		in.Job.CreatedAt = in.Job.CreatedAt.Add(time.Duration(hegel.Draw(ht, hegel.Integers(0, 999999999))))
		in.Now = in.Job.CreatedAt.Add(time.Duration(int64(n)*step) * time.Second)
		in.Job.Definition = jsontext.Value(fmt.Sprintf(`{"version":1,"kind":"job","enabled":true,"trigger":{"kind":"interval","interval_seconds":%d},"action":{"kind":"execute","prompt":"Inspect"},"issue":{"kind":"per-run","title":"Inspection"},"overlap":%q,"catchup":%q,"grace_seconds":%d}`, step, overlap, policy, grace))
		in.ActiveRuns = activeRuns
		// Materialize retained pending plus newly enumerated work on the same
		// fractional grid; trace sizes stay bounded after drawing full domains.
		splitDraw := hegel.Draw(ht, hegel.Integers(0, int(^uint(0)>>1)))
		split := splitDraw % (n + 1)
		pending := []time.Time{}
		for i := 1; i <= split; i++ {
			pending = append(pending, in.Job.CreatedAt.Add(time.Duration(int64(i)*step)*time.Second))
		}
		in.Checkpoint = &Checkpoint{Through: in.Job.CreatedAt.Add(time.Duration(int64(split)*step) * time.Second), DefinitionEventUID: in.Job.DefinitionEventUID, Pending: pending}
		out, drops, err := (Reference{Policy: d}).PlanWithDrops(ht.Context(), in)
		if err != nil {
			ht.Fatal(err)
		}
		candidates := []time.Time{}
		selected := []time.Time{}
		for i := 1; i <= n; i++ {
			at := in.Job.CreatedAt.Add(time.Duration(int64(i)*step) * time.Second)
			candidates = append(candidates, at)
			keep := policy == "all" || policy == "latest" && i == n || policy == "skip" && int64(n-i)*step <= grace+int64(tolerance)
			if keep && (window == 0 || int64(n-i)*step <= window) {
				selected = append(selected, at)
			}
		}
		if limit > 0 && len(selected) > limit {
			selected = selected[len(selected)-limit:]
		}
		expectedCatchup := []time.Time{}
		expectedOverlap := []time.Time{}
		dispositions := map[time.Time]int{}
		for _, at := range candidates {
			found := false
			for _, kept := range selected {
				if kept.Equal(at) {
					found = true
					break
				}
			}
			if !found {
				expectedCatchup = append(expectedCatchup, at)
			}
		}
		for _, at := range selected {
			if active && overlap == "forbid" && deferral == "drop_after_grace" && int64(in.Now.Sub(at)/time.Second) > grace+int64(tolerance) {
				expectedOverlap = append(expectedOverlap, at)
			}
		}
		for _, at := range out.Launch {
			dispositions[at]++
		}
		for _, at := range out.Checkpoint.Pending {
			dispositions[at]++
		}
		groups := map[string][]time.Time{}
		for _, drop := range drops {
			groups[drop.Reason] = drop.Instants
			for _, at := range drop.Instants {
				dispositions[at]++
			}
		}
		for _, tc := range []struct {
			reason string
			want   []time.Time
		}{{"catchup_missed", expectedCatchup}, {"overlap_skipped", expectedOverlap}} {
			got := groups[tc.reason]
			if len(got) != len(tc.want) {
				ht.Fatalf("%s got %v want %v", tc.reason, got, tc.want)
			}
			for i := range got {
				if !got[i].Equal(tc.want[i]) {
					ht.Fatalf("wrong %s partition", tc.reason)
				}
			}
		}
		if len(dispositions) != n {
			ht.Fatal("lost or invented candidate")
		}
		for _, at := range candidates {
			if dispositions[at] != 1 {
				ht.Fatal("dispositions overlap")
			}
		}
		wantLaunch, wantPending := []time.Time{}, []time.Time{}
		for _, at := range selected {
			if active && overlap == "forbid" && deferral == "drop_after_grace" && int64(in.Now.Sub(at)/time.Second) > grace+int64(tolerance) {
				continue
			}
			if overlap == "forbid" && (active || len(wantLaunch) > 0) {
				wantPending = append(wantPending, at)
			} else {
				wantLaunch = append(wantLaunch, at)
			}
		}
		if !reflect.DeepEqual(out.Launch, wantLaunch) || !reflect.DeepEqual(out.Checkpoint.Pending, wantPending) {
			ht.Fatalf("wrong disposition: %+v", out)
		}

	})

	t.Run("pending revision", func(t *testing.T) {
		hegel.Test(t, func(ht *hegel.T) {
			mask := hegel.Draw(ht, hegel.Integers(0, 1023))
			changed := hegel.Draw(ht, hegel.Integers(0, 1)) == 1
			in := testPlan(t)
			in.Job.Definition = []byte(strings.ReplaceAll(string(in.Job.Definition), "3600", "60"))
			in.Now = in.Job.CreatedAt.Add(10 * time.Minute)
			pending := []time.Time{}
			for i := range 10 {
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
	})
}

func TestNativeDecisionAgreement(t *testing.T) {
	base := testPlan(t)
	hegel.Test(t, func(ht *hegel.T) {
		drawnCount := hegel.Draw(ht, hegel.Integers(0, int(^uint(0)>>1)))
		minutes := drawnCount%200 + 1
		interval := hegel.Draw(ht, hegel.Integers[int64](0, maxSeconds))
		catchup := hegel.Draw(ht, hegel.SampledFrom([]string{"all", "latest", "skip"}))
		grace := hegel.Draw(ht, hegel.Integers[int64](1, maxSeconds))
		native, err := cron.ParseJob([]byte(`{"version":1,"kind":"job","enabled":true,"trigger":{"kind":"cron","cron":"* * * * *"},"action":{"kind":"notify","recipient":"operator"},"issue":{"kind":"per-run","parent_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAC","title":"Inspect"},"overlap":"allow","catchup":"all"}`))
		if err != nil {
			ht.Fatal(err)
		}
		native.Catchup = catchup
		native.GraceSeconds = grace
		period := int64(60)
		if interval > 0 {
			period = 60 + (interval-1)%(maxSeconds/int64(minutes)-59)
			native.Trigger = cron.Trigger{Kind: "interval", IntervalSeconds: period}
		}
		adapter := native
		adapter.Action = cron.Action{Kind: "execute", Prompt: "Inspect"}
		adapter.Issue = &cron.IssuePolicy{Kind: "per-run", Title: "Inspection"}
		in := base
		in.Job.Definition, err = json.Marshal(adapter)
		if err != nil {
			ht.Fatal(err)
		}
		in.Now = in.Job.CreatedAt.Add(time.Duration(int64(minutes)*period) * time.Second)
		out, err := (Reference{}).Plan(ht.Context(), in)
		if err != nil {
			ht.Fatal(err)
		}
		progress := cron.RecurringProgress{Version: 1, Anchor: in.Job.CreatedAt, Through: in.Job.CreatedAt}
		due := []time.Time{}
		// Fully drain native batches at one clock. Inputs deliberately exclude
		// pending, adapter-specific timezones, local gates and count truncation.
		for {
			batch, err := cron.PlanRecurring(native, in.Job.DefinitionEventUID, progress, in.Now)
			if err != nil {
				ht.Fatal(err)
			}
			due = append(due, batch.Due...)
			progress = batch.Progress
			if !batch.More {
				break
			}
		}
		if !reflect.DeepEqual(due, out.Launch) {
			ht.Fatalf("native %v adapter %v", due, out.Launch)
		}
	})
}

type wrongPolicyAdapter struct{ declared, actual Declaration }

func (a wrongPolicyAdapter) Declare() Declaration { return a.declared }
func (a wrongPolicyAdapter) Plan(ctx context.Context, in PlanInput) (PlanOutput, error) {
	return (Reference{Policy: a.actual}).Plan(ctx, in)
}
func (a wrongPolicyAdapter) Settle(ctx context.Context, in SettleInput) (SettleOutput, error) {
	return (Reference{Policy: a.actual}).Settle(ctx, in)
}

func TestPolicySuiteDetectsWrongAdapters(t *testing.T) {
	for _, policy := range []string{"tolerance", "count", "age", "branches"} {
		t.Run(policy, func(t *testing.T) {
			declared := Reference{}.Declare()
			declared.Name = "example-adapter"
			actual := declared
			switch policy {
			case "tolerance":
				declared.ToleranceSeconds = 120
			case "count":
				declared.CatchupLimit = 100
			case "age":
				declared.CatchupWindowSeconds = 86400
			case "branches":
				declared.FailurePolicy = "stop"
			}
			suite, err := Suite(t.Context(), declared)
			if err != nil {
				t.Fatal(err)
			}
			wanted := map[string][]string{
				"tolerance": {"schedule/policy-tolerance-boundary"},
				"count":     {"catchup/policy-count-boundary"},
				"age":       {"catchup/policy-age-window-boundary"},
				"branches":  {"retries/independent-continue"},
			}
			answers := []Answer{}
			a := wrongPolicyAdapter{declared: declared, actual: actual}
			for _, s := range suite.Scenarios {
				answer, err := Execute(t.Context(), a, s)
				if err != nil {
					t.Fatal(err)
				}
				answers = append(answers, answer)
			}
			report, err := Check(t.Context(), declared, answers)
			if err != nil {
				t.Fatal(err)
			}
			failed := map[string]bool{}
			for _, failure := range report.Failed {
				failed[failure.Name] = true
			}
			for _, name := range wanted[policy] {
				if !failed[name] {
					t.Fatalf("missing named policy failure %s: %+v", name, report)
				}
			}
		})
	}
}

func TestExternalConsumer(t *testing.T) {
	root, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	// A path with spaces exercises the quoted replacement even on a checkout
	// whose own path has none. The consumer can import only public packages.
	dir := filepath.Join(t.TempDir(), "external adapter")
	if err := os.MkdirAll(dir, 0750); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "kata source")
	if err := os.Symlink(root, link); err != nil {
		t.Skipf("directory symlink unavailable: %v", err)
	}
	// #nosec G304 -- Repository-owned module file resolved relative to this package.
	rawModule, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	parsedModule, err := modfile.Parse("go.mod", rawModule, nil)
	if err != nil {
		t.Fatal(err)
	}
	module := fmt.Sprintf("module example.com/adapter\n\ngo %s\n\nrequire go.kenn.io/kata v0.0.0\nreplace go.kenn.io/kata => %s\n", parsedModule.Go.Version, strconv.Quote(filepath.ToSlash(link)))
	// #nosec G304 -- Repository-owned checksum file resolved relative to this package.
	sums, err := os.ReadFile(filepath.Join(root, "go.sum"))
	if err != nil {
		t.Fatal(err)
	}
	// #nosec G703 -- The destination is inside this test's own temporary external module.
	if err := os.WriteFile(filepath.Join(dir, "go.sum"), sums, 0600); err != nil {
		t.Fatal(err)
	}
	// #nosec G703 -- The destination is inside this test's own temporary external module.
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(module), 0600); err != nil {
		t.Fatal(err)
	}
	source := `package adapter_test
import("context";"testing";_ "time/tzdata";c "go.kenn.io/kata/pkg/cron/conformance")
type bridge struct{c.Reference}
func(b bridge)Plan(ctx context.Context,in c.PlanInput)(c.PlanOutput,error){return b.Reference.Plan(ctx,in)}
func TestProfile(t *testing.T){
 _=c.Job{};_=c.Checkpoint{};_=c.WorkflowStep{};_=c.Attempt{};_=c.SettleInput{};_=c.SettleOutput{};_=c.Drop{};_=c.Decision{};_=c.FixtureSuite{};_=c.Failure{};_=c.Report{}
 mode:="queue";_ = c.Declaration{ForbidDeferral:mode}
 files,err:=c.ExportFiles(t.Context(),bridge{}.Declare());if err!=nil{t.Fatal(err)}
 vectors,err:=c.Load(files["schedule.json"]);if err!=nil||len(vectors)==0{t.Fatal(err)}
 c.Run(t,bridge{})
 suite,err:=c.NewSuite(t.Context(),c.SchemaV2,bridge{}.Declare());if err!=nil{t.Fatal(err)}
 c.RunSuite(t,bridge{},suite)
}`
	if err := os.WriteFile(filepath.Join(dir, "adapter_test.go"), []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), "go", "test", "-trimpath", "-mod=mod", "-run", "TestProfile", ".")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOWORK=off", "GOTOOLCHAIN=local")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("external consumer: %v\n%s", err, out)
	}
	if strings.Contains(string(out), "no test files") {
		t.Fatal("consumer did not run")
	}
}

func TestSkipBoundaryProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		grace := hegel.Draw(ht, hegel.Integers(0, 86400))
		tolerance := hegel.Draw(ht, hegel.Integers(0, 120))
		delta := hegel.Draw(ht, hegel.Integers(-1, 1))
		in := testPlan(t)
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
		in := testPlan(t)
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
	start := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name, status                                     string
		attemptStart, attemptEnd, timeout, cancel, count int
		certain                                          bool
		want                                             string
	}{
		{"no attempt at deadline", "succeeded", 5, 6, 5, 0, 0, false, "failed"},
		{"completed before equal cutoffs", "succeeded", 0, 3, 5, 5, 1, true, "succeeded"},
		{"earlier cancellation", "running", 0, 0, 5, 4, 1, true, "cancelled"},
		{"earlier timeout", "running", 0, 0, 5, 6, 1, true, "failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			attempt := Attempt{StartedAt: start.Add(time.Duration(tc.attemptStart) * time.Second), Status: tc.status}
			if tc.status != "running" {
				attempt.EndedAt = start.Add(time.Duration(tc.attemptEnd) * time.Second)
			}
			in := SettleInput{Steps: []WorkflowStep{{Key: "a"}}, Outcomes: map[string][]Attempt{"a": {attempt}}, StartedAt: start, Now: start.Add(20 * time.Second), TimeoutSeconds: int64(tc.timeout), StoppedWithCertainty: tc.certain}
			if tc.cancel > 0 {
				at := start.Add(time.Duration(tc.cancel) * time.Second)
				in.CancelledAt = &at
			}
			out, err := (Reference{}).Settle(t.Context(), in)
			require.NoError(t, err)
			require.Equal(t, tc.want, out.Status)
			require.False(t, out.Occupied)
			require.Equal(t, tc.count, out.Attempts["a"])
			require.Empty(t, out.Skipped)
		})
	}

	hegel.Test(t, func(ht *hegel.T) {
		timeout := hegel.Draw(ht, hegel.Integers(1, 3600))
		cancel := hegel.Draw(ht, hegel.Integers(1, 3600))
		delta := hegel.Draw(ht, hegel.Integers(-1, 1))
		certain := hegel.Draw(ht, hegel.Integers(0, 1)) == 1
		start := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
		cancelledAt := start.Add(time.Duration(cancel) * time.Second)
		in := SettleInput{StartedAt: start, Now: start.Add(time.Duration(max(timeout, cancel)+1) * time.Second), TimeoutSeconds: int64(timeout), CancelledAt: &cancelledAt, StoppedWithCertainty: certain, Steps: []WorkflowStep{{Key: "a"}}, Outcomes: map[string][]Attempt{"a": {{StartedAt: start, EndedAt: start.Add(time.Duration(min(timeout, cancel)+delta) * time.Second), Status: "succeeded"}}}}
		out, err := (Reference{}).Settle(ht.Context(), in)
		require.NoError(ht, err)
		want := "succeeded"
		if delta >= 0 {
			want = "failed"
			if cancel <= timeout {
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
	t.Run("retry budget", func(t *testing.T) {
		hegel.Test(t, func(ht *hegel.T) {
			retries := hegel.Draw(ht, hegel.Integers(0, 100))
			unknown := hegel.Draw(ht, hegel.Integers(0, retries+2))
			start := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
			offers := []Attempt{}
			for i := range retries + 3 {
				status := "failed"
				if i == unknown {
					status = "unknown"
				}
				offers = append(offers, Attempt{StartedAt: start.Add(time.Duration(i*2) * time.Second), EndedAt: start.Add(time.Duration(i*2+1) * time.Second), Status: status})
			}
			in := SettleInput{Steps: []WorkflowStep{{Key: "a", Retries: retries}}, Outcomes: map[string][]Attempt{"a": offers}, StartedAt: start, Now: start.Add(time.Hour)}
			out, err := (Reference{}).Settle(ht.Context(), in)
			if err != nil {
				ht.Fatal(err)
			}
			wantCount := retries + 1
			wantStatus := "failed"
			if unknown <= retries {
				wantCount = unknown + 1
				wantStatus = "unknown"
			}
			if out.Attempts["a"] != wantCount || out.Status != wantStatus {
				ht.Fatalf("retry %d unknown %d: %+v", retries, unknown, out)
			}
		})
	})
	hegel.Test(t, func(ht *hegel.T) {
		retries := hegel.Draw(ht, hegel.Integers(0, 5))
		failures := hegel.Draw(ht, hegel.Integers(0, retries))
		unknown := hegel.Draw(ht, hegel.Integers(0, 1)) == 1
		start := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
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
