package daemon_test

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
	"hegel.dev/go/hegel"
)

// Contract: actions change enabled alone, use CAS even for a no-op, and never
// restore a tombstone. A repeated action at the root emits no event.
func TestCronJobPauseResume(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("tok"))
	project := seedProject(t, env, "example-project")
	definition := cronHTTPDefinition(t)
	job, _, err := env.DB.PutCronJob(t.Context(), db.PutCronJob{ProjectID: project.ID, Name: "Inspect", Actor: "operator", Definition: definition})
	require.NoError(t, err)
	path := fmt.Sprintf("/api/v1/projects/%d/cron/jobs/%s", project.ID, job.UID)
	original := job
	for _, action := range []string{"pause", "resume"} {
		status, raw := cronHTTP(t, env, http.MethodPost, path+"/"+action, map[string]any{"actor": "operator"})
		require.Equalf(t, 200, status, "%s", raw)
		var out api.CronJobResponse
		require.NoError(t, json.Unmarshal(raw, &out.Body))
		require.Equal(t, original.Name, out.Body.Job.Name)
		require.Equal(t, original.UID, out.Body.Job.UID)
		require.Len(t, out.Body.Events, 1)
		require.Equal(t, "cron.job.updated", out.Body.Events[0].Type)
		require.Equal(t, "operator", out.Body.Events[0].Actor)
		job = db.CronJob{CronDefinition: out.Body.Job.CronDefinition, Definition: out.Body.Job.Definition.Native()}
		before, err := env.DB.MaxEventID(t.Context())
		require.NoError(t, err)
		status, raw = cronHTTP(t, env, http.MethodPost, path+"/"+action, map[string]any{"actor": "operator", "expected_event_uid": job.DefinitionEventUID})
		require.Equalf(t, 200, status, "%s", raw)
		out = api.CronJobResponse{}
		require.NoError(t, json.Unmarshal(raw, &out.Body))
		require.Empty(t, out.Body.Events)
		require.Equal(t, job.DefinitionEventUID, out.Body.Job.DefinitionEventUID)
		after, err := env.DB.MaxEventID(t.Context())
		require.NoError(t, err)
		require.Equal(t, before, after)
		status, raw = cronHTTP(t, env, http.MethodPost, path+"/"+action, map[string]any{"actor": "operator", "expected_event_uid": original.DefinitionEventUID})
		require.Equalf(t, 409, status, "%s", raw)
		var conflict api.ErrorEnvelope
		require.NoError(t, json.Unmarshal(raw, &conflict))
		require.Equal(t, "cron_conflict", conflict.Error.Code)
		require.Contains(t, string(raw), job.DefinitionEventUID)
	}
	job, _, err = env.DB.PutCronJob(t.Context(), db.PutCronJob{ProjectID: project.ID, UID: job.UID, ExpectedEventUID: job.DefinitionEventUID, Name: job.Name, Actor: "operator", Definition: job.Definition, Deleted: true})
	require.NoError(t, err)
	for _, action := range []string{"pause", "resume"} {
		status, raw := cronHTTP(t, env, http.MethodPost, path+"/"+action, map[string]any{"actor": "operator"})
		require.Equalf(t, 409, status, "%s", raw)
	}
}

// Contract: generated states and large opaque integers survive both actions;
// matching target states do not advance history.
func TestCronJobEnabledPreservationProperty(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("tok"))
	project := seedProject(t, env, "example-project")
	parent, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: project.ID, Title: "Recurring source", Author: "operator"})
	require.NoError(t, err)
	workflow, _, err := env.DB.PutCronWorkflow(t.Context(), db.PutCronWorkflow{ProjectID: project.ID, Name: "Review workflow", Actor: "operator", Definition: cron.WorkflowDefinition{Version: 1, Steps: []cron.WorkflowStep{{Key: "inspect", Kind: "prompt", Prompt: "Inspect"}}}})
	require.NoError(t, err)
	hegel.Test(t, func(ht *hegel.T) {
		definition := cronEnabledGeneratedDefinition(ht, parent.UID, workflow.UID)
		job, _, err := env.DB.PutCronJob(t.Context(), db.PutCronJob{ProjectID: project.ID, Name: "Inspect", Actor: "operator", Definition: definition})
		require.NoError(ht, err)
		// Put normalizes the portable document, including empty options.
		// The actions must preserve this accepted stored representation.
		definition = job.Definition
		for _, action := range []string{"pause", "resume", "resume", "pause"} {
			status, raw := cronHTTP(t, env, http.MethodPost, fmt.Sprintf("/api/v1/projects/%d/cron/jobs/%s/%s", project.ID, job.UID, action), map[string]any{"actor": "operator"})
			require.Equal(ht, 200, status, string(raw))
			var out api.CronJobResponse
			require.NoError(ht, json.Unmarshal(raw, &out.Body))
			want := definition
			want.Enabled = action == "resume"
			stored, err := env.DB.CronJob(t.Context(), project.ID, job.UID)
			require.NoError(ht, err)
			require.Equal(ht, want, stored.Definition)
			// The existing wire format omits empty options objects.
			if string(want.Options) == "{}" {
				want.Options = nil
			}
			require.Equal(ht, want, out.Body.Job.Definition.Native())
			if job.Definition.Enabled == want.Enabled {
				require.Empty(ht, out.Body.Events)
			} else {
				require.Len(ht, out.Body.Events, 1)
			}
			job = db.CronJob{CronDefinition: out.Body.Job.CronDefinition, Definition: out.Body.Job.Definition.Native()}
		}
	})
}

// HTTP contract: pausing an adapter retains its opaque definition after a
// deleted dependency; a refused resume returns 400 without advancing its revision.
func TestCronJobEnabledDeletedAdapterParentProperty(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("tok"))
	project := seedProject(t, env, "example-project")
	hegel.Test(t, func(ht *hegel.T) {
		parent, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: project.ID, Title: "Adapter dependency", Author: "operator"})
		require.NoError(ht, err)
		drawText := func() string {
			text := []rune(hegel.Draw(ht, hegel.Text()))
			return string(text[:min(len(text), 40)])
		}
		definition := cron.JobDefinition{
			Version: 1, Kind: "job", Enabled: true,
			Trigger: cron.Trigger{Kind: "interval", IntervalSeconds: int64(hegel.Draw(ht, hegel.Integers(60, 86400)))},
			Action:  cron.Action{Kind: "notify", Recipient: "operator", Message: "Review " + drawText()},
			Issue:   &cron.IssuePolicy{Kind: "existing", UID: parent.UID},
			Overlap: "allow", Catchup: "latest",
			Options: jsontext.Value(fmt.Appendf(nil, `{"opaque":%d}`, hegel.Draw(ht, hegel.Integers(0, 999999)))),
		}
		job, _, err := env.DB.PutCronJob(t.Context(), db.PutCronJob{ProjectID: project.ID, Name: "Adapter job", Actor: "operator", Definition: definition})
		require.NoError(ht, err)
		_, _, _, err = env.DB.SoftDeleteIssue(t.Context(), parent.ID, "operator")
		require.NoError(ht, err)
		path := fmt.Sprintf("/api/v1/projects/%d/cron/jobs/%s", project.ID, job.UID)
		for _, action := range []struct {
			name    string
			enabled bool
		}{{"pause", false}, {"resume", true}} {
			status, raw := cronHTTP(t, env, http.MethodPost, path+"/"+action.name, map[string]any{
				"actor": "operator", "expected_event_uid": job.DefinitionEventUID,
			})
			if action.enabled {
				require.Equalf(ht, http.StatusBadRequest, status, "%s", raw)
				require.Contains(ht, string(raw), "tombstoned")
				current, err := env.DB.CronJob(t.Context(), project.ID, job.UID)
				require.NoError(ht, err)
				require.Equal(ht, job.DefinitionEventUID, current.DefinitionEventUID)
				require.Equal(ht, job.Definition, current.Definition)
				continue
			}
			require.Equalf(ht, http.StatusOK, status, "%s", raw)
			var out api.CronJobResponse
			require.NoError(ht, json.Unmarshal(raw, &out.Body))
			want := job.Definition
			want.Enabled = action.enabled
			require.Equal(ht, want, out.Body.Job.Definition.Native())
			require.Len(ht, out.Body.Events, 1)
			job = db.CronJob{CronDefinition: out.Body.Job.CronDefinition, Definition: out.Body.Job.Definition.Native()}
		}
	})
}

func TestCronJobPauseLosesConcurrentEdit(t *testing.T) {
	env := testenv.New(t)
	project := seedProject(t, env, "example-project")
	definition := cronHTTPDefinition(t)
	job, _, err := env.DB.PutCronJob(t.Context(), db.PutCronJob{ProjectID: project.ID, Name: "Inspect", Actor: "operator", Definition: definition})
	require.NoError(t, err)
	store := beforeCronJobStore{Storage: env.DB, before: func() error {
		changed := definition
		changed.Action.Prompt = "Concurrent edit"
		_, _, err := env.DB.PutCronJob(t.Context(), db.PutCronJob{ProjectID: project.ID, UID: job.UID, ExpectedEventUID: job.DefinitionEventUID, Name: "Edited", Actor: "operator", Definition: changed})
		return err
	}}
	server := daemon.NewServer(daemon.ServerConfig{DB: store})
	t.Cleanup(func() { require.NoError(t, server.Close()) })
	endpoint := httptest.NewServer(server.Handler())
	t.Cleanup(endpoint.Close)
	target := &testenv.Env{URL: endpoint.URL, HTTP: endpoint.Client()}
	response, raw := envDoRaw(t, target, http.MethodPost, fmt.Sprintf("/api/v1/projects/%d/cron/jobs/%s/pause", project.ID, job.UID), map[string]any{"actor": "operator"}, nil)
	require.Equalf(t, 409, response.StatusCode, "%s", raw)
	current, err := env.DB.CronJob(t.Context(), project.ID, job.UID)
	require.NoError(t, err)
	require.True(t, current.Definition.Enabled)
	require.Equal(t, "Concurrent edit", current.Definition.Action.Prompt)
}

// HTTP actions must accept enabled-only native writes with a deleted parent.
// Storage conformance separately owns restoration and delivery gating.
func TestCronJobEnabledDeletedParent(t *testing.T) {
	for _, action := range []string{"pause", "resume"} {
		t.Run(action, func(t *testing.T) {
			env, project, parent, definition := nativeCronEnrollmentFixture(t, testenv.WithAuthToken("tok"))
			definition.Enabled = action == "pause"
			job, _, err := env.DB.PutCronJob(t.Context(), db.PutCronJob{ProjectID: project.ID, Name: "Review", Actor: "tester", Definition: definition})
			require.NoError(t, err)
			_, _, _, err = env.DB.SoftDeleteIssue(t.Context(), parent.ID, "tester")
			require.NoError(t, err)
			path := fmt.Sprintf("/api/v1/projects/%d/cron/jobs/%s/%s", project.ID, job.UID, action)
			status, raw := cronHTTP(t, env, http.MethodPost, path, map[string]any{"actor": "tester", "expected_event_uid": job.DefinitionEventUID})
			require.Equalf(t, 200, status, "%s", raw)
			var out api.CronJobResponse
			require.NoError(t, json.Unmarshal(raw, &out.Body))
			require.Equal(t, action == "resume", out.Body.Job.Definition.Enabled)
		})
	}
}

// Contract: credentials with a bound actor may omit the optional action body.
func TestCronJobEnabledBodylessEnrollment(t *testing.T) {
	env, project, _, definition := nativeCronEnrollmentFixture(t, testenv.WithAuthToken("tok"))
	job, _, err := env.DB.PutCronJob(t.Context(), db.PutCronJob{ProjectID: project.ID, Name: "Review", Actor: "tester", Definition: definition})
	require.NoError(t, err)
	enrollment := createClaimEnrollment(t, env, project.ID, cronHTTPUID(t), "push")
	for _, action := range []string{"pause", "resume"} {
		path := fmt.Sprintf("/api/v1/projects/%d/cron/jobs/%s/%s", project.ID, job.UID, action)
		response, raw := envDoRaw(t, env, http.MethodPost, path, nil, map[string]string{"Authorization": "Bearer " + enrollment.Token})
		require.Equalf(t, 200, response.StatusCode, "%s", raw)
	}
}

// Construct valid native, notification, prompt and workflow jobs independently
// of API conversion fields. Bound only materialized text size for test cost.
func cronEnabledGeneratedDefinition(ht *hegel.T, parentUID, workflowUID string) cron.JobDefinition {
	drawText := func() string { r := []rune(hegel.Draw(ht, hegel.Text())); return string(r[:min(len(r), 100)]) }
	definition := cron.JobDefinition{Version: 1, Kind: "job", Enabled: hegel.Draw(ht, hegel.SampledFrom([]bool{false, true})), Overlap: hegel.Draw(ht, hegel.SampledFrom([]string{"allow", "forbid"})), Catchup: hegel.Draw(ht, hegel.SampledFrom([]string{"all", "latest", "skip"})), GraceSeconds: int64(hegel.Draw(ht, hegel.Integers(1, 9223372036))), TimeoutSeconds: int64(hegel.Draw(ht, hegel.Integers(0, 9223372036))), SecretRefs: map[string]string{"access": "example-secret", "secondary": "other-secret"}}
	definition.Trigger = hegel.Draw(ht, hegel.SampledFrom([]cron.Trigger{{Kind: "cron", Cron: "0 9 * * *", Timezone: "UTC"}, {Kind: "interval", IntervalSeconds: 60}, {Kind: "once", At: "2030-01-02T03:04:05Z"}, {Kind: "manual"}, {Kind: "issue-scheduled", IssueUID: parentUID}}))
	definition.Issue = &cron.IssuePolicy{Kind: "per-run", Title: "Review " + drawText(), Body: drawText()}
	offset := int64(hegel.Draw(ht, hegel.Integers(-9223372036, 9223372036)))
	if hegel.Draw(ht, hegel.SampledFrom([]bool{false, true})) {
		definition.Issue.ScheduledOffsetSeconds = &offset
	}
	if hegel.Draw(ht, hegel.SampledFrom([]bool{false, true})) {
		definition.Issue.DeadlineOffsetSeconds = &offset
	}
	switch hegel.Draw(ht, hegel.Integers(0, 3)) {
	case 0: // Native recurring job.
		definition.Action = cron.Action{Kind: "notify", Recipient: "current-owner-or-author", Message: drawText()}
		definition.Issue.ParentUID = parentUID
		definition.Issue.ScheduledOffsetSeconds = nil
		definition.Issue.DeadlineOffsetSeconds = nil
		definition.Overlap = "allow"
		definition.TimeoutSeconds = 0
		definition.SecretRefs = nil
		definition.Trigger = hegel.Draw(ht, hegel.SampledFrom([]cron.Trigger{{Kind: "interval", IntervalSeconds: 60}, {Kind: "cron", Cron: "0 9 * * *", Timezone: "UTC"}}))
	case 1: // Adapter notification.
		definition.Action = cron.Action{Kind: "notify", Recipient: "operator", Message: "Inspect " + drawText()}
		definition.Issue = &cron.IssuePolicy{Kind: "existing", UID: parentUID}
		if hegel.Draw(ht, hegel.SampledFrom([]bool{false, true})) {
			definition.Trigger = cron.Trigger{Kind: "issue-deadline", IssueUID: parentUID, LeadSeconds: int64(hegel.Draw(ht, hegel.Integers(0, 9223372036)))}
		}
	default:
		definition.Action = cron.Action{Kind: "execute", Prompt: "Inspect " + drawText()}
		definition.CheckoutKey = fmt.Sprintf("checkout-%d", hegel.Draw(ht, hegel.Integers(0, 99999)))
		if hegel.Draw(ht, hegel.SampledFrom([]bool{false, true})) {
			definition.Action.Prompt = ""
			definition.Action.WorkflowUID = workflowUID
		}
		if hegel.Draw(ht, hegel.SampledFrom([]bool{false, true})) {
			definition.Issue = &cron.IssuePolicy{Kind: "existing", UID: parentUID}
		}
	}
	number := hegel.Draw(ht, hegel.Integers(0, 99999))
	definition.Options = hegel.Draw(ht, hegel.SampledFrom([]jsontext.Value{nil, []byte(`{}`), []byte(fmt.Sprintf(`{"opaque":90071992547%05d,"nested":[null,true,"escaped\ntext",{"n":1.234567890123456789}]}`, number))}))
	if cron.IsRecurringIssueJob(definition) {
		definition.Options = nil
	}
	return definition
}
