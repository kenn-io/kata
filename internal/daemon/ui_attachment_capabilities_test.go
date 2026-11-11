package daemon

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
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

func TestUIAttachmentCapabilityCacheLifecycle(t *testing.T) {
	store := openClaimGateHelperDB(t)
	project, _ := createClaimGateHelperIssue(t, store)
	var calls atomic.Int32
	var configured atomic.Bool
	configured.Store(true)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		if !configured.Load() {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = fmt.Fprint(w, `{"status":503,"error":{"code":"docbank_not_configured","message":"Docbank is not configured"}}`)
			return
		}
		_, _ = fmt.Fprintf(w, `{"version":1,"project_uid":%q,"origin":%q}`, project.UID, store.InstanceUID())
	}))
	t.Cleanup(upstream.Close)
	binding := db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: upstream.URL, HubProjectID: 1, HubProjectUID: project.UID, Enabled: true, PushEnabled: true, Actor: "operator", AllowInsecure: true}
	_, err := store.UpsertFederationBinding(t.Context(), binding)
	require.NoError(t, err)
	cfg := ServerConfig{DB: store, FederationCredentials: uiCapabilityCredentials{credential: config.FederationCredential{Token: "fixture", AllowInsecure: true}}}
	cache := newUIAttachmentCapabilityCache()
	now := time.Now()
	cache.now = func() time.Time { return now }
	routes := func() []bool {
		current, err := store.FederationBindingByProject(t.Context(), project.ID)
		require.NoError(t, err)
		r := uiAttachmentRoute(t.Context(), cfg, cache, current)
		return []bool{r.Read, r.Write}
	}
	require.Equal(t, []bool{true, true}, routes())
	configured.Store(false)
	require.Equal(t, []bool{true, true}, routes())
	require.EqualValues(t, 1, calls.Load())
	now = now.Add(5 * time.Second)
	require.Equal(t, []bool{false, false}, routes())
	require.EqualValues(t, 2, calls.Load())
	binding.PushEnabled = false
	_, err = store.UpsertFederationBinding(t.Context(), binding)
	require.NoError(t, err)
	now = now.Add(5 * time.Second)
	require.Equal(t, []bool{false, false}, routes())
	require.EqualValues(t, 3, calls.Load())
	configured.Store(true)
	now = now.Add(5 * time.Second)
	require.Equal(t, []bool{true, false}, routes())
	require.EqualValues(t, 4, calls.Load())
	binding.PushEnabled = true
	binding.HubProjectID = 2
	_, err = store.UpsertFederationBinding(t.Context(), binding)
	require.NoError(t, err)
	configured.Store(true)
	require.Equal(t, []bool{true, true}, routes())
	require.EqualValues(t, 5, calls.Load())
	binding.Enabled = false
	_, err = store.UpsertFederationBinding(t.Context(), binding)
	require.NoError(t, err)
	require.Equal(t, []bool{false, false}, routes())
	require.EqualValues(t, 5, calls.Load())
}

func TestUIAttachmentCapabilityParallelTimeout(t *testing.T) {
	store := openClaimGateHelperDB(t)
	blocked, _ := createClaimGateHelperIssue(t, store)
	healthy, err := store.CreateProject(t.Context(), "healthy")
	require.NoError(t, err)
	var calls atomic.Int32
	started := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Query().Get("project_uid") == blocked.UID {
			close(started)
			<-r.Context().Done()
			return
		}
		_, _ = fmt.Fprintf(w, `{"version":1,"project_uid":%q,"origin":%q}`, healthy.UID, store.InstanceUID())
	}))
	t.Cleanup(upstream.Close)
	for _, project := range []db.Project{blocked, healthy} {
		_, err = store.UpsertFederationBinding(t.Context(), db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: upstream.URL, HubProjectID: 1, HubProjectUID: project.UID, Enabled: true, PushEnabled: true, Actor: "operator", AllowInsecure: true})
		require.NoError(t, err)
	}
	cfg := ServerConfig{DB: store, FederationCredentials: uiCapabilityCredentials{credential: config.FederationCredential{Token: "fixture", AllowInsecure: true}}}
	cache := newUIAttachmentCapabilityCache()
	blockedBinding, err := store.FederationBindingByProject(t.Context(), blocked.ID)
	require.NoError(t, err)
	healthyBinding, err := store.FederationBindingByProject(t.Context(), healthy.ID)
	require.NoError(t, err)
	result := make(chan *api.UIAttachmentRoute, 1)
	go func() { result <- uiAttachmentRoute(t.Context(), cfg, cache, blockedBinding) }()
	<-started
	healthyRoute := uiAttachmentRoute(t.Context(), cfg, cache, healthyBinding)
	require.True(t, healthyRoute.Read)
	require.True(t, healthyRoute.Write)
	blockedRoute := <-result
	require.True(t, blockedRoute.Read)
	require.False(t, blockedRoute.Write)
	require.EqualValues(t, 2, calls.Load())
	_ = uiAttachmentRoute(t.Context(), cfg, cache, blockedBinding)
	require.EqualValues(t, 2, calls.Load())
}
