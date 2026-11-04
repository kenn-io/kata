package dbtest

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
)

// Contract: retained versions are scoped, newest first by definition HLC,
// attributed to the edit actor, and page without repeats on unchanged history.
func checkCronDefinitionVersions(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	other, err := store.CreateProject(ctx, "other-project")
	require.NoError(t, err)
	job, workflow := nativeFederationDefinitions(t, store, project)
	job.Definition.Action = cron.Action{Kind: "execute", Prompt: "Review"}
	for _, kind := range []string{"job", "workflow"} {
		t.Run(kind, func(t *testing.T) {
			var identity, winner string
			var created string
			if kind == "job" {
				identity, winner = job.UID, job.DefinitionEventUID
			} else {
				identity, winner = workflow.UID, workflow.DefinitionEventUID
			}
			created = winner
			for i, actor := range []string{"editor", "archiver", "restorer"} {
				if kind == "job" {
					value, _, err := store.PutCronJob(ctx, db.PutCronJob{ProjectID: project.ID, UID: identity, Name: "Revision", Definition: job.Definition, ExpectedEventUID: winner, Actor: actor, Deleted: i == 1})
					require.NoError(t, err)
					winner = value.DefinitionEventUID
				} else {
					value, _, err := store.PutCronWorkflow(ctx, db.PutCronWorkflow{ProjectID: project.ID, UID: identity, Name: "Revision", Definition: workflow.Definition, ExpectedEventUID: winner, Actor: actor, Deleted: i == 1})
					require.NoError(t, err)
					winner = value.DefinitionEventUID
				}
			}
			list := store.ListCronJobVersions
			if kind == "workflow" {
				list = store.ListCronWorkflowVersions
			}
			page, err := list(ctx, db.CronVersionList{ProjectID: project.ID, UID: strings.ToLower(identity)})
			require.NoError(t, err)
			require.Len(t, page.Versions, 4)
			require.Empty(t, page.NextBeforeEventUID)
			for i, op := range []string{"restored", "deleted", "updated", "created"} {
				require.Equal(t, op, page.Versions[i].Operation)
				require.Equal(t, i == 0, page.Versions[i].Current)
			}
			require.Equal(t, winner, page.Versions[0].EventUID)
			require.Equal(t, created, page.Versions[3].EventUID)
			require.Equal(t, "restorer", *page.Versions[0].Actor)
			require.Equal(t, "worker", *page.Versions[3].Actor)
			require.NotNil(t, page.Versions[1].DeletedAt)
			require.Nil(t, page.Versions[0].DeletedAt)
			first, err := list(ctx, db.CronVersionList{ProjectID: project.ID, UID: identity, Limit: 2})
			require.NoError(t, err)
			require.Equal(t, page.Versions[:2], first.Versions)
			require.Equal(t, first.Versions[1].EventUID, first.NextBeforeEventUID)
			last, err := list(ctx, db.CronVersionList{ProjectID: project.ID, UID: identity, Limit: 2, BeforeEventUID: first.NextBeforeEventUID})
			require.NoError(t, err)
			require.Equal(t, page.Versions[2:], last.Versions)
			require.Empty(t, last.NextBeforeEventUID)
			terminal, err := list(ctx, db.CronVersionList{ProjectID: project.ID, UID: identity, BeforeEventUID: created})
			require.NoError(t, err)
			require.NotNil(t, terminal.Versions)
			require.Empty(t, terminal.Versions)
			for _, limit := range []int{-1, 101} {
				_, err = list(ctx, db.CronVersionList{ProjectID: project.ID, UID: identity, Limit: limit})
				require.ErrorIs(t, err, cron.ErrInvalid)
			}
			unknown, err := uid.New()
			require.NoError(t, err)
			_, err = list(ctx, db.CronVersionList{ProjectID: project.ID, UID: identity, BeforeEventUID: unknown})
			require.ErrorIs(t, err, cron.ErrInvalid)
			require.ErrorContains(t, err, "version not found")
			_, err = list(ctx, db.CronVersionList{ProjectID: other.ID, UID: identity})
			require.ErrorIs(t, err, db.ErrNotFound)
		})
	}
	// Kind isolation applies even when the job and workflow share a UID.
	twin, _, err := store.PutCronWorkflow(ctx, db.PutCronWorkflow{ProjectID: project.ID, UID: job.UID, Name: "Same UID workflow", Actor: "operator", Definition: workflow.Definition})
	require.NoError(t, err)
	jobPage, err := store.ListCronJobVersions(ctx, db.CronVersionList{ProjectID: project.ID, UID: job.UID})
	require.NoError(t, err)
	require.Len(t, jobPage.Versions, 4)
	twinPage, err := store.ListCronWorkflowVersions(ctx, db.CronVersionList{ProjectID: project.ID, UID: twin.UID})
	require.NoError(t, err)
	require.Len(t, twinPage.Versions, 1)
	require.Equal(t, twin.DefinitionEventUID, twinPage.Versions[0].EventUID)
	for range 20 {
		twin, _, err = store.PutCronWorkflow(ctx, db.PutCronWorkflow{ProjectID: project.ID, UID: twin.UID, Name: twin.Name, Actor: "editor", Definition: twin.Definition, ExpectedEventUID: twin.DefinitionEventUID})
		require.NoError(t, err)
	}
	defaultPage, err := store.ListCronWorkflowVersions(ctx, db.CronVersionList{ProjectID: project.ID, UID: twin.UID})
	require.NoError(t, err)
	require.Len(t, defaultPage.Versions, 20)
	require.NotEmpty(t, defaultPage.NextBeforeEventUID)
	return nil
}

func checkCronDefinitionVersionsSnapshots(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	job, workflow := nativeFederationDefinitions(t, store, project)
	// Federation baseline includes snapshots of the exact current definition;
	// the original edit must retain its operation and actor when both exist.
	_, err = store.EnableProjectFederation(ctx, project.ID, "operator")
	require.NoError(t, err)
	for _, tc := range []struct {
		uid  string
		list func(context.Context, db.CronVersionList) (db.CronVersionPage, error)
	}{{job.UID, store.ListCronJobVersions}, {workflow.UID, store.ListCronWorkflowVersions}} {
		page, err := tc.list(ctx, db.CronVersionList{ProjectID: project.ID, UID: tc.uid})
		require.NoError(t, err)
		require.Len(t, page.Versions, 1)
		require.Equal(t, "created", page.Versions[0].Operation)
		require.Equal(t, "worker", *page.Versions[0].Actor)
	}
	events, err := store.EventsAfter(ctx, db.EventsAfterParams{ProjectID: project.ID, Limit: 100})
	require.NoError(t, err)
	for _, event := range events {
		if event.Type == "cron.job.snapshot" {
			_, err := store.ListCronJobVersions(ctx, db.CronVersionList{ProjectID: project.ID, UID: job.UID, BeforeEventUID: event.UID})
			require.ErrorIs(t, err, cron.ErrInvalid, "snapshot envelope is not a version cursor")
		}
	}
	// The replica retains only snapshots. Use fixture SQL to remove original
	// edits, as an adoption/reset does, without touching production storage.
	q := store.(interface {
		ExecContext(context.Context, string, ...any) (sql.Result, error)
	})
	_, err = q.ExecContext(ctx, `DELETE FROM events WHERE project_id=$1 AND type IN ('cron.job.created','cron.workflow.created')`, project.ID)
	require.NoError(t, err)
	for _, tc := range []struct {
		uid  string
		list func(context.Context, db.CronVersionList) (db.CronVersionPage, error)
	}{{job.UID, store.ListCronJobVersions}, {workflow.UID, store.ListCronWorkflowVersions}} {
		page, err := tc.list(ctx, db.CronVersionList{ProjectID: project.ID, UID: tc.uid})
		require.NoError(t, err)
		require.Len(t, page.Versions, 1)
		require.Equal(t, "baseline", page.Versions[0].Operation)
		require.Nil(t, page.Versions[0].Actor)
		require.True(t, page.Versions[0].Current)
	}
	return nil
}

func checkCronDefinitionVersionsCorruptionMerge(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "source-project")
	require.NoError(t, err)
	target, err := store.CreateProject(ctx, "target-project")
	require.NoError(t, err)
	job, workflow := nativeFederationDefinitions(t, store, project)
	_, err = store.MergeProjects(ctx, db.MergeProjectsParams{SourceProjectID: project.ID, TargetProjectID: target.ID, Actor: "operator"})
	require.NoError(t, err)
	for _, tc := range []struct {
		uid  string
		list func(context.Context, db.CronVersionList) (db.CronVersionPage, error)
	}{{job.UID, store.ListCronJobVersions}, {workflow.UID, store.ListCronWorkflowVersions}} {
		page, err := tc.list(ctx, db.CronVersionList{ProjectID: target.ID, UID: tc.uid})
		require.NoError(t, err)
		require.Len(t, page.Versions, 1)
		require.True(t, page.Versions[0].Current)
	}
	// Contract: an undecodable matching event is reported, never promoted to
	// current, and cannot prevent other decodable versions being returned.
	q := store.(interface {
		ExecContext(context.Context, string, ...any) (sql.Result, error)
	})
	payload := map[string]jsontext.Value{}
	events, err := store.EventsByUIDs(ctx, target.ID, []string{job.DefinitionEventUID})
	require.NoError(t, err)
	require.Len(t, events, 1)
	require.NoError(t, json.Unmarshal([]byte(events[0].Payload), &payload))
	payload["definition"] = jsontext.Value(`{"version":"bad"}`)
	broken, err := json.Marshal(payload)
	require.NoError(t, err)
	_, err = q.ExecContext(ctx, `UPDATE events SET payload=$1 WHERE uid=$2`, string(broken), job.DefinitionEventUID)
	require.NoError(t, err)
	page, err := store.ListCronJobVersions(ctx, db.CronVersionList{ProjectID: target.ID, UID: job.UID})
	require.NoError(t, err)
	require.NotNil(t, page.Versions)
	require.Empty(t, page.Versions)
	require.Equal(t, []string{job.DefinitionEventUID}, page.SkippedEventUIDs)

	return nil
}
