package federation

import (
	"bytes"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	clientpkg "go.kenn.io/kata/internal/client"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/db/pgstore/pgstoretest"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/jsonl"
)

func TestBackupRestoredPullSuppressesAcceptedEcho(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			ctx := t.Context()
			open := func() (db.Storage, func() db.Storage) {
				if backend == "sqlite" {
					path := filepath.Join(t.TempDir(), "kata.db")
					reopen := func() db.Storage {
						s, err := sqlitestore.Open(ctx, path)
						require.NoError(t, err)
						t.Cleanup(func() { _ = s.Close() })
						return s
					}
					return reopen(), reopen
				}
				dsn, cleanup := pgstoretest.NewDatabase(t, ctx)
				t.Cleanup(cleanup)
				reopen := func() db.Storage {
					s, err := pgstore.Open(ctx, dsn)
					require.NoError(t, err)
					t.Cleanup(func() { _ = s.Close() })
					return s
				}
				return reopen(), reopen
			}
			source, _ := open()
			project, err := source.CreateProject(ctx, "spoke-project")
			require.NoError(t, err)
			_, pending, err := source.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "pending", Author: "tester"})
			require.NoError(t, err)
			_, accepted, err := source.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "accepted", Author: "tester"})
			require.NoError(t, err)
			hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				require.Equal(t, "/api/v1/projects/42/federation/events", r.URL.Path)
				require.NoError(t, json.MarshalWrite(w, api.PollEventsBody{Events: []api.EventEnvelope{eventEnvelopeForSyncTest(accepted, 1)}, NextAfterID: 1}))
			}))
			t.Cleanup(hub.Close)
			_, err = source.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: hub.URL, HubProjectID: 42, HubProjectUID: project.UID, ReplayHorizonEventID: 1, PushEnabled: true, Actor: "tester", Enabled: true, AllowInsecure: true})
			require.NoError(t, err)
			require.NoError(t, source.RecordFederationPushAccepted(ctx, project.ID, []int64{accepted.ID}))
			var backup bytes.Buffer
			require.NoError(t, jsonl.Export(ctx, source, &backup, jsonl.ExportOptions{IncludeDeleted: true, ProjectID: project.ID}))
			restored, reopen := open()
			require.NoError(t, jsonl.Import(ctx, &backup, restored))
			require.NoError(t, restored.Close())
			restored = reopen()
			binding, err := restored.FederationBindingByProject(ctx, project.ID)
			require.NoError(t, err)
			var delivered []db.Event
			require.NoError(t, SyncFederationPullOnlyOnceWithPulledEvents(ctx, restored, binding, config.FederationCredential{HubURL: hub.URL, HubProjectID: 42, Token: "token"}, clientpkg.Opts{}, func(_ int64, events []db.Event) { delivered = append(delivered, events...) }))
			require.Empty(t, delivered)
			binding, err = restored.FederationBindingByProject(ctx, project.ID)
			require.NoError(t, err)
			require.Less(t, binding.PushCursorEventID, pending.ID)
			events, err := restored.PendingFederationPushEvents(ctx, project.ID, restored.InstanceUID(), binding.PushCursorEventID, 10)
			require.NoError(t, err)
			require.Len(t, events, 1)
			require.Equal(t, pending.UID, events[0].UID)
		})
	}
}
