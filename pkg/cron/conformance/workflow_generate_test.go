package conformance

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"os"
	"path/filepath"
	"testing"
)

// These five literal outcomes apply under both branch policies: already-started
// parents continue, and dependency success/waiting/blocking is policy-independent.
func TestWorkflowFixtureArtifacts(t *testing.T) {
	for _, policy := range []string{"continue", "stop"} {
		var scenarios []Scenario
		if err := json.Unmarshal([]byte(workflowFixtureCases), &scenarios); err != nil {
			t.Fatal(err)
		}
		d := Declaration{Name: "kata-reference", CatchupLimit: 1000, FailurePolicy: policy}
		raw, err := json.Marshal(bundle{Schema: SchemaV2, Declaration: &d, Scenarios: scenarios}, jsontext.WithIndent("  "), json.Deterministic(true))
		if err != nil {
			t.Fatal(err)
		}
		raw = append(raw, '\n')
		name := "workflow.json"
		if policy == "stop" {
			name = "workflow-stop.json"
		}
		path := filepath.Join("testdata", "adapter-v2", name)
		if *updateFixtures {
			if err := os.MkdirAll(filepath.Dir(path), 0750); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
		}
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, raw) {
			t.Fatalf("%s differs from independent literals", name)
		}
	}
}

const workflowFixtureCases = `[
{"name":"workflow/successful-dependency","steps":[{"settle":{"steps":[{"key":"a","after":[],"retries":0},{"key":"b","after":["a"],"retries":0}],"outcomes":{"a":[{"started_at":"2030-01-01T00:00:00Z","ended_at":"2030-01-01T00:00:01Z","status":"succeeded"}],"b":[{"started_at":"2030-01-01T00:00:01Z","ended_at":"2030-01-01T00:00:02Z","status":"succeeded"}]},"started_at":"2030-01-01T00:00:00Z","now":"2030-01-01T00:00:04Z","timeout_seconds":0,"cancelled_at":null,"stopped_with_certainty":true},"expect":{"attempts":{"a":1,"b":1},"skipped":[],"status":"succeeded","occupied":false}}]},
{"name":"workflow/waiting-dependency","steps":[{"settle":{"steps":[{"key":"a","after":[],"retries":0},{"key":"b","after":["a"],"retries":0}],"outcomes":{"a":[{"started_at":"2030-01-01T00:00:00Z","ended_at":"0001-01-01T00:00:00Z","status":"running"}],"b":[{"started_at":"2030-01-01T00:00:01Z","ended_at":"2030-01-01T00:00:02Z","status":"succeeded"}]},"started_at":"2030-01-01T00:00:00Z","now":"2030-01-01T00:00:04Z","timeout_seconds":0,"cancelled_at":null,"stopped_with_certainty":false},"expect":{"attempts":{"a":1,"b":0},"skipped":[],"status":"running","occupied":true}}]},
{"name":"workflow/multiple-successful-parents","steps":[{"settle":{"steps":[{"key":"a","after":[],"retries":0},{"key":"b","after":[],"retries":0},{"key":"c","after":["a","b"],"retries":0}],"outcomes":{"a":[{"started_at":"2030-01-01T00:00:00Z","ended_at":"2030-01-01T00:00:01Z","status":"succeeded"}],"b":[{"started_at":"2030-01-01T00:00:00Z","ended_at":"2030-01-01T00:00:02Z","status":"succeeded"}],"c":[{"started_at":"2030-01-01T00:00:02Z","ended_at":"2030-01-01T00:00:03Z","status":"succeeded"}]},"started_at":"2030-01-01T00:00:00Z","now":"2030-01-01T00:00:04Z","timeout_seconds":0,"cancelled_at":null,"stopped_with_certainty":true},"expect":{"attempts":{"a":1,"b":1,"c":1},"skipped":[],"status":"succeeded","occupied":false}}]},
{"name":"workflow/blocked-parent","steps":[{"settle":{"steps":[{"key":"a","after":[],"retries":0},{"key":"b","after":[],"retries":0},{"key":"c","after":["a","b"],"retries":0}],"outcomes":{"a":[{"started_at":"2030-01-01T00:00:00Z","ended_at":"2030-01-01T00:00:01Z","status":"failed"}],"b":[{"started_at":"2030-01-01T00:00:00Z","ended_at":"2030-01-01T00:00:02Z","status":"succeeded"}],"c":[{"started_at":"2030-01-01T00:00:02Z","ended_at":"2030-01-01T00:00:03Z","status":"succeeded"}]},"started_at":"2030-01-01T00:00:00Z","now":"2030-01-01T00:00:04Z","timeout_seconds":0,"cancelled_at":null,"stopped_with_certainty":true},"expect":{"attempts":{"a":1,"b":1,"c":0},"skipped":["c"],"status":"failed","occupied":false}}]},
{"name":"workflow/waiting-parent","steps":[{"settle":{"steps":[{"key":"a","after":[],"retries":0},{"key":"b","after":[],"retries":0},{"key":"c","after":["a","b"],"retries":0}],"outcomes":{"a":[{"started_at":"2030-01-01T00:00:00Z","ended_at":"2030-01-01T00:00:01Z","status":"succeeded"}],"b":[{"started_at":"2030-01-01T00:00:00Z","ended_at":"0001-01-01T00:00:00Z","status":"running"}],"c":[{"started_at":"2030-01-01T00:00:02Z","ended_at":"2030-01-01T00:00:03Z","status":"succeeded"}]},"started_at":"2030-01-01T00:00:00Z","now":"2030-01-01T00:00:04Z","timeout_seconds":0,"cancelled_at":null,"stopped_with_certainty":false},"expect":{"attempts":{"a":1,"b":1,"c":0},"skipped":[],"status":"running","occupied":true}}]}
]`
