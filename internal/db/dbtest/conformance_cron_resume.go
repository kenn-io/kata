package dbtest

import (
	"encoding/json/jsontext"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
)

// Contract: discard requires a retained precondition and writes policy before
// enable, atomically. An old retry may never resume a subsequent pause.
func checkCronDiscardResume(t *testing.T, _ db.Storage, b Backend) error {
	t.Run("job", func(t *testing.T) {
		s := b.Open(t)
		t.Cleanup(func() { require.NoError(t, s.Close()) })
		p, _, job, _ := recurringFixture(t, s, "all")
		noop, err := s.ResumeCronJob(t.Context(), db.ResumeCronJobIn{ProjectID: p.ID, UID: job.UID, Actor: "operator", DiscardMissed: true, ExpectedEventUID: job.DefinitionEventUID})
		require.NoError(t, err)
		require.Empty(t, noop.Events, "already enabled with no cutoff is a no-op")
		def := job.Definition
		def.Enabled = false
		job, _, err = s.PutCronJob(t.Context(), db.PutCronJob{ProjectID: p.ID, UID: job.UID, Name: job.Name, Definition: def, ExpectedEventUID: job.DefinitionEventUID, Actor: "operator"})
		require.NoError(t, err)
		in := db.ResumeCronJobIn{ProjectID: p.ID, UID: job.UID, Actor: "operator", DiscardMissed: true}
		_, err = s.ResumeCronJob(t.Context(), in)
		require.ErrorIs(t, err, cron.ErrInvalid)
		in.ExpectedEventUID = "stale"
		_, err = s.ResumeCronJob(t.Context(), in)
		require.ErrorIs(t, err, db.ErrCronConflict)
		in.ExpectedEventUID = job.DefinitionEventUID
		before := time.Now().UTC()
		out, err := s.ResumeCronJob(t.Context(), in)
		require.NoError(t, err)
		require.True(t, out.Job.Definition.Enabled)
		require.Len(t, out.Events, 2)
		require.Equal(t, "project.metadata_updated", out.Events[0].Type)
		require.Equal(t, "cron.job.updated", out.Events[1].Type)
		project, err := s.ProjectByID(t.Context(), p.ID)
		require.NoError(t, err)
		pass := db.CronPassoverFromMetadata(project.Metadata, job.UID)
		require.NotNil(t, pass)
		require.False(t, pass.Invalid)
		require.False(t, pass.Through.Before(before))
		require.Equal(t, "job", pass.Scope)
		in.ExpectedEventUID = out.Job.DefinitionEventUID
		repeat, err := s.ResumeCronJob(t.Context(), in)
		require.NoError(t, err)
		require.Empty(t, repeat.Events)
		def = out.Job.Definition
		def.Enabled = false
		again, _, err := s.PutCronJob(t.Context(), db.PutCronJob{ProjectID: p.ID, UID: job.UID, Name: job.Name, Definition: def, ExpectedEventUID: out.Job.DefinitionEventUID, Actor: "operator"})
		require.NoError(t, err)
		_, err = s.ResumeCronJob(t.Context(), in)
		require.ErrorIs(t, err, db.ErrCronConflict)
		current, err := s.CronJob(t.Context(), p.ID, job.UID)
		require.NoError(t, err)
		require.False(t, current.Definition.Enabled)
		require.Equal(t, again.DefinitionEventUID, current.DefinitionEventUID)
		project, err = s.ProjectByID(t.Context(), p.ID)
		require.NoError(t, err)
		require.Equal(t, pass, db.CronPassoverFromMetadata(project.Metadata, job.UID))
		// An already enabled job can repair its malformed retained cutoff.
		in.ExpectedEventUID = again.DefinitionEventUID
		in.DiscardMissed = false
		enabled, err := s.ResumeCronJob(t.Context(), in)
		require.NoError(t, err)
		require.Len(t, enabled.Events, 1)
		seedPassover(t, s, p.ID, map[string]jsontext.Value{"cron.passover." + job.UID: []byte(`false`)})
		in.ExpectedEventUID = enabled.Job.DefinitionEventUID
		in.DiscardMissed = true
		repaired, err := s.ResumeCronJob(t.Context(), in)
		require.NoError(t, err)
		require.Len(t, repaired.Events, 1)
		require.Equal(t, "project.metadata_updated", repaired.Events[0].Type)
		require.Equal(t, enabled.Job.DefinitionEventUID, repaired.Job.DefinitionEventUID)
		// A future retained cutoff stays whole but must not suppress enable.
		def = repaired.Job.Definition
		def.Enabled = false
		paused, _, err := s.PutCronJob(t.Context(), db.PutCronJob{ProjectID: p.ID, UID: job.UID, Name: job.Name, Definition: def, ExpectedEventUID: repaired.Job.DefinitionEventUID, Actor: "operator"})
		require.NoError(t, err)
		future := time.Now().UTC().Add(time.Hour)
		seedPassover(t, s, p.ID, map[string]jsontext.Value{"cron.passover." + job.UID: passoverFixtureValue(t, future)})
		in.ExpectedEventUID = paused.DefinitionEventUID
		resumed, err := s.ResumeCronJob(t.Context(), in)
		require.NoError(t, err)
		require.True(t, resumed.Job.Definition.Enabled)
		require.Len(t, resumed.Events, 1)
		project, err = s.ProjectByID(t.Context(), p.ID)
		require.NoError(t, err)
		require.Equal(t, future, db.CronPassoverFromMetadata(project.Metadata, job.UID).Through)
		deleted, _, err := s.PutCronJob(t.Context(), db.PutCronJob{ProjectID: p.ID, UID: job.UID, Name: job.Name, Definition: resumed.Job.Definition, ExpectedEventUID: resumed.Job.DefinitionEventUID, Actor: "operator", Deleted: true})
		require.NoError(t, err)
		in.ExpectedEventUID = deleted.DefinitionEventUID
		_, err = s.ResumeCronJob(t.Context(), in)
		require.ErrorIs(t, err, db.ErrCronConflict)

	})
	t.Run("project", func(t *testing.T) {
		s := b.Open(t)
		t.Cleanup(func() { require.NoError(t, s.Close()) })
		p, err := s.CreateProject(t.Context(), "discard-project")
		require.NoError(t, err)
		paused, err := s.SetCronProjectPause(t.Context(), db.SetCronProjectPauseIn{ProjectID: p.ID, Actor: "operator", Paused: true})
		require.NoError(t, err)
		in := db.SetCronProjectPauseIn{ProjectID: p.ID, Actor: "operator", DiscardMissed: true}
		_, err = s.SetCronProjectPause(t.Context(), in)
		require.ErrorIs(t, err, cron.ErrInvalid)
		revision := paused.NewRevision
		in.ExpectedRevision = &revision
		resumed, err := s.SetCronProjectPause(t.Context(), in)
		require.NoError(t, err)
		require.True(t, resumed.Changed)
		require.Nil(t, db.CronProjectPauseFromMetadata(resumed.Project.Metadata))
		require.NotNil(t, db.CronPassoverFromMetadata(resumed.Project.Metadata, ""))
		require.Contains(t, string(resumed.Event.Payload), `"cron.pause"`)
		require.Contains(t, string(resumed.Event.Payload), `"cron.passover"`)
		require.Contains(t, string(resumed.Project.Metadata), `"paused_at"`)
		_, err = s.SetCronProjectPause(t.Context(), in)
		require.ErrorAs(t, err, new(*db.RevisionConflictError))
		second, err := s.SetCronProjectPause(t.Context(), db.SetCronProjectPauseIn{ProjectID: p.ID, Actor: "operator", Paused: true})
		require.NoError(t, err)
		_, err = s.SetCronProjectPause(t.Context(), in)
		require.ErrorAs(t, err, new(*db.RevisionConflictError))
		current, err := s.ProjectByID(t.Context(), p.ID)
		require.NoError(t, err)
		require.Equal(t, second.Project.Metadata, current.Metadata)
		revision = current.Revision
		in.ExpectedRevision = &revision
		_, err = s.SetCronProjectPause(t.Context(), in)
		require.NoError(t, err)
	})
	return nil
}
