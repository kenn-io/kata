package conformance

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
	_ "time/tzdata"
)

func TestFrozenReference(t *testing.T) {
	scenarios, err := Scenarios()
	if err != nil {
		t.Fatal(err)
	}
	if len(scenarios) != 54 {
		t.Fatalf("got %d default scenarios", len(scenarios))
	}
	Run(t, Reference{})
}

func TestDefaultExportBytes(t *testing.T) {
	files, err := ExportFiles(t.Context(), Reference{}.Declare())
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join("testdata", "default-export-sha256.json"))
	if err != nil {
		t.Fatal(err)
	}
	var hashes map[string]string
	if err := json.Unmarshal(raw, &hashes); err != nil {
		t.Fatal(err)
	}
	if len(files) != len(hashes) {
		t.Fatalf("files %d, want %d", len(files), len(hashes))
	}
	for name, data := range files {
		sum := sha256.Sum256(data)
		if hex.EncodeToString(sum[:]) != hashes[name] {
			t.Errorf("changed default bytes: %s", name)
		}
	}
}

func TestDeclarationContract(t *testing.T) {
	base := Reference{}.Declare()
	maximum := base
	maximum.CatchupLimit = 100001
	if err := maximum.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		field  string
		change func(*Declaration)
	}{
		{"name", func(d *Declaration) { d.Name = " " }},
		{"tolerance_seconds", func(d *Declaration) { d.ToleranceSeconds = 121 }},
		{"catchup_limit", func(d *Declaration) { d.CatchupLimit = 99 }},
		{"catchup_window_seconds", func(d *Declaration) { d.CatchupWindowSeconds = 86399 }},
		{"catchup_window_seconds", func(d *Declaration) { d.CatchupWindowSeconds = 1<<63 - 1 }},
		{"failure_policy", func(d *Declaration) { d.FailurePolicy = "" }},
		{"forbid_deferral", func(d *Declaration) { d.ForbidDeferral = "invalid" }},
	} {
		t.Run(tc.field, func(t *testing.T) {
			d := base
			tc.change(&d)
			if err := d.Validate(); err == nil || !strings.Contains(err.Error(), tc.field) {
				t.Fatalf("expected named %s error, got %v", tc.field, err)
			}
		})
	}
	for _, mode := range []string{`""`, `null`, `"invalid"`} {
		raw := fmt.Appendf(nil, `{"name":"example","failure_policy":"continue","forbid_deferral":%s}`, mode)
		var d Declaration
		if err := json.Unmarshal(raw, &d); err == nil {
			t.Errorf("accepted %s", raw)
		}
	}
	for _, raw := range []string{`{"name":"example","failure_policy":"continue"}`, `{"name":"example","failure_policy":"continue","forbid_deferral":"queue"}`} {
		var d Declaration
		if err := json.Unmarshal([]byte(raw), &d); err != nil {
			t.Fatal(err)
		}
		if err := d.Validate(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestStrictFixtureLoader(t *testing.T) {
	// Each mutation starts from the same valid independent literal.
	raw := fixtureLiteral(t, fixtureSources["schedule.json"])
	var original map[string]jsontext.Value
	if err := json.Unmarshal(raw, &original); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		mutate func(map[string]jsontext.Value)
	}{
		{"unknown", func(m map[string]jsontext.Value) { m["typo"] = jsontext.Value(`true`) }},
		{"schema", func(m map[string]jsontext.Value) { m["schema"] = jsontext.Value(`"v0"`) }},
		{"empty", func(m map[string]jsontext.Value) { m["scenarios"] = jsontext.Value(`[]`) }},
		{"duplicate", func(m map[string]jsontext.Value) {
			var ss []Scenario
			_ = json.Unmarshal(m["scenarios"], &ss)
			ss = append(ss, ss[0])
			m["scenarios"], _ = json.Marshal(ss)
		}},
		{"both inputs", func(m map[string]jsontext.Value) {
			var ss []Scenario
			_ = json.Unmarshal(m["scenarios"], &ss)
			ss[0].Steps[0].Settle = &SettleInput{}
			m["scenarios"], _ = json.Marshal(ss)
		}},
		{"missing checkpoint", func(m map[string]jsontext.Value) {
			var ss []Scenario
			_ = json.Unmarshal(m["scenarios"], &ss)
			ss[0].Steps[0].UseCheckpoint = true
			m["scenarios"], _ = json.Marshal(ss)
		}},
		{"invalid job", func(m map[string]jsontext.Value) {
			var ss []Scenario
			_ = json.Unmarshal(m["scenarios"], &ss)
			ss[0].Steps[0].Plan.Job.Definition = jsontext.Value(`{}`)
			m["scenarios"], _ = json.Marshal(ss)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := map[string]jsontext.Value{}
			for k, v := range original {
				m[k] = v
			}
			tc.mutate(m)
			b, err := json.Marshal(m)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := loadBundle(b); err == nil {
				t.Fatal("accepted invalid bundle")
			}
		})
	}
	if _, err := loadBundle(raw); err != nil {
		t.Fatal(err)
	}
}

func testPlan(t *testing.T) PlanInput {
	t.Helper()
	ss, err := Scenarios()
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range ss {
		if s.Name == "schedule/interval-grid" {
			return *s.Steps[0].Plan
		}
	}
	t.Fatal("missing interval vector")
	return PlanInput{}
}

func TestPlanningCancellationAndBounds(t *testing.T) {
	in := testPlan(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := (Reference{}).Plan(ctx, in); err == nil {
		t.Fatal("ignored cancellation")
	}
	in.Job.Definition = jsontext.Value(`{"version":1,"kind":"job","enabled":true,"trigger":{"kind":"interval","interval_seconds":1},"action":{"kind":"execute","prompt":"Inspect"},"issue":{"kind":"per-run","title":"Inspection"},"overlap":"allow","catchup":"all"}`)
	in.Now = in.Job.CreatedAt.Add(100001 * time.Second)
	if _, err := (Reference{}).Plan(t.Context(), in); err == nil || !strings.Contains(err.Error(), "100000") {
		t.Fatalf("scan bound: %v", err)
	}
}

func TestCheckerRejectsAnswerSets(t *testing.T) {
	d := Reference{}.Declare()
	suite, err := Suite(t.Context(), d)
	if err != nil {
		t.Fatal(err)
	}
	answers := make([]Answer, 0, len(suite.Scenarios))
	for _, s := range suite.Scenarios {
		a, err := Execute(t.Context(), Reference{}, s)
		if err != nil {
			t.Fatal(err)
		}
		answers = append(answers, a)
	}
	for _, tc := range []struct {
		name    string
		answers []Answer
	}{
		{"valid", answers}, {"missing", answers[1:]}, {"duplicate", append(append([]Answer{}, answers...), answers[0])},
		{"extra", append(append([]Answer{}, answers...), Answer{Name: "unknown", Outputs: []jsontext.Value{}})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			report, err := Check(t.Context(), d, tc.answers)
			if err != nil {
				t.Fatal(err)
			}
			if (len(report.Failed) == 0) != (tc.name == "valid") {
				t.Fatalf("%+v", report)
			}
		})
	}
	// A real settlement fixture with a different integer count is a mismatch.
	var settled Scenario
	for _, scenario := range suite.Scenarios {
		if scenario.Name == "retries/success" {
			settled = scenario
			break
		}
	}
	if settled.Name == "" {
		t.Fatal("missing settlement fixture")
	}
	var out SettleOutput
	if err := json.Unmarshal(settled.Steps[0].Expect, &out); err != nil {
		t.Fatal(err)
	}
	out.Attempts["a"]++
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	report, err := (FixtureSuite{Declaration: d, Scenarios: []Scenario{settled}}).Check(t.Context(), []Answer{{Name: settled.Name, Outputs: []jsontext.Value{raw}}})
	if err != nil || len(report.Failed) != 1 {
		t.Fatalf("integer comparison: %+v %v", report, err)
	}
}

func TestDropBundleUsesOwnLiterals(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "adapter-v1", "drops.json"))
	if err != nil {
		t.Fatal(err)
	}
	var b bundle
	if err := json.Unmarshal(raw, &b); err != nil {
		t.Fatal(err)
	}
	Run(t, Reference{Policy: *b.Declaration})
	suite, err := Suite(t.Context(), *b.Declaration)
	if err != nil {
		t.Fatal(err)
	}
	for _, literal := range b.Scenarios {
		found := false
		for _, s := range suite.Scenarios {
			if s.Name == literal.Name {
				found = true
				for i, step := range s.Steps {
					if !reflect.DeepEqual(step.Expect, literal.Steps[i].Expect) {
						t.Errorf("literal was rematerialized: %s", s.Name)
					}
				}
			}
		}
		if !found {
			t.Errorf("missing %s", literal.Name)
		}
	}
}

func TestMalformedLaterDecisionIsNotHiddenByMismatch(t *testing.T) {
	scenarios, err := Scenarios()
	if err != nil {
		t.Fatal(err)
	}
	var sequence Scenario
	for _, scenario := range scenarios {
		if scenario.Name == "overlap/same-clock-release" {
			sequence = scenario
			break
		}
	}
	if len(sequence.Steps) != 2 {
		t.Fatal("missing two-decision fixture")
	}
	outputs := []jsontext.Value{}
	for i, step := range sequence.Steps {
		var fields map[string]jsontext.Value
		if err := json.Unmarshal(step.Expect, &fields); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			fields["excluded"] = jsontext.Value(`"disabled"`)
		} else {
			delete(fields, "excluded")
		}
		raw, err := json.Marshal(fields)
		if err != nil {
			t.Fatal(err)
		}
		outputs = append(outputs, raw)
	}
	if report, err := (FixtureSuite{Schema: Schema, Declaration: Reference{}.Declare(), Scenarios: []Scenario{sequence}}).Check(t.Context(), []Answer{{Name: sequence.Name, Outputs: outputs}}); err == nil {
		t.Fatalf("malformed later output hidden: %+v", report)
	}
}
