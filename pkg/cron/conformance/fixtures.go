package conformance

import (
	"embed"
	"encoding/json/v2"
	"fmt"
	"io/fs"
	"strings"
	"time"
)

//go:embed testdata
var fixtures embed.FS

type bundle struct {
	Schema      string       `json:"schema"`
	Declaration *Declaration `json:"declaration,omitempty"`
	Scenarios   []Scenario   `json:"scenarios"`
}

// Load strictly decodes a frozen v1 bundle, including every input and expected
// output. The returned scenarios are independent of later loads.
func Load(raw []byte) ([]Scenario, error) {
	b, err := loadBundle(raw)
	return b.Scenarios, err
}

func loadBundle(raw []byte) (bundle, error) { return loadBundleVersion(raw, Schema) }

func loadBundleVersion(raw []byte, schema string) (bundle, error) {
	var b bundle
	if err := json.Unmarshal(raw, &b, json.RejectUnknownMembers(true)); err != nil {
		return b, err
	}
	if b.Schema != schema || len(b.Scenarios) == 0 {
		return b, fmt.Errorf("unsupported schema or empty scenarios")
	}
	d := Reference{}.Declare()
	if b.Declaration != nil {
		d = *b.Declaration
		if err := d.Validate(); err != nil {
			return b, err
		}
	}
	seen := map[string]bool{}
	for _, s := range b.Scenarios {
		if strings.TrimSpace(s.Name) == "" || seen[s.Name] || len(s.Steps) == 0 {
			return b, fmt.Errorf("invalid or duplicate scenario %q", s.Name)
		}
		seen[s.Name] = true
		hasPlan, hasSettle := false, false
		for _, step := range s.Steps {
			if (step.Plan == nil) == (step.Settle == nil) || step.UseCheckpoint && (!hasPlan || step.Plan == nil) || step.UseOccupancy && (!hasSettle || step.Plan == nil) {
				return b, fmt.Errorf("invalid decision in %s", s.Name)
			}
			if step.Plan != nil {
				if _, err := validatePlan(*step.Plan); err != nil {
					return b, fmt.Errorf("%s: %w", s.Name, err)
				}
				out, _, err := decodePlanOutput(step.Expect, d)
				if err != nil {
					return b, err
				}
				if out.Checkpoint.Through.IsZero() || out.Checkpoint.DefinitionEventUID == "" || !ordered(out.Launch) || !ordered(out.Checkpoint.Pending) {
					return b, fmt.Errorf("invalid expected plan in %s", s.Name)
				}
				hasPlan = true
			} else {
				if err := validateSettle(*step.Settle); err != nil {
					return b, err
				}
				out, err := decodeSettleOutput(step.Expect)
				if err != nil {
					return b, err
				}
				switch out.Status {
				case "running", "succeeded", "failed", "cancelled", "unknown":
				default:
					return b, fmt.Errorf("invalid expected status in %s", s.Name)
				}
				hasSettle = true
			}
		}
	}
	return b, nil
}

func ordered(values []time.Time) bool {
	for i, at := range values {
		if at.IsZero() || i > 0 && !at.After(values[i-1]) {
			return false
		}
	}
	return true
}

func readBundles(includeDrops bool) ([]bundle, error) {
	paths, err := fs.Glob(fixtures, "testdata/adapter-v1/*.json")
	if err != nil {
		return nil, err
	}
	bundles := []bundle{}
	seen := map[string]bool{}
	for _, path := range paths {
		if !includeDrops && strings.HasSuffix(path, "/drops.json") {
			continue
		}
		raw, err := fixtures.ReadFile(path)
		if err != nil {
			return nil, err
		}
		b, err := loadBundle(raw)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		for _, s := range b.Scenarios {
			if seen[s.Name] {
				return nil, fmt.Errorf("duplicate scenario %s", s.Name)
			}
			seen[s.Name] = true
		}
		bundles = append(bundles, b)
	}
	if len(bundles) == 0 {
		return nil, fmt.Errorf("empty embedded suite")
	}
	return bundles, nil
}

// Scenarios returns a fresh copy of all frozen default-policy vectors.
func Scenarios() ([]Scenario, error) {
	bundles, err := readBundles(false)
	if err != nil {
		return nil, err
	}
	var all []Scenario
	for _, b := range bundles {
		all = append(all, b.Scenarios...)
	}
	return all, nil
}
