package daemon_test

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/db/pgstore/pgstoretest"
	"go.kenn.io/kata/internal/uid"
)

type pausedSessionObservation struct {
	db.Storage
	store           *pgstore.Store
	entered, resume chan struct{}
}

func (s *pausedSessionObservation) TryAcquireFederationProjectSharedLock(ctx context.Context, id int64) (func(), bool, error) {
	return s.store.TryAcquireFederationProjectSharedLock(ctx, id)
}
func (s *pausedSessionObservation) ObserveCronRun(ctx context.Context, in db.ObserveCronRun) (db.CronRunObservationResult, error) {
	close(s.entered)
	select {
	case <-s.resume:
	case <-ctx.Done():
		return db.CronRunObservationResult{}, ctx.Err()
	}
	return s.Storage.ObserveCronRun(ctx, in)
}

// The PostgreSQL admission must cover the observation commit after the probe context ends.
func TestCronAdmissionOutlivesProbeContextAcrossPostgresStores(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres testcontainer")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	dsn, cleanup := pgstoretest.NewDatabase(t, ctx)
	t.Cleanup(cleanup)
	writer, err := pgstore.Open(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, writer.Close()) })
	other, err := pgstore.Open(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, other.Close()) })
	project, err := writer.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	definition := cron.JobDefinition{Version: 1, Kind: "job", Trigger: cron.Trigger{Kind: "manual"}, Action: cron.Action{Kind: "execute", Prompt: "Review"}, Issue: &cron.IssuePolicy{Kind: "per-run", Title: "Review"}, Overlap: "forbid", Catchup: "skip"}
	job, _, err := writer.PutCronJob(ctx, db.PutCronJob{ProjectID: project.ID, Name: "Review", Actor: "worker", Definition: definition})
	require.NoError(t, err)
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set(db.EventFeaturesHeader, db.SupportedEventFeatures)
		_, _ = fmt.Fprintf(w, `{"project_id":42,"project_uid":%q}`, project.UID)
	}))
	t.Cleanup(hub.Close)
	_, err = writer.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: hub.URL, HubProjectID: 42, HubProjectUID: project.UID, Enabled: true, PushEnabled: true})
	require.NoError(t, err)
	credentials := newReplicaCredentialStore()
	credentials.put(project.UID, config.FederationCredential{HubURL: hub.URL, HubProjectID: 42, Token: "enrollment-token"})
	probeDone := make(chan (<-chan struct{}), 1)
	paused := &pausedSessionObservation{Storage: writer, store: writer, entered: make(chan struct{}), resume: make(chan struct{})}
	cfg := forwardingTestConfig(daemon.ServerConfig{DB: paused, FederationCredentials: credentials})
	cfg.FederationHubClient = func(ctx context.Context, _ config.FederationCredential) (*http.Client, error) {
		probeDone <- ctx.Done()
		return hub.Client(), nil
	}
	srv := daemon.NewServer(cfg)
	t.Cleanup(func() { require.NoError(t, srv.Close()) })
	server := httptest.NewServer(srv.Handler())
	t.Cleanup(server.Close)
	runUID, err := uid.New()
	require.NoError(t, err)
	body := fmt.Sprintf(`{"job_uid":%q,"definition_event_uid":%q,"actor":"worker","status":"succeeded","expected_revision":0,"summary":{"version":1,"sessions":[{"agent":"claude","session_id":"00000000-0000-4000-8000-000000000001"}]}}`, job.UID, job.DefinitionEventUID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, fmt.Sprintf("%s/api/v1/projects/%d/cron/runs/%s", server.URL, project.ID, runUID), bytes.NewBufferString(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	done := make(chan error, 1)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		resp, err := server.Client().Do(req)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				err = fmt.Errorf("observation status: %s", resp.Status)
			}
		}
		done <- err
	}()
	t.Cleanup(func() {
		select {
		case <-paused.resume:
		default:
			close(paused.resume)
		}
		<-finished
	})
	select {
	case <-paused.entered:
	case err := <-done:
		t.Fatalf("observation ended early: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	select {
	case <-<-probeDone:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	// Try the same project-scoped exclusive advisory lock from the other store.
	// The statement transaction releases it immediately if it was acquired.
	var acquired bool
	err = other.QueryRowContext(ctx, `SELECT pg_catalog.pg_try_advisory_xact_lock(
		pg_catalog.hashtextextended(pg_catalog.current_database() || ':kata:federation:project:' ||
		pg_catalog.current_schema() || ':' || $1::bigint::text, 0))`, project.ID).Scan(&acquired)
	require.NoError(t, err)
	require.False(t, acquired, "exclusive lock acquired before observation commit")
	close(paused.resume)
	require.NoError(t, <-done)
	_, err = writer.CronRun(ctx, project.ID, runUID)
	require.NoError(t, err)
	exclusive, err := other.AcquireFederationProjectExclusiveLock(ctx, project.ID)
	require.NoError(t, err)
	exclusive()
}
