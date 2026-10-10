package tickticksync

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Contract: a scoped task has one stable identity; unchanged observations reuse
// their version, and completion never restamps content in two-way mode.
func TestTickTickConfigAndMappingContract(t *testing.T) {
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	c := Config{ProjectID: "project-1", StatusSync: "two-way", TitlePrefix: new(false)}
	raw, err := EncodeConfig(c)
	require.NoError(t, err)
	c, err = DecodeConfig(raw)
	require.NoError(t, err)
	require.Equal(t, "ticktick:project-1", c.SourceKey())
	data := ProjectData{Project: Project{ID: "project-1", Name: "Example tasks", Kind: "TASK"}, Tasks: []Task{{ID: "task-1", ProjectID: "project-1", Title: "Example task", Content: "Body", Priority: 5, Status: new(0), Items: []ChecklistItem{{ID: "item-1", Title: "Step", Status: 1}}}}}
	batch, cp, err := BuildImportBatch(c, data, Checkpoint{}, at)
	require.NoError(t, err)
	require.Len(t, batch.Items, 1)
	require.Equal(t, "task:task-1", batch.Items[0].ExternalID)
	require.Equal(t, "Example task", batch.Items[0].Title)
	require.Equal(t, int64(1), *batch.Items[0].Priority)
	require.Contains(t, batch.Items[0].Body, "- [x] Step")
	require.Equal(t, []string{"ticktick"}, batch.Items[0].Labels)
	again, next, err := BuildImportBatch(c, data, cp, at.Add(time.Minute))
	require.NoError(t, err)
	require.Equal(t, batch.Items[0].UpdatedAt, again.Items[0].UpdatedAt)
	require.Equal(t, cp, next)
	data.Tasks[0].Status = new(2)
	completed, _, err := BuildImportBatch(c, data, cp, at.Add(time.Minute))
	require.NoError(t, err)
	require.Equal(t, batch.Items[0].UpdatedAt, completed.Items[0].UpdatedAt)
	c.StatusSync = "one-way"
	one, cp, err := BuildImportBatch(c, data, Checkpoint{}, at)
	require.NoError(t, err)
	require.Equal(t, "closed", one.Items[0].Status)
	require.Equal(t, "done", *one.Items[0].ClosedReason)
	data.Tasks[0].Status = new(0)
	reopened, _, err := BuildImportBatch(c, data, cp, at.Add(time.Minute))
	require.NoError(t, err)
	require.Equal(t, one.Items[0].UpdatedAt, reopened.Items[0].UpdatedAt, "a status-only change must not advance the source-content timestamp")
	require.Contains(t, reopened.ImportStatusObservations, "task:task-1")
	for _, p := range []struct {
		in              int
		want            int64
		wantPrioritized bool
	}{{0, 0, false}, {1, 3, true}, {3, 2, true}, {5, 1, true}} {
		data.Tasks[0].Priority = p.in
		got, _, err := BuildImportBatch(c, data, Checkpoint{}, at)
		require.NoError(t, err)
		if p.wantPrioritized {
			require.Equal(t, p.want, *got.Items[0].Priority)
		} else {
			require.Nil(t, got.Items[0].Priority)
		}
	}
}

func TestTickTickRejectsUnscopedAndInvalidObservations(t *testing.T) {
	for _, id := range []string{"", "../project", "project/task", "project?token=x", "https://api.example", "project\n"} {
		_, err := EncodeConfig(Config{ProjectID: id})
		require.Error(t, err)
	}
	at := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	c := Config{ProjectID: "project-1"}
	base := Task{ID: "task-1", ProjectID: "project-1", Title: "Task", Status: new(0)}
	for _, mutate := range []func(*Task){func(x *Task) { x.ProjectID = "other-project" }, func(x *Task) { x.Status = nil }, func(x *Task) { x.Status = new(3) }, func(x *Task) { x.Priority = 2 }, func(x *Task) { x.Title = "bad\x00title" }} {
		x := base
		mutate(&x)
		_, _, err := BuildImportBatch(c, ProjectData{Project: Project{ID: "project-1", Kind: "TASK"}, Tasks: []Task{x}}, Checkpoint{}, at)
		require.Error(t, err)
	}
	data := ProjectData{Project: Project{ID: "project-1", Kind: "TASK"}, Tasks: []Task{base, base}}
	b, _, err := BuildImportBatch(c, data, Checkpoint{}, at)
	require.NoError(t, err)
	require.Len(t, b.Items, 1)
	data.Tasks[1].Title = "Conflict"
	_, _, err = BuildImportBatch(c, data, Checkpoint{}, at)
	require.Error(t, err)
	data.Tasks = nil
	data.Project.Closed = true
	_, _, err = BuildImportBatch(c, data, Checkpoint{}, at)
	require.Error(t, err)
}

// A status-direction change leaves unchanged content at its saved version.
// It must not turn existing source text into a newer edit over local scalars.
func TestStatusModeOnlyPreservesContentVersion(t *testing.T) {
	at := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	data := ProjectData{Project: Project{ID: "project-1", Kind: "TASK"}, Tasks: []Task{{ID: "task-1", ProjectID: "project-1", Title: "Source title", Status: new(0)}}}
	for _, mode := range []string{"one-way", "two-way"} {
		t.Run(mode, func(t *testing.T) {
			c := Config{ProjectID: "project-1", StatusSync: mode}
			before, cp, err := BuildImportBatch(c, data, Checkpoint{}, at)
			require.NoError(t, err)
			if mode == "one-way" {
				c.StatusSync = "two-way"
			} else {
				c.StatusSync = "one-way"
			}
			after, _, err := BuildImportBatch(c, data, cp, at.Add(time.Hour))
			require.NoError(t, err)
			require.Equal(t, before.Items[0].UpdatedAt, after.Items[0].UpdatedAt)
		})
	}
}

func TestReenabledTwoWaySyncIgnoresStatusChangesSinceOneWayCheckpoint(t *testing.T) {
	at := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	c := Config{ProjectID: "project-1", StatusSync: "one-way"}
	data := ProjectData{Project: Project{ID: "project-1", Kind: "TASK"}, Tasks: []Task{{ID: "task-1", ProjectID: "project-1", Title: "Source title", Status: new(0)}}}

	_, checkpoint, err := BuildImportBatch(c, data, Checkpoint{}, at)
	require.NoError(t, err)

	data.Tasks[0].Status = new(2)
	completed, checkpoint, err := BuildImportBatch(c, data, checkpoint, at.Add(time.Minute))
	require.NoError(t, err)
	require.Equal(t, at, completed.Items[0].UpdatedAt, "the acknowledged completion is independent of content version")
	require.Contains(t, completed.ImportStatusObservations, "task:task-1")

	// Sync is disabled while the local issue is edited. TickTick reopens the
	// task before two-way mode is enabled again, with source content unchanged.
	data.Tasks[0].Status = new(0)
	c.StatusSync = "two-way"
	reopened, next, err := BuildImportBatch(c, data, checkpoint, at.Add(2*time.Minute))
	require.NoError(t, err)
	require.Equal(t, completed.Items[0].UpdatedAt, reopened.Items[0].UpdatedAt)
	require.Equal(t, checkpoint.Versions["task-1"].Version, next.Versions["task-1"].Version)
}

func TestPresentationModeOnlyPreservesContentVersion(t *testing.T) {
	at := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	data := ProjectData{Project: Project{ID: "project-1", Kind: "TASK"}, Tasks: []Task{{ID: "task-1", ProjectID: "project-1", Title: "Source title", Status: new(0)}}}
	for _, tc := range []struct {
		name         string
		beforeMode   string
		beforePrefix bool
		afterMode    string
		afterPrefix  bool
	}{
		{name: "prefix on to off", beforeMode: "one-way", beforePrefix: true, afterMode: "one-way", afterPrefix: false},
		{name: "prefix off to on", beforeMode: "one-way", beforePrefix: false, afterMode: "one-way", afterPrefix: true},
		{name: "prefix and status mode change", beforeMode: "one-way", beforePrefix: true, afterMode: "two-way", afterPrefix: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			beforeConfig := Config{ProjectID: "project-1", StatusSync: tc.beforeMode, TitlePrefix: new(tc.beforePrefix)}
			before, cp, err := BuildImportBatch(beforeConfig, data, Checkpoint{}, at)
			require.NoError(t, err)

			afterConfig := Config{ProjectID: "project-1", StatusSync: tc.afterMode, TitlePrefix: new(tc.afterPrefix)}
			after, _, err := BuildImportBatch(afterConfig, data, cp, at.Add(time.Hour))
			require.NoError(t, err)
			require.Equal(t, before.Items[0].UpdatedAt, after.Items[0].UpdatedAt)
		})
	}
}

func TestTickTickLabelReconciliationFollowsTitlePrefixMode(t *testing.T) {
	at := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	data := ProjectData{Project: Project{ID: "project-1", Kind: "TASK"}, Tasks: []Task{{ID: "task-1", ProjectID: "project-1", Title: "Source title", Status: new(0)}}}
	for _, tc := range []struct {
		name   string
		prefix bool
	}{{name: "prefix enabled", prefix: true}, {name: "prefix disabled", prefix: false}} {
		t.Run(tc.name, func(t *testing.T) {
			c := Config{ProjectID: "project-1", StatusSync: "one-way", TitlePrefix: new(tc.prefix)}
			batch, _, err := BuildImportBatch(c, data, Checkpoint{}, at)
			require.NoError(t, err)
			require.Len(t, batch.Items, 1)
			if tc.prefix {
				require.Empty(t, batch.Items[0].Labels)
			} else {
				require.Equal(t, []string{"ticktick"}, batch.Items[0].Labels)
			}
			require.Equal(t, []string{"ticktick"}, batch.ReconcileLabelsForUnchanged[batch.Items[0].ExternalID])
		})
	}
}
