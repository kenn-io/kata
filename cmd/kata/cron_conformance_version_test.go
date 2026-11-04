package main

import (
	"encoding/json/v2"
	"os"
	"path/filepath"
	"testing"

	"go.kenn.io/kata/pkg/cron/conformance"
)

func TestCronConformanceV2(t *testing.T) {
	dir := t.TempDir()
	if _, err := runConformance(t.Context(), t, nil, "export", "--schema", conformance.SchemaV2, "--dir", dir); err != nil {
		t.Fatal(err)
	}
	// #nosec G304 -- This test reads the manifest it just exported into t.TempDir.
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
	first, err := json.Marshal(header)
	if err != nil {
		t.Fatal(err)
	}
	lines := append(first, '\n')
	for _, s := range suite.Scenarios {
		a, err := conformance.Execute(t.Context(), conformance.Reference{Policy: header.Declaration}, s)
		if err != nil {
			t.Fatal(err)
		}
		encoded, err := json.Marshal(a)
		if err != nil {
			t.Fatal(err)
		}
		lines = append(append(lines, encoded...), '\n')
	}
	output, err := runConformance(t.Context(), t, lines, "check", "--answers", "-")
	if err != nil {
		t.Fatal(err)
	}
	var report conformance.Report
	if err := json.Unmarshal(output, &report); err != nil {
		t.Fatal(err)
	}
	if report.Schema != conformance.SchemaV2 || len(report.Failed) != 0 || report.Passed != len(suite.Scenarios) {
		t.Fatalf("%s", output)
	}
}
