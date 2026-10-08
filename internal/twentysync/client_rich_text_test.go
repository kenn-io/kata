package twentysync

import (
	"encoding/json/v2"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestTaskEmptyMarkdownDoesNotDiscardBlocknote(t *testing.T) {
	for _, tc := range []struct {
		name, blocknote string
		fail            bool
	}{
		{"text", `[{"type":"paragraph","content":[{"type":"text","text":"Keep details"}]}]`, true},
		{"image", `[{"type":"image","props":{"url":"https://files.example/image.png"}}]`, true},
		{"nested", `[{"type":"paragraph","content":[],"children":[{"type":"paragraph","content":[{"type":"text","text":"Keep details"}]}]}]`, true},
		{"invalid", `not block JSON`, true},
		{"empty array", `[]`, false},
		{"blank paragraph", `[{"id":"example-block","type":"paragraph","content":[],"children":[],"props":{"backgroundColor":"default","textColor":"default","textAlignment":"left"}}]`, false},
		{"blank text", `[{"type":"paragraph","content":[{"type":"text","text":"","styles":{}}],"children":[]}]`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, markdown := range []any{nil, ""} {
				row := taskWire(taskID)
				row["bodyV2"] = map[string]any{"markdown": markdown, "blocknote": tc.blocknote}
				raw, err := json.Marshal(row)
				require.NoError(t, err)
				task, err := parseTask(raw, true)
				if tc.fail {
					require.ErrorContains(t, err, "without Markdown")
				} else {
					require.NoError(t, err)
					require.Empty(t, task.Markdown)
				}
			}
		})
	}
}

func TestRunnerRejectsEmptyMarkdownBeforeReplacingBodyOrCursor(t *testing.T) {
	var incomplete atomic.Bool
	client, c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/metadata" {
			standardMetadata(t, w, r, workspaceID, defaultFields())
			return
		}
		require.Equal(t, "/rest/tasks", r.URL.Path)
		row := taskWire(taskID)
		if incomplete.Load() {
			row["updatedAt"] = "2026-10-05T01:00:00Z"
			row["bodyV2"] = map[string]any{"markdown": "", "blocknote": `[{"type":"paragraph","content":[{"type":"text","text":"Keep details"}]}]`}
		}
		writeJSON(t, w, map[string]any{"data": map[string]any{"tasks": []any{row}}, "pageInfo": map[string]any{"hasNextPage": false}})
	})
	store := adapterStore(t)
	binding := adapterBinding(t, store, c)
	at := time.Now().UTC().Truncate(time.Millisecond)
	runner := NewRunner(RunnerConfig{Store: store, Fetcher: client, Clock: func() time.Time { return at }})
	first, err := runner.RunOnce(t.Context(), binding.ID)
	require.NoError(t, err)
	before := importedIssue(t, store, binding)
	require.Contains(t, before.Body, "# Task details")
	incomplete.Store(true)
	at = at.Add(time.Minute)
	_, err = runner.RunOnce(t.Context(), binding.ID)
	require.ErrorContains(t, err, "without Markdown")
	require.Equal(t, before.Body, importedIssue(t, store, binding).Body)
	after, err := store.IssueSyncBindingByID(t.Context(), binding.ID)
	require.NoError(t, err)
	require.Equal(t, first.Binding.LastCursorAt.UTC(), after.LastCursorAt.UTC())
}
