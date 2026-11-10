package main

import (
	"encoding/json/v2"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
)

func TestCronBundleImportAuthoritativeConflicts(t *testing.T) {
	for _, scenario := range []string{"unchanged", "replace", "tombstone", "wrong UID", "wrong project", "other conflict", "CAS race", "lost accepted response"} {
		t.Run(scenario, func(t *testing.T) {
			source, target, jobUID, eventUID := cronCLIUID(t), cronCLIUID(t), cronCLIUID(t), cronCLIUID(t)
			input := cronCLIJob(t)
			raw, err := json.Marshal(input.Definition.Native())
			require.NoError(t, err)
			b := cron.Bundle{Format: "kata.cron.bundle", Version: 1, Source: cron.BundleSource{ProjectUID: source, ProjectName: "source-project", ExportedAt: "2026-11-04T00:00:00Z"}, Jobs: []cron.BundleDefinition{{UID: jobUID, DefinitionEventUID: eventUID, Name: input.Name, Definition: raw}}, Workflows: []cron.BundleDefinition{}}
			plan, err := cron.PlanBundle(b, cron.BundleTarget{ProjectUID: target})
			require.NoError(t, err)
			current := map[string]any{"uid": plan.Jobs[0].UID, "project_id": 7, "name": plan.Jobs[0].Name, "definition_event_uid": eventUID, "definition": plan.Jobs[0].Definition}
			code := "cron_conflict"
			switch scenario {
			case "replace", "CAS race":
				current["name"] = "Other definition"
			case "tombstone":
				current["deleted_at"] = "2026-11-04T00:00:00Z"
			case "wrong UID":
				current["uid"] = cronCLIUID(t)
			case "wrong project":
				current["project_id"] = 8
			case "other conflict":
				code = "federated_read_only"
			}
			puts, reads := 0, 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/api/v1/projects" {
					_, _ = io.WriteString(w, `{"projects":[{"id":7,"name":"spoke-project","uid":"`+target+`"}]}`)
					return
				}
				if r.URL.Path == "/api/v1/projects/7" {
					_, _ = io.WriteString(w, `{"aliases":[]}`)
					return
				}
				if r.Method == http.MethodGet {
					reads++
					w.WriteHeader(404)
					_, _ = io.WriteString(w, `{"error":{"code":"not_found","message":"stale local projection"}}`)
					return
				}
				require.Equal(t, http.MethodPut, r.Method)
				puts++
				if scenario == "lost accepted response" && puts == 1 {
					conn, _, err := w.(http.Hijacker).Hijack()
					require.NoError(t, err)
					_ = conn.Close()
					return
				}
				if scenario == "replace" && puts == 2 {
					var request map[string]any
					require.NoError(t, json.UnmarshalRead(r.Body, &request))
					require.Equal(t, eventUID, request["expected_event_uid"])
					current["name"] = plan.Jobs[0].Name
					require.NoError(t, json.MarshalWrite(w, map[string]any{"job": current}))
					return
				}
				w.WriteHeader(409)
				require.NoError(t, json.MarshalWrite(w, map[string]any{"error": map[string]any{"code": code, "message": "conflict", "data": map[string]any{"current": current}}}))
			}))
			t.Cleanup(server.Close)
			encoded, err := json.Marshal(b)
			require.NoError(t, err)
			args := []string{"import", "--file", "-"}
			if scenario == "replace" || scenario == "CAS race" {
				args = append(args, "--replace")
			}
			out, _, err := executeCronCLI(t, server.URL, strings.NewReader(string(encoded)), args...)
			switch scenario {
			case "unchanged":
				require.NoError(t, err)
				require.Contains(t, out, `"unchanged"`)
			case "replace":
				require.NoError(t, err)
				require.Contains(t, out, `"replaced"`)
				require.Equal(t, 2, puts)
			case "lost accepted response":
				require.Error(t, err)
				require.Contains(t, out, `"uncertain"`)
				out, _, err = executeCronCLI(t, server.URL, strings.NewReader(string(encoded)), args...)
				require.NoError(t, err)
				require.Contains(t, out, `"unchanged"`)
			default:
				require.Error(t, err)
			}
			if scenario == "CAS race" {
				require.Equal(t, 2, puts)
			}
			require.Zero(t, reads, "conflict recovery must not consult the stale spoke projection")
		})
	}
}
