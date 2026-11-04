package main

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"go.kenn.io/kata/pkg/cron/conformance"
	"os"
	"path/filepath"
	"testing"
)

func TestCronConformanceV2(t *testing.T) {
	dir := t.TempDir()
	if _, err := runConformance(t, t.Context(), nil, "export", "--schema", conformance.SchemaV2, "--dir", dir); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var header conformanceHeader
	if err := json.Unmarshal(raw, &header); err != nil {
		t.Fatal(err)
	}
	if header.Schema != conformance.SchemaV2 {
		t.Fatal(header.Schema)
	}
	suite, err := conformance.NewSuite(t.Context(), header.Schema, header.Declaration)
	if err != nil {
		t.Fatal(err)
	}
	for _, wrong := range []bool{false, true} {
		first, _ := json.Marshal(header)
		lines := append(first, '\n')
		for _, s := range suite.Scenarios {
			a, err := conformance.Execute(t.Context(), conformance.Reference{Policy: header.Declaration}, s)
			if err != nil {
				t.Fatal(err)
			}
			if wrong && s.Name == "workflow/successful-dependency" {
				var out conformance.SettleOutput
				if err := json.Unmarshal(a.Outputs[0], &out); err != nil {
					t.Fatal(err)
				}
				out.Attempts["b"] = 0
				out.Skipped = append(out.Skipped, "b")
				a.Outputs[0], _ = json.Marshal(out)
			}
			encoded, _ := json.Marshal(a)
			lines = append(append(lines, encoded...), '\n')
		}
		output, err := runConformance(t, t.Context(), lines, "check", "--answers", "-")
		if !wrong && err != nil {
			t.Fatal(err)
		}
		var cli *cliError
		if wrong && (!errors.As(err, &cli) || cli.ExitCode != 1) {
			t.Fatalf("want mismatch exit 1: %v", err)
		}
		var report conformance.Report
		if err := json.Unmarshal(output, &report); err != nil {
			t.Fatal(err)
		}
		if report.Schema != conformance.SchemaV2 {
			t.Fatal(report.Schema)
		}
		if wrong && (len(report.Failed) != 1 || report.Failed[0].Name != "workflow/successful-dependency") {
			t.Fatalf("%s", output)
		}
	}
	for _, version := range []string{"kata.cron.adapter-conformance/v3", ""} {
		input, _ := json.Marshal(conformanceHeader{Schema: version, Declaration: conformance.Reference{}.Declare()})
		_, err := runConformance(t, t.Context(), input, "check", "--answers", "-")
		var cli *cliError
		if !errors.As(err, &cli) || cli.ExitCode != 3 {
			t.Fatal(err)
		}
	}
}

// Exactly-zero counts are valid. Null is not an alternative spelling of zero.
func TestCronConformanceCountWireValues(t *testing.T) {
	original := conformanceAnswers(t, conformance.Reference{}.Declare())
	for _, tc := range []struct {
		value string
		exit  int
	}{{"null", 3}, {"0", 0}, {"1", 1}} {
		t.Run(tc.value, func(t *testing.T) {
			lines := bytes.Split(bytes.TrimSpace(original), []byte{'\n'})
			for i := 1; i < len(lines); i++ {
				var a conformance.Answer
				if err := json.Unmarshal(lines[i], &a); err != nil {
					t.Fatal(err)
				}
				if a.Name != "retries/failed-dependency" {
					continue
				}
				var fields map[string]jsontext.Value
				_ = json.Unmarshal(a.Outputs[0], &fields)
				fields["attempts"] = jsontext.Value(`{"a":1,"b":` + tc.value + `}`)
				a.Outputs[0], _ = json.Marshal(fields)
				lines[i], _ = json.Marshal(a)
			}
			_, err := runConformance(t, t.Context(), bytes.Join(lines, []byte{'\n'}), "check", "--answers", "-")
			if tc.exit == 0 {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			var cli *cliError
			if !errors.As(err, &cli) || cli.ExitCode != tc.exit {
				t.Fatalf("want exit %d, got %v", tc.exit, err)
			}
		})
	}
}
