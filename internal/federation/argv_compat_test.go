package federation_test

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	clientpkg "go.kenn.io/kata/internal/client"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/federation"
)

// Negotiate the reserved feature before sending any prefix of a mixed batch.
func TestArgvCompatLegacyPublication(t *testing.T) {
	for _, tc := range []struct {
		name, supported string
		accepted        bool
	}{
		{name: "cron only", supported: "cron_v1"},
		{name: "positive", supported: "cron_v1,cron_workflow_argv_v1", accepted: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			posts := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.Method == http.MethodPost {
					posts++
					var body api.FederationIngestEventsRequestBody
					require.NoError(t, json.UnmarshalRead(r.Body, &body))
					require.Equal(t, 31, body.SchemaVersion)
					require.Len(t, body.Events, 2)
					require.Equal(t, db.SupportedEventFeatures, r.Header.Get(db.EventFeaturesHeader))
					_, _ = w.Write([]byte(`{"accepted":2,"push_cursor_event_id":11}`))
					return
				}
				w.Header().Set(db.EventFeaturesHeader, tc.supported)
				_, _ = w.Write([]byte(`{"project_id":1,"project_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAA","replay_horizon_event_id":1,"baseline_through_event_id":1}`))
			}))
			t.Cleanup(server.Close)
			client, err := federation.NewClient(t.Context(), server.URL, "token", clientpkg.Opts{})
			require.NoError(t, err)
			events := []api.FederationIngestEventEnvelope{
				{EventID: 10, Type: "project.metadata_updated", Payload: jsontext.Value(`{}`)},
				{EventID: 11, Type: "cron.workflow.created", Payload: jsontext.Value(`{"definition":{"steps":[{"kind":"argv"}]}}`)},
			}
			ack, err := client.IngestProjectEvents(t.Context(), 1, events)
			if tc.accepted {
				require.NoError(t, err)
				require.Equal(t, 1, posts)
				require.Equal(t, int64(11), ack.PushCursorEventID)
			} else {
				require.Error(t, err)
				require.Zero(t, posts)
				require.Zero(t, ack.PushCursorEventID)
			}
		})
	}
}

// Exercise the real cursor-owning sender, rather than inferring persisted
// cursor safety from an empty HTTP acknowledgement or an event-table maximum.
func TestArgvCompatPushRetainsStoredCursor(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			ctx := t.Context()
			store := nativeStore(t, backend)
			project, err := store.CreateProject(ctx, "spoke-project")
			require.NoError(t, err)
			checkpoint, err := store.MaxEventID(ctx)
			require.NoError(t, err)
			require.Positive(t, checkpoint)
			definition := cron.WorkflowDefinition{Version: 1, Steps: []cron.WorkflowStep{{Key: "legacy", Kind: "command", Command: "true"}}}
			_, _, err = store.PutCronWorkflow(ctx, db.PutCronWorkflow{ProjectID: project.ID, Name: "Legacy", Actor: "worker", Definition: definition})
			require.NoError(t, err)
			_, event, err := store.PutCronWorkflow(ctx, db.PutCronWorkflow{ProjectID: project.ID, Name: "Imported reserved format", Actor: "worker", Definition: definition})
			require.NoError(t, err)
			var payload map[string]jsontext.Value
			require.NoError(t, json.Unmarshal([]byte(event.Payload), &payload))
			payload["definition"] = jsontext.Value(`{"version":1,"steps":[{"key":"reserved","kind":"argv"}]}`)
			raw, err := json.Marshal(payload)
			require.NoError(t, err)
			_, err = store.(interface {
				ExecContext(context.Context, string, ...any) (sql.Result, error)
			}).ExecContext(ctx, `UPDATE events SET payload=$1 WHERE uid=$2`, string(raw), event.UID)
			require.NoError(t, err)
			posts := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.Method == http.MethodPost {
					posts++
					_, _ = fmt.Fprintf(w, `{"accepted":2,"push_cursor_event_id":%d}`, event.ID)
					return
				}
				w.Header().Set(db.EventFeaturesHeader, db.CronEventFeature)
				_, _ = fmt.Fprintf(w, `{"project_id":1,"project_uid":%q,"replay_horizon_event_id":1,"baseline_through_event_id":1}`, project.UID)
			}))
			t.Cleanup(server.Close)
			binding, err := store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: server.URL, HubProjectID: 1, HubProjectUID: project.UID, ReplayHorizonEventID: checkpoint + 1, PullCursorEventID: checkpoint, PushCursorEventID: checkpoint, PushEnabled: true, Enabled: true, Actor: "worker"})
			require.NoError(t, err)
			client, err := federation.NewClient(ctx, server.URL, "fixture-token", clientpkg.Opts{})
			require.NoError(t, err)
			err = federation.PushDirectSpoke(ctx, store, binding, 1, client)
			require.ErrorIs(t, err, db.ErrUnsupportedEventFeatures)
			require.Zero(t, posts)
			after, err := store.FederationBindingByProject(ctx, project.ID)
			require.NoError(t, err)
			require.Equal(t, checkpoint, after.PushCursorEventID)
		})
	}
}
