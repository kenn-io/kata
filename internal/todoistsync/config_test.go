package todoistsync

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func testConfig() Config {
	return Config{APIOrigin: "https://api.todoist.com", AccountID: "1234567", ProjectID: "project123", HistorySince: "2026-09-01T00:00:00Z"}
}
func testTask() Task {
	at := time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC)
	return Task{ID: "task123", ProjectID: testConfig().ProjectID, Content: "Example task", Description: "Keep the details", AddedBy: "7654321", AddedAt: at, UpdatedAt: at, Priority: 4}
}
func TestConfigStrictIdentity(t *testing.T) {
	c := testConfig()
	raw, err := EncodeConfig(c)
	require.NoError(t, err)
	got, err := DecodeConfig(raw)
	require.NoError(t, err)
	require.Equal(t, "one-way", got.StatusSync)
	require.True(t, got.UseTitlePrefix())
	require.Equal(t, "todoist:https://api.todoist.com/1234567/project123", got.SourceKey())
	for _, raw := range []string{`{"token":"secret"}`, `null`, `{"api_origin":"https://api.todoist.com","account_id":"1234567","project_id":"","history_since":"2026-09-01"}`} {
		_, err := DecodeConfig([]byte(raw))
		require.Error(t, err)
		require.NotContains(t, err.Error(), "secret")
	}
}

func TestImportProjectionOwnership(t *testing.T) {
	c := testConfig()
	row := testTask()
	row.Labels = []string{"upstream"}
	row.Assignee = "8765432"
	row.CompletedAt = new(row.UpdatedAt)
	row.Checked = true
	batch, err := BuildImportBatch(c, []Task{row})
	require.NoError(t, err)
	require.Len(t, batch.Items, 1)
	item := batch.Items[0]
	require.Equal(t, "task:task123", item.ExternalID)
	require.Equal(t, "[Todoist] Example task", item.Title)
	require.Equal(t, "closed", item.Status)
	require.Equal(t, "done", *item.ClosedReason)
	require.EqualValues(t, 1, *item.Priority)
	require.Equal(t, "todoist:7654321", item.Author)
	require.Equal(t, "todoist:8765432", *item.Owner)
	require.Contains(t, item.Body, "Keep the details")
	require.Contains(t, item.Body, "https://app.todoist.com/app/task/task123")
	c.TitlePrefix = new(false)
	batch, err = BuildImportBatch(c, []Task{testTask()})
	require.NoError(t, err)
	require.Equal(t, "Example task", batch.Items[0].Title)
	require.Contains(t, batch.Items[0].Labels, "todoist")
}

// Contract: ordinary provider label names import using Kata's canonical label spelling.
func TestImportNormalizesProviderLabels(t *testing.T) {
	row := testTask()
	row.Labels = []string{"Needs Review", "needs-review", "Important!", "!!!", "Todoist"}
	for _, prefix := range []bool{true, false} {
		c := testConfig()
		c.TitlePrefix = new(prefix)
		batch, err := BuildImportBatch(c, []Task{row})
		require.NoError(t, err)
		require.Equal(t, []string{"needs-review", "important", "imported", "todoist"}, batch.Items[0].Labels)
	}
}
