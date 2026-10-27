package main

import (
	"bytes"
	"encoding/json/v2"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
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
		require.NoError(t, os.MkdirAll(root, 0750))
		phase := os.Getenv("KATA_DISPLAY_PHASE")
		if phase == "" {
			phase = "after"
		}
		require.NoError(t, os.WriteFile(filepath.Join(root, "cli-"+phase+"-show.ansi"), human.Bytes(), 0600))
		cmd := &cobra.Command{}
		var events bytes.Buffer
		cmd.SetOut(&events)
		require.NoError(t, printEventsHuman(cmd, []byte(`{"events":[{"event_id":1,"type":"issue.created","project_id":1,"actor":"coordinator","attributed_teammate":"planner","created_at":"2026-08-01T12:00:00Z"},{"event_id":2,"type":"issue.commented","project_id":1,"actor":"coordinator","attributed_teammate":"writer","created_at":"2026-08-01T12:01:00Z"},{"event_id":3,"type":"issue.comment_edited","project_id":1,"actor":"editor","attributed_teammate":"editor","created_at":"2026-08-01T12:05:00Z"}]}`)))
		require.NoError(t, os.WriteFile(filepath.Join(root, "cli-"+phase+"-events.ansi"), events.Bytes(), 0600))
	}
}
