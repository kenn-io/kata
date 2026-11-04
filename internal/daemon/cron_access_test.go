package daemon_test

import (
	"encoding/json/v2"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
)

// The owner may administer independent local grants; ordinary project writers
// receive only their current grant and cannot select another actor for it.
func TestCronAccessAdministrationAndEnforcement(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		ctx := t.Context()
		f := newProjectAccessFixture(t, store)
		base := fmt.Sprintf("/api/v1/projects/%d/cron", f.public.ID)
		code, _, raw := f.request(t, http.MethodGet, base+"/access", "admin", nil, nil)
		require.Equal(t, 200, code, string(raw))
		var response struct {
			Policy db.ProjectCronAccessPolicy `json:"policy"`
			Event  db.Event                   `json:"event"`
		}
		require.NoError(t, json.Unmarshal(raw, &response))
		require.Zero(t, response.Policy.Revision)
		body := map[string]any{"define": db.CronGrant{Mode: "teams", TeamUIDs: []string{f.team.UID}}, "observe": db.CronGrant{Mode: "teams", TeamUIDs: []string{}}, "revision": 0}
		delete(body, "revision")
		code, _, raw = f.request(t, http.MethodPut, base+"/access", "admin", body, nil)
		require.Equal(t, 400, code, string(raw))
		body["revision"] = 0
		sub := f.broadcaster.Subscribe(daemon.SubFilter{})
		defer sub.Unsub()
		code, _, raw = f.request(t, http.MethodPut, base+"/access", "admin", body, nil)
		require.Equal(t, 200, code, string(raw))
		require.NoError(t, json.Unmarshal(raw, &response))
		require.Equal(t, int64(1), response.Policy.Revision)
		require.Equal(t, "project.cron_access_changed", response.Event.Type)
		require.Equal(t, db.BootstrapActor, response.Event.Actor)
		select {
		case event := <-sub.Ch:
			require.Equal(t, response.Event.UID, event.Event.UID)
		default:
			t.Fatal("policy audit not published")
		}
		code, _, raw = f.request(t, http.MethodPut, base+"/access", "admin", body, nil)
		require.Equal(t, 409, code, string(raw))
		require.Contains(t, string(raw), "revision_conflict")
		for _, method := range []string{http.MethodGet, http.MethodPut} {
			code, _, raw = f.request(t, method, base+"/access", "member", body, nil)
			require.Equal(t, 404, code, string(raw))
		}
		code, _, raw = f.request(t, http.MethodGet, base+"/capabilities", "member", nil, nil)
		require.Equal(t, 200, code, string(raw))
		var capabilities struct {
			Access db.CronAccess `json:"access"`
		}
		require.NoError(t, json.Unmarshal(raw, &capabilities))
		require.True(t, capabilities.Access.Define)
		require.False(t, capabilities.Access.Observe)
		code, _, raw = f.request(t, http.MethodGet, base+"/capabilities", "admin", nil, nil)
		require.Equal(t, 200, code, string(raw))
		require.NoError(t, json.Unmarshal(raw, &capabilities))
		require.Equal(t, db.CronAccess{}, capabilities.Access, "bootstrap remains read-only")
		jobBody := map[string]any{"actor": "member", "name": "Example job", "definition": cronHTTPDefinition(t)}
		code, _, raw = f.request(t, http.MethodPost, base+"/jobs", "nonmember", jobBody, nil)
		require.Equal(t, 403, code, string(raw))
		require.Contains(t, string(raw), "cron_permission_denied")
		require.Contains(t, string(raw), "define grant")
		code, _, raw = f.request(t, http.MethodPost, base+"/jobs", "member", jobBody, nil)
		require.Equal(t, 201, code, string(raw))
		var job struct {
			Job db.CronJob `json:"job"`
		}
		require.NoError(t, json.Unmarshal(raw, &job))
		runBody := map[string]any{"job_uid": job.Job.UID, "definition_event_uid": job.Job.DefinitionEventUID, "actor": "member", "status": "running", "summary": map[string]any{"version": 1}, "expected_revision": 0}
		code, _, raw = f.request(t, http.MethodPut, base+"/runs/"+cronHTTPUID(t), "member", runBody, nil)
		require.Equal(t, 403, code, string(raw))
		require.Contains(t, string(raw), "observe grant")
		_, err := store.SetTeamMembership(ctx, f.team.UID, "member", false, "owner")
		require.NoError(t, err)
		code, _, raw = f.request(t, http.MethodGet, base+"/capabilities", "member", nil, nil)
		require.Equal(t, 200, code, string(raw))
		require.NoError(t, json.Unmarshal(raw, &capabilities))
		require.Equal(t, db.CronAccess{}, capabilities.Access)
		paused := job.Job.Definition
		paused.Enabled = false
		code, _, raw = f.request(t, http.MethodPut, base+"/jobs/"+job.Job.UID, "member", map[string]any{"actor": "member", "name": job.Job.Name, "definition": paused, "expected_event_uid": job.Job.DefinitionEventUID}, nil)
		require.Equal(t, 200, code, string(raw), "pure pause remains permitted")
	})
}

// Admission is not a grant lease: a revocation committed before storage begins
// must fence the mutation, including a membership-only revocation.
func TestCronAccessRevokedAfterAdmission(t *testing.T) {
	for _, kind := range []string{"policy", "membership"} {
		t.Run(kind, func(t *testing.T) {
			projectAccessBackends(t, func(t *testing.T, store db.Storage) {
				var projectUID, teamUID string
				wrapped := beforeCronJobStore{Storage: store, before: func() error {
					if kind == "membership" {
						_, err := store.SetTeamMembership(t.Context(), teamUID, "member", false, "owner")
						return err
					}
					policy, err := store.CronAccessPolicy(t.Context(), projectUID)
					if err != nil {
						return err
					}
					policy.Define = db.CronGrant{Mode: "teams"}
					_, _, err = store.SetCronAccessPolicy(t.Context(), policy, "owner")
					return err
				}}
				f := newProjectAccessFixture(t, wrapped)
				// Keep project visibility independent of the cron grant being revoked.
				team, _, err := store.CreateTeam(t.Context(), "cron-definers", "owner")
				require.NoError(t, err)
				_, err = store.SetTeamMembership(t.Context(), team.UID, "member", true, "owner")
				require.NoError(t, err)
				projectUID, teamUID = f.public.UID, team.UID
				policy, err := store.CronAccessPolicy(t.Context(), projectUID)
				require.NoError(t, err)
				policy.Define = db.CronGrant{Mode: "teams", TeamUIDs: []string{teamUID}}
				_, _, err = store.SetCronAccessPolicy(t.Context(), policy, "owner")
				require.NoError(t, err)
				code, _, raw := f.request(t, http.MethodPost, fmt.Sprintf("/api/v1/projects/%d/cron/jobs", f.public.ID), "member", map[string]any{"actor": "member", "name": "Example job", "definition": cronHTTPDefinition(t)}, nil)
				require.Equal(t, 403, code, string(raw))
				require.Contains(t, string(raw), "cron_permission_denied")
				jobs, err := store.ListCronJobs(t.Context(), db.CronList{ProjectID: f.public.ID})
				require.NoError(t, err)
				require.Empty(t, jobs)
			})
		})
	}
}
