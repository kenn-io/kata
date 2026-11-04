package daemon_test

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

func cronPruneHTTPFixture(t *testing.T, opts ...testenv.Option) (*testenv.Env, db.Project, string) {
	t.Helper()
	env := testenv.New(t, opts...)
	project := seedProject(t, env, "example-project")
	job, _, err := env.DB.PutCronJob(t.Context(), db.PutCronJob{ProjectID: project.ID, Actor: "operator", Name: "Inspect", Definition: cronHTTPDefinition(t)})
	require.NoError(t, err)
	for i := range 2 {
		id := cronHTTPUID(t)
		_, err := env.DB.ObserveCronRun(t.Context(), db.ObserveCronRun{ProjectID: project.ID, UID: id, Actor: "operator", JobUID: &job.UID, DefinitionEventUID: &job.DefinitionEventUID, Status: "succeeded", Summary: cron.Summary{Version: 1}})
		require.NoError(t, err)
		old := time.Now().Add(-72*time.Hour + time.Duration(i)*time.Second)
		_, err = env.DB.ExecContext(t.Context(), `UPDATE cron_runs SET created_at=$1,updated_at=$2 WHERE uid=$3`, old.UTC().Format(db.CronRunCreatedAtFormat), old.UTC().Format(time.RFC3339Nano), id)
		require.NoError(t, err)
	}
	return env, project, fmt.Sprintf("/api/v1/projects/%d/cron/runs/actions/prune", project.ID)
}

// Contract: preview is non-destructive, real batches require an exact
// confirmation, and every committed nonempty batch publishes its durable reset.
func TestCronRunPruneAPI(t *testing.T) {
	env, project, path := cronPruneHTTPFixture(t)
	body := map[string]any{"actor": "operator", "before": time.Now().Add(-48 * time.Hour), "keep": 1, "dry_run": true}
	sub := env.Broadcaster.Subscribe(daemon.SubFilter{ProjectID: project.ID})
	defer sub.Unsub()
	response, raw := envDoRaw(t, env, http.MethodPost, path, body, nil)
	require.Equal(t, 200, response.StatusCode, string(raw))
	var preview db.CronRunPruneResult
	require.NoError(t, json.Unmarshal(raw, &preview))
	require.Equal(t, 1, preview.Pruned)
	require.Zero(t, preview.ResetAfterEventID)
	body["dry_run"] = false
	for _, confirm := range []string{"", "PRUNE wrong-project cron runs"} {
		response, raw = envDoRaw(t, env, http.MethodPost, path, body, map[string]string{"X-Kata-Confirm": confirm})
		require.Equal(t, 412, response.StatusCode, string(raw))
	}
	headers := map[string]string{"X-Kata-Confirm": "PRUNE example-project cron runs"}
	response, raw = envDoRaw(t, env, http.MethodPost, path, body, headers)
	require.Equal(t, 200, response.StatusCode, string(raw))
	var result db.CronRunPruneResult
	require.NoError(t, json.Unmarshal(raw, &result))
	require.Equal(t, preview.Groups, result.Groups)
	require.Positive(t, result.ResetAfterEventID)
	select {
	case message := <-sub.Ch:
		require.Equal(t, daemon.StreamKindEvent, message.Kind)
		require.NotNil(t, message.Event)
		require.Equal(t, "project.metadata_updated", message.Event.Type)
		require.Less(t, message.Event.ID, result.ResetAfterEventID)
	case <-time.After(time.Second):
		t.Fatal("committed prune did not publish exact tally event")
	}
	select {
	case message := <-sub.Ch:
		require.Equal(t, daemon.StreamKindReset, message.Kind)
		require.Equal(t, result.ResetAfterEventID, message.ResetID)
	case <-time.After(time.Second):
		t.Fatal("committed prune did not publish reset")
	}
	response, raw = envDoRaw(t, env, http.MethodPost, path, body, headers)
	require.Equal(t, 200, response.StatusCode, string(raw))
	var empty db.CronRunPruneResult
	require.NoError(t, json.Unmarshal(raw, &empty))
	require.Zero(t, empty.Pruned)
	require.Zero(t, empty.ResetAfterEventID)
	select {
	case message := <-sub.Ch:
		t.Fatalf("empty batch published %+v", message)
	default:
	}
}

func TestCronRunPruneRefreshFailure(t *testing.T) {
	env, project, path := cronPruneHTTPFixture(t, func(cfg *daemon.ServerConfig) { cfg.DB = purgeBaselineFailureStore{cfg.DB} })
	body := map[string]any{"actor": "operator", "before": time.Now().Add(-48 * time.Hour), "keep": 1, "dry_run": false}
	sub := env.Broadcaster.Subscribe(daemon.SubFilter{ProjectID: project.ID})
	defer sub.Unsub()
	before, err := env.DB.MaxEventID(t.Context())
	require.NoError(t, err)
	response, raw := envDoRaw(t, env, http.MethodPost, path, body, map[string]string{"X-Kata-Confirm": "PRUNE example-project cron runs"})
	require.Equal(t, 500, response.StatusCode, string(raw))
	runs, err := env.DB.ListCronRuns(t.Context(), db.CronRunList{ProjectID: project.ID})
	require.NoError(t, err)
	require.Len(t, runs, 1, "prune committed before response-only refresh failed")
	reset, err := env.DB.PurgeResetCheck(t.Context(), before, project.ID)
	require.NoError(t, err)
	require.Greater(t, reset, before)
	select {
	case message := <-sub.Ch:
		require.Equal(t, daemon.StreamKindEvent, message.Kind)
		require.NotNil(t, message.Event)
		require.Equal(t, "project.metadata_updated", message.Event.Type)
		require.Less(t, message.Event.ID, reset)
	case <-time.After(time.Second):
		t.Fatal("refresh failure suppressed committed tally event")
	}
	select {
	case message := <-sub.Ch:
		require.Equal(t, reset, message.ResetID)
	case <-time.After(time.Second):
		t.Fatal("refresh failure suppressed committed reset")
	}
}

func TestCronRunPruneSpokeRefusal(t *testing.T) {
	for _, push := range []bool{false, true} {
		t.Run(fmt.Sprint(push), func(t *testing.T) {
			env, project, path := cronPruneHTTPFixture(t)
			_, err := env.DB.UpsertFederationBinding(t.Context(), db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: "https://daemon.example", HubProjectID: 42, HubProjectUID: project.UID, Enabled: true, PushEnabled: push, Actor: "operator"})
			require.NoError(t, err)
			response, raw := envDoRaw(t, env, http.MethodPost, path, map[string]any{"actor": "operator", "before": time.Now().Add(-48 * time.Hour), "dry_run": true}, nil)
			require.Equal(t, 409, response.StatusCode, string(raw))
			require.Contains(t, string(raw), "federated_admin_required")
		})
	}
}

func TestCronRunPruneInvalidBounds(t *testing.T) {
	env, _, path := cronPruneHTTPFixture(t)
	// Huma accepts the offset timestamp; the store rejects its UTC year zero.
	response, raw := envDoRaw(t, env, http.MethodPost, path, map[string]any{"actor": "operator", "before": "0001-01-01T00:00:00+01:00", "keep": 1, "dry_run": true}, nil)
	require.Equal(t, http.StatusBadRequest, response.StatusCode, string(raw))
	response, raw = envDoRaw(t, env, http.MethodPost, path, map[string]any{"actor": "operator", "before": time.Now().Add(-23 * time.Hour), "keep": 1, "dry_run": true}, nil)
	require.Equal(t, 400, response.StatusCode, string(raw))
	for _, field := range []string{"keep", "limit"} {
		for _, value := range []int{-1, 0, 20001} {
			body := map[string]any{"actor": "operator", "before": time.Now().Add(-48 * time.Hour), "dry_run": true, field: value}
			response, raw := envDoRaw(t, env, http.MethodPost, path, body, nil)
			require.Equal(t, 400, response.StatusCode, string(raw))
		}
	}
}

// Contract: preview uses ordinary attributed write authority, including the
// live transaction fence; federation transport tokens and scoped denials do
// not gain pruning authority.
func TestCronRunPruneWriteAuthority(t *testing.T) {
	for _, kind := range []string{"bootstrap", "identity", "enrollment", "project-denied", "revoked"} {
		t.Run(kind, func(t *testing.T) {
			var tokenID int64
			opts := []testenv.Option{testenv.WithAuthToken("tok"), testenv.WithRequireTokenIdentity()}
			if kind == "revoked" {
				opts = append(opts, func(cfg *daemon.ServerConfig) { cfg.DB = revokingPruneStore{cfg.DB, &tokenID} })
			}
			env, project, path := cronPruneHTTPFixture(t, opts...)
			token, want := "tok", 403
			if kind != "bootstrap" && kind != "enrollment" {
				record, _, err := env.DB.CreateAPIToken(t.Context(), db.CreateAPITokenParams{PlaintextToken: "example-prune-token", Actor: "credential-worker", AdminActor: db.BootstrapActor})
				require.NoError(t, err)
				tokenID = record.ID
				token, want = "example-prune-token", 200
			}
			if kind == "revoked" {
				want = 401
			}
			if kind == "enrollment" {
				_, err := env.DB.EnableProjectFederation(t.Context(), project.ID, "operator")
				require.NoError(t, err)
				enrollment := createClaimEnrollment(t, env, project.ID, cronHTTPUID(t), "pull,push")
				token, want = enrollment.Token, 403
			}
			if kind == "project-denied" {
				team, _, err := env.DB.CreateTeam(t.Context(), "project-readers", "admin")
				require.NoError(t, err)
				_, _, err = env.DB.SetProjectAccessPolicy(t.Context(), db.ProjectAccessPolicy{ProjectUID: project.UID, Visibility: "teams", TeamUIDs: []string{team.UID}}, "admin")
				require.NoError(t, err)
				want = 404
			}
			response, raw := envDoRaw(t, env, http.MethodPost, path, map[string]any{"actor": "supplied-worker", "before": time.Now().Add(-48 * time.Hour), "keep": 1, "dry_run": true}, bearer(token))
			require.Equal(t, want, response.StatusCode, string(raw))
			runs, err := env.DB.ListCronRuns(t.Context(), db.CronRunList{ProjectID: project.ID})
			require.NoError(t, err)
			require.Len(t, runs, 2)
		})
	}
}

type revokingPruneStore struct {
	db.Storage
	tokenID *int64
}

func (s revokingPruneStore) PruneCronRuns(ctx context.Context, in db.PruneCronRuns) (db.CronRunPruneResult, error) {
	if _, _, err := s.RevokeAPIToken(ctx, *s.tokenID, db.BootstrapActor); err != nil {
		return db.CronRunPruneResult{}, err
	}
	return s.Storage.PruneCronRuns(ctx, in)
}
