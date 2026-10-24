package jobform

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"strings"
	"testing"

	"go.kenn.io/kata/internal/jsonutil"
)

func TestDraftIssueReferenceDisplayUsesProject(t *testing.T) {
	for _, input := range []string{"abcd5", "#abcd5", "example-project#abcd5"} {
		d := Draft{Values: map[string]string{"issue": input}}
		display, edit := Get(Field{Key: "issue"}, d, Context{Project: "example-project", IssueReference: func(string) string { return "" }})
		if display != "example-project#abcd5" || edit != input {
			t.Fatal(input, display, edit)
		}
	}
}

func TestDraftEmptyWorkflowUIDUsesPromptFields(t *testing.T) {
	d := NewDraft(map[string]any{"action": map[string]any{"kind": "execute", "workflow_uid": "", "prompt": "work"}}, "Example")
	display, _ := Get(Field{Key: "mode"}, d, Context{})
	if display != "prompt" {
		t.Fatalf("action mode = %q, want prompt", display)
	}
	found := false
	for _, f := range Fields(d, Context{}) {
		found = found || f.Key == "prompt"
	}
	if !found {
		t.Fatal("empty workflow UID hid the prompt field")
	}
}

func TestNewDraftHydratesEditValues(t *testing.T) {
	for _, mode := range []struct{ action, issue, want string }{
		{`{"kind":"execute","prompt":"work"}`, "per-run", "prompt"},
		{`{"kind":"execute","prompt":"work","workflow_uid":""}`, "per-run", "prompt"},
		{`{"kind":"execute","workflow_uid":"flow"}`, "per-run", "workflow"},
		{`{"kind":"notify","recipient":"worker"}`, "existing", "notify"},
		{`{"kind":"notify","recipient":"worker/inbox"}`, "per-run", "recurring"},
	} {
		for _, trigger := range []struct{ raw, want string }{
			{`{"kind":"manual"}`, ""},
			{`{"kind":"cron","cron":"0 * * * *"}`, "0 * * * *"},
			{`{"kind":"interval","interval_seconds":3600}`, "1h0m0s"},
			{`{"kind":"once","at":"2030-01-01T00:00:00Z"}`, "2030-01-01T00:00:00Z"},
		} {
			t.Run(mode.want+mode.action+trigger.raw, func(t *testing.T) {
				var raw map[string]any
				body := `{"enabled":false,"action":` + mode.action + `,"trigger":` + trigger.raw + `,"issue":{"kind":"` + mode.issue + `","deadline_offset_seconds":-60},"options":{"tags":["daily","example"],"input":"input","run_parent":"parent","description":"description","opaque":9007199254740993},"secret_refs":{"Z_TOKEN":"last","A_TOKEN":"first"}}`
				decoder := jsontext.NewDecoder(strings.NewReader(body))
				if err := json.UnmarshalDecode(decoder, &raw, jsonutil.PreserveNumberLiterals()); err != nil {
					t.Fatal(err)
				}
				d := NewDraft(raw, "Example")
				// Names in this fixture are secret references, never credentials.
				//nolint:gosec // G101: neutral secret-reference fixture.
				for key, want := range map[string]string{"name": "Example", "mode": mode.want, "schedule": trigger.want, "enabled": "false", "deadline": "-1m0s", "tags": "daily, example", "secret_refs": "A_TOKEN=first, Z_TOKEN=last", "input": "input", "description": "description", "run_parent": "parent"} {
					if got := d.Values[key]; got != want {
						t.Fatalf("%s = %q, want %q", key, got, want)
					}
				}
				if len(d.Dirty) != 0 {
					t.Fatal("hydration dirtied fields", d.Dirty)
				}

			})
		}
	}
}
