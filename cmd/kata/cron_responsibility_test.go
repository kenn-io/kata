package main

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
	"hegel.dev/go/hegel"
)

func TestCronResponsibilityPreservesPayload(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		resource := hegel.Draw(ht, hegel.SampledFrom([]string{"job", "workflow", "run"}))
		list := hegel.Draw(ht, hegel.Booleans())
		actor := hegel.Draw(ht, hegel.Text())
		text, err := json.Marshal(actor)
		require.NoError(ht, err)
		entry := fmt.Sprintf(`{"author":%s,"actor":%s,"executor_label":"runtime-a","definition":{"options":{"adapter":"adapter-a","limit":9007199254740993}},"future":18446744073709551615}`, text, text)
		key, value := resource, entry
		if list {
			key += "s"
			value = "[" + entry + "]"
		}
		raw := []byte(fmt.Sprintf(`{"%s":%s,"next_before_uid":"cursor","extra":9007199254740993}`, key, value))
		out, err := cronResponsibilityResponse(raw, resource, list)
		require.NoError(ht, err)
		var before, after map[string]jsontext.Value
		require.NoError(ht, json.Unmarshal(raw, &before))
		require.NoError(ht, json.Unmarshal(out, &after))
		for _, field := range []string{"next_before_uid", "extra"} {
			require.Equal(ht, string(before[field]), string(after[field]))
		}
		var entries []map[string]jsontext.Value
		if list {
			require.NoError(ht, json.Unmarshal(after[key], &entries))
		} else {
			var item map[string]jsontext.Value
			require.NoError(ht, json.Unmarshal(after[key], &item))
			entries = append(entries, item)
		}
		var original map[string]jsontext.Value
		require.NoError(ht, json.Unmarshal([]byte(entry), &original))
		for field, expected := range original {
			require.JSONEq(ht, string(expected), string(entries[0][field]))
		}
		require.Contains(ht, string(out), "18446744073709551615")
		require.Contains(ht, string(out), "9007199254740993")
		var responsibility cron.Responsibility
		require.NoError(ht, json.Unmarshal(entries[0]["responsibility"], &responsibility))
		if resource == "run" {
			require.Equal(ht, actor, *responsibility.Actor)
			require.Equal(ht, "runtime-a", *responsibility.Runtime)
		} else {
			require.Nil(ht, responsibility.Actor)
			require.Nil(ht, responsibility.Runtime)
		}
	}, hegel.WithTestCases(1000))
}

func TestCronResponsibilityRejectsNonObjectEnvelope(t *testing.T) {
	for _, tc := range []struct {
		action string
		list   bool
	}{{"show", false}, {"list", true}} {
		t.Run(tc.action, func(t *testing.T) {
			_, err := cronResponsibilityResponse([]byte("null"), "job", tc.list)
			require.EqualError(t, err, "cron job response must be a JSON object")
		})
	}
}

// The user contract covers both list/show, all three resources, and agent/JSON.
func TestCronCLIReadResponsibility(t *testing.T) {
	t.Setenv("KATA_AUTHOR", "creator")
	env := testenv.New(t)
	p, err := env.DB.CreateProject(t.Context(), "spoke-project")
	require.NoError(t, err)
	j, _, err := env.DB.PutCronJob(t.Context(), db.PutCronJob{ProjectID: p.ID, Name: "Inspect", Actor: "creator", Definition: cronCLIJob(t).Definition.Native()})
	require.NoError(t, err)
	w, _, err := env.DB.PutCronWorkflow(t.Context(), db.PutCronWorkflow{ProjectID: p.ID, Name: "Inspect", Actor: "creator", Definition: cron.WorkflowDefinition{Version: 1, Steps: []cron.WorkflowStep{{Key: "inspect", Kind: "command", Command: "true"}}}})
	require.NoError(t, err)
	runUID := cronCLIUID(t)
	_, err = env.DB.ObserveCronRun(t.Context(), db.ObserveCronRun{ProjectID: p.ID, UID: runUID, JobUID: &j.UID, DefinitionEventUID: &j.DefinitionEventUID, Actor: "runner", ExecutorLabel: new("runtime-a"), Status: "succeeded", Summary: cron.Summary{Version: 1}})
	require.NoError(t, err)
	for _, mode := range []string{"--json", "--agent"} {
		for _, resource := range []struct{ name, uid, status string }{{"job", j.UID, "unknown"}, {"workflow", w.UID, "unknown"}, {"run", runUID, "reported"}} {
			for _, action := range []string{"list", "show"} {
				t.Run(mode+resource.name+action, func(t *testing.T) {
					resetFlags(t)
					cmd := newRootCmd()
					var out strings.Builder
					cmd.SetOut(&out)
					cmd.SetContext(contextWithBaseURL(t.Context(), env.URL))
					args := []string{"--project", "spoke-project", mode, "cron", resource.name, action}
					if action == "show" {
						args = append(args, resource.uid)
					}
					cmd.SetArgs(args)
					require.NoError(t, cmd.Execute())
					require.Contains(t, out.String(), `"responsibility"`)
					var body map[string]jsontext.Value
					require.NoError(t, json.Unmarshal([]byte(out.String()), &body))
					var records []struct {
						Responsibility cron.Responsibility `json:"responsibility"`
					}
					if action == "list" {
						require.NoError(t, json.Unmarshal(body[resource.name+"s"], &records))
					} else {
						var record struct {
							Responsibility cron.Responsibility `json:"responsibility"`
						}
						require.NoError(t, json.Unmarshal(body[resource.name], &record))
						records = append(records, record)
					}
					require.Len(t, records, 1)
					require.Equal(t, resource.status, records[0].Responsibility.RuntimeStatus)
				})
			}
		}
	}
}

func TestCronResponsibilityPreservesListOrderAndNumbers(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		names := hegel.Draw(ht, hegel.Lists(hegel.Text()))
		// Limit materialization cost, not the generator's input domain.
		if len(names) > 100 {
			names = names[:100]
		}
		resource := hegel.Draw(ht, hegel.SampledFrom([]string{"job", "workflow", "run"}))
		entries := make([]jsontext.Value, 0, len(names))
		for _, name := range names {
			encoded, err := json.Marshal(name)
			require.NoError(ht, err)
			entries = append(entries, []byte(fmt.Sprintf(`{"name":%s,"actor":"runner","definition":{},"number":9.007199254740993e+15}`, encoded)))
		}
		array, err := json.Marshal(entries)
		require.NoError(ht, err)
		out, err := cronResponsibilityResponse([]byte(fmt.Sprintf(`{"%ss":%s}`, resource, array)), resource, true)
		require.NoError(ht, err)
		var body map[string][]map[string]jsontext.Value
		require.NoError(ht, json.Unmarshal(out, &body))
		require.Len(ht, body[resource+"s"], len(names))
		for i, item := range body[resource+"s"] {
			var name string
			require.NoError(ht, json.Unmarshal(item["name"], &name))
			require.Equal(ht, names[i], name)
			require.Equal(ht, "9.007199254740993e+15", string(item["number"]))
		}
	}, hegel.WithTestCases(1000))
}

func TestCronResponsibilityNativeClassification(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		native := hegel.Draw(ht, hegel.Booleans())
		enabled := hegel.Draw(ht, hegel.Booleans())
		deleted := hegel.Draw(ht, hegel.SampledFrom([]string{`null`, `"2026-01-01T00:00:00Z"`}))
		kind := "execute"
		if native {
			kind = "notify"
		}
		out, err := cronResponsibilityResponse([]byte(fmt.Sprintf(`{"job":{"deleted_at":%s,"definition":{"enabled":%t,"action":{"kind":%q},"issue":{"kind":"per-run"}}}}`, deleted, enabled, kind)), "job", false)
		require.NoError(ht, err)
		var body struct {
			Job struct {
				Responsibility cron.Responsibility `json:"responsibility"`
			} `json:"job"`
		}
		require.NoError(ht, json.Unmarshal(out, &body))
		status := "unknown"
		if native {
			status = "daemon"
		}
		require.Equal(ht, status, body.Job.Responsibility.RuntimeStatus)
	}, hegel.WithTestCases(1000))
}
