package conformance

import (
	"context"
	"encoding/json/jsontext"
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
			for _, mode := range []string{"queue", "drop_after_grace"} {
				t.Run(fmt.Sprintf("%s/%v/%s", policy, record, mode), func(t *testing.T) {
					d := Reference{}.Declare()
					d.FailurePolicy = policy
					d.DropRecords = record
					d.ForbidDeferral = mode
					suite, err := NewSuite(t.Context(), SchemaV2, d)
					if err != nil {
						t.Fatal(err)
					}
					ref := Reference{Policy: d}
					RunSuite(t, ref, suite)
					answers := []Answer{}
					workflows := 0
					for _, s := range suite.Scenarios {
						if strings.HasPrefix(s.Name, "workflow/") {
							workflows++
						}
						a, err := Execute(t.Context(), skipDependents{Reference: ref}, s)
						if err != nil {
							t.Fatal(err)
						}
						answers = append(answers, a)
					}
					if workflows != 5 {
						t.Fatalf("workflow cases %d, want 5", workflows)
					}
					report, err := suite.Check(t.Context(), answers)
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
					valid := []Answer{}
					for _, s := range suite.Scenarios {
						a, err := Execute(t.Context(), ref, s)
						if err != nil {
							t.Fatal(err)
						}
						valid = append(valid, a)
					}
					for name, answers := range map[string][]Answer{"missing": valid[1:], "duplicate": append(append([]Answer{}, valid...), valid[0]), "extra": append(append([]Answer{}, valid...), Answer{Name: "unknown", Outputs: []jsontext.Value{}})} {
						t.Run(name, func(t *testing.T) {
							report, err := suite.Check(t.Context(), answers)
							if err != nil || len(report.Failed) == 0 {
								t.Fatalf("%+v %v", report, err)
							}
						})
					}
				})
			}
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
			a, err := json.Marshal(s)
			if err != nil {
				t.Fatal(err)
			}
			b, err := json.Marshal(v2.Scenarios[i])
			if err != nil {
				t.Fatal(err)
			}
			if string(a) != string(b) {
				t.Fatalf("v2 changed %s", s.Name)
			}
		}
	}
}
