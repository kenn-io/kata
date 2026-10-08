package tickticksync

import (
	"encoding/json/jsontext"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Contract: a private durable checkpoint and its binding container must be
// JSON objects. Invalid state must fail rather than reset source versions.
func TestCheckpointObjectContract(t *testing.T) {
	for _, raw := range []string{"null", "[]", `{"project_id":"project-1","_provider_checkpoint":null}`, `{"project_id":"project-1","_provider_checkpoint":[]}`} {
		t.Run(raw, func(t *testing.T) {
			_, err := DecodeCheckpoint(jsontext.Value(raw))
			require.Error(t, err)
		})
	}
	for _, raw := range []string{"null", "[]"} {
		t.Run("stage-"+raw, func(t *testing.T) {
			require.NotPanics(t, func() { _, err := WithCheckpoint(jsontext.Value(raw), Checkpoint{}); require.Error(t, err) })
		})
	}
}

// The checkpoint records each task's last observed status in readable fields
// beside its content hash; status never changes the content hash.
func TestCheckpointRecordsReadableStatus(t *testing.T) {
	c := Config{ProjectID: "project-1", StatusSync: "one-way"}
	at := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)
	task := Task{ID: "task-1", ProjectID: "project-1", Title: "Task", Status: new(2), CompletedTime: "2026-10-01T09:00:00.000+0000"}
	data := ProjectData{Project: Project{ID: "project-1", Kind: "TASK"}, Tasks: []Task{task}}
	_, cp, err := BuildImportBatch(c, data, Checkpoint{}, at)
	require.NoError(t, err)
	v := cp.Versions["task-1"]
	require.Equal(t, 2, v.Status)
	require.Equal(t, task.CompletedTime, v.CompletedTime)
	require.Len(t, v.Hash, 64)
	task.Status, task.CompletedTime = new(0), ""
	data.Tasks = []Task{task}
	_, reopened, err := BuildImportBatch(c, data, cp, at.Add(time.Minute))
	require.NoError(t, err)
	require.Equal(t, v.Hash, reopened.Versions["task-1"].Hash)
	require.Zero(t, reopened.Versions["task-1"].Status)

	raw, err := WithCheckpoint(jsontext.Value(`{"project_id":"project-1"}`), cp)
	require.NoError(t, err)
	decoded, err := DecodeCheckpoint(raw)
	require.NoError(t, err)
	require.Equal(t, cp, decoded)
	bad := cp
	bad.Versions = map[string]TaskVersion{"task-1": {Hash: v.Hash, Status: 7, FirstSeen: v.FirstSeen, Version: v.Version}}
	raw, err = WithCheckpoint(jsontext.Value(`{"project_id":"project-1"}`), bad)
	require.NoError(t, err)
	_, err = DecodeCheckpoint(raw)
	require.Error(t, err)
}
