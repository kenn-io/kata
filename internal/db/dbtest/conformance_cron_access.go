package dbtest

import (
	"encoding/json/v2"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
)

// Approved table-free contract: independent team grants, mandatory CAS, and
// local attributed audit share the existing project-access epoch.
func checkCronAccessPolicy(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	p, err := store.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	initial, err := store.CronAccessPolicy(ctx, p.UID)
	require.NoError(t, err)
	require.Equal(t, int64(0), initial.Revision)
	require.Equal(t, "any", initial.Define.Mode)
	require.Equal(t, "any", initial.Observe.Mode)
	anonymous, err := store.CronActorAccess(ctx, p.UID, "")
	require.NoError(t, err)
	require.True(t, anonymous.Define)
	require.True(t, anonymous.Observe)
	team, _, err := store.CreateTeam(ctx, "operators", "owner")
	require.NoError(t, err)
	_, err = store.SetTeamMembership(ctx, team.UID, "runtime", true, "owner")
	require.NoError(t, err)
	epoch, err := store.ProjectAccessRevision(ctx)
	require.NoError(t, err)
	initial.Define = db.CronGrant{Mode: "teams", TeamUIDs: []string{team.UID}}
	initial.Observe = db.CronGrant{Mode: "teams", TeamUIDs: []string{}}
	policy, event, err := store.SetCronAccessPolicy(ctx, initial, "owner")
	require.NoError(t, err)
	require.Equal(t, int64(1), policy.Revision)
	require.Equal(t, "project.cron_access_changed", event.Type)
	require.Equal(t, "owner", event.Actor)
	require.Equal(t, db.SystemProjectUID, event.ProjectUID)
	after, err := store.ProjectAccessRevision(ctx)
	require.NoError(t, err)
	require.Equal(t, epoch+1, after)
	got, err := store.CronAccessPolicy(ctx, p.UID)
	require.NoError(t, err)
	require.Equal(t, policy, got)
	anonymous, err = store.CronActorAccess(ctx, p.UID, "")
	require.NoError(t, err)
	require.Equal(t, db.CronAccess{}, anonymous)
	access, err := store.CronActorAccess(ctx, p.UID, "runtime")
	require.NoError(t, err)
	require.True(t, access.Define)
	require.False(t, access.Observe)
	access, err = store.CronActorAccess(ctx, p.UID, "outsider")
	require.NoError(t, err)
	require.False(t, access.Define)
	require.False(t, access.Observe)
	_, _, err = store.SetCronAccessPolicy(ctx, initial, "owner")
	require.ErrorIs(t, err, db.ErrCronAccessRevisionConflict)
	_, err = store.SetTeamMembership(ctx, team.UID, "runtime", false, "owner")
	require.NoError(t, err)
	access, err = store.CronActorAccess(ctx, p.UID, "runtime")
	require.NoError(t, err)
	require.False(t, access.Define)
	policy.Define = db.CronGrant{Mode: "any"}
	policy.Observe = db.CronGrant{Mode: "any"}
	policy, _, err = store.SetCronAccessPolicy(ctx, policy, "owner")
	require.NoError(t, err)
	require.Equal(t, int64(2), policy.Revision)
	missing, err := uid.New()
	require.NoError(t, err)
	policy.Define = db.CronGrant{Mode: "teams", TeamUIDs: []string{missing}}
	_, _, err = store.SetCronAccessPolicy(ctx, policy, "owner")
	require.ErrorIs(t, err, db.ErrNotFound)
	return nil
}

func checkCronAccessConcurrentCAS(t *testing.T, store db.Storage) error {
	p, err := store.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	policy, err := store.CronAccessPolicy(t.Context(), p.UID)
	require.NoError(t, err)
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for range 2 {
		wg.Go(func() { _, _, err := store.SetCronAccessPolicy(t.Context(), policy, "owner"); results <- err })
	}
	wg.Wait()
	close(results)
	successes, conflicts := 0, 0
	for err := range results {
		if err == nil {
			successes++
		} else if errors.Is(err, db.ErrCronAccessRevisionConflict) {
			conflicts++
		} else {
			require.NoError(t, err)
		}
	}
	require.Equal(t, 1, successes)
	require.Equal(t, 1, conflicts)
	return nil
}

func checkCronAccessCorruption(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	p, err := store.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	tx := store.(db.Transaction)
	for _, raw := range []any{`{}`, `null`, `{"version":2}`, `{"version":1,"revision":1,"define":{"mode":"invalid"},"observe":{"mode":"any"}}`, `[]`} {
		_, err := tx.ExecContext(ctx, `INSERT INTO meta(key,value) VALUES($1,$2) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, db.CronAccessPolicyPrefix+p.UID, raw)
		require.NoError(t, err)
		_, err = store.CronAccessPolicy(ctx, p.UID)
		require.Error(t, err)
		access, err := store.CronActorAccess(ctx, p.UID, "runtime")
		require.Error(t, err)
		require.False(t, access.Define)
		require.False(t, access.Observe)
	}
	return nil
}

// Grant authority follows Access.Actor rather than requested attribution. The
// enabled-only pause exception does not exempt rename, archive, or restoration.
func checkCronAccessMutations(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	p, err := store.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	job, workflow := nativeFederationDefinitionsAs(t, store, p, "owner")
	enabled := job.Definition
	enabled.Enabled = true
	job, _, err = store.PutCronJob(ctx, db.PutCronJob{UID: job.UID, ProjectID: p.ID, Name: job.Name, Actor: "owner", ExpectedEventUID: job.DefinitionEventUID, Definition: enabled})
	require.NoError(t, err)
	policy, err := store.CronAccessPolicy(ctx, p.UID)
	require.NoError(t, err)
	policy.Define = db.CronGrant{Mode: "teams"}
	policy.Observe = db.CronGrant{Mode: "teams"}
	_, _, err = store.SetCronAccessPolicy(ctx, policy, "owner")
	require.NoError(t, err)
	ordinary := db.CronAccessCheck{Actor: "runtime", Enforce: true}
	put := func(name string, definition cron.JobDefinition, deleted bool) (db.CronJob, error) {
		value, _, err := store.PutCronJob(ctx, db.PutCronJob{UID: job.UID, ProjectID: p.ID, Name: name, Actor: "owner", ExpectedEventUID: job.DefinitionEventUID, Definition: definition, Deleted: deleted, Access: ordinary})
		return value, err
	}
	_, err = put(job.Name, job.Definition, false)
	require.ErrorIs(t, err, db.ErrCronPermissionDenied)
	paused := job.Definition
	paused.Enabled = false
	_, err = put("renamed", paused, false)
	require.ErrorIs(t, err, db.ErrCronPermissionDenied)
	_, err = put(job.Name, paused, true)
	require.ErrorIs(t, err, db.ErrCronPermissionDenied)
	job, err = put(job.Name, paused, false)
	require.NoError(t, err)
	_, err = put(job.Name, enabled, false)
	require.ErrorIs(t, err, db.ErrCronPermissionDenied)
	_, _, err = store.PutCronWorkflow(ctx, db.PutCronWorkflow{UID: workflow.UID, ProjectID: p.ID, Name: workflow.Name, Actor: "owner", ExpectedEventUID: workflow.DefinitionEventUID, Definition: workflow.Definition, Access: ordinary})
	require.ErrorIs(t, err, db.ErrCronPermissionDenied)
	// A notify job retains ordinary project-writer behavior.
	issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: p.ID, Title: "Example issue", Author: "runtime"})
	require.NoError(t, err)
	notify := paused
	notify.Action = cron.Action{Kind: "notify", Recipient: "runtime", Message: "Inspect"}
	notify.Issue = &cron.IssuePolicy{Kind: "existing", UID: issue.UID}
	_, _, err = store.PutCronJob(ctx, db.PutCronJob{ProjectID: p.ID, Name: "notice", Actor: "runtime", Definition: notify, Access: ordinary})
	require.NoError(t, err)
	// Ordinary callers cannot create execute jobs, even initially disabled.
	_, _, err = store.PutCronJob(ctx, db.PutCronJob{ProjectID: p.ID, Name: "new execution", Actor: "owner", Definition: paused, Access: ordinary})
	require.ErrorIs(t, err, db.ErrCronPermissionDenied)
	runUID, err := uid.New()
	require.NoError(t, err)
	observe := db.ObserveCronRun{ProjectID: p.ID, UID: runUID, JobUID: &job.UID, DefinitionEventUID: &job.DefinitionEventUID, Actor: "owner", Status: "running", Summary: cron.Summary{Version: 1}, Access: ordinary}
	_, err = store.ObserveCronRun(ctx, observe)
	require.ErrorIs(t, err, db.ErrCronPermissionDenied)
	observe.Access = db.CronAccessCheck{}
	result, err := store.ObserveCronRun(ctx, observe)
	require.NoError(t, err)
	require.False(t, result.Replayed)
	observe.Access = ordinary
	_, err = store.ObserveCronRun(ctx, observe)
	require.ErrorIs(t, err, db.ErrCronPermissionDenied)
	observe.ExpectedRevision = 1
	observe.Status = "succeeded"
	_, err = store.ObserveCronRun(ctx, observe)
	require.ErrorIs(t, err, db.ErrCronPermissionDenied)
	return nil
}

func checkCronAccessTeamDeletion(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	p, err := store.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	team, _, err := store.CreateTeam(ctx, "operators", "owner")
	require.NoError(t, err)
	policy, err := store.CronAccessPolicy(ctx, p.UID)
	require.NoError(t, err)
	policy.Define = db.CronGrant{Mode: "teams", TeamUIDs: []string{team.UID}}
	policy.Observe = policy.Define
	policy, _, err = store.SetCronAccessPolicy(ctx, policy, "owner")
	require.NoError(t, err)
	_, err = store.DeleteTeam(ctx, team.UID, "owner")
	require.NoError(t, err)
	got, err := store.CronAccessPolicy(ctx, p.UID)
	require.NoError(t, err)
	require.Equal(t, policy.Revision+1, got.Revision)
	require.Equal(t, db.CronGrant{Mode: "teams", TeamUIDs: []string{}}, got.Define)
	require.Equal(t, got.Define, got.Observe)
	return nil
}

func checkCronAccessProjectLifecycle(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	source, err := store.CreateProject(ctx, "source-project")
	require.NoError(t, err)
	target, err := store.CreateProject(ctx, "target-project")
	require.NoError(t, err)
	policy := cronAccessRestricted(t, store, source)
	_, err = store.MergeProjects(ctx, db.MergeProjectsParams{SourceProjectID: source.ID, TargetProjectID: target.ID, Actor: "owner"})
	require.ErrorIs(t, err, db.ErrProjectMergeAccessPolicy)
	targetPolicy := cronAccessRestricted(t, store, target)
	_, err = store.MergeProjects(ctx, db.MergeProjectsParams{SourceProjectID: source.ID, TargetProjectID: target.ID, Actor: "owner"})
	require.NoError(t, err)
	tx := store.(db.Transaction)
	var count int
	require.NoError(t, tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM meta WHERE key=$1`, db.CronAccessPolicyPrefix+source.UID).Scan(&count))
	require.Zero(t, count)
	got, err := store.CronAccessPolicy(ctx, target.UID)
	require.NoError(t, err)
	require.Equal(t, targetPolicy, got)
	_, _, err = store.RemoveProject(ctx, db.RemoveProjectParams{ProjectID: target.ID, Actor: "owner", Force: true})
	require.NoError(t, err)
	got, err = store.CronAccessPolicy(ctx, target.UID)
	require.NoError(t, err)
	require.Equal(t, policy.Define, got.Define)
	_, err = store.PurgeProject(ctx, db.PurgeProjectParams{ProjectID: target.ID, Actor: "owner"})
	require.NoError(t, err)
	require.NoError(t, tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM meta WHERE key=$1`, db.CronAccessPolicyPrefix+target.UID).Scan(&count))
	require.Zero(t, count)
	return nil
}

func checkCronAccessAdoptionLocal(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	p, err := store.CreateProject(ctx, "spoke-project")
	require.NoError(t, err)
	policy := cronAccessRestricted(t, store, p)
	hubUID, err := uid.New()
	require.NoError(t, err)
	adopted, err := store.AdoptProjectIntoFederation(ctx, db.AdoptProjectIntoFederationParams{ProjectID: p.ID, HubURL: "https://daemon.example", HubProjectID: 42, HubProjectUID: hubUID, Actor: "runtime", EmptyOnly: true})
	require.NoError(t, err)
	got, err := store.CronAccessPolicy(ctx, adopted.Project.UID)
	require.NoError(t, err)
	policy.ProjectUID = hubUID
	require.Equal(t, policy, got, "local policy follows the project when adoption rewrites its UID")
	var count int
	require.NoError(t, store.(db.Transaction).QueryRowContext(ctx, `SELECT COUNT(*) FROM meta WHERE key=$1`, db.CronAccessPolicyPrefix+p.UID).Scan(&count))
	require.Zero(t, count)
	require.NoError(t, store.ResetFederatedProject(ctx, p.ID, 1, 0))
	upstream, err := uid.New()
	require.NoError(t, err)
	event := cronAccessWorkflowEvent(t, adopted.Project, "upstream-runtime", upstream)
	_, err = store.InsertRemoteEvent(ctx, p.ID, event)
	require.NoError(t, err)
	require.NoError(t, store.MaterializeFederatedProject(ctx, p.ID))
	var payload db.CronDefinitionEvent
	require.NoError(t, json.Unmarshal(event.Payload, &payload))
	_, err = store.CronWorkflow(ctx, p.ID, payload.UID)
	require.NoError(t, err, "trusted upstream history is not checked against local use grants")
	got, err = store.CronAccessPolicy(ctx, hubUID)
	require.NoError(t, err)
	require.Equal(t, policy, got)
	return nil
}

// Owner backup policy is typed; raw metadata must never carry local authority.
func checkCronAccessBackup(t *testing.T, target db.Storage, backend Backend) error {
	ctx := t.Context()
	source := backend.Open(t)
	t.Cleanup(func() { require.NoError(t, source.Close()) })
	project, err := source.CreateProject(ctx, "backup-project")
	require.NoError(t, err)
	team, _, err := source.CreateTeam(ctx, "operators", "owner")
	require.NoError(t, err)
	before, _, err := source.SetCronAccessPolicy(ctx, db.ProjectCronAccessPolicy{ProjectUID: project.UID, Define: db.CronGrant{Mode: "teams", TeamUIDs: []string{team.UID}}, Observe: db.CronGrant{Mode: "teams"}}, "owner")
	require.NoError(t, err)
	for meta, err := range source.ExportMeta(ctx) {
		require.NoError(t, err)
		require.False(t, strings.HasPrefix(meta.Key, db.CronAccessPolicyPrefix), "raw metadata leaks authority")
	}
	records, err := CollectImportRecords(ctx, source, db.ExportFilter{IncludeDeleted: true})
	require.NoError(t, err)
	found := false
	for record, err := range source.ExportProjectAccess(ctx) {
		require.NoError(t, err)
		records = append(records, record)
		if record.ImportKind() == "project_cron_access_policy" {
			found = true
		}
	}
	require.True(t, found, "typed cron policy absent")
	require.NoError(t, target.ImportReplay(ctx, records, db.ImportOptions{}))
	after, err := target.CronAccessPolicy(ctx, project.UID)
	require.NoError(t, err)
	require.Equal(t, before, after)
	raw := &db.MetaKV{Key: db.CronAccessPolicyPrefix + project.UID, Value: `{}`}
	require.Error(t, target.ImportReplay(ctx, []db.ImportRecord{raw}, db.ImportOptions{}))
	after, err = target.CronAccessPolicy(ctx, project.UID)
	require.NoError(t, err)
	require.Equal(t, before, after)
	// Typed records validate references and revisions before replacing state.
	missing, err := uid.New()
	require.NoError(t, err)
	for _, invalid := range []db.ProjectCronAccessPolicy{
		{ProjectUID: missing, Define: db.CronGrant{Mode: "any"}, Observe: db.CronGrant{Mode: "any"}, Revision: 1},
		{ProjectUID: project.UID, Define: db.CronGrant{Mode: "teams", TeamUIDs: []string{missing}}, Observe: db.CronGrant{Mode: "any"}, Revision: 1},
		{ProjectUID: project.UID, Define: db.CronGrant{Mode: "any"}, Observe: db.CronGrant{Mode: "any"}, Revision: 0},
	} {
		candidate := append(append([]db.ImportRecord{}, records...), &invalid)
		require.Error(t, target.ImportReplay(ctx, candidate, db.ImportOptions{}))
		got, err := target.CronAccessPolicy(ctx, project.UID)
		require.NoError(t, err)
		require.Equal(t, before, got)
	}
	_, err = db.PrepareProjectMergeRecords([]db.ImportRecord{&before}, db.ProjectMergeOffsets{}, nil)
	require.Error(t, err)
	return nil
}
