package dbtest

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
)

// Contract: root pause is idempotent, uses exact metadata events, and serializes with delivery.
func checkCronProjectPause(t *testing.T, s db.Storage, b Backend) error {
	ctx := t.Context()
	t.Run("lifecycle", func(t *testing.T) {
		p, e := s.CreateProject(ctx, "pause-project")
		require.NoError(t, e)
		out, e := s.SetCronProjectPause(ctx, db.SetCronProjectPauseIn{ProjectID: p.ID, Actor: "operator", Reason: "maintenance", Paused: true})
		require.NoError(t, e)
		require.True(t, out.Changed)
		require.Equal(t, p.Revision+1, out.NewRevision)
		require.Equal(t, "project.metadata_updated", out.Event.Type)
		require.Equal(t, "operator", out.Event.Actor)
		pause := db.CronProjectPauseFromMetadata(out.Project.Metadata)
		require.NotNil(t, pause)
		require.False(t, pause.Invalid)
		require.Equal(t, "maintenance", pause.Reason)
		require.Equal(t, "operator", pause.PausedBy)
		require.WithinDuration(t, time.Now(), pause.PausedAt, time.Minute)
		repeat, e := s.SetCronProjectPause(ctx, db.SetCronProjectPauseIn{ProjectID: p.ID, Actor: "other", Reason: "changed", Paused: true})
		require.NoError(t, e)
		require.False(t, repeat.Changed)
		require.Empty(t, repeat.Event.UID)
		require.Equal(t, out.Project.Metadata, repeat.Project.Metadata)
		require.Equal(t, out.NewRevision, repeat.NewRevision)
		cleared, e := s.SetCronProjectPause(ctx, db.SetCronProjectPauseIn{ProjectID: p.ID, Actor: "operator"})
		require.NoError(t, e)
		require.True(t, cleared.Changed)
		require.Equal(t, out.NewRevision+1, cleared.NewRevision)
		require.Nil(t, db.CronProjectPauseFromMetadata(cleared.Project.Metadata))
		repeat, e = s.SetCronProjectPause(ctx, db.SetCronProjectPauseIn{ProjectID: p.ID, Actor: "operator"})
		require.NoError(t, e)
		require.False(t, repeat.Changed)
		_, e = s.SetCronProjectPause(ctx, db.SetCronProjectPauseIn{ProjectID: p.ID, Actor: "operator", Reason: strings.Repeat("é", 129), Paused: true})
		require.Error(t, e)
	})
	t.Run("reserved metadata", func(t *testing.T) {
		p, e := s.CreateProject(ctx, "reserved-project")
		require.NoError(t, e)
		for _, key := range []string{"cron.pause", "cron.request.01ARZ3NDEKTSV4RRFFQ69G5FAV"} {
			for _, value := range []string{`true`, `null`} {
				_, e = s.PatchProjectMetadata(ctx, db.PatchProjectMetadataIn{ProjectID: p.ID, Actor: "operator", Patch: map[string]jsontext.Value{key: []byte(value)}})
				require.Error(t, e, key)
			}
		}
		_, e = s.PatchProjectMetadata(ctx, db.PatchProjectMetadataIn{ProjectID: p.ID, Actor: "operator", Patch: map[string]jsontext.Value{"cron.pause.note": []byte(`"ordinary"`)}})
		require.NoError(t, e)
	})
	t.Run("malformed recovery", func(t *testing.T) {
		p, e := s.CreateProject(ctx, "legacy-project")
		require.NoError(t, e)
		q := s.(interface {
			ExecContext(context.Context, string, ...any) (sql.Result, error)
		})
		_, e = q.ExecContext(ctx, `UPDATE projects SET metadata=$1 WHERE id=$2`, `{"cron.pause":{"legacy":true},"keep":9007199254740993}`, p.ID)
		require.NoError(t, e)
		out, e := s.SetCronProjectPause(ctx, db.SetCronProjectPauseIn{ProjectID: p.ID, Actor: "operator", Paused: true})
		require.NoError(t, e)
		require.False(t, out.Changed)
		require.True(t, db.CronProjectPauseFromMetadata(out.Project.Metadata).Invalid)
		out, e = s.SetCronProjectPause(ctx, db.SetCronProjectPauseIn{ProjectID: p.ID, Actor: "operator"})
		require.NoError(t, e)
		require.True(t, out.Changed)
		require.Nil(t, db.CronProjectPauseFromMetadata(out.Project.Metadata))
		require.Contains(t, string(out.Project.Metadata), "9007199254740993")
	})
	for _, policy := range []string{"all", "latest", "skip"} {
		t.Run("dispatch "+policy, func(t *testing.T) {
			// Separate backend because recurringFixture has a fixed project name.
			store := b.Open(t)
			t.Cleanup(func() { require.NoError(t, store.Close()) })
			p, parent, job, progress := recurringFixture(t, store, policy)
			before, e := store.IssueByID(ctx, parent.ID)
			require.NoError(t, e)
			_, e = store.SetCronProjectPause(ctx, db.SetCronProjectPauseIn{ProjectID: p.ID, Actor: "operator", Paused: true})
			require.NoError(t, e)
			in := db.DispatchRecurringCronIn{ProjectID: p.ID, JobUID: job.UID, ExpectedEventUID: job.DefinitionEventUID, Now: progress.Anchor.Add(6 * time.Hour)}
			out, e := store.DispatchRecurringCron(ctx, in)
			require.NoError(t, e)
			require.Empty(t, out.Events)
			require.False(t, out.More)
			after, e := store.IssueByID(ctx, parent.ID)
			require.NoError(t, e)
			require.Equal(t, before.Metadata, after.Metadata)
			_, e = store.SetCronProjectPause(ctx, db.SetCronProjectPauseIn{ProjectID: p.ID, Actor: "operator"})
			require.NoError(t, e)
			out, e = store.DispatchRecurringCron(ctx, in)
			require.NoError(t, e)
			children, e := store.ChildrenOfIssue(ctx, parent.ID)
			require.NoError(t, e)
			want := map[string]int{"all": 3, "latest": 1, "skip": 1}[policy]
			require.Len(t, children, want)
			current, e := store.CronJob(ctx, p.ID, job.UID)
			require.NoError(t, e)
			require.Equal(t, job.DefinitionEventUID, current.DefinitionEventUID)
			require.True(t, current.Definition.Enabled)
		})
	}

	// Contract: pause gates admission, not reporting already running work.
	t.Run("observations while paused", func(t *testing.T) {
		p, err := s.CreateProject(ctx, "paused-evidence-project")
		require.NoError(t, err)
		job, _ := nativeFederationDefinitions(t, s, p)
		for _, invalid := range []bool{false, true} {
			_, err = s.SetCronProjectPause(ctx, db.SetCronProjectPauseIn{ProjectID: p.ID, Actor: "operator", Paused: true})
			require.NoError(t, err)
			if invalid {
				q := s.(interface {
					ExecContext(context.Context, string, ...any) (sql.Result, error)
				})
				_, err = q.ExecContext(ctx, `UPDATE projects SET metadata=$1 WHERE id=$2`, `{"cron.pause":false}`, p.ID)
				require.NoError(t, err)
			}
			for _, status := range []string{"succeeded", "failed", "cancelled", "unknown"} {
				id, err := uid.New()
				require.NoError(t, err)
				in := db.ObserveCronRun{ProjectID: p.ID, UID: id, JobUID: &job.UID, DefinitionEventUID: &job.DefinitionEventUID, Actor: "operator", Status: "running", Summary: cron.Summary{Version: 1}}
				first, err := s.ObserveCronRun(ctx, in)
				require.NoError(t, err)
				in.ExpectedRevision, in.Status = first.Run.Revision, status
				out, err := s.ObserveCronRun(ctx, in)
				require.NoError(t, err)
				require.Equal(t, status, out.Run.Status)
				require.Equal(t, first.Run.Revision+1, out.Run.Revision)
			}
		}
	})
	t.Run("null recovery", func(t *testing.T) {
		p, err := s.CreateProject(ctx, "null-pause-project")
		require.NoError(t, err)
		q := s.(interface {
			ExecContext(context.Context, string, ...any) (sql.Result, error)
		})
		_, err = q.ExecContext(ctx, `UPDATE projects SET metadata=$1 WHERE id=$2`, `{"cron.pause":null,"keep":9007199254740993}`, p.ID)
		require.NoError(t, err)
		paused, err := s.SetCronProjectPause(ctx, db.SetCronProjectPauseIn{ProjectID: p.ID, Actor: "operator", Paused: true})
		require.NoError(t, err)
		require.False(t, paused.Changed)
		require.True(t, db.CronProjectPauseFromMetadata(paused.Project.Metadata).Invalid)
		out, err := s.SetCronProjectPause(ctx, db.SetCronProjectPauseIn{ProjectID: p.ID, Actor: "operator"})
		require.NoError(t, err)
		require.True(t, out.Changed, "resume must delete a retained null policy")
		require.Nil(t, db.CronProjectPauseFromMetadata(out.Project.Metadata))
		require.Equal(t, p.Revision+1, out.NewRevision)
		require.Contains(t, string(out.Event.Payload), `"cron.pause":{"from":null,"to":null}`)
		require.Contains(t, string(out.Project.Metadata), "9007199254740993")
		repeat, err := s.SetCronProjectPause(ctx, db.SetCronProjectPauseIn{ProjectID: p.ID, Actor: "operator"})
		require.NoError(t, err)
		require.False(t, repeat.Changed)
	})
	t.Run("spoke denied even disabled", func(t *testing.T) {
		p, e := s.CreateProject(ctx, "spoke-project")
		require.NoError(t, e)
		adopted, e := s.AdoptProjectIntoFederation(ctx, db.AdoptProjectIntoFederationParams{ProjectID: p.ID, HubURL: "https://daemon.example", HubProjectID: 42, HubProjectUID: "01ARZ3NDEKTSV4RRFFQ69G5FAV", ReplayHorizonEventID: 1, Actor: "operator", EmptyOnly: true})
		require.NoError(t, e)
		for _, paused := range []bool{true, false} {
			_, e = s.SetCronProjectPause(ctx, db.SetCronProjectPauseIn{ProjectID: adopted.Project.ID, Actor: "operator", Paused: paused})
			require.ErrorIs(t, e, db.ErrFederatedSpokeUnsupported)
		}
	})
	t.Run("adoption refuses retained project pause", func(t *testing.T) {
		for _, emptyOnly := range []bool{false, true} {
			t.Run(fmt.Sprintf("empty_only_%t", emptyOnly), func(t *testing.T) {
				p, e := s.CreateProject(ctx, fmt.Sprintf("paused-adoption-%t", emptyOnly))
				require.NoError(t, e)
				paused, e := s.SetCronProjectPause(ctx, db.SetCronProjectPauseIn{ProjectID: p.ID, Actor: "operator", Paused: true, Reason: "maintenance"})
				require.NoError(t, e)
				beforeEventID, e := s.MaxEventID(ctx)
				require.NoError(t, e)
				hubUID, e := uid.New()
				require.NoError(t, e)

				_, e = s.AdoptProjectIntoFederation(ctx, db.AdoptProjectIntoFederationParams{
					ProjectID: p.ID, HubURL: "https://daemon.example", HubProjectID: 42,
					HubProjectUID: hubUID, ReplayHorizonEventID: 1, Actor: "operator", EmptyOnly: emptyOnly,
				})
				require.ErrorIs(t, e, db.ErrFederatedSpokeUnsupported)
				require.Contains(t, e.Error(), "resume")

				after, e := s.ProjectByID(ctx, p.ID)
				require.NoError(t, e)
				require.Equal(t, p.UID, after.UID, "rejected adoption must keep the local project identity")
				require.Equal(t, paused.Project.Metadata, after.Metadata, "rejected adoption must keep the pause metadata")
				afterEventID, e := s.MaxEventID(ctx)
				require.NoError(t, e)
				require.Equal(t, beforeEventID, afterEventID, "rejected adoption must preserve event history")
				_, e = s.FederationBindingByProject(ctx, p.ID)
				require.ErrorIs(t, e, db.ErrNotFound, "rejected adoption must not create a federation binding")
			})
		}
	})
	t.Run("root disabled binding", func(t *testing.T) {
		p, e := s.CreateProject(ctx, "disabled-root-project")
		require.NoError(t, e)
		_, e = s.EnableProjectFederation(ctx, p.ID, "operator")
		require.NoError(t, e)
		q := s.(interface {
			ExecContext(context.Context, string, ...any) (sql.Result, error)
		})
		_, e = q.ExecContext(ctx, `UPDATE federation_bindings SET enabled=0 WHERE project_id=$1`, p.ID)
		require.NoError(t, e)
		_, e = s.SetCronProjectPause(ctx, db.SetCronProjectPauseIn{ProjectID: p.ID, Actor: "operator", Paused: true})
		require.NoError(t, e)
	})
	t.Run("merge target policy", func(t *testing.T) {
		for _, targetPaused := range []bool{false, true} {
			source, e := s.CreateProject(ctx, fmt.Sprintf("source-pause-%t", targetPaused))
			require.NoError(t, e)
			target, e := s.CreateProject(ctx, fmt.Sprintf("target-pause-%t", targetPaused))
			require.NoError(t, e)
			_, e = s.SetCronProjectPause(ctx, db.SetCronProjectPauseIn{ProjectID: source.ID, Actor: "operator", Paused: true, Reason: "source"})
			require.NoError(t, e)
			if targetPaused {
				_, e = s.SetCronProjectPause(ctx, db.SetCronProjectPauseIn{ProjectID: target.ID, Actor: "operator", Paused: true, Reason: "target"})
				require.NoError(t, e)
			}
			_, e = s.MergeProjects(ctx, db.MergeProjectsParams{SourceProjectID: source.ID, TargetProjectID: target.ID, Actor: "operator"})
			require.NoError(t, e)
			target, e = s.ProjectByID(ctx, target.ID)
			require.NoError(t, e)
			pause := db.CronProjectPauseFromMetadata(target.Metadata)
			if targetPaused {
				require.NotNil(t, pause)
				require.Equal(t, "target", pause.Reason)
			} else {
				require.Nil(t, pause)
			}
		}
	})
	t.Run("pause and dispatch race", func(t *testing.T) {
		store := b.Open(t)
		t.Cleanup(func() { require.NoError(t, store.Close()) })
		p, parent, job, progress := recurringFixture(t, store, "all")
		other := b.OpenSame(t, store)
		t.Cleanup(func() { require.NoError(t, other.Close()) })
		var wg sync.WaitGroup
		errs := make(chan error, 2)
		wg.Go(func() {
			_, e := store.SetCronProjectPause(ctx, db.SetCronProjectPauseIn{ProjectID: p.ID, Actor: "operator", Paused: true})
			errs <- e
		})
		wg.Go(func() {
			_, e := other.DispatchRecurringCron(ctx, db.DispatchRecurringCronIn{ProjectID: p.ID, JobUID: job.UID, ExpectedEventUID: job.DefinitionEventUID, Now: progress.Anchor.Add(6 * time.Hour)})
			errs <- e
		})
		wg.Wait()
		close(errs)
		for e := range errs {
			require.NoError(t, e)
		}
		// Either serial order is valid. Once pause commits, no further progress occurs.
		before, e := store.IssueByID(ctx, parent.ID)
		require.NoError(t, e)
		out, e := store.DispatchRecurringCron(ctx, db.DispatchRecurringCronIn{ProjectID: p.ID, JobUID: job.UID, ExpectedEventUID: job.DefinitionEventUID, Now: progress.Anchor.Add(8 * time.Hour)})
		require.NoError(t, e)
		require.Empty(t, out.Events)
		after, e := store.IssueByID(ctx, parent.ID)
		require.NoError(t, e)
		require.Equal(t, before.Metadata, after.Metadata)
	})
	return nil
}
