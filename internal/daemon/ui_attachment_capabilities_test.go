package daemon

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
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

func TestUIAttachmentCapabilityCacheLifecycle(t *testing.T) {
	store := openClaimGateHelperDB(t)
	project, _ := createClaimGateHelperIssue(t, store)
	var calls atomic.Int32
	var configured atomic.Bool
	configured.Store(true)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
		_, r := uiAttachmentRoutes(t.Context(), cfg, cache)
		require.Len(t, r, 1)
		return []bool{r[0].Read, r[0].Write}
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
	_, routes := uiAttachmentRoutes(t.Context(), cfg, cache)
	<-started
	require.Len(t, routes, 2)
	for _, route := range routes {
		require.True(t, route.Read)
		require.Equal(t, route.ProjectUID == healthy.UID, route.Write)
	}
	require.EqualValues(t, 2, calls.Load())
	_, _ = uiAttachmentRoutes(t.Context(), cfg, cache)
	require.EqualValues(t, 2, calls.Load())
}
