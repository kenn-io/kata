package daemon_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func TestFederationBridgePreflightCrossLinkFeatures(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, localStore db.Storage) {
		ctx := t.Context()
		hub := newBridgeHubFixture(t)
		peer, err := hub.store.CreateProject(ctx, "other-shared-project")
		require.NoError(t, err)
		_, err = hub.store.EnableProjectFederation(ctx, peer.ID, "admin")
		require.NoError(t, err)
		pin, err := hub.store.RootAuthority(ctx, hub.root.private.UID)
		require.NoError(t, err)
		pin.ProjectUID = peer.UID
		require.NoError(t, hub.store.PinRootAuthority(ctx, pin))
		source, _, err := hub.store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: hub.root.private.ID, Author: "member", Title: "Source"})
		require.NoError(t, err)
		target, _, err := hub.store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: peer.ID, Author: "member", Title: "Target"})
		require.NoError(t, err)
		_, _, err = hub.store.CreateLinkAndEvent(ctx, db.CreateLinkParams{FromIssueID: source.ID, ToIssueID: target.ID, Type: "blocks", Author: "member"}, db.LinkEventParams{EventType: "issue.linked", EventIssueID: source.ID, Actor: "member", FromUID: source.UID, ToUID: target.UID, FromShortID: source.ShortID, ToShortID: target.ShortID})
		require.NoError(t, err)
		var old atomic.Bool
		old.Store(true)
		var enrollments atomic.Int32
		proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/federation/enrollments") {
				enrollments.Add(1)
			}
			req := r.Clone(r.Context())
			req.RequestURI = ""
			req.URL.Scheme = "http"
			req.URL.Host = hub.root.server.Listener.Addr().String()
			if old.Load() && strings.HasSuffix(r.URL.Path, "/federation") {
				req.Header.Set(db.EventFeaturesHeader, db.CronEventFeature+","+db.IssueMoveEventFeature+","+db.RepeatedAdoptionEventFeature)
			}
			resp, err := hub.root.server.Client().Do(req)
			if err != nil {
				http.Error(w, "proxy failed", http.StatusBadGateway)
				return
			}
			defer func() { _ = resp.Body.Close() }()
			for key, values := range resp.Header {
				for _, value := range values {
					w.Header().Add(key, value)
				}
			}
			w.WriteHeader(resp.StatusCode)
			_, _ = io.Copy(w, resp.Body)
		}))
		t.Cleanup(proxy.Close)
		local := newBridgeLocalDaemon(t, localStore, hub.catalog(proxy.URL))
		code, _, raw := local.request(t, http.MethodPost, "/api/v1/federation/bridges", "", bridgeConnectBody(hub.root.private.Name, false), bridgeOwnerHeaders)
		require.Equal(t, http.StatusConflict, code, string(raw))
		require.Equal(t, "unsupported_event_features", bridgeErrorCode(t, raw))
		require.Zero(t, enrollments.Load())
		_, err = localStore.ProjectByName(ctx, "shared-replica")
		require.ErrorIs(t, err, db.ErrNotFound)
		old.Store(false)
		code, _, raw = local.request(t, http.MethodPost, "/api/v1/federation/bridges", "", bridgeConnectBody(hub.root.private.Name, true), bridgeOwnerHeaders)
		require.Equal(t, http.StatusOK, code, string(raw))
	})
}
