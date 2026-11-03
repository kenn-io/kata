package tui

import (
	"encoding/json/v2"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/commentref"
)

func TestCommentDisplayContribution(t *testing.T) {
	var comment CommentEntry
	require.NoError(t, json.Unmarshal([]byte(`{"author":"actor","teammate":"route","attributed_teammate":"writer","body":"Hello"}`), &comment))
	require.Equal(t, "actor / writer", commentAttribution(comment))
}

func TestDisplayContributionRendering(t *testing.T) {
	defer snapshotInit(t)()
	when := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	renderNow = func() time.Time { return when.Add(time.Hour) }
	issue := Issue{UID: "01EXAMPLE", ShortID: "abcd", Title: "Review display contribution", Status: "open", Author: "coordinator", Body: "Read the original contributor after edits and refreshes."}
	comments := []CommentEntry{
		{ID: 1, UID: "comment-1", Handle: "c:000001", Author: "coordinator", Teammate: "route", AttributedTeammate: new("writer"), Body: "Revised evidence from the original contributor.", CreatedAt: when, EditedAt: new(when.Add(time.Minute)), Backlinks: []commentref.Link{{UID: "comment-4", Handle: "peer:000004", Kind: "confirm", Author: "reviewer", Teammate: "route-review", AttributedTeammate: new("checker"), Status: "moved", Body: "The moved response still identifies its creator.", CreatedAt: when}}},
		{ID: 2, UID: "comment-2", Handle: "c:000002", Author: "reviewer", Teammate: "legacy", Body: "Legacy explicit teammate remains readable.", CreatedAt: when},
		{ID: 3, UID: "comment-3", Handle: "c:000003", Author: "coordinator", Teammate: "route", AttributedTeammate: new(""), Body: "Empty display has no contributor suffix.", CreatedAt: when},
	}
	events := []EventLogEntry{
		{ID: 1, Type: "issue.created", Actor: "coordinator", AttributedTeammate: "planner", CreatedAt: when},
		{ID: 2, Type: "issue.commented", Actor: "coordinator", AttributedTeammate: "writer", CreatedAt: when},
		{ID: 3, Type: "issue.comment_edited", Actor: "editor", AttributedTeammate: "editor", CreatedAt: when},
		{ID: 4, Type: "issue.closed", Actor: "reviewer", AttributedTeammate: "checker", CreatedAt: when, Payload: map[string]any{"transcript": map[string]any{"session_id": "00000000-0000-4000-8000-000000000001"}}},
	}
	for _, theme := range []string{"light", "dark"} {
		t.Run(theme, func(t *testing.T) {
			t.Setenv("KATA_COLOR_MODE", theme)
			applyDefaultColorMode()
			for _, state := range []string{"populated", "narrow", "empty"} {
				width, height := 120, 32
				if state == "narrow" {
					width, height = 60, 42
				}
				dm := detailModel{issue: &issue, comments: comments, events: events}
				if state == "empty" {
					dm.comments = nil
					dm.events = nil
				}
				for _, tab := range []struct {
					name  string
					value detailTab
				}{{"comments", tabComments}, {"history", tabEvents}} {
					dm.activeTab = tab.value
					output := dm.View(width, height, viewChrome{})
					if state != "empty" && os.Getenv("KATA_DISPLAY_PHASE") != "before" {
						require.Contains(t, ansi.Strip(output), "coordinator / ")
					}
					if root := os.Getenv("KATA_DISPLAY_CAPTURE_DIR"); root != "" {
						// #nosec G703 -- The developer explicitly selects this local test-artifact directory.
						require.NoError(t, os.MkdirAll(root, 0750))
						phase := os.Getenv("KATA_DISPLAY_PHASE")
						if phase == "" {
							phase = "after"
						}
						path := filepath.Join(root, fmt.Sprintf("tui-%s-%s-%s-%s.ansi", phase, theme, state, tab.name))
						// #nosec G703 -- The developer explicitly selects the local test-artifact path and phase.
						require.NoError(t, os.WriteFile(path, []byte(output), 0600))
					}
				}
			}
		})
	}
}
