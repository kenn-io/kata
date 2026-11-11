package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
)

type uiCapabilityCredentials struct {
	config.FederationCredentialStore
	credential config.FederationCredential
}

func TestUIAttachmentRouteOriginAvailability(t *testing.T) {
	store := openClaimGateHelperDB(t)
	project, _ := createClaimGateHelperIssue(t, store)
	var status int
	var body string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = fmt.Fprint(w, body)
	}))
	t.Cleanup(upstream.Close)
	_, err := store.UpsertFederationBinding(t.Context(), db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: upstream.URL, HubProjectID: 1, HubProjectUID: project.UID, Enabled: true, PushEnabled: true, Actor: "operator", AllowInsecure: true})
	require.NoError(t, err)
	srv := NewServer(ServerConfig{DB: store, FederationCredentials: uiCapabilityCredentials{credential: config.FederationCredential{HubURL: upstream.URL, HubProjectID: 1, Token: "fixture", AllowInsecure: true}}})
	t.Cleanup(func() { require.NoError(t, srv.Close()) })
	for _, tc := range []struct {
		name   string
		status int
		body   string
		read   bool
		write  bool
	}{
		{"temporary upstream failure", http.StatusBadGateway, `{"error":{"code":"upstream_failed"}}`, true, true},
		{"no docbank", http.StatusServiceUnavailable, `{"error":{"code":"docbank_not_configured"}}`, false, false},
		{"unsupported", http.StatusNotFound, ``, false, false},
		{"origin mismatch", http.StatusConflict, `{"error":{"code":"attachment_origin_mismatch"}}`, true, false},
		{"recovered", http.StatusOK, fmt.Sprintf(`{"version":1,"project_uid":%q,"origin":%q}`, project.UID, store.InstanceUID()), true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, body = tc.status, tc.body
			r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/ui/attachment-route?project_uid="+project.UID, nil)
			r.RemoteAddr = "127.0.0.1:1234"
			w := httptest.NewRecorder()
			srv.Handler().ServeHTTP(w, r)
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			var response api.UIAttachmentRouteResponseBody
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
			require.NotNil(t, response.Route)
			require.Equal(t, tc.read, response.Route.Read)
			require.Equal(t, tc.write, response.Route.Write)
		})
	}
}

func (c uiCapabilityCredentials) FederationCredential(context.Context, string) (config.FederationCredential, bool, error) {
	return c.credential, true, nil
}

func TestUISnapshotDoesNotWaitForAttachmentOrigin(t *testing.T) {
	store := openClaimGateHelperDB(t)
	project, issue := createClaimGateHelperIssue(t, store)
	other, err := store.CreateProject(t.Context(), "other")
	require.NoError(t, err)
	started, release := make(chan struct{}, 1), make(chan struct{})
	releaseOrigin := sync.OnceFunc(func() { close(release) })
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started <- struct{}{}
		select {
		case <-release:
			_, _ = fmt.Fprintf(w, `{"version":1,"project_uid":%q,"origin":%q}`, project.UID, store.InstanceUID())
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(upstream.Close)
	_, err = store.UpsertFederationBinding(t.Context(), db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: upstream.URL, HubProjectID: 1, HubProjectUID: project.UID, Enabled: true, PushEnabled: true, Actor: "operator", AllowInsecure: true})
	require.NoError(t, err)
	srv := NewServer(ServerConfig{DB: store, FederationCredentials: uiCapabilityCredentials{credential: config.FederationCredential{HubURL: upstream.URL, HubProjectID: 1, Token: "fixture", AllowInsecure: true}}})
	t.Cleanup(func() { require.NoError(t, srv.Close()) })
	defer releaseOrigin()
	request := func(path, etag string) *httptest.ResponseRecorder {
		ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond) //nolint:kennlint // deadlinetest: Prove completion before the blocked probe's deadline.
		defer cancel()
		r := httptest.NewRequestWithContext(ctx, http.MethodGet, path, nil)
		r.RemoteAddr = "127.0.0.1:1234"
		r.Header.Set("If-None-Match", etag)
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, r)
		return w
	}
	first := request("/api/v1/ui/snapshot?view=inbox", "")
	require.Equal(t, http.StatusOK, first.Code, first.Body.String())
	select {
	case <-started:
		t.Fatal("snapshot probed the attachment origin")
	default:
	}
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/ui/attachment-route?project_uid="+project.UID, nil)
		r.RemoteAddr = "127.0.0.1:1234"
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, r)
		done <- w
	}()
	<-started
	for _, tc := range []struct {
		path, etag string
		status     int
	}{
		{"/api/v1/ui/snapshot?view=inbox", first.Header().Get("ETag"), http.StatusNotModified},
		{"/api/v1/ui/references?q=issue", "", http.StatusOK},
		{"/api/v1/ui/references?issue_uid=" + issue.UID, "", http.StatusOK},
		{"/api/v1/ui/attachment-route?project_uid=" + other.UID, "", http.StatusOK},
	} {
		w := request(tc.path, tc.etag)
		require.Equal(t, tc.status, w.Code, "%s: %s", tc.path, w.Body.String())
	}
	releaseOrigin()
	w := <-done
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
}
