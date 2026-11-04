package conformance

import (
	"bytes"
	"encoding/json/jsontext"
	"flag"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// These literals were independently specified before the reference model.
// $t is seconds from 2030-01-01 UTC; $range expands [start, stop) by step.
// Expected decisions are never evaluated by Reference during generation.
var updateFixtures = flag.Bool("update-golden", false, "rewrite frozen conformance artifacts from independent literals")
var fixtureToken = regexp.MustCompile(`"\$(t|range):([^" ]+)"`)

func fixtureLiteral(t *testing.T, source string) []byte {
	t.Helper()
	anchor := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	timestamp := func(v float64) string {
		return `"` + anchor.Add(time.Duration(v*float64(time.Second))).Format(time.RFC3339Nano) + `"`
	}
	source = fixtureToken.ReplaceAllStringFunc(source, func(token string) string {
		parts := strings.Split(strings.Trim(token, `"`), ":")
		values := []float64{}
		for _, part := range parts[1:] {
			v, err := strconv.ParseFloat(part, 64)
			if err != nil {
				t.Fatal(err)
			}
			values = append(values, v)
		}
		if parts[0] == "$t" {
			return timestamp(values[0])
		}
		instants := []string{}
		for v := values[0]; v < values[1]; v += values[2] {
			instants = append(instants, timestamp(v))
		}
		return "[" + strings.Join(instants, ",") + "]"
	})
	raw := jsontext.Value(source)
	if err := raw.Indent(jsontext.WithIndent("  ")); err != nil {
		t.Fatal(err)
	}
	return append(raw, '\n')
}

func TestFixtureArtifacts(t *testing.T) {
	for name, source := range fixtureSources {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join("testdata", "adapter-v1", name)
			want := fixtureLiteral(t, source)
			if *updateFixtures {
				if err := os.MkdirAll(filepath.Dir(path), 0750); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, want, 0600); err != nil {
					t.Fatal(err)
				}
			}
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want) {
				t.Fatal("fixture differs from independent source; regenerate with -update-golden")
			}
		})
	}
	path := filepath.Join("testdata", "default-export-sha256.json")
	want := []byte(defaultExportHashes + "\n")
	if *updateFixtures {
		if err := os.WriteFile(path, want, 0600); err != nil {
			t.Fatal(err)
		}
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("default export fingerprints changed")
	}
}

// Identical decisions share their literal source while retaining every frozen name.
const intervalGridDecision = `{"plan":{"adapter":"example-adapter","job":{"created_at":"$t:0","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAB","deleted":false,"definition":{"version":1,"kind":"job","enabled":true,"trigger":{"kind":"interval","interval_seconds":3600},"action":{"kind":"execute","prompt":"Inspect"},"issue":{"kind":"per-run","title":"Inspection"},"overlap":"allow","catchup":"all"}},"installed_at":"2029-12-31T00:00:00Z","now":"$t:10800","checkpoint":null,"active_runs":0},"expect":{"launch":["$t:3600","$t:7200","$t:10800"],"checkpoint":{"through":"$t:10800","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAB","pending":[]},"excluded":""}}`

const checkpointExclusiveDecision = `{"plan":{"adapter":"example-adapter","job":{"created_at":"$t:0","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAB","deleted":false,"definition":{"version":1,"kind":"job","enabled":true,"trigger":{"kind":"interval","interval_seconds":3600},"action":{"kind":"execute","prompt":"Inspect"},"issue":{"kind":"per-run","title":"Inspection"},"overlap":"allow","catchup":"all"}},"installed_at":"2029-12-31T00:00:00Z","now":"$t:10800","checkpoint":{"through":"$t:7200","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAB","pending":[]},"active_runs":0},"expect":{"launch":["$t:10800"],"checkpoint":{"through":"$t:10800","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAB","pending":[]},"excluded":""}}`

var fixtureSources = map[string]string{
	"catchup.json": `{
"schema":"kata.cron.adapter-conformance/v1",
"scenarios":[
{"name":"catchup/all","steps":[
` + intervalGridDecision + `
]},
{"name":"catchup/latest","steps":[
{"plan":{"adapter":"example-adapter","job":{"created_at":"$t:0","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAB","deleted":false,"definition":{"version":1,"kind":"job","enabled":true,"trigger":{"kind":"interval","interval_seconds":3600},"action":{"kind":"execute","prompt":"Inspect"},"issue":{"kind":"per-run","title":"Inspection"},"overlap":"allow","catchup":"latest"}},"installed_at":"2029-12-31T00:00:00Z","now":"$t:10800","checkpoint":null,"active_runs":0},"expect":{"launch":["$t:10800"],"checkpoint":{"through":"$t:10800","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAB","pending":[]},"excluded":""}}
]},
{"name":"catchup/skip-inclusive-grace","steps":[
{"plan":{"adapter":"example-adapter","job":{"created_at":"$t:0","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAB","deleted":false,"definition":{"version":1,"kind":"job","enabled":true,"trigger":{"kind":"interval","interval_seconds":3600},"action":{"kind":"execute","prompt":"Inspect"},"issue":{"kind":"per-run","title":"Inspection"},"overlap":"allow","catchup":"skip","grace_seconds":3600}},"installed_at":"2029-12-31T00:00:00Z","now":"$t:10800","checkpoint":null,"active_runs":0},"expect":{"launch":["$t:7200","$t:10800"],"checkpoint":{"through":"$t:10800","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAB","pending":[]},"excluded":""}}
]},
{"name":"catchup/skip-zero","steps":[
{"plan":{"adapter":"example-adapter","job":{"created_at":"$t:0","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAB","deleted":false,"definition":{"version":1,"kind":"job","enabled":true,"trigger":{"kind":"interval","interval_seconds":3600},"action":{"kind":"execute","prompt":"Inspect"},"issue":{"kind":"per-run","title":"Inspection"},"overlap":"allow","catchup":"skip"}},"installed_at":"2029-12-31T00:00:00Z","now":"$t:10800","checkpoint":null,"active_runs":0},"expect":{"launch":["$t:10800"],"checkpoint":{"through":"$t:10800","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAB","pending":[]},"excluded":""}}
]},
{"name":"catchup/skip-expired","steps":[
{"plan":{"adapter":"example-adapter","job":{"created_at":"$t:0","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAB","deleted":false,"definition":{"version":1,"kind":"job","enabled":true,"trigger":{"kind":"interval","interval_seconds":3600},"action":{"kind":"execute","prompt":"Inspect"},"issue":{"kind":"per-run","title":"Inspection"},"overlap":"allow","catchup":"skip"}},"installed_at":"2029-12-31T00:00:00Z","now":"$t:12600","checkpoint":null,"active_runs":0},"expect":{"launch":[],"checkpoint":{"through":"$t:12600","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAB","pending":[]},"excluded":""}}
]},
{"name":"catchup/backward-clock","steps":[
{"plan":{"adapter":"example-adapter","job":{"created_at":"$t:0","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAB","deleted":false,"definition":{"version":1,"kind":"job","enabled":true,"trigger":{"kind":"interval","interval_seconds":3600},"action":{"kind":"execute","prompt":"Inspect"},"issue":{"kind":"per-run","title":"Inspection"},"overlap":"allow","catchup":"all"}},"installed_at":"2029-12-31T00:00:00Z","now":"$t:7200","checkpoint":{"through":"$t:10800","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAB","pending":[]},"active_runs":0},"expect":{"launch":[],"checkpoint":{"through":"$t:10800","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAB","pending":[]},"excluded":""}}
]},
{"name":"catchup/protected-newest-100","steps":[
{"plan":{"adapter":"example-adapter","job":{"created_at":"$t:0","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAB","deleted":false,"definition":{"version":1,"kind":"job","enabled":true,"trigger":{"kind":"interval","interval_seconds":60},"action":{"kind":"execute","prompt":"Inspect"},"issue":{"kind":"per-run","title":"Inspection"},"overlap":"allow","catchup":"all"}},"installed_at":"2029-12-31T00:00:00Z","now":"$t:12060","checkpoint":null,"active_runs":0},"expect":{"launch":"$range:60:12120:60","checkpoint":{"through":"$t:12060","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAB","pending":[]},"excluded":""}}
]},
{"name":"catchup/count-cap-oldest-first","steps":[
{"plan":{"adapter":"example-adapter","job":{"created_at":"$t:0","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAB","deleted":false,"definition":{"version":1,"kind":"job","enabled":true,"trigger":{"kind":"interval","interval_seconds":60},"action":{"kind":"execute","prompt":"Inspect"},"issue":{"kind":"per-run","title":"Inspection"},"overlap":"allow","catchup":"all"}},"installed_at":"2029-12-31T00:00:00Z","now":"$t:60060","checkpoint":null,"active_runs":0},"expect":{"launch":"$range:120:60120:60","checkpoint":{"through":"$t:60060","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAB","pending":[]},"excluded":""}}
]}
]} `,
	"definition-change.json": `{
"schema":"kata.cron.adapter-conformance/v1",
"scenarios":[
{"name":"definition-change/same-revision-pending-union","steps":[
{"plan":{"adapter":"example-adapter","job":{"created_at":"$t:0","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAB","deleted":false,"definition":{"version":1,"kind":"job","enabled":true,"trigger":{"kind":"interval","interval_seconds":3600},"action":{"kind":"execute","prompt":"Inspect"},"issue":{"kind":"per-run","title":"Inspection"},"overlap":"allow","catchup":"all"}},"installed_at":"2029-12-31T00:00:00Z","now":"$t:10800","checkpoint":{"through":"$t:7200","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAB","pending":["$t:3600","$t:7200"]},"active_runs":0},"expect":{"launch":["$t:3600","$t:7200","$t:10800"],"checkpoint":{"through":"$t:10800","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAB","pending":[]},"excluded":""}}
]},
{"name":"definition-change/replacement-discards-pending","steps":[
{"plan":{"adapter":"example-adapter","job":{"created_at":"$t:0","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAC","deleted":false,"definition":{"version":1,"kind":"job","enabled":true,"trigger":{"kind":"interval","interval_seconds":3600},"action":{"kind":"execute","prompt":"Inspect"},"issue":{"kind":"per-run","title":"Inspection"},"overlap":"allow","catchup":"all"}},"installed_at":"2029-12-31T00:00:00Z","now":"$t:10800","checkpoint":{"through":"$t:7200","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAB","pending":["$t:3600","$t:7200"]},"active_runs":0},"expect":{"launch":["$t:10800"],"checkpoint":{"through":"$t:10800","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAC","pending":[]},"excluded":""}}
]},
{"name":"definition-change/disabled-freezes-horizon","steps":[
{"plan":{"adapter":"example-adapter","job":{"created_at":"$t:0","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAB","deleted":false,"definition":{"version":1,"kind":"job","enabled":false,"trigger":{"kind":"interval","interval_seconds":3600},"action":{"kind":"execute","prompt":"Inspect"},"issue":{"kind":"per-run","title":"Inspection"},"overlap":"allow","catchup":"all"}},"installed_at":"2029-12-31T00:00:00Z","now":"$t:10800","checkpoint":{"through":"$t:7200","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAB","pending":["$t:3600"]},"active_runs":0},"expect":{"launch":[],"checkpoint":{"through":"$t:7200","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAB","pending":["$t:3600"]},"excluded":"disabled"}}
]},
{"name":"definition-change/disabled-edit-discards-pending","steps":[
{"plan":{"adapter":"example-adapter","job":{"created_at":"$t:0","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAC","deleted":false,"definition":{"version":1,"kind":"job","enabled":false,"trigger":{"kind":"interval","interval_seconds":3600},"action":{"kind":"execute","prompt":"Inspect"},"issue":{"kind":"per-run","title":"Inspection"},"overlap":"allow","catchup":"all"}},"installed_at":"2029-12-31T00:00:00Z","now":"$t:10800","checkpoint":{"through":"$t:7200","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAB","pending":["$t:3600"]},"active_runs":0},"expect":{"launch":[],"checkpoint":{"through":"$t:7200","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAC","pending":[]},"excluded":"disabled"}}
]},
{"name":"definition-change/delete-clears-pending","steps":[
{"plan":{"adapter":"example-adapter","job":{"created_at":"$t:0","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAB","deleted":true,"definition":{"version":1,"kind":"job","enabled":true,"trigger":{"kind":"interval","interval_seconds":3600},"action":{"kind":"execute","prompt":"Inspect"},"issue":{"kind":"per-run","title":"Inspection"},"overlap":"allow","catchup":"all"}},"installed_at":"2029-12-31T00:00:00Z","now":"$t:10800","checkpoint":{"through":"$t:7200","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAB","pending":["$t:3600"]},"active_runs":0},"expect":{"launch":[],"checkpoint":{"through":"$t:7200","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAB","pending":[]},"excluded":"deleted"}}
]},
{"name":"definition-change/resume-after-frozen-horizon","steps":[
` + checkpointExclusiveDecision + `
]}
]} `,
	"drops.json": `{
"schema":"kata.cron.adapter-conformance/v1",
"declaration":{"name":"kata-reference","tolerance_seconds":0,"catchup_limit":1000,"catchup_window_seconds":86400,"failure_policy":"continue","drop_records":true,"forbid_deferral":"drop_after_grace"},
"scenarios":[
{"name":"drops/latest","steps":[
{"plan":{"adapter":"example-adapter","job":{"created_at":"$t:0","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAB","definition":{"version":1,"kind":"job","enabled":true,"trigger":{"kind":"interval","interval_seconds":3600},"action":{"kind":"execute","prompt":"Inspect"},"issue":{"kind":"per-run","title":"Inspection"},"overlap":"allow","catchup":"latest"},"deleted":false},"installed_at":"2029-12-31T00:00:00Z","now":"$t:10800","checkpoint":null,"active_runs":0},"expect":{"launch":["$t:10800"],"checkpoint":{"through":"$t:10800","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAB","pending":[]},"excluded":"","drops":[{"reason":"catchup_missed","instants":["$t:3600","$t:7200"]}]}}
]},
{"name":"drops/skip-expired","steps":[
{"plan":{"adapter":"example-adapter","job":{"created_at":"$t:0","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAB","definition":{"version":1,"kind":"job","enabled":true,"trigger":{"kind":"interval","interval_seconds":3600},"action":{"kind":"execute","prompt":"Inspect"},"issue":{"kind":"per-run","title":"Inspection"},"overlap":"allow","catchup":"skip","grace_seconds":3600},"deleted":false},"installed_at":"2029-12-31T00:00:00Z","now":"$t:12600","checkpoint":null,"active_runs":0},"expect":{"launch":["$t:10800"],"checkpoint":{"through":"$t:12600","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAB","pending":[]},"excluded":"","drops":[{"reason":"catchup_missed","instants":["$t:3600","$t:7200"]}]}}
]},
{"name":"drops/count-cap","steps":[
{"plan":{"adapter":"example-adapter","job":{"created_at":"$t:0","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAB","definition":{"version":1,"kind":"job","enabled":true,"trigger":{"kind":"interval","interval_seconds":60},"action":{"kind":"execute","prompt":"Inspect"},"issue":{"kind":"per-run","title":"Inspection"},"overlap":"allow","catchup":"all"},"deleted":false},"installed_at":"2029-12-31T00:00:00Z","now":"$t:60060","checkpoint":null,"active_runs":0},"expect":{"launch":"$range:120:60120:60","checkpoint":{"through":"$t:60060","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAB","pending":[]},"excluded":"","drops":[{"reason":"catchup_missed","instants":["$t:60"]}]}}
]},
{"name":"drops/age-window","steps":[
{"plan":{"adapter":"example-adapter","job":{"created_at":"$t:0","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAB","definition":{"version":1,"kind":"job","enabled":true,"trigger":{"kind":"interval","interval_seconds":3600},"action":{"kind":"execute","prompt":"Inspect"},"issue":{"kind":"per-run","title":"Inspection"},"overlap":"allow","catchup":"all"},"deleted":false},"installed_at":"2029-12-31T00:00:00Z","now":"$t:172800","checkpoint":null,"active_runs":0},"expect":{"launch":"$range:86400:176400:3600","checkpoint":{"through":"$t:172800","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAB","pending":[]},"excluded":"","drops":[{"reason":"catchup_missed","instants":"$range:3600:86400:3600"}]}}
]},
{"name":"drops/combined-pending","steps":[
{"plan":{"adapter":"example-adapter","job":{"created_at":"$t:0","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAB","definition":{"version":1,"kind":"job","enabled":true,"trigger":{"kind":"interval","interval_seconds":3600},"action":{"kind":"execute","prompt":"Inspect"},"issue":{"kind":"per-run","title":"Inspection"},"overlap":"allow","catchup":"latest"},"deleted":false},"installed_at":"2029-12-31T00:00:00Z","now":"$t:10800","checkpoint":{"through":"$t:7200","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAB","pending":["$t:3600","$t:7200"]},"active_runs":0},"expect":{"launch":["$t:10800"],"checkpoint":{"through":"$t:10800","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAB","pending":[]},"excluded":"","drops":[{"reason":"catchup_missed","instants":["$t:3600","$t:7200"]}]}}
]},
{"name":"drops/held-late","steps":[
{"plan":{"adapter":"example-adapter","job":{"created_at":"$t:0","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAB","definition":{"version":1,"kind":"job","enabled":true,"trigger":{"kind":"interval","interval_seconds":3600},"action":{"kind":"execute","prompt":"Inspect"},"issue":{"kind":"per-run","title":"Inspection"},"overlap":"forbid","catchup":"all"},"deleted":false},"installed_at":"2029-12-31T00:00:00Z","now":"$t:10801","checkpoint":null,"active_runs":1},"expect":{"launch":[],"checkpoint":{"through":"$t:10801","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAB","pending":[]},"excluded":"","drops":[{"reason":"overlap_skipped","instants":["$t:3600","$t:7200","$t:10800"]}]}}
]},
{"name":"drops/held-within-grace","steps":[
{"plan":{"adapter":"example-adapter","job":{"created_at":"$t:0","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAB","definition":{"version":1,"kind":"job","enabled":true,"trigger":{"kind":"interval","interval_seconds":3600},"action":{"kind":"execute","prompt":"Inspect"},"issue":{"kind":"per-run","title":"Inspection"},"overlap":"forbid","catchup":"all","grace_seconds":3600},"deleted":false},"installed_at":"2029-12-31T00:00:00Z","now":"$t:10800","checkpoint":null,"active_runs":1},"expect":{"launch":[],"checkpoint":{"through":"$t:10800","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAB","pending":["$t:7200","$t:10800"]},"excluded":"","drops":[{"reason":"overlap_skipped","instants":["$t:3600"]}]}}
]},
{"name":"drops/equality-then-expiry-and-replay","steps":[
{"plan":{"adapter":"example-adapter","job":{"created_at":"$t:0","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAB","definition":{"version":1,"kind":"job","enabled":true,"trigger":{"kind":"interval","interval_seconds":3600},"action":{"kind":"execute","prompt":"Inspect"},"issue":{"kind":"per-run","title":"Inspection"},"overlap":"forbid","catchup":"all"},"deleted":false},"installed_at":"2029-12-31T00:00:00Z","now":"$t:3600","checkpoint":null,"active_runs":1},"expect":{"launch":[],"checkpoint":{"through":"$t:3600","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAB","pending":["$t:3600"]},"excluded":"","drops":[]}},
{"plan":{"adapter":"example-adapter","job":{"created_at":"$t:0","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAB","definition":{"version":1,"kind":"job","enabled":true,"trigger":{"kind":"interval","interval_seconds":3600},"action":{"kind":"execute","prompt":"Inspect"},"issue":{"kind":"per-run","title":"Inspection"},"overlap":"forbid","catchup":"all"},"deleted":false},"installed_at":"2029-12-31T00:00:00Z","now":"$t:3601","checkpoint":null,"active_runs":1},"use_checkpoint":true,"expect":{"launch":[],"checkpoint":{"through":"$t:3601","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAB","pending":[]},"excluded":"","drops":[{"reason":"overlap_skipped","instants":["$t:3600"]}]}},
{"plan":{"adapter":"example-adapter","job":{"created_at":"$t:0","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAB","definition":{"version":1,"kind":"job","enabled":true,"trigger":{"kind":"interval","interval_seconds":3600},"action":{"kind":"execute","prompt":"Inspect"},"issue":{"kind":"per-run","title":"Inspection"},"overlap":"forbid","catchup":"all"},"deleted":false},"installed_at":"2029-12-31T00:00:00Z","now":"$t:3601","checkpoint":null,"active_runs":1},"use_checkpoint":true,"expect":{"launch":[],"checkpoint":{"through":"$t:3601","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAB","pending":[]},"excluded":"","drops":[]}}
]},
{"name":"drops/equality-then-free","steps":[
{"plan":{"adapter":"example-adapter","job":{"created_at":"$t:0","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAB","definition":{"version":1,"kind":"job","enabled":true,"trigger":{"kind":"interval","interval_seconds":3600},"action":{"kind":"execute","prompt":"Inspect"},"issue":{"kind":"per-run","title":"Inspection"},"overlap":"forbid","catchup":"all"},"deleted":false},"installed_at":"2029-12-31T00:00:00Z","now":"$t:3600","checkpoint":null,"active_runs":1},"expect":{"launch":[],"checkpoint":{"through":"$t:3600","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAB","pending":["$t:3600"]},"excluded":"","drops":[]}},
{"plan":{"adapter":"example-adapter","job":{"created_at":"$t:0","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAB","definition":{"version":1,"kind":"job","enabled":true,"trigger":{"kind":"interval","interval_seconds":3600},"action":{"kind":"execute","prompt":"Inspect"},"issue":{"kind":"per-run","title":"Inspection"},"overlap":"forbid","catchup":"all"},"deleted":false},"installed_at":"2029-12-31T00:00:00Z","now":"$t:3601","checkpoint":null,"active_runs":0},"use_checkpoint":true,"expect":{"launch":["$t:3600"],"checkpoint":{"through":"$t:3601","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAB","pending":[]},"excluded":"","drops":[]}}
]},
{"name":"drops/free-then-held","steps":[
{"plan":{"adapter":"example-adapter","job":{"created_at":"$t:0","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAB","definition":{"version":1,"kind":"job","enabled":true,"trigger":{"kind":"interval","interval_seconds":3600},"action":{"kind":"execute","prompt":"Inspect"},"issue":{"kind":"per-run","title":"Inspection"},"overlap":"forbid","catchup":"all"},"deleted":false},"installed_at":"2029-12-31T00:00:00Z","now":"$t:10800","checkpoint":null,"active_runs":0},"expect":{"launch":["$t:3600"],"checkpoint":{"through":"$t:10800","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAB","pending":["$t:7200","$t:10800"]},"excluded":"","drops":[]}},
{"plan":{"adapter":"example-adapter","job":{"created_at":"$t:0","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAB","definition":{"version":1,"kind":"job","enabled":true,"trigger":{"kind":"interval","interval_seconds":3600},"action":{"kind":"execute","prompt":"Inspect"},"issue":{"kind":"per-run","title":"Inspection"},"overlap":"forbid","catchup":"all"},"deleted":false},"installed_at":"2029-12-31T00:00:00Z","now":"$t:10801","checkpoint":null,"active_runs":1},"use_checkpoint":true,"expect":{"launch":[],"checkpoint":{"through":"$t:10801","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAB","pending":[]},"excluded":"","drops":[{"reason":"overlap_skipped","instants":["$t:7200","$t:10800"]}]}}
]},
{"name":"drops/unknown-holds-gate","steps":[
{"settle":{"steps":[{"key":"a","after":[],"retries":0}],"outcomes":{"a":[{"started_at":"$t:10800","ended_at":"$t:10801","status":"unknown"}]},"started_at":"$t:10800","now":"$t:10810","timeout_seconds":0,"cancelled_at":null,"stopped_with_certainty":false},"expect":{"attempts":{"a":1},"skipped":[],"status":"unknown","occupied":true}},
{"plan":{"adapter":"example-adapter","job":{"created_at":"$t:0","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAB","definition":{"version":1,"kind":"job","enabled":true,"trigger":{"kind":"interval","interval_seconds":3600},"action":{"kind":"execute","prompt":"Inspect"},"issue":{"kind":"per-run","title":"Inspection"},"overlap":"forbid","catchup":"all"},"deleted":false},"installed_at":"2029-12-31T00:00:00Z","now":"$t:10810","checkpoint":null,"active_runs":0},"use_occupancy":true,"expect":{"launch":[],"checkpoint":{"through":"$t:10810","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAB","pending":[]},"excluded":"","drops":[{"reason":"overlap_skipped","instants":["$t:3600","$t:7200","$t:10800"]}]}}
]},
{"name":"drops/allow-no-overlap-drops","steps":[
{"plan":{"adapter":"example-adapter","job":{"created_at":"$t:0","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAB","definition":{"version":1,"kind":"job","enabled":true,"trigger":{"kind":"interval","interval_seconds":3600},"action":{"kind":"execute","prompt":"Inspect"},"issue":{"kind":"per-run","title":"Inspection"},"overlap":"allow","catchup":"all"},"deleted":false},"installed_at":"2029-12-31T00:00:00Z","now":"$t:10800","checkpoint":null,"active_runs":1},"expect":{"launch":["$t:3600","$t:7200","$t:10800"],"checkpoint":{"through":"$t:10800","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAB","pending":[]},"excluded":"","drops":[]}}
]},
{"name":"drops/two-attribution-phases","steps":[
{"plan":{"adapter":"example-adapter","job":{"created_at":"$t:0","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAB","definition":{"version":1,"kind":"job","enabled":true,"trigger":{"kind":"interval","interval_seconds":3600},"action":{"kind":"execute","prompt":"Inspect"},"issue":{"kind":"per-run","title":"Inspection"},"overlap":"forbid","catchup":"latest"},"deleted":false},"installed_at":"2029-12-31T00:00:00Z","now":"$t:10801","checkpoint":null,"active_runs":1},"expect":{"launch":[],"checkpoint":{"through":"$t:10801","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAB","pending":[]},"excluded":"","drops":[{"reason":"catchup_missed","instants":["$t:3600","$t:7200"]},{"reason":"overlap_skipped","instants":["$t:10800"]}]}}
]},
{"name":"drops/revision-removal-is-not-drop","steps":[
{"plan":{"adapter":"example-adapter","job":{"created_at":"$t:0","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAA","definition":{"version":1,"kind":"job","enabled":true,"trigger":{"kind":"interval","interval_seconds":3600},"action":{"kind":"execute","prompt":"Inspect"},"issue":{"kind":"per-run","title":"Inspection"},"overlap":"allow","catchup":"all"},"deleted":false},"installed_at":"2029-12-31T00:00:00Z","now":"$t:7200","checkpoint":{"through":"$t:7200","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAB","pending":["$t:3600"]},"active_runs":0},"expect":{"launch":[],"checkpoint":{"through":"$t:7200","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAA","pending":[]},"excluded":"","drops":[]}}
]},
{"name":"drops/replacement-fresh-misses","steps":[
{"plan":{"adapter":"example-adapter","job":{"created_at":"$t:0","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAB","definition":{"version":1,"kind":"job","enabled":true,"trigger":{"kind":"interval","interval_seconds":3600},"action":{"kind":"execute","prompt":"Inspect"},"issue":{"kind":"per-run","title":"Inspection"},"overlap":"allow","catchup":"latest"},"deleted":false},"installed_at":"2029-12-31T00:00:00Z","now":"$t:18000","checkpoint":{"through":"$t:7200","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAC","pending":["$t:3600"]},"active_runs":0},"expect":{"launch":["$t:18000"],"checkpoint":{"through":"$t:18000","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAB","pending":[]},"excluded":"","drops":[{"reason":"catchup_missed","instants":["$t:10800","$t:14400"]}]}}
]},
{"name":"drops/disabled-freezes","steps":[
{"plan":{"adapter":"example-adapter","job":{"created_at":"$t:0","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAB","definition":{"version":1,"kind":"job","enabled":false,"trigger":{"kind":"interval","interval_seconds":3600},"action":{"kind":"execute","prompt":"Inspect"},"issue":{"kind":"per-run","title":"Inspection"},"overlap":"allow","catchup":"all"},"deleted":false},"installed_at":"2029-12-31T00:00:00Z","now":"$t:10800","checkpoint":{"through":"$t:7200","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAB","pending":["$t:3600"]},"active_runs":0},"expect":{"launch":[],"checkpoint":{"through":"$t:7200","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAB","pending":["$t:3600"]},"excluded":"disabled","drops":[]}}
]},
{"name":"drops/deleted-no-drops","steps":[
{"plan":{"adapter":"example-adapter","job":{"created_at":"$t:0","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAB","definition":{"version":1,"kind":"job","enabled":true,"trigger":{"kind":"interval","interval_seconds":3600},"action":{"kind":"execute","prompt":"Inspect"},"issue":{"kind":"per-run","title":"Inspection"},"overlap":"allow","catchup":"all"},"deleted":true},"installed_at":"2029-12-31T00:00:00Z","now":"$t:10800","checkpoint":{"through":"$t:7200","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAB","pending":["$t:3600"]},"active_runs":0},"expect":{"launch":[],"checkpoint":{"through":"$t:7200","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAB","pending":[]},"excluded":"deleted","drops":[]}}
]},
{"name":"drops/other-adapter-no-drops","steps":[
{"plan":{"adapter":"example-adapter","job":{"created_at":"$t:0","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAB","definition":{"version":1,"kind":"job","enabled":true,"trigger":{"kind":"interval","interval_seconds":3600},"action":{"kind":"execute","prompt":"Inspect"},"issue":{"kind":"per-run","title":"Inspection"},"overlap":"allow","catchup":"all","options":{"adapter":"another-adapter"}},"deleted":false},"installed_at":"2029-12-31T00:00:00Z","now":"$t:10800","checkpoint":null,"active_runs":0},"expect":{"launch":[],"checkpoint":{"through":"$t:0","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAB","pending":[]},"excluded":"other_adapter","drops":[]}}
]},
{"name":"drops/fractional-instants","steps":[
{"plan":{"adapter":"example-adapter","job":{"created_at":"$t:0.5","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAB","definition":{"version":1,"kind":"job","enabled":true,"trigger":{"kind":"interval","interval_seconds":1},"action":{"kind":"execute","prompt":"Inspect"},"issue":{"kind":"per-run","title":"Inspection"},"overlap":"allow","catchup":"latest"},"deleted":false},"installed_at":"2029-12-31T00:00:00Z","now":"$t:3.5","checkpoint":null,"active_runs":0},"expect":{"launch":["$t:3.5"],"checkpoint":{"through":"$t:3.5","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAB","pending":[]},"excluded":"","drops":[{"reason":"catchup_missed","instants":["$t:1.5","$t:2.5"]}]}}
]}
]} `,
	"overlap.json": `{
"schema":"kata.cron.adapter-conformance/v1",
"scenarios":[
{"name":"overlap/blocked","steps":[
{"plan":{"adapter":"example-adapter","job":{"created_at":"$t:0","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAB","deleted":false,"definition":{"version":1,"kind":"job","enabled":true,"trigger":{"kind":"interval","interval_seconds":3600},"action":{"kind":"execute","prompt":"Inspect"},"issue":{"kind":"per-run","title":"Inspection"},"overlap":"forbid","catchup":"all"}},"installed_at":"2029-12-31T00:00:00Z","now":"$t:10800","checkpoint":null,"active_runs":1},"expect":{"launch":[],"checkpoint":{"through":"$t:10800","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAB","pending":["$t:3600","$t:7200","$t:10800"]},"excluded":""}}
]},
{"name":"overlap/one-launch","steps":[
{"plan":{"adapter":"example-adapter","job":{"created_at":"$t:0","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAB","deleted":false,"definition":{"version":1,"kind":"job","enabled":true,"trigger":{"kind":"interval","interval_seconds":3600},"action":{"kind":"execute","prompt":"Inspect"},"issue":{"kind":"per-run","title":"Inspection"},"overlap":"forbid","catchup":"all"}},"installed_at":"2029-12-31T00:00:00Z","now":"$t:10800","checkpoint":null,"active_runs":0},"expect":{"launch":["$t:3600"],"checkpoint":{"through":"$t:10800","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAB","pending":["$t:7200","$t:10800"]},"excluded":""}}
]},
{"name":"overlap/same-clock-release","steps":[
{"plan":{"adapter":"example-adapter","job":{"created_at":"$t:0","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAB","deleted":false,"definition":{"version":1,"kind":"job","enabled":true,"trigger":{"kind":"interval","interval_seconds":3600},"action":{"kind":"execute","prompt":"Inspect"},"issue":{"kind":"per-run","title":"Inspection"},"overlap":"forbid","catchup":"all"}},"installed_at":"2029-12-31T00:00:00Z","now":"$t:10800","checkpoint":null,"active_runs":1},"expect":{"launch":[],"checkpoint":{"through":"$t:10800","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAB","pending":["$t:3600","$t:7200","$t:10800"]},"excluded":""}},
{"plan":{"adapter":"example-adapter","job":{"created_at":"$t:0","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAB","deleted":false,"definition":{"version":1,"kind":"job","enabled":true,"trigger":{"kind":"interval","interval_seconds":3600},"action":{"kind":"execute","prompt":"Inspect"},"issue":{"kind":"per-run","title":"Inspection"},"overlap":"forbid","catchup":"all"}},"installed_at":"2029-12-31T00:00:00Z","now":"$t:10800","checkpoint":null,"active_runs":0},"expect":{"launch":["$t:3600"],"checkpoint":{"through":"$t:10800","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAB","pending":["$t:7200","$t:10800"]},"excluded":""},"use_checkpoint":true}
]},
{"name":"overlap/latest-on-release","steps":[
{"plan":{"adapter":"example-adapter","job":{"created_at":"$t:0","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAB","deleted":false,"definition":{"version":1,"kind":"job","enabled":true,"trigger":{"kind":"interval","interval_seconds":3600},"action":{"kind":"execute","prompt":"Inspect"},"issue":{"kind":"per-run","title":"Inspection"},"overlap":"forbid","catchup":"latest"}},"installed_at":"2029-12-31T00:00:00Z","now":"$t:10800","checkpoint":null,"active_runs":1},"expect":{"launch":[],"checkpoint":{"through":"$t:10800","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAB","pending":["$t:10800"]},"excluded":""}},
{"plan":{"adapter":"example-adapter","job":{"created_at":"$t:0","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAB","deleted":false,"definition":{"version":1,"kind":"job","enabled":true,"trigger":{"kind":"interval","interval_seconds":3600},"action":{"kind":"execute","prompt":"Inspect"},"issue":{"kind":"per-run","title":"Inspection"},"overlap":"forbid","catchup":"latest"}},"installed_at":"2029-12-31T00:00:00Z","now":"$t:14400","checkpoint":null,"active_runs":0},"expect":{"launch":["$t:14400"],"checkpoint":{"through":"$t:14400","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAB","pending":[]},"excluded":""},"use_checkpoint":true}
]},
{"name":"overlap/skip-expires-pending","steps":[
{"plan":{"adapter":"example-adapter","job":{"created_at":"$t:0","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAB","deleted":false,"definition":{"version":1,"kind":"job","enabled":true,"trigger":{"kind":"interval","interval_seconds":3600},"action":{"kind":"execute","prompt":"Inspect"},"issue":{"kind":"per-run","title":"Inspection"},"overlap":"forbid","catchup":"skip","grace_seconds":3600}},"installed_at":"2029-12-31T00:00:00Z","now":"$t:10800","checkpoint":null,"active_runs":1},"expect":{"launch":[],"checkpoint":{"through":"$t:10800","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAB","pending":["$t:7200","$t:10800"]},"excluded":""}},
{"plan":{"adapter":"example-adapter","job":{"created_at":"$t:0","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAB","deleted":false,"definition":{"version":1,"kind":"job","enabled":true,"trigger":{"kind":"interval","interval_seconds":3600},"action":{"kind":"execute","prompt":"Inspect"},"issue":{"kind":"per-run","title":"Inspection"},"overlap":"forbid","catchup":"skip","grace_seconds":3600}},"installed_at":"2029-12-31T00:00:00Z","now":"$t:18000","checkpoint":null,"active_runs":0},"expect":{"launch":["$t:14400"],"checkpoint":{"through":"$t:18000","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAB","pending":["$t:18000"]},"excluded":""},"use_checkpoint":true}
]},
{"name":"overlap/plan-settle-plan-continuity","steps":[
{"plan":{"adapter":"example-adapter","job":{"created_at":"$t:0","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAB","deleted":false,"definition":{"version":1,"kind":"job","enabled":true,"trigger":{"kind":"interval","interval_seconds":3600},"action":{"kind":"execute","prompt":"Inspect"},"issue":{"kind":"per-run","title":"Inspection"},"overlap":"forbid","catchup":"all"}},"installed_at":"2029-12-31T00:00:00Z","now":"$t:10800","checkpoint":null,"active_runs":0},"expect":{"launch":["$t:3600"],"checkpoint":{"through":"$t:10800","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAB","pending":["$t:7200","$t:10800"]},"excluded":""}},
{"settle":{"steps":[{"key":"a","after":[],"retries":0}],"outcomes":{"a":[{"started_at":"$t:10800","ended_at":"$t:10801","status":"unknown"}]},"started_at":"$t:10800","now":"$t:10820","timeout_seconds":0,"cancelled_at":null,"stopped_with_certainty":false},"expect":{"attempts":{"a":1},"skipped":[],"status":"unknown","occupied":true}},
{"plan":{"adapter":"example-adapter","job":{"created_at":"$t:0","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAB","deleted":false,"definition":{"version":1,"kind":"job","enabled":true,"trigger":{"kind":"interval","interval_seconds":3600},"action":{"kind":"execute","prompt":"Inspect"},"issue":{"kind":"per-run","title":"Inspection"},"overlap":"forbid","catchup":"all"}},"installed_at":"2029-12-31T00:00:00Z","now":"$t:10820","checkpoint":null,"active_runs":0},"expect":{"launch":[],"checkpoint":{"through":"$t:10820","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAB","pending":["$t:7200","$t:10800"]},"excluded":""},"use_checkpoint":true,"use_occupancy":true},
{"settle":{"steps":[{"key":"a","after":[],"retries":0}],"outcomes":{"a":[{"started_at":"$t:10800","ended_at":"$t:10801","status":"unknown"}]},"started_at":"$t:10800","now":"$t:10820","timeout_seconds":0,"cancelled_at":null,"stopped_with_certainty":true},"expect":{"attempts":{"a":1},"skipped":[],"status":"unknown","occupied":false}},
{"plan":{"adapter":"example-adapter","job":{"created_at":"$t:0","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAB","deleted":false,"definition":{"version":1,"kind":"job","enabled":true,"trigger":{"kind":"interval","interval_seconds":3600},"action":{"kind":"execute","prompt":"Inspect"},"issue":{"kind":"per-run","title":"Inspection"},"overlap":"forbid","catchup":"all"}},"installed_at":"2029-12-31T00:00:00Z","now":"$t:10820","checkpoint":null,"active_runs":0},"expect":{"launch":["$t:7200"],"checkpoint":{"through":"$t:10820","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAB","pending":["$t:10800"]},"excluded":""},"use_checkpoint":true,"use_occupancy":true}
]}
]} `,
	"ownership.json": `{
"schema":"kata.cron.adapter-conformance/v1",
"scenarios":[
{"name":"ownership/disabled","steps":[
{"plan":{"adapter":"example-adapter","job":{"created_at":"$t:0","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAB","deleted":false,"definition":{"version":1,"kind":"job","enabled":false,"trigger":{"kind":"interval","interval_seconds":3600},"action":{"kind":"execute","prompt":"Inspect"},"issue":{"kind":"per-run","title":"Inspection"},"overlap":"allow","catchup":"all"}},"installed_at":"2029-12-31T00:00:00Z","now":"$t:10800","checkpoint":null,"active_runs":0},"expect":{"launch":[],"checkpoint":{"through":"$t:0","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAB","pending":[]},"excluded":"disabled"}}
]},
{"name":"ownership/manual","steps":[
{"plan":{"adapter":"example-adapter","job":{"created_at":"$t:0","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAB","deleted":false,"definition":{"version":1,"kind":"job","enabled":true,"trigger":{"kind":"manual"},"action":{"kind":"execute","prompt":"Inspect"},"issue":{"kind":"per-run","title":"Inspection"},"overlap":"allow","catchup":"all"}},"installed_at":"2029-12-31T00:00:00Z","now":"$t:10800","checkpoint":null,"active_runs":0},"expect":{"launch":[],"checkpoint":{"through":"$t:0","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAB","pending":[]},"excluded":"manual"}}
]},
{"name":"ownership/other-adapter","steps":[
{"plan":{"adapter":"example-adapter","job":{"created_at":"$t:0","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAB","deleted":false,"definition":{"version":1,"kind":"job","enabled":true,"trigger":{"kind":"interval","interval_seconds":3600},"action":{"kind":"execute","prompt":"Inspect"},"issue":{"kind":"per-run","title":"Inspection"},"overlap":"allow","catchup":"all","options":{"adapter":"another-adapter"}}},"installed_at":"2029-12-31T00:00:00Z","now":"$t:10800","checkpoint":null,"active_runs":0},"expect":{"launch":[],"checkpoint":{"through":"$t:0","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAB","pending":[]},"excluded":"other_adapter"}}
]},
{"name":"ownership/matching-adapter","steps":[
{"plan":{"adapter":"example-adapter","job":{"created_at":"$t:0","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAB","deleted":false,"definition":{"version":1,"kind":"job","enabled":true,"trigger":{"kind":"interval","interval_seconds":3600},"action":{"kind":"execute","prompt":"Inspect"},"issue":{"kind":"per-run","title":"Inspection"},"overlap":"allow","catchup":"all","options":{"adapter":"example-adapter"}}},"installed_at":"2029-12-31T00:00:00Z","now":"$t:10800","checkpoint":null,"active_runs":0},"expect":{"launch":["$t:3600","$t:7200","$t:10800"],"checkpoint":{"through":"$t:10800","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAB","pending":[]},"excluded":""}}
]},
{"name":"ownership/native-only-exclusion","steps":[
{"plan":{"adapter":"example-adapter","job":{"created_at":"$t:0","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAB","deleted":false,"definition":{"version":1,"kind":"job","enabled":true,"trigger":{"kind":"interval","interval_seconds":3600},"action":{"kind":"notify","recipient":"operator"},"issue":{"kind":"per-run","parent_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAC","title":"Inspection"},"overlap":"allow","catchup":"all"}},"installed_at":"2029-12-31T00:00:00Z","now":"$t:10800","checkpoint":null,"active_runs":0},"expect":{"launch":[],"checkpoint":{"through":"$t:0","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAB","pending":[]},"excluded":"notify"}}
]},
{"name":"ownership/adapter-notification-retained","steps":[
{"plan":{"adapter":"example-adapter","job":{"created_at":"$t:0","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAB","deleted":false,"definition":{"version":1,"kind":"job","enabled":true,"trigger":{"kind":"interval","interval_seconds":3600},"action":{"kind":"notify","recipient":"operator","message":"Inspect"},"issue":{"kind":"existing","uid":"01ARZ3NDEKTSV4RRFFQ69G5FAC"},"overlap":"allow","catchup":"all"}},"installed_at":"2029-12-31T00:00:00Z","now":"$t:10800","checkpoint":null,"active_runs":0},"expect":{"launch":["$t:3600","$t:7200","$t:10800"],"checkpoint":{"through":"$t:10800","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAB","pending":[]},"excluded":""}}
]}
]} `,
	"retries.json": `{
"schema":"kata.cron.adapter-conformance/v1",
"scenarios":[
{"name":"retries/success","steps":[
{"settle":{"steps":[{"key":"a","after":[],"retries":0}],"outcomes":{"a":[{"started_at":"$t:0","ended_at":"$t:1","status":"succeeded"}]},"started_at":"$t:0","now":"$t:20","timeout_seconds":0,"cancelled_at":null,"stopped_with_certainty":true},"expect":{"attempts":{"a":1},"skipped":[],"status":"succeeded","occupied":false}}
]},
{"name":"retries/certain-failure-retry","steps":[
{"settle":{"steps":[{"key":"a","after":[],"retries":1}],"outcomes":{"a":[{"started_at":"$t:0","ended_at":"$t:1","status":"failed"},{"started_at":"$t:2","ended_at":"$t:3","status":"succeeded"}]},"started_at":"$t:0","now":"$t:20","timeout_seconds":0,"cancelled_at":null,"stopped_with_certainty":true},"expect":{"attempts":{"a":2},"skipped":[],"status":"succeeded","occupied":false}}
]},
{"name":"retries/budget-exhausted","steps":[
{"settle":{"steps":[{"key":"a","after":[],"retries":1}],"outcomes":{"a":[{"started_at":"$t:0","ended_at":"$t:1","status":"failed"},{"started_at":"$t:2","ended_at":"$t:3","status":"failed"},{"started_at":"$t:4","ended_at":"$t:5","status":"succeeded"}]},"started_at":"$t:0","now":"$t:20","timeout_seconds":0,"cancelled_at":null,"stopped_with_certainty":true},"expect":{"attempts":{"a":2},"skipped":[],"status":"failed","occupied":false}}
]},
{"name":"retries/unknown-not-retried","steps":[
{"settle":{"steps":[{"key":"a","after":[],"retries":2}],"outcomes":{"a":[{"started_at":"$t:0","ended_at":"$t:1","status":"unknown"},{"started_at":"$t:2","ended_at":"$t:3","status":"succeeded"}]},"started_at":"$t:0","now":"$t:20","timeout_seconds":0,"cancelled_at":null,"stopped_with_certainty":false},"expect":{"attempts":{"a":1},"skipped":[],"status":"unknown","occupied":true}}
]},
{"name":"retries/success-not-retried","steps":[
{"settle":{"steps":[{"key":"a","after":[],"retries":2}],"outcomes":{"a":[{"started_at":"$t:0","ended_at":"$t:1","status":"succeeded"},{"started_at":"$t:2","ended_at":"$t:3","status":"failed"}]},"started_at":"$t:0","now":"$t:20","timeout_seconds":0,"cancelled_at":null,"stopped_with_certainty":true},"expect":{"attempts":{"a":1},"skipped":[],"status":"succeeded","occupied":false}}
]},
{"name":"retries/failed-dependency","steps":[
{"settle":{"steps":[{"key":"a","after":[],"retries":0},{"key":"b","after":["a"],"retries":0}],"outcomes":{"a":[{"started_at":"$t:0","ended_at":"$t:1","status":"failed"}],"b":[{"started_at":"$t:2","ended_at":"$t:3","status":"succeeded"}]},"started_at":"$t:0","now":"$t:20","timeout_seconds":0,"cancelled_at":null,"stopped_with_certainty":true},"expect":{"attempts":{"a":1,"b":0},"skipped":["b"],"status":"failed","occupied":false}}
]},
{"name":"retries/independent-continue","steps":[
{"settle":{"steps":[{"key":"a","after":[],"retries":0},{"key":"b","after":[],"retries":0}],"outcomes":{"a":[{"started_at":"$t:0","ended_at":"$t:1","status":"failed"}],"b":[{"started_at":"$t:2","ended_at":"$t:3","status":"succeeded"}]},"started_at":"$t:0","now":"$t:20","timeout_seconds":0,"cancelled_at":null,"stopped_with_certainty":true},"expect":{"attempts":{"a":1,"b":1},"skipped":[],"status":"failed","occupied":false}}
]},
{"name":"retries/incomplete-is-running","steps":[
{"settle":{"steps":[{"key":"a","after":[],"retries":0}],"outcomes":{"a":[{"started_at":"$t:0","ended_at":"0001-01-01T00:00:00Z","status":"running"}]},"started_at":"$t:0","now":"$t:20","timeout_seconds":0,"cancelled_at":null,"stopped_with_certainty":true},"expect":{"attempts":{"a":1},"skipped":[],"status":"running","occupied":true}}
]}
]} `,
	"schedule.json": `{
"schema":"kata.cron.adapter-conformance/v1",
"scenarios":[
{"name":"schedule/interval-no-creation-run","steps":[
{"plan":{"adapter":"example-adapter","job":{"created_at":"$t:0","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAB","deleted":false,"definition":{"version":1,"kind":"job","enabled":true,"trigger":{"kind":"interval","interval_seconds":3600},"action":{"kind":"execute","prompt":"Inspect"},"issue":{"kind":"per-run","title":"Inspection"},"overlap":"allow","catchup":"all"}},"installed_at":"2029-12-31T00:00:00Z","now":"$t:0","checkpoint":null,"active_runs":0},"expect":{"launch":[],"checkpoint":{"through":"$t:0","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAB","pending":[]},"excluded":""}}
]},
{"name":"schedule/interval-first-after-anchor","steps":[
{"plan":{"adapter":"example-adapter","job":{"created_at":"$t:0","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAB","deleted":false,"definition":{"version":1,"kind":"job","enabled":true,"trigger":{"kind":"interval","interval_seconds":3600},"action":{"kind":"execute","prompt":"Inspect"},"issue":{"kind":"per-run","title":"Inspection"},"overlap":"allow","catchup":"all"}},"installed_at":"2029-12-31T00:00:00Z","now":"$t:3600","checkpoint":null,"active_runs":0},"expect":{"launch":["$t:3600"],"checkpoint":{"through":"$t:3600","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAB","pending":[]},"excluded":""}}
]},
{"name":"schedule/interval-grid","steps":[
` + intervalGridDecision + `
]},
{"name":"schedule/installation-no-backfill","steps":[
{"plan":{"adapter":"example-adapter","job":{"created_at":"$t:0","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAB","deleted":false,"definition":{"version":1,"kind":"job","enabled":true,"trigger":{"kind":"interval","interval_seconds":3600},"action":{"kind":"execute","prompt":"Inspect"},"issue":{"kind":"per-run","title":"Inspection"},"overlap":"allow","catchup":"all"}},"installed_at":"$t:5400","now":"$t:10800","checkpoint":null,"active_runs":0},"expect":{"launch":["$t:7200","$t:10800"],"checkpoint":{"through":"$t:10800","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAB","pending":[]},"excluded":""}}
]},
{"name":"schedule/checkpoint-exclusive-now-inclusive","steps":[
` + checkpointExclusiveDecision + `
]},
{"name":"schedule/before-first","steps":[
{"plan":{"adapter":"example-adapter","job":{"created_at":"$t:0","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAB","deleted":false,"definition":{"version":1,"kind":"job","enabled":true,"trigger":{"kind":"interval","interval_seconds":3600},"action":{"kind":"execute","prompt":"Inspect"},"issue":{"kind":"per-run","title":"Inspection"},"overlap":"allow","catchup":"all"}},"installed_at":"2029-12-31T00:00:00Z","now":"$t:3500","checkpoint":null,"active_runs":0},"expect":{"launch":[],"checkpoint":{"through":"$t:3500","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAB","pending":[]},"excluded":""}}
]},
{"name":"schedule/every-anchored","steps":[
{"plan":{"adapter":"example-adapter","job":{"created_at":"$t:0","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAB","deleted":false,"definition":{"version":1,"kind":"job","enabled":true,"trigger":{"kind":"cron","cron":"@every 90m"},"action":{"kind":"execute","prompt":"Inspect"},"issue":{"kind":"per-run","title":"Inspection"},"overlap":"allow","catchup":"all"}},"installed_at":"2029-12-31T00:00:00Z","now":"$t:10800","checkpoint":null,"active_runs":0},"expect":{"launch":["$t:5400","$t:10800"],"checkpoint":{"through":"$t:10800","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAB","pending":[]},"excluded":""}}
]},
{"name":"schedule/once","steps":[
{"plan":{"adapter":"example-adapter","job":{"created_at":"$t:0","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAB","deleted":false,"definition":{"version":1,"kind":"job","enabled":true,"trigger":{"kind":"once","at":"$t:7200"},"action":{"kind":"execute","prompt":"Inspect"},"issue":{"kind":"per-run","title":"Inspection"},"overlap":"allow","catchup":"all"}},"installed_at":"2029-12-31T00:00:00Z","now":"$t:10800","checkpoint":null,"active_runs":0},"expect":{"launch":["$t:7200"],"checkpoint":{"through":"$t:10800","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAB","pending":[]},"excluded":""}}
]},
{"name":"schedule/once-at-creation","steps":[
{"plan":{"adapter":"example-adapter","job":{"created_at":"$t:0","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAB","deleted":false,"definition":{"version":1,"kind":"job","enabled":true,"trigger":{"kind":"once","at":"$t:0"},"action":{"kind":"execute","prompt":"Inspect"},"issue":{"kind":"per-run","title":"Inspection"},"overlap":"allow","catchup":"all"}},"installed_at":"2029-12-31T00:00:00Z","now":"$t:10800","checkpoint":null,"active_runs":0},"expect":{"launch":[],"checkpoint":{"through":"$t:0","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAB","pending":[]},"excluded":"once_past"}}
]},
{"name":"schedule/directive-precedence","steps":[
{"plan":{"adapter":"example-adapter","job":{"created_at":"$t:0","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAB","deleted":false,"definition":{"version":1,"kind":"job","enabled":true,"trigger":{"kind":"cron","cron":"TZ=UTC 0 2 * * *","timezone":"America/New_York"},"action":{"kind":"execute","prompt":"Inspect"},"issue":{"kind":"per-run","title":"Inspection"},"overlap":"allow","catchup":"all"}},"installed_at":"2029-12-31T00:00:00Z","now":"$t:10800","checkpoint":null,"active_runs":0},"expect":{"launch":["$t:7200"],"checkpoint":{"through":"$t:10800","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAB","pending":[]},"excluded":""}}
]},
{"name":"schedule/dst-gap","steps":[
{"plan":{"adapter":"example-adapter","job":{"created_at":"2026-03-08T00:00:00Z","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAB","deleted":false,"definition":{"version":1,"kind":"job","enabled":true,"trigger":{"kind":"cron","cron":"30 2 * * *","timezone":"America/New_York"},"action":{"kind":"execute","prompt":"Inspect"},"issue":{"kind":"per-run","title":"Inspection"},"overlap":"allow","catchup":"all"}},"installed_at":"2026-03-08T00:00:00Z","now":"2026-03-08T08:00:00Z","checkpoint":null,"active_runs":0},"expect":{"launch":["2026-03-08T07:30:00Z"],"checkpoint":{"through":"2026-03-08T08:00:00Z","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAB","pending":[]},"excluded":""}}
]},
{"name":"schedule/dst-repeat","steps":[
{"plan":{"adapter":"example-adapter","job":{"created_at":"2026-11-01T00:00:00Z","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAB","deleted":false,"definition":{"version":1,"kind":"job","enabled":true,"trigger":{"kind":"cron","cron":"30 1 * * *","timezone":"America/New_York"},"action":{"kind":"execute","prompt":"Inspect"},"issue":{"kind":"per-run","title":"Inspection"},"overlap":"allow","catchup":"all"}},"installed_at":"2026-11-01T00:00:00Z","now":"2026-11-01T07:00:00Z","checkpoint":null,"active_runs":0},"expect":{"launch":["2026-11-01T05:30:00Z","2026-11-01T06:30:00Z"],"checkpoint":{"through":"2026-11-01T07:00:00Z","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAB","pending":[]},"excluded":""}}
]}
]} `,
	"timeout.json": `{
"schema":"kata.cron.adapter-conformance/v1",
"scenarios":[
{"name":"timeout/completed-before-deadline","steps":[
{"settle":{"steps":[{"key":"a","after":[],"retries":0}],"outcomes":{"a":[{"started_at":"$t:0","ended_at":"$t:1","status":"succeeded"}]},"started_at":"$t:0","now":"$t:20","timeout_seconds":10,"cancelled_at":null,"stopped_with_certainty":true},"expect":{"attempts":{"a":1},"skipped":[],"status":"succeeded","occupied":false}}
]},
{"name":"timeout/certain-stop","steps":[
{"settle":{"steps":[{"key":"a","after":[],"retries":0}],"outcomes":{"a":[{"started_at":"$t:0","ended_at":"0001-01-01T00:00:00Z","status":"running"}]},"started_at":"$t:0","now":"$t:20","timeout_seconds":10,"cancelled_at":null,"stopped_with_certainty":true},"expect":{"attempts":{"a":1},"skipped":[],"status":"failed","occupied":false}}
]},
{"name":"timeout/uncertain-stop","steps":[
{"settle":{"steps":[{"key":"a","after":[],"retries":0}],"outcomes":{"a":[{"started_at":"$t:0","ended_at":"0001-01-01T00:00:00Z","status":"running"}]},"started_at":"$t:0","now":"$t:20","timeout_seconds":10,"cancelled_at":null,"stopped_with_certainty":false},"expect":{"attempts":{"a":1},"skipped":[],"status":"unknown","occupied":true}}
]},
{"name":"timeout/equality-is-expired","steps":[
{"settle":{"steps":[{"key":"a","after":[],"retries":0}],"outcomes":{"a":[{"started_at":"$t:0","ended_at":"$t:10","status":"succeeded"}]},"started_at":"$t:0","now":"$t:20","timeout_seconds":10,"cancelled_at":null,"stopped_with_certainty":true},"expect":{"attempts":{"a":1},"skipped":[],"status":"failed","occupied":false}}
]},
{"name":"timeout/retry-starts-at-deadline","steps":[
{"settle":{"steps":[{"key":"a","after":[],"retries":1}],"outcomes":{"a":[{"started_at":"$t:0","ended_at":"$t:5","status":"failed"},{"started_at":"$t:10","ended_at":"$t:11","status":"succeeded"}]},"started_at":"$t:0","now":"$t:20","timeout_seconds":10,"cancelled_at":null,"stopped_with_certainty":true},"expect":{"attempts":{"a":1},"skipped":[],"status":"failed","occupied":false}}
]},
{"name":"timeout/cancellation","steps":[
{"settle":{"steps":[{"key":"a","after":[],"retries":0}],"outcomes":{"a":[{"started_at":"$t:0","ended_at":"0001-01-01T00:00:00Z","status":"running"}]},"started_at":"$t:0","now":"$t:20","timeout_seconds":0,"cancelled_at":"$t:5","stopped_with_certainty":true},"expect":{"attempts":{"a":1},"skipped":[],"status":"cancelled","occupied":false}}
]},
{"name":"timeout/uncertain-cancellation","steps":[
{"settle":{"steps":[{"key":"a","after":[],"retries":0}],"outcomes":{"a":[{"started_at":"$t:0","ended_at":"0001-01-01T00:00:00Z","status":"running"}]},"started_at":"$t:0","now":"$t:20","timeout_seconds":0,"cancelled_at":"$t:5","stopped_with_certainty":false},"expect":{"attempts":{"a":1},"skipped":[],"status":"unknown","occupied":true}}
]},
{"name":"timeout/unknown-retains-overlap","steps":[
{"settle":{"steps":[{"key":"a","after":[],"retries":0}],"outcomes":{"a":[{"started_at":"$t:0","ended_at":"0001-01-01T00:00:00Z","status":"running"}]},"started_at":"$t:0","now":"$t:20","timeout_seconds":10,"cancelled_at":null,"stopped_with_certainty":false},"expect":{"attempts":{"a":1},"skipped":[],"status":"unknown","occupied":true}},
{"plan":{"adapter":"example-adapter","job":{"created_at":"$t:0","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAB","deleted":false,"definition":{"version":1,"kind":"job","enabled":true,"trigger":{"kind":"interval","interval_seconds":3600},"action":{"kind":"execute","prompt":"Inspect"},"issue":{"kind":"per-run","title":"Inspection"},"overlap":"forbid","catchup":"all"}},"installed_at":"2029-12-31T00:00:00Z","now":"$t:10800","checkpoint":null,"active_runs":0},"expect":{"launch":[],"checkpoint":{"through":"$t:10800","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAB","pending":["$t:3600","$t:7200","$t:10800"]},"excluded":""},"use_occupancy":true}
]}
]} `,
}

const defaultExportHashes = `{"catchup.json":"b4d84f9e199c8d30a4a2f13bed69798a01e3daba917db4b2aebd0126804b8ce6","definition-change.json":"eca310d80aad7a648e3cf77d54a30ca10067feb3876bd402fa216052fe04a2ec","manifest.json":"73e33f5e44d89fa077fd954d6a3430358e34266855ba412df58062b1a075535c","overlap.json":"a50ea84f26a8306ec1510fe0c76dd785700dfbd989be41a49dbfdc85b42748eb","ownership.json":"32e8efe880a99f6a7ef4b6a14d34c81536d0fe8bd90c989d569dec99a0ffb62c","retries.json":"34a6285eeea584d1a60407038866ef95ce7ad52dc8b029f0324b12016447e5d5","schedule.json":"a5ec1754262f39b51dd036f0854b7ab77e83f12f841c96648b5c222fa8816bb5","timeout.json":"69cef2c72cdd79b037682fcea0b467d1c0d63bbca7e42ace45970093c5bcee53"}`
