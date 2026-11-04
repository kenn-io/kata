package conformance

import (
	"embed"
	"encoding/json/v2"
	"fmt"
	"io/fs"
	"strings"
	"time"
)

//go:embed testdata/adapter-v1/*.json
var fixtures embed.FS

type bundle struct {
	Schema    string     `json:"schema"`
	Scenarios []Scenario `json:"scenarios"`
}

// Load strictly decodes a versioned bundle. Every public input and expected
// output is decoded too, so a typo cannot silently remove fixture coverage.
func Load(raw []byte) ([]Scenario, error) {
	var b bundle
	if err := json.Unmarshal(raw, &b, json.RejectUnknownMembers(true)); err != nil {
		return nil, err
	}
	if b.Schema != Schema || len(b.Scenarios) == 0 {
		return nil, fmt.Errorf("unsupported schema or empty scenarios")
	}
	seen := map[string]bool{}
	for _, s := range b.Scenarios {
		if strings.TrimSpace(s.Name) == "" || len(s.Steps) == 0 {
			return nil, fmt.Errorf("invalid scenario")
		}
		if seen[s.Name] {
			return nil, fmt.Errorf("duplicate scenario %s", s.Name)
		}
		seen[s.Name] = true
		hasPlan, hasSettle := false, false
		for _, step := range s.Steps {
			if (step.Plan == nil) == (step.Settle == nil) || step.UseCheckpoint && (!hasPlan || step.Plan == nil) || step.UseOccupancy && (!hasSettle || step.Plan == nil) {
				return nil, fmt.Errorf("invalid decision in %s", s.Name)
			}
			if step.Plan != nil {
				hasPlan = true
				if _, err := validatePlan(*step.Plan); err != nil {
					return nil, fmt.Errorf("%s: %w", s.Name, err)
				}
				var out PlanOutput
				if err := json.Unmarshal(step.Expect, &out, json.RejectUnknownMembers(true)); err != nil {
					return nil, err
				}
				if out.Checkpoint.Through.IsZero() || out.Checkpoint.DefinitionEventUID == "" || !ordered(out.Launch) || !ordered(out.Checkpoint.Pending) {
					return nil, fmt.Errorf("invalid expected plan in %s", s.Name)
				}
			} else {
				hasSettle = true
				if err := validateSettle(*step.Settle); err != nil {
					return nil, err
				}
				var out SettleOutput
				if err := json.Unmarshal(step.Expect, &out, json.RejectUnknownMembers(true)); err != nil {
					return nil, err
				}
				if out.Status != "running" && out.Status != "succeeded" && out.Status != "failed" && out.Status != "cancelled" && out.Status != "unknown" {
					return nil, fmt.Errorf("invalid expected status")
				}
			}
		}
	}
	return b.Scenarios, nil
}
func ordered(values []time.Time) bool {
	for i, v := range values {
		if v.IsZero() || i > 0 && !v.After(values[i-1]) {
			return false
		}
	}
	return true
}

// Scenarios returns freshly decoded default-policy vectors; callers cannot
// mutate another invocation's fixtures.
func Scenarios() ([]Scenario, error) {
	paths, err := fs.Glob(fixtures, "testdata/adapter-v1/*.json")
	if err != nil {
		return nil, err
	}
	var all []Scenario
	seen := map[string]bool{}
	for _, path := range paths {
		raw, err := fixtures.ReadFile(path)
		if err != nil {
			return nil, err
		}
		scenarios, err := Load(raw)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		for _, s := range scenarios {
			if seen[s.Name] {
				return nil, fmt.Errorf("duplicate scenario %s", s.Name)
			}
			seen[s.Name] = true
			all = append(all, s)
		}
	}
	if len(all) == 0 {
		return nil, fmt.Errorf("empty embedded suite")
	}
	return all, nil
}
