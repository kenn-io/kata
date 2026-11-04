package daemon_test

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

// The operator's authenticated identity belongs to the event; the starter
// remains the immutable run actor, even when the request tries to spoof it.
func TestCronReapAuthenticatedOperator(t *testing.T) {
	env := testenv.New(t)
	p := staleProject(t, env, "example-project")
	job, _, err := env.DB.PutCronJob(t.Context(), db.PutCronJob{ProjectID: p.ID, Name: "Example job", Actor: "starter", Definition: staleDefinition(t)})
	require.NoError(t, err)
	r, err := env.DB.ObserveCronRun(t.Context(), db.ObserveCronRun{ProjectID: p.ID, UID: staleUID(t), JobUID: &job.UID, DefinitionEventUID: &job.DefinitionEventUID, Actor: "starter", Status: "running", Summary: cron.Summary{Version: 1}})
	require.NoError(t, err)
	_, _, err = env.DB.CreateAPIToken(t.Context(), db.CreateAPITokenParams{PlaintextToken: "example-operator-token", Actor: "operator", AdminActor: db.BootstrapActor})
	require.NoError(t, err)
	now := r.Run.UpdatedAt.Add(24 * time.Hour)
	app := daemon.NewServer(daemon.ServerConfig{DB: env.DB, Broadcaster: env.Broadcaster, CronClock: func() time.Time { return now }, Auth: config.AuthConfig{Token: "tok", RequireTokenIdentity: true}})
	t.Cleanup(func() { require.NoError(t, app.Close()) })
	s := httptest.NewServer(app.Handler())
	t.Cleanup(s.Close)
	sub := env.Broadcaster.Subscribe(daemon.SubFilter{ProjectID: p.ID})
	defer sub.Unsub()
	raw, err := json.Marshal(map[string]any{"actor": "starter", "expected_revision": r.Run.Revision, "message": "Runtime stopped"})
	require.NoError(t, err)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, s.URL+fmt.Sprintf("/api/v1/projects/%d/cron/runs/%s/reap", p.ID, r.Run.UID), strings.NewReader(string(raw)))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer example-operator-token")
	req.Header.Set("Content-Type", "application/json")
	res, err := s.Client().Do(req)
	require.NoError(t, err)
	defer func() { require.NoError(t, res.Body.Close()) }()
	data, err := io.ReadAll(res.Body)
	require.NoError(t, err)
	require.Equalf(t, 200, res.StatusCode, "%s", data)
	var out struct {
		Run    db.CronRun `json:"run"`
		Events []db.Event `json:"events"`
	}
	require.NoError(t, json.Unmarshal(data, &out))
	require.Equal(t, "starter", out.Run.Actor)
	require.Equal(t, "unknown", out.Run.Status)
	require.Nil(t, out.Run.EndedAt)
	require.Len(t, out.Events, 1)
	require.Equal(t, "operator", out.Events[0].Actor)
	select {
	case event := <-sub.Ch:
		require.Equal(t, out.Events[0].UID, event.Event.UID)
	default:
		t.Fatal("committed reap was not broadcast before response")
	}
}

func TestCronReapConflictsAndForce(t *testing.T) {
	var now time.Time
	env := testenv.New(t, testenv.WithAuthToken("tok"), func(cfg *daemon.ServerConfig) { cfg.CronClock = func() time.Time { return now } })
	p := staleProject(t, env, "example-project")
	job, _, err := env.DB.PutCronJob(t.Context(), db.PutCronJob{ProjectID: p.ID, Name: "Example job", Actor: "starter", Definition: staleDefinition(t)})
	require.NoError(t, err)
	r, err := env.DB.ObserveCronRun(t.Context(), db.ObserveCronRun{ProjectID: p.ID, UID: staleUID(t), JobUID: &job.UID, DefinitionEventUID: &job.DefinitionEventUID, Actor: "starter", Status: "running", Summary: cron.Summary{Version: 1}})
	require.NoError(t, err)
	now = r.Run.UpdatedAt
	path := fmt.Sprintf("/api/v1/projects/%d/cron/runs/%s/reap", p.ID, r.Run.UID)
	body := map[string]any{"actor": "operator", "expected_revision": r.Run.Revision}
	status, raw := staleHTTP(t, env, http.MethodPost, path, body)
	require.Equalf(t, 409, status, "%s", raw)
	require.Contains(t, string(raw), "cron_run_not_stale")
	require.Contains(t, string(raw), now.Add(24*time.Hour).Format(time.RFC3339Nano))
	body["expected_revision"] = r.Run.Revision + 1
	status, raw = staleHTTP(t, env, http.MethodPost, path, body)
	require.Equalf(t, 409, status, "%s", raw)
	require.Contains(t, string(raw), "cron_conflict")
	require.Contains(t, string(raw), `"current"`)
	body["expected_revision"] = r.Run.Revision
	body["force"] = true
	status, raw = staleHTTP(t, env, http.MethodPost, path, body)
	require.Equalf(t, 200, status, "%s", raw)
	require.NotContains(t, string(raw), `"staleness"`)
	body["expected_revision"] = r.Run.Revision + 1
	status, raw = staleHTTP(t, env, http.MethodPost, path, body)
	require.Equalf(t, 409, status, "%s", raw)
	require.Contains(t, string(raw), "cron_run_not_running")
}

type reapFencedStore struct {
	db.Storage
	before func(context.Context) error
}

func (s reapFencedStore) ReapCronRun(ctx context.Context, in db.ReapCronRun) (db.CronRunObservationResult, error) {
	if err := s.before(ctx); err != nil {
		return db.CronRunObservationResult{}, err
	}
	return s.Storage.ReapCronRun(ctx, in)
}

func TestCronReapRevalidatesOperatorToken(t *testing.T) {
	for _, mode := range []string{"revoked", "expired"} {
		t.Run(mode, func(t *testing.T) {
			env := testenv.New(t)
			p := staleProject(t, env, "example-project")
			job, _, err := env.DB.PutCronJob(t.Context(), db.PutCronJob{ProjectID: p.ID, Name: "Example job", Actor: "starter", Definition: staleDefinition(t)})
			require.NoError(t, err)
			r, err := env.DB.ObserveCronRun(t.Context(), db.ObserveCronRun{ProjectID: p.ID, UID: staleUID(t), JobUID: &job.UID, DefinitionEventUID: &job.DefinitionEventUID, Actor: "starter", Status: "running", Summary: cron.Summary{Version: 1}})
			require.NoError(t, err)
			var tokenID int64
			var expiresAt time.Time
			store := reapFencedStore{Storage: env.DB, before: func(ctx context.Context) error {
				if mode == "revoked" {
					_, _, err := env.DB.RevokeAPIToken(ctx, tokenID, db.BootstrapActor)
					return err
				}
				timer := time.NewTimer(time.Until(expiresAt) + time.Millisecond)
				defer timer.Stop()
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-timer.C:
					return nil
				}
			}}
			app := daemon.NewServer(daemon.ServerConfig{DB: store, Auth: config.AuthConfig{Token: "tok", RequireTokenIdentity: true}})
			t.Cleanup(func() { require.NoError(t, app.Close()) })
			s := httptest.NewServer(app.Handler())
			t.Cleanup(s.Close)
			expiresAt = time.Now().UTC().Add(2 * time.Second)
			params := db.CreateAPITokenParams{PlaintextToken: "example-reap-token", Actor: "operator", AdminActor: db.BootstrapActor}
			if mode == "expired" {
				params.ExpiresAt = &expiresAt
			}
			token, _, err := env.DB.CreateAPIToken(t.Context(), params)
			require.NoError(t, err)
			tokenID = token.ID
			body := fmt.Sprintf(`{"actor":"starter","expected_revision":%d,"force":true}`, r.Run.Revision)
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, s.URL+fmt.Sprintf("/api/v1/projects/%d/cron/runs/%s/reap", p.ID, r.Run.UID), strings.NewReader(body))
			require.NoError(t, err)
			req.Header.Set("Authorization", "Bearer example-reap-token")
			req.Header.Set("Content-Type", "application/json")
			res, err := s.Client().Do(req)
			require.NoError(t, err)
			data, err := io.ReadAll(res.Body)
			require.NoError(t, err)
			require.NoError(t, res.Body.Close())
			require.Equalf(t, 401, res.StatusCode, "%s", data)
			stored, err := env.DB.CronRun(t.Context(), p.ID, r.Run.UID)
			require.NoError(t, err)
			require.Equal(t, r.Run, stored)
		})
	}
}
