package federation

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/testfix"
)

const benchmarkPollProjects = 50

var benchmarkPollDatabases atomic.Int64

// BenchmarkFederationPollPass measures one runner pass over 50 bound projects
// that share one credential. hub-ns/pass is the time the hub spends serving the
// pass and requests/pass counts the HTTP requests the hub receives. PostgreSQL
// runs need KATA_TEST_POSTGRES_DSN.
func BenchmarkFederationPollPass(b *testing.B) {
	for _, backend := range []string{"sqlite", "postgres"} {
		for _, pages := range []string{"empty", "nonempty"} {
			b.Run(backend+"/"+pages, func(b *testing.B) {
				benchmarkFederationPollPass(b, backend, pages == "nonempty")
			})
		}
	}
}

func benchmarkFederationPollPass(b *testing.B, backend string, nonEmpty bool) {
	ctx := b.Context()
	hubStore := openBenchmarkHubStore(b, backend)
	var hubNanos, hubRequests atomic.Int64
	server := daemon.NewServer(daemon.ServerConfig{DB: hubStore, FederationBatchPoll: true})
	b.Cleanup(func() { _ = server.Close() })
	handler := server.Handler()
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		handler.ServeHTTP(w, r)
		hubNanos.Add(int64(time.Since(started)))
		hubRequests.Add(1)
	}))
	b.Cleanup(hub.Close)

	spokeStore, err := testfix.OpenSQLite(ctx, filepath.Join(b.TempDir(), "spoke.db"))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = spokeStore.Close() })
	const token = "benchmark-shared-token"
	if _, err := hubStore.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{ //nolint:gosec // benchmark-only bearer token
		Token: token, SpokeInstanceUID: spokeStore.InstanceUID(), Capabilities: "pull,push", Actor: "worker",
	}); err != nil {
		b.Fatal(err)
	}
	credentials := newMoveCredentialStore()
	hubProjects := make([]db.Project, benchmarkPollProjects)
	for i := range hubProjects {
		project, err := hubStore.CreateProject(ctx, fmt.Sprintf("hub-project-%d", i))
		if err != nil {
			b.Fatal(err)
		}
		if _, err := hubStore.EnableProjectFederation(ctx, project.ID, "worker"); err != nil {
			b.Fatal(err)
		}
		local, err := spokeStore.CreateProjectWithUID(ctx, project.Name, project.UID)
		if err != nil {
			b.Fatal(err)
		}
		if _, err := spokeStore.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: local.ID, Role: db.FederationRoleSpoke, HubURL: hub.URL, HubProjectID: project.ID, HubProjectUID: project.UID, Enabled: true, PushEnabled: true, Actor: "worker"}); err != nil {
			b.Fatal(err)
		}
		credentials[project.UID] = config.FederationCredential{HubURL: hub.URL, HubProjectID: project.ID, Token: token, Actor: "worker"}
		hubProjects[i] = project
	}
	runner := &Runner{DB: spokeStore, Credentials: credentials}
	// Settle the first pushes and pulls before measuring idle passes.
	for range 3 {
		if err := runner.RunOnce(ctx); err != nil {
			b.Fatal(err)
		}
	}
	hubNanos.Store(0)
	hubRequests.Store(0)
	b.ResetTimer()
	for i := 0; b.Loop(); i++ {
		if nonEmpty {
			b.StopTimer()
			for _, project := range hubProjects {
				if _, _, err := hubStore.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: fmt.Sprintf("work %d", i), Author: "worker"}); err != nil {
					b.Fatal(err)
				}
			}
			b.StartTimer()
		}
		if err := runner.RunOnce(ctx); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(float64(hubNanos.Load())/float64(b.N), "hub-ns/pass")
	b.ReportMetric(float64(hubRequests.Load())/float64(b.N), "requests/pass")
}

func openBenchmarkHubStore(b *testing.B, backend string) db.Storage {
	b.Helper()
	ctx := b.Context()
	if backend == "sqlite" {
		store, err := testfix.OpenSQLite(ctx, filepath.Join(b.TempDir(), "hub.db"))
		if err != nil {
			b.Fatal(err)
		}
		b.Cleanup(func() { _ = store.Close() })
		return store
	}
	baseDSN := os.Getenv("KATA_TEST_POSTGRES_DSN")
	if baseDSN == "" {
		b.Skip("KATA_TEST_POSTGRES_DSN is not set")
	}
	admin, err := sql.Open("pgx", baseDSN)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = admin.Close() })
	name := fmt.Sprintf("kata_bench_%d_%d", os.Getpid(), benchmarkPollDatabases.Add(1))
	if _, err := admin.ExecContext(ctx, `CREATE DATABASE "`+name+`"`); err != nil { //nolint:gosec // G202: generated benchmark database name.
		b.Fatal(err)
	}
	b.Cleanup(func() {
		// b.Context is canceled before cleanups run.
		_, _ = admin.ExecContext(context.WithoutCancel(ctx), `DROP DATABASE "`+name+`" WITH (FORCE)`)
	})
	dsn, err := url.Parse(baseDSN)
	if err != nil {
		b.Fatal(err)
	}
	dsn.Path = "/" + name
	store, err := pgstore.Open(ctx, dsn.String())
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = store.Close() })
	return store
}
