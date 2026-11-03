package daemon_test

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/jsonl"
	"go.kenn.io/kata/internal/testenv"
)

func cronClaimJob(t *testing.T, env *testenv.Env, p db.Project) db.CronJob {
	t.Helper()
	def, err := cron.ParseJob([]byte(`{"version":1,"kind":"job","enabled":true,"trigger":{"kind":"manual"},"action":{"kind":"execute","prompt":"Review"},"issue":{"kind":"per-run","title":"Review"},"overlap":"forbid","catchup":"skip"}`))
	require.NoError(t, err)
	j, _, err := env.DB.PutCronJob(t.Context(), db.PutCronJob{ProjectID: p.ID, Name: "Review", Actor: "worker", Definition: def})
	require.NoError(t, err)
	return j
}
func cronClaimPath(p int64, j string) string {
	return fmt.Sprintf("/api/v1/projects/%d/cron/jobs/%s/claims", p, j)
}
func cronClaimBody(j db.CronJob, run string) map[string]any {
	return map[string]any{"run_uid": run, "occurrence_key": "2030-01-02T03:04:05Z", "definition_event_uid": j.DefinitionEventUID, "actor": "worker", "executor_label": "executor-a"}
}

func TestCronOccurrenceClaimLocalGrantReplayConflict(t *testing.T) {
	env := testenv.New(t)
	p, err := env.DB.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	j := cronClaimJob(t, env, p)
	before, err := env.DB.UIEventCursor(t.Context())
	require.NoError(t, err)
	body := cronClaimBody(j, "00000000000000000000000003")
	var first struct {
		Claimed  bool                   `json:"claimed"`
		Replayed bool                   `json:"replayed"`
		Claim    db.CronOccurrenceClaim `json:"claim"`
	}
	for i, want := range []int{201, 200} {
		resp, raw := envDoRaw(t, env, http.MethodPost, cronClaimPath(p.ID, j.UID), body, nil)
		require.Equal(t, want, resp.StatusCode, string(raw))
		require.NoError(t, json.Unmarshal(raw, &first))
		require.True(t, first.Claimed)
		require.Equal(t, i > 0, first.Replayed)
		require.Equal(t, env.DB.InstanceUID(), first.Claim.HolderInstanceUID)
	}
	body["run_uid"] = "00000000000000000000000004"
	resp, raw := envDoRaw(t, env, http.MethodPost, cronClaimPath(p.ID, j.UID), body, nil)
	assertAPIError(t, resp.StatusCode, raw, 409, "cron_occurrence_claimed")
	require.Contains(t, string(raw), "00000000000000000000000003")
	require.Contains(t, string(raw), "executor-a")
	resp, raw = envDoRaw(t, env, http.MethodGet, fmt.Sprintf("/api/v1/projects/%d/cron/claims/%s", p.ID, first.Claim.RunUID), nil, nil)
	require.Equal(t, 200, resp.StatusCode, string(raw))
	require.Contains(t, string(raw), first.Claim.RunUID)
	body["run_uid"] = first.Claim.RunUID
	body["occurrence_key"] = "changed"
	resp, raw = envDoRaw(t, env, http.MethodPost, cronClaimPath(p.ID, j.UID), body, nil)
	assertAPIError(t, resp.StatusCode, raw, 409, "cron_claim_identity_conflict")
	after, err := env.DB.UIEventCursor(t.Context())
	require.NoError(t, err)
	require.Equal(t, before, after, "control state is not a projection event")
	resp, raw = envDoRaw(t, env, http.MethodGet, fmt.Sprintf("/api/v1/projects/%d/cron/capabilities", p.ID), nil, nil)
	require.Equal(t, 200, resp.StatusCode)
	require.Contains(t, string(raw), "cron_occurrence_claims_v1")
}

func TestCronOccurrenceClaimLookupRequiresAuthority(t *testing.T) {
	root := testenv.New(t)
	p, err := root.DB.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	j := cronClaimJob(t, root, p)
	run := "00000000000000000000000003"
	path := fmt.Sprintf("/api/v1/projects/%d/cron/claims/%s", p.ID, run)
	resp, raw := envDoRaw(t, root, http.MethodPost, cronClaimPath(p.ID, j.UID), cronClaimBody(j, run), nil)
	require.Equal(t, 201, resp.StatusCode, string(raw))
	resp, raw = envDoRaw(t, root, http.MethodGet, path+"unknown", nil, nil)
	require.Equal(t, 404, resp.StatusCode, string(raw))
	var backup bytes.Buffer
	require.NoError(t, jsonl.Export(t.Context(), root.DB, &backup, jsonl.ExportOptions{IncludeDeleted: true}))
	for _, newInstance := range []bool{false, true} {
		t.Run(fmt.Sprintf("new_instance_%t", newInstance), func(t *testing.T) {
			target := testenv.New(t)
			require.NoError(t, jsonl.ImportWithOptions(t.Context(), bytes.NewReader(backup.Bytes()), target.DB, jsonl.ImportOptions{NewInstance: newInstance}))
			resp, raw := envDoRaw(t, target, http.MethodGet, path, nil, nil)
			if newInstance {
				assertAPIError(t, resp.StatusCode, raw, 503, "cron_claim_pending")
				require.NotContains(t, string(raw), `"claimed":true`)
			} else {
				require.Equal(t, 200, resp.StatusCode, string(raw))
				require.Contains(t, string(raw), run)
			}
		})
	}
	t.Run("detached_replica", func(t *testing.T) {
		_, err := root.DB.EnableProjectFederation(t.Context(), p.ID, "worker")
		require.NoError(t, err)
		spoke, local, _, _ := newCronClaimSpoke(t, root, p, "member")
		_, err = spoke.DB.LeaveFederationReplica(t.Context(), local.ID)
		require.NoError(t, err)
		resp, raw := envDoRaw(t, spoke, http.MethodGet, fmt.Sprintf("/api/v1/projects/%d/cron/claims/%s", local.ID, run), nil, nil)
		assertAPIError(t, resp.StatusCode, raw, 503, "cron_claim_pending")
		require.NotContains(t, string(raw), `"claimed":true`)
	})
}

func TestCronOccurrenceClaimAuthBoundaries(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("owner-test-token"))
	p, err := env.DB.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	j := cronClaimJob(t, env, p)
	body := cronClaimBody(j, "00000000000000000000000003")
	resp, raw := envDoRaw(t, env, http.MethodPost, cronClaimPath(p.ID, j.UID), body, nil)
	require.Equal(t, 401, resp.StatusCode, string(raw))
	body["origin"] = map[string]any{"holder_instance_uid": "00000000000000000000000009", "actor": "other", "path": []string{"00000000000000000000000009"}}
	resp, raw = envDoRaw(t, env, http.MethodPost, cronClaimPath(p.ID, j.UID), body, bearer("owner-test-token"))
	require.Equal(t, 403, resp.StatusCode, string(raw))
	delete(body, "origin")
	resp, raw = envDoRaw(t, env, http.MethodPost, cronClaimPath(p.ID, j.UID), body, bearer("owner-test-token"))
	require.Equal(t, 201, resp.StatusCode, string(raw))
	other, err := env.DB.CreateProject(t.Context(), "other-project")
	require.NoError(t, err)
	resp, raw = envDoRaw(t, env, http.MethodGet, fmt.Sprintf("/api/v1/projects/%d/cron/claims/00000000000000000000000003", other.ID), nil, bearer("owner-test-token"))
	require.Equal(t, 404, resp.StatusCode, string(raw))
	require.NotContains(t, string(raw), "executor-a")
}

func TestCronOccurrenceClaimReadonlyCannotGrant(t *testing.T) {
	env := testenv.New(t, testenv.WithInsecureReadonly())
	p, err := env.DB.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	j := cronClaimJob(t, env, p)
	resp, raw := envDoRaw(t, env, http.MethodPost, cronClaimPath(p.ID, j.UID), cronClaimBody(j, "00000000000000000000000003"), nil)
	require.Equal(t, 401, resp.StatusCode, string(raw))
}

type revokingCronClaimStore struct {
	db.Storage
	before func()
}

func (s revokingCronClaimStore) ClaimCronOccurrence(ctx context.Context, in db.CronOccurrenceClaim) (db.CronClaimResult, error) {
	s.before()
	return s.Storage.ClaimCronOccurrence(ctx, in)
}

func TestCronOccurrenceClaimRevokedTokenFence(t *testing.T) {
	env := testenv.New(t)
	p, err := env.DB.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	j := cronClaimJob(t, env, p)
	token, _, err := env.DB.CreateAPIToken(t.Context(), db.CreateAPITokenParams{PlaintextToken: "actor-test-token", Actor: "worker", AdminActor: "admin"})
	require.NoError(t, err)
	store := revokingCronClaimStore{Storage: env.DB, before: func() { _, _, e := env.DB.RevokeAPIToken(t.Context(), token.ID, "admin"); require.NoError(t, e) }}
	server := daemon.NewServer(daemon.ServerConfig{DB: store, Auth: config.AuthConfig{Token: "bootstrap-test-token", RequireTokenIdentity: true}})
	t.Cleanup(func() { _ = server.Close() })
	local := httptest.NewServer(server.Handler())
	t.Cleanup(local.Close)
	copyEnv := *env
	copyEnv.URL = local.URL
	resp, raw := envDoRaw(t, &copyEnv, http.MethodPost, cronClaimPath(p.ID, j.UID), cronClaimBody(j, "00000000000000000000000003"), bearer("actor-test-token"))
	require.Equal(t, 401, resp.StatusCode, string(raw))
	_, err = env.DB.CronOccurrenceClaim(t.Context(), p.ID, "00000000000000000000000003")
	require.ErrorIs(t, err, db.ErrNotFound)
}

func TestCronOccurrenceClaimScopeAndArchive(t *testing.T) {
	env := testenv.New(t, testenv.WithRequireTokenIdentity())
	p, err := env.DB.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	j := cronClaimJob(t, env, p)
	issue, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: p.ID, Title: "Delegated work", Author: "worker"})
	require.NoError(t, err)
	expiry := time.Now().Add(time.Hour)
	_, _, err = env.DB.CreateAPIToken(t.Context(), db.CreateAPITokenParams{PlaintextToken: "scoped-claim-test-token", Actor: "worker", AdminActor: db.BootstrapActor, Scope: &db.APITokenScope{Kind: db.APITokenScopeIssueSubtree, ProjectUID: p.UID, RootIssueUID: issue.UID}, ExpiresAt: &expiry}) // #nosec G101 -- synthetic token used only by this test.
	require.NoError(t, err)
	resp, raw := envDoRaw(t, env, http.MethodPost, cronClaimPath(p.ID, j.UID), cronClaimBody(j, "00000000000000000000000003"), bearer("scoped-claim-test-token"))
	require.Equal(t, 403, resp.StatusCode, string(raw))
	_, _, err = env.DB.CreateAPIToken(t.Context(), db.CreateAPITokenParams{PlaintextToken: "worker-claim-test-token", Actor: "worker", AdminActor: db.BootstrapActor}) // #nosec G101 -- synthetic token used only by this test.
	require.NoError(t, err)
	_, _, err = env.DB.RemoveProject(t.Context(), db.RemoveProjectParams{ProjectID: p.ID, Actor: "worker", Force: true})
	require.NoError(t, err)
	resp, raw = envDoRaw(t, env, http.MethodPost, cronClaimPath(p.ID, j.UID), cronClaimBody(j, "00000000000000000000000003"), bearer("worker-claim-test-token"))
	require.Equal(t, 404, resp.StatusCode, string(raw))
}
