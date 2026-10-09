package twentysync

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func testTask() Task {
	at := time.Date(2026, 10, 4, 0, 0, 0, 123456789, time.UTC)
	return Task{ID: taskID, Title: "Example task", Markdown: "# Details\n\n- **keep formatting**", Status: new("DONE"), CreatedAt: at, UpdatedAt: at.Add(time.Hour), CreatorID: workspaceID, AssigneeID: new(workspaceID)}
}

func TestTaskMapping(t *testing.T) {
	c := testConfig()
	s := Schema{StatusOptions: []string{"TODO", "IN_PROGRESS", "DONE"}}
	batch, err := BuildImportBatch(c.SourceKey(), c, s, []Task{testTask()})
	require.NoError(t, err)
	require.Len(t, batch.Items, 1)
	i := batch.Items[0]
	require.Equal(t, "task:"+taskID, i.ExternalID)
	require.Equal(t, "[Twenty 22222222] Example task", i.Title)
	require.Contains(t, i.Body, "# Details\n\n- **keep formatting**")
	require.Contains(t, i.Body, "https://ui.example/object/task/"+taskID)
	require.Equal(t, "twenty:"+workspaceID, i.Author)
	require.Equal(t, "twenty:"+workspaceID, *i.Owner)
	require.Equal(t, "closed", i.Status)
	require.Equal(t, "done", *i.ClosedReason)
	require.Equal(t, testTask().UpdatedAt.Truncate(time.Millisecond), i.UpdatedAt)
	c.TitlePrefix = new(false)
	task := testTask()
	task.Title = ""
	task.AssigneeID = nil
	task.CreatorID = ""
	task.Status = nil
	batch, err = BuildImportBatch(c.SourceKey(), c, s, []Task{task})
	require.NoError(t, err)
	require.Equal(t, "(untitled)", batch.Items[0].Title)
	require.Equal(t, []string{"twenty"}, batch.Items[0].Labels)
	require.Nil(t, batch.Items[0].Owner)
	require.Equal(t, "twenty-unknown", batch.Items[0].Author)
	require.Equal(t, "open", batch.Items[0].Status)
}

func TestTaskMappingRejectsIncompleteBatch(t *testing.T) {
	c := testConfig()
	schema := Schema{StatusOptions: []string{"TODO", "IN_PROGRESS", "DONE"}}
	for _, edit := range []func(*Task){
		func(t *Task) { t.ID = "../secret" },
		func(t *Task) { t.Status = new("UNKNOWN") },
		func(t *Task) { t.UpdatedAt = time.Time{} },
		func(t *Task) { t.CreatedAt = t.UpdatedAt.Add(time.Hour) },
		func(t *Task) { t.Title = "bad\x00title" },
		func(t *Task) { t.Markdown = string([]byte{0xff}) },
		func(t *Task) { t.CreatorID = "bad" },
		func(t *Task) { t.AssigneeID = new("bad") },
	} {
		task := testTask()
		edit(&task)
		_, err := BuildImportBatch(c.SourceKey(), c, schema, []Task{testTask(), task})
		require.Error(t, err)
	}
	_, err := BuildImportBatch("wrong-source", c, schema, nil)
	require.Error(t, err)
	_, err = BuildImportBatch(c.SourceKey(), c, schema, []Task{testTask(), testTask()})
	require.Error(t, err)
}
