package main

import (
	"bytes"
	"encoding/json/v2"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"
)

func TestEventsDisplayContribution(t *testing.T) {
	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)
	data := []byte(`{"events":[{"actor":"actor","attributed_teammate":"writer","type":"issue.updated"}]}`)
	require.NoError(t, printEventsHuman(cmd, data))
	require.Contains(t, out.String(), "actor / writer")
	out.Reset()
	require.NoError(t, printEventsAgent(cmd, data))
	require.Contains(t, out.String(), "attributed_teammate=writer")
}

func TestShowDisplayContribution(t *testing.T) {
	var response showResponseForCLI
	require.NoError(t, json.Unmarshal([]byte(`{"issue":{"uid":"issue","short_id":"abcd","title":"Review display contribution","status":"open","author":"coordinator","body":"Read the original contributor after edits and refreshes."},"comments":[{"uid":"comment-1","handle":"c:000001","author":"coordinator","teammate":"route","attributed_teammate":"writer","body":"Revised evidence from the original contributor.","edited_at":"2026-08-01T12:05:00Z"},{"uid":"comment-2","handle":"c:000002","author":"reviewer","teammate":"legacy","body":"Legacy explicit teammate remains readable."},{"uid":"comment-3","handle":"c:000003","author":"coordinator","teammate":"route","attributed_teammate":"","body":"Empty display has no contributor suffix."}]}`), &response))
	var human, agent bytes.Buffer
	require.NoError(t, printShowHuman(&human, response, "example-project", nil))
	require.NoError(t, printShowAgent(&agent, response, "example-project", "show"))
	if os.Getenv("KATA_DISPLAY_PHASE") != "before" {
		require.Contains(t, human.String(), "coordinator / writer")
		require.NotContains(t, human.String(), "coordinator / route")
		require.Contains(t, human.String(), "reviewer / legacy")
		require.Contains(t, agent.String(), "attributed_teammate=writer")
		require.Contains(t, agent.String(), "teammate=route")
	}
	if root := os.Getenv("KATA_DISPLAY_CAPTURE_DIR"); root != "" {
		// #nosec G703 -- The developer explicitly selects this local test-artifact directory.
		require.NoError(t, os.MkdirAll(root, 0750))
		phase := os.Getenv("KATA_DISPLAY_PHASE")
		if phase == "" {
			phase = "after"
		}
		// #nosec G703 -- The developer explicitly selects the local test-artifact path and phase.
		require.NoError(t, os.WriteFile(filepath.Join(root, "cli-"+phase+"-show.ansi"), human.Bytes(), 0600))
		cmd := &cobra.Command{}
		var events bytes.Buffer
		cmd.SetOut(&events)
		require.NoError(t, printEventsHuman(cmd, []byte(`{"events":[{"event_id":1,"type":"issue.created","project_id":1,"actor":"coordinator","attributed_teammate":"planner","created_at":"2026-08-01T12:00:00Z"},{"event_id":2,"type":"issue.commented","project_id":1,"actor":"coordinator","attributed_teammate":"writer","created_at":"2026-08-01T12:01:00Z"},{"event_id":3,"type":"issue.comment_edited","project_id":1,"actor":"editor","attributed_teammate":"editor","created_at":"2026-08-01T12:05:00Z"}]}`)))
		// #nosec G703 -- The developer explicitly selects the local test-artifact path and phase.
		require.NoError(t, os.WriteFile(filepath.Join(root, "cli-"+phase+"-events.ansi"), events.Bytes(), 0600))
	}
}

func TestShowAgentRetainsPresentCommentAttributionProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		display := hegel.Draw(ht, hegel.SampledFrom([]string{"writer-1", "reviewer-2"}))
		empty := ""
		for _, testCase := range []struct {
			name        string
			value       *string
			wantPresent bool
		}{
			{name: "absent"},
			{name: "empty", value: &empty, wantPresent: true},
			{name: "nonempty", value: &display, wantPresent: true},
		} {
			response := showResponseForCLI{
				Comments: []cliShowComment{{
					UID: "comment-1", Author: "coordinator", CreatedAt: "2026-11-04T12:00:00Z",
					AttributedTeammate: testCase.value,
					Teammate:           "route-worker",
				}},
			}
			var agent bytes.Buffer
			if err := printShowAgent(&agent, response, "example-project", "show"); err != nil {
				ht.Fatalf("print agent show: %v", err)
			}
			present := bytes.Contains(agent.Bytes(), []byte("attributed_teammate="))
			if present != testCase.wantPresent {
				ht.Fatalf("agent show attributed_teammate presence for %s = %t, want %t: %s", testCase.name, present, testCase.wantPresent, agent.String())
			}
			if testCase.value != nil && !bytes.Contains(agent.Bytes(), []byte("attributed_teammate="+*testCase.value)) {
				ht.Fatalf("agent show omitted present display value %q: %s", *testCase.value, agent.String())
			}
			if !bytes.Contains(agent.Bytes(), []byte("teammate=route-worker")) {
				ht.Fatalf("agent show omitted routing teammate: %s", agent.String())
			}
		}
	}, hegel.WithTestCases(50), hegel.WithDatabase(t.TempDir()))
}
