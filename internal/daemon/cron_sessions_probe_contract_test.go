package daemon_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
)

// A permanent enrollment or project mismatch must not masquerade as an offline hub.
func TestCronSessionProbeClassifiesHubFailures(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		for _, tc := range []struct {
			name                         string
			status                       int
			features                     string
			wrongID, wrongUID, transport bool
			wantStatus                   int
			wantCode                     string
		}{
			{name: "unauthorized", status: 401, wantStatus: 409, wantCode: "federation_credential_conflict"},
			{name: "forbidden", status: 403, wantStatus: 409, wantCode: "federation_credential_conflict"},
			{name: "missing project", status: 404, wantStatus: 409, wantCode: "federation_binding_conflict"},
			{name: "wrong ID", status: 200, wrongID: true, wantStatus: 409, wantCode: "federation_binding_conflict"},
			{name: "wrong UID", status: 200, wrongUID: true, wantStatus: 409, wantCode: "federation_binding_conflict"},
			{name: "unsupported", status: 200, features: db.CronEventFeature, wantStatus: 409, wantCode: "unsupported_event_features"},
			{name: "unavailable", status: 503, wantStatus: 503, wantCode: "federation_offline"},
			{name: "transport", transport: true, wantStatus: 503, wantCode: "federation_offline"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				projectUID, err := uid.New()
				require.NoError(t, err)
				project, err := store.CreateProject(t.Context(), "probe-"+projectUID)
				require.NoError(t, err)
				existing := adoptionRun(t, store, project, false)
				hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					if tc.status != http.StatusOK {
						http.Error(w, "private-hub-response", tc.status)
						return
					}
					id, projectUID := int64(42), project.UID
					if tc.wrongID {
						id++
					}
					if tc.wrongUID {
						projectUID = "01HZNQ7VFPK1XGD8R5MABCD4EX"
					}
					advertised := db.SupportedEventFeatures
					if tc.features != "" {
						advertised = tc.features
					}
					w.Header().Set(db.EventFeaturesHeader, advertised)
					_, _ = fmt.Fprintf(w, `{"project_id":%d,"project_uid":%q}`, id, projectUID)
				}))
				t.Cleanup(hub.Close)
				if tc.transport {
					hub.Close()
				}
				_, err = store.UpsertFederationBinding(t.Context(), db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: hub.URL, HubProjectID: 42, HubProjectUID: project.UID, Actor: "worker", Enabled: true, PushEnabled: true})
				require.NoError(t, err)
				credentials := newReplicaCredentialStore()
				credentials.put(project.UID, config.FederationCredential{HubURL: hub.URL, HubProjectID: 42, Token: "private-enrollment-token", Actor: "worker"})
				cfg := forwardingTestConfig(daemon.ServerConfig{DB: store, FederationCredentials: credentials})
				cfg.FederationHubClient = func(context.Context, config.FederationCredential) (*http.Client, error) { return hub.Client(), nil }
				server := daemon.NewServer(cfg)
				t.Cleanup(func() { require.NoError(t, server.Close()) })
				web := httptest.NewServer(server.Handler())
				t.Cleanup(web.Close)
				before, err := store.EventsAfter(t.Context(), db.EventsAfterParams{ProjectID: project.ID, Limit: 100})
				require.NoError(t, err)
				runUID, err := uid.New()
				require.NoError(t, err)
				body := fmt.Sprintf(`{"job_uid":%q,"definition_event_uid":%q,"actor":"worker","status":"succeeded","expected_revision":0,"summary":{"version":1,"sessions_incomplete":true}}`, *existing.JobUID, *existing.DefinitionEventUID)
				req, err := http.NewRequestWithContext(t.Context(), http.MethodPut, fmt.Sprintf("%s/api/v1/projects/%d/cron/runs/%s", web.URL, project.ID, runUID), bytes.NewBufferString(body))
				require.NoError(t, err)
				req.Header.Set("Content-Type", "application/json")
				resp, err := web.Client().Do(req)
				require.NoError(t, err)
				raw, err := io.ReadAll(resp.Body)
				require.NoError(t, err)
				require.NoError(t, resp.Body.Close())
				require.Equal(t, tc.wantStatus, resp.StatusCode, string(raw))
				var result api.ErrorEnvelope
				require.NoError(t, json.Unmarshal(raw, &result))
				require.Equal(t, tc.wantCode, result.Error.Code)
				require.NotEmpty(t, result.Error.Hint)
				require.NotContains(t, string(raw), "private-enrollment-token")
				require.NotContains(t, string(raw), "private-hub-response")
				_, err = store.CronRun(t.Context(), project.ID, runUID)
				require.ErrorIs(t, err, db.ErrNotFound)
				after, err := store.EventsAfter(t.Context(), db.EventsAfterParams{ProjectID: project.ID, Limit: 100})
				require.NoError(t, err)
				require.Equal(t, before, after)
			})
		}
	})
}
