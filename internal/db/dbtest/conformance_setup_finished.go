package dbtest

import (
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func checkSetupFinishedDelivery(t *testing.T, store db.Storage, backend Backend) error {
	ctx := t.Context()
	observed := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	pending, complete, err := store.PrepareSetupFinished(ctx, observed)
	require.NoError(t, err)
	require.Nil(t, pending)
	require.False(t, complete)
	for _, state := range []string{"closed", "deleted-only", "archived project", "system project"} {
		t.Run(state, func(t *testing.T) {
			target := backend.Open(t)
			defer func() { require.NoError(t, target.Close()) }()
			project, err := target.SystemProject(ctx)
			require.NoError(t, err)
			if state != "system project" {
				project, err = target.CreateProject(ctx, "visibility-project")
				require.NoError(t, err)
			}
			issue, _, err := target.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Visibility issue", Author: "tester"})
			require.NoError(t, err)
			switch state {
			case "closed":
				_, _, _, err = target.CloseIssue(ctx, issue.ID, "done", "tester", "Completed issue", nil)
			case "deleted-only":
				_, _, _, err = target.SoftDeleteIssue(ctx, issue.ID, "tester")
			case "archived project":
				_, _, err = target.RemoveProject(ctx, db.RemoveProjectParams{ProjectID: project.ID, Actor: "tester", Force: true})
			}
			require.NoError(t, err)
			delivery, complete, err := target.PrepareSetupFinished(ctx, observed)
			require.NoError(t, err)
			require.False(t, complete)
			require.Equal(t, state == "closed", delivery != nil)
		})
	}
	project, err := store.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "First issue", Author: "tester"})
	require.NoError(t, err)
	other := backend.OpenSame(t, store)
	deliveries := make(chan *db.SetupFinishedDelivery, 8)
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			target := store
			if i%2 == 0 {
				target = other
			}
			retry, done, err := target.PrepareSetupFinished(ctx, observed)
			assert.NoError(t, err)
			assert.False(t, done)
			deliveries <- retry
		})
	}
	wg.Wait()
	close(deliveries)
	pending = <-deliveries
	require.NotNil(t, pending)
	require.NotEqual(t, uuid.Nil(), pending.ID)
	require.Equal(t, observed, pending.Time)
	require.Equal(t, store.InstanceUID(), pending.InstanceUID)
	for delivery := range deliveries {
		require.Equal(t, pending, delivery)
	}
	_, _, _, err = store.SoftDeleteIssue(ctx, issue.ID, "tester")
	require.NoError(t, err)
	retry, _, err := other.PrepareSetupFinished(ctx, observed.Add(time.Hour))
	require.NoError(t, err)
	require.Equal(t, pending, retry, "delivery survives issue removal and reopen")
	var records []db.ImportRecord
	for row, err := range store.ExportMeta(ctx) {
		require.NoError(t, err)
		records = append(records, &db.MetaKV{Key: row.Key, Value: row.Value})
	}
	restored := backend.Open(t)
	defer func() { require.NoError(t, restored.Close()) }()
	require.NoError(t, restored.ImportReplay(ctx, records, db.ImportOptions{}))
	retry, _, err = restored.PrepareSetupFinished(ctx, observed.Add(time.Hour))
	require.NoError(t, err)
	require.Equal(t, pending, retry)
	wrong := *pending
	wrong.ID = uuid.New()
	require.Error(t, other.CompleteSetupFinished(ctx, wrong))
	wrong = *pending
	wrong.Time = wrong.Time.Add(time.Second)
	require.Error(t, other.CompleteSetupFinished(ctx, wrong))
	retry, _, err = other.PrepareSetupFinished(ctx, observed)
	require.NoError(t, err)
	require.Equal(t, pending, retry, "failed completion retains pending identity")
	require.NoError(t, other.CompleteSetupFinished(ctx, *pending))
	require.NoError(t, store.CompleteSetupFinished(ctx, *pending))
	retry, complete, err = store.PrepareSetupFinished(ctx, observed)
	require.NoError(t, err)
	require.Nil(t, retry)
	require.True(t, complete)
	for _, record := range records {
		row := record.(*db.MetaKV)
		if row.Key == "instance_uid" {
			row.Value = "01HZNQ7VFPK1XGD8R5MABCD4EX"
		}
	}
	require.NoError(t, store.ImportReplay(ctx, records, db.ImportOptions{}))
	retry, complete, err = other.PrepareSetupFinished(ctx, observed)
	require.NoError(t, err)
	require.Nil(t, retry, "new install waits for a visible issue")
	require.False(t, complete)
	project, err = store.CreateProject(ctx, "new-project")
	require.NoError(t, err)
	_, _, err = store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "New install issue", Author: "tester"})
	require.NoError(t, err)
	retry, _, err = other.PrepareSetupFinished(ctx, observed.Add(time.Hour))
	require.NoError(t, err)
	require.NotNil(t, retry)
	require.Equal(t, "01HZNQ7VFPK1XGD8R5MABCD4EX", retry.InstanceUID, "read actual identity instead of another daemon's cache")
	require.NotEqual(t, pending.ID, retry.ID)
	require.Equal(t, observed.Add(time.Hour), retry.Time)
	require.Error(t, store.CompleteSetupFinished(ctx, *pending), "stale completion preserves new pending delivery")
	next, _, err := store.PrepareSetupFinished(ctx, observed)
	require.NoError(t, err)
	require.Equal(t, retry, next)
	require.NoError(t, store.ImportReplay(ctx, []db.ImportRecord{&db.MetaKV{Key: "setup_finished_pending", Value: "{broken"}}, db.ImportOptions{}))
	_, _, err = store.PrepareSetupFinished(ctx, observed)
	require.Error(t, err, "malformed pending state must retain its identity")
	return nil
}
