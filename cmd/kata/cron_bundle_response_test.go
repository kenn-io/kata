package main

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
)

func TestCronBundleRejectsIncompleteAuthoritativeCurrent(t *testing.T) {
	for _, kind := range []string{"job", "workflow"} {
		for _, mode := range []string{"null", "empty", "wrong-kind", "missing"} {
			for _, dry := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/dry=%v", kind, mode, dry), func(t *testing.T) { cronBundleResponseCase(t, kind, mode, dry, true, false) })
			}
		}
	}
}
func TestCronBundleRejectsIncompleteWriteSuccess(t *testing.T) {
	for _, mode := range []string{"empty-envelope", "empty-record", "bad-json", "wrong-uid", "wrong-project", "missing-event", "wrong-definition", "async"} {
		for _, replace := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/replace=%v", mode, replace), func(t *testing.T) { cronBundleResponseCase(t, "workflow", mode, false, false, replace) })
		}
	}
}

func cronBundleResponseCase(t *testing.T, kind, mode string, dry, currentCase, replace bool) {
	t.Helper()
	source, target, sourceUID, event := cronCLIUID(t), cronCLIUID(t), cronCLIUID(t), cronCLIUID(t)
	b := cron.Bundle{Format: "kata.cron.bundle", Version: 1, Source: cron.BundleSource{ProjectUID: source, ProjectName: "source-project", ExportedAt: "2026-11-04T00:00:00Z"}, Workflows: []cron.BundleDefinition{}, Jobs: []cron.BundleDefinition{}}
	job := cronCLIJob(t).Definition.Native()
	if kind == "workflow" {
		b.Workflows = append(b.Workflows, cron.BundleDefinition{UID: sourceUID, DefinitionEventUID: event, Name: "Inspect", Definition: []byte(`{"version":1,"steps":[{"key":"inspect","kind":"command","command":"git status"}]}`)})
		job.Action = cron.Action{Kind: "execute", WorkflowUID: sourceUID}
	}
	raw, err := json.Marshal(job)
	require.NoError(t, err)
	b.Jobs = append(b.Jobs, cron.BundleDefinition{UID: cronCLIUID(t), DefinitionEventUID: event, Name: "Inspect", Definition: raw})
	plan, err := cron.PlanBundle(b, cron.BundleTarget{ProjectUID: target})
	require.NoError(t, err)
	entry := plan.Jobs[0]
	if kind == "workflow" {
		entry = plan.Workflows[0]
	}
	record := func() map[string]any {
		return map[string]any{"uid": entry.UID, "project_id": 7, "name": entry.Name, "definition_event_uid": event, "definition": entry.Definition}
	}
	current := record()
	current["name"] = "Other"
	if currentCase {
		switch mode {
		case "null":
			current["definition"] = jsontext.Value(`null`)
		case "empty":
			current["definition"] = jsontext.Value(`{}`)
		case "wrong-kind":
			current["definition"] = jsontext.Value(`{"version":1,"kind":"workflow"}`)
		}
	}
	puts, jobs := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v1/projects" {
			_, _ = fmt.Fprintf(w, `{"projects":[{"id":7,"name":"spoke-project","uid":%q}]}`, target)
			return
		}
		if r.URL.Path == "/api/v1/projects/7" {
			_, _ = io.WriteString(w, `{"aliases":[]}`)
			return
		}
		if r.Method == http.MethodGet {
			if mode == "missing" {
				_, _ = io.WriteString(w, `{}`)
				return
			}
			require.NoError(t, json.MarshalWrite(w, map[string]any{kind: current}))
			return
		}
		puts++
		if kind == "workflow" && strings.Contains(r.URL.Path, "/cron/jobs/") {
			jobs++
			require.NoError(t, json.MarshalWrite(w, map[string]any{"job": map[string]any{"uid": plan.Jobs[0].UID, "project_id": 7, "name": plan.Jobs[0].Name, "definition_event_uid": event, "definition": plan.Jobs[0].Definition}}))
			return
		}
		if (currentCase || replace) && puts == 1 {
			data := map[string]any{"current": current}
			if mode == "missing" {
				data = map[string]any{}
			}
			w.WriteHeader(409)
			require.NoError(t, json.MarshalWrite(w, map[string]any{"error": map[string]any{"code": "cron_conflict", "message": "conflict", "data": data}}))
			return
		}
		response := record()
		switch mode {
		case "empty-envelope":
			_, _ = io.WriteString(w, `{}`)
			return
		case "empty-record":
			_, _ = fmt.Fprintf(w, `{%q:{}}`, kind)
			return
		case "bad-json":
			_, _ = io.WriteString(w, `{`)
			return
		case "wrong-uid":
			response["uid"] = sourceUID
		case "wrong-project":
			response["project_id"] = 8
		case "missing-event":
			delete(response, "definition_event_uid")
		case "wrong-definition":
			response["definition"] = jsontext.Value(`{"version":1,"steps":[]}`)
		case "async":
			w.WriteHeader(202)
		}
		require.NoError(t, json.MarshalWrite(w, map[string]any{kind: response}))
	}))
	t.Cleanup(server.Close)
	encoded, err := json.Marshal(b)
	require.NoError(t, err)
	args := []string{"import", "--file", "-"}
	if currentCase || replace {
		args = append(args, "--replace")
	}
	if dry {
		args = append(args, "--dry-run")
	}
	out, _, err := executeCronCLI(t, server.URL, strings.NewReader(string(encoded)), args...)
	require.Error(t, err, "incomplete evidence must not authorize completion/replacement")
	var report bundleImportResult
	require.NoError(t, json.Unmarshal([]byte(out), &report))
	require.Len(t, report.Items, 1)
	if !currentCase {
		require.Equal(t, "uncertain", report.Items[0].Outcome)
	}
	require.Zero(t, jobs, "no dependent job after an unresolved workflow")
	if currentCase {
		if dry {
			require.Zero(t, puts)
		} else {
			require.Equal(t, 1, puts, "do not replace from incomplete authoritative current")
		}
	}
}
