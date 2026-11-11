package federation

import (
	"bytes"
	"encoding/json/jsontext"
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

func TestScopedBackupRestoresInterruptedProjectReplay(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			ctx := t.Context()
			open := func() db.Storage {
				if backend == "sqlite" {
					s, err := sqlitestore.Open(ctx, filepath.Join(t.TempDir(), "kata.db"))
					require.NoError(t, err)
					t.Cleanup(func() { _ = s.Close() })
					return s
				}
				dsn, cleanup := pgstoretest.NewDatabase(t, ctx)
				t.Cleanup(cleanup)
				s, err := pgstore.Open(ctx, dsn)
				require.NoError(t, err)
				t.Cleanup(func() { _ = s.Close() })
				return s
			}
			source := open()
			project, err := source.CreateProject(ctx, "spoke-project")
			require.NoError(t, err)
			updated, err := source.PatchProjectMetadata(ctx, db.PatchProjectMetadataIn{ProjectID: project.ID, IfMatchRev: new(project.Revision), Actor: "tester", Patch: map[string]jsontext.Value{"area": jsontext.Value(`"ops"`)}})
			require.NoError(t, err)
			event := updated.Event
			envelope := eventEnvelopeForSyncTest(event, 100)
			hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v1/projects/42/federation/events":
					switch r.URL.Query().Get("after_id") {
					case "50":
						require.NoError(t, json.MarshalWrite(w, api.PollEventsBody{ResetRequired: true, ResetAfterID: 99, NextAfterID: 99}))
					case "99":
						require.NoError(t, json.MarshalWrite(w, api.PollEventsBody{Events: []api.EventEnvelope{envelope}, NextAfterID: 100}))
					default:
						require.NoError(t, json.MarshalWrite(w, api.PollEventsBody{NextAfterID: 100}))
					}
				case "/api/v1/projects/42/federation/metadata":
					require.NoError(t, json.MarshalWrite(w, api.ProjectFederationBody{ProjectID: 42, ProjectUID: project.UID, ProjectName: project.Name, ReplayHorizonEventID: 100, BaselineThroughEventID: 100}))
				default:
					http.NotFound(w, r)
				}
			}))
			t.Cleanup(hub.Close)
			binding, err := source.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: hub.URL, HubProjectID: 42, HubProjectUID: project.UID, ReplayHorizonEventID: 51, PullCursorEventID: 50, PushCursorEventID: event.ID, Enabled: true, Actor: "tester", AllowInsecure: true})
			require.NoError(t, err)
			creds := config.FederationCredential{HubURL: hub.URL, HubProjectID: 42, Token: "token", AllowInsecure: true}
			t.Setenv("KATA_TEST_FEDERATION_FAILPOINTS", "during_spoke_pull_apply_before_materialize=unexpected")
			err = SyncFederationOnce(ctx, source, binding, creds)
			require.Error(t, err)
			var backup bytes.Buffer
			require.NoError(t, jsonl.Export(ctx, source, &backup, jsonl.ExportOptions{IncludeDeleted: true, ProjectID: project.ID}))
			restored := open()
			require.NoError(t, jsonl.Import(ctx, &backup, restored))
			binding, err = restored.FederationBindingByProject(ctx, project.ID)
			require.NoError(t, err)
			t.Setenv("KATA_TEST_FEDERATION_FAILPOINTS", "")
			var delivered []db.Event
			require.NoError(t, SyncFederationOnceWithPulledEvents(ctx, restored, binding, creds, clientpkg.Opts{}, func(_ int64, events []db.Event) { delivered = append(delivered, events...) }))
			require.Len(t, delivered, 1)
			require.Equal(t, event.UID, delivered[0].UID)
			repaired, err := restored.ProjectByID(ctx, project.ID)
			require.NoError(t, err)
			require.JSONEq(t, `{"area":"ops"}`, string(repaired.Metadata))
			binding, err = restored.FederationBindingByProject(ctx, project.ID)
			require.NoError(t, err)
			require.GreaterOrEqual(t, binding.PullCursorEventID, int64(100))
		})
	}
}
