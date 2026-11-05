package conformance

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"strings"
	"testing"
)

type skipDependents struct{ Reference }

func (a skipDependents) Settle(ctx context.Context, in SettleInput) (SettleOutput, error) {
	out, err := a.Reference.Settle(ctx, in)
	if err != nil {
		return out, err
	}
	for _, step := range in.Steps {
		if len(step.After) > 0 {
			out.Attempts[step.Key] = 0
			out.Skipped = append(out.Skipped, step.Key)
		}
	}
	return out, nil
}

func TestVersionedWorkflowCoverage(t *testing.T) {
	for _, policy := range []string{"continue", "stop"} {
		for _, record := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/%v", policy, record), func(t *testing.T) {
				d := Reference{}.Declare()
				d.FailurePolicy = policy
				d.DropRecords = record
				suite, err := NewSuite(t.Context(), SchemaV2, d)
				if err != nil {
					t.Fatal(err)
				}
				files, err := suite.ExportFiles(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				if _, ok := files["drops.json"]; ok != record {
					t.Fatalf("drop bundle present=%v, recording=%v", ok, record)
				}
				for name, raw := range files {
					var header struct {
						Schema string `json:"schema"`
					}
					if err := json.Unmarshal(raw, &header); err != nil {
						t.Fatal(err)
					}
					if header.Schema != SchemaV2 {
						t.Fatalf("%s schema %s", name, header.Schema)
					}
				}
				workflows := FixtureSuite{Schema: suite.Schema, Declaration: d}
				for _, s := range suite.Scenarios {
					if strings.HasPrefix(s.Name, "workflow/") {
						workflows.Scenarios = append(workflows.Scenarios, s)
					}
				}
				ref := Reference{Policy: d}
				RunSuite(t, ref, workflows)
				answers := []Answer{}
				for _, s := range workflows.Scenarios {
					a, err := Execute(t.Context(), skipDependents{Reference: ref}, s)
					if err != nil {
						t.Fatal(err)
					}
					answers = append(answers, a)
				}
				report, err := workflows.Check(t.Context(), answers)
				if err != nil {
					t.Fatal(err)
				}
				named := false
				for _, failure := range report.Failed {
					if failure.Name == "workflow/successful-dependency" {
						named = true
					}
				}
				if !named {
					t.Fatalf("skip-dependent bridge passed: %+v", report)
				}
			})
		}
	}
}

func TestFixtureVersionsAreExplicit(t *testing.T) {
	for _, schema := range []string{"", "kata.cron.adapter-conformance/v3"} {
		if _, err := NewSuite(t.Context(), schema, Reference{}.Declare()); err == nil {
			t.Fatalf("accepted %q", schema)
		}
	}
	raw, err := fixtures.ReadFile("testdata/adapter-v1/retries.json")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := loadBundleVersion(raw, SchemaV2); err == nil {
		t.Fatal("accepted v1 bundle as v2")
	}
}

func TestV2PreservesV1Decisions(t *testing.T) {
	for _, policy := range []string{"continue", "stop"} {
		d := Reference{}.Declare()
		d.FailurePolicy = policy
		v1, err := Suite(t.Context(), d)
		if err != nil {
			t.Fatal(err)
		}
		v2, err := NewSuite(t.Context(), SchemaV2, d)
		if err != nil {
			t.Fatal(err)
		}
		if len(v2.Scenarios) != len(v1.Scenarios)+5 {
			t.Fatal("v2 must add only workflow vectors")
		}
		for i, s := range v1.Scenarios {
			a, err := json.Marshal(s, json.Deterministic(true))
			if err != nil {
				t.Fatal(err)
			}
			b, err := json.Marshal(v2.Scenarios[i], json.Deterministic(true))
			if err != nil {
				t.Fatal(err)
			}
			if string(a) != string(b) {
				t.Fatalf("v2 changed %s", s.Name)
			}
		}
	}
}
