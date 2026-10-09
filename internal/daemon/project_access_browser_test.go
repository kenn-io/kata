package daemon_test

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
)

// R9: identity bootstrap can administer teams/ACLs in the browser, while
// ordinary attributed writes, token administration and remote hops stay fenced.
func TestIdentityBrowserAccessAdministration(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newProjectAccessFixture(t, store)
		const origin = "http://127.0.0.1:27123"
		auth := config.AuthConfig{Token: "bootstrap-test-token", RequireTokenIdentity: true}
		now := time.Now()
		manager, err := daemon.NewWebSessionManager(daemon.WebSessionManagerConfig{Origin: origin, InstanceID: "browseradmin", Writable: true, Updates: "sse", Auth: auth, DB: store, Clock: func() time.Time { return now }})
		require.NoError(t, err)
		issued, err := manager.Login(t.Context(), "bootstrap-test-token", "/kata")
		require.NoError(t, err)
		require.False(t, issued.Writable)
		require.False(t, manager.CanWrite(issued.Principal))
		server := daemon.NewServer(daemon.ServerConfig{DB: store, Auth: auth, WebSessions: manager})
		t.Cleanup(func() { require.NoError(t, server.Close()) })
		handler, err := server.HandlerFor(daemon.ListenerPolicy{Kind: daemon.ListenerBrowser, Origin: origin, RequireBrowserSession: true})
		require.NoError(t, err)
		request := func(method, path, body, csrf, requestOrigin string) *httptest.ResponseRecorder {
			req := httptest.NewRequest(method, origin+path, strings.NewReader(body))
			req.RemoteAddr = "127.0.0.1:40123"
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Origin", requestOrigin)
			req.Header.Set("X-Kata-Web-Session", issued.Session)
			req.Header.Set("X-Kata-CSRF", csrf)
			req.AddCookie(manager.Cookie(issued.Cookie))
			req = req.WithContext(context.WithValue(req.Context(), http.LocalAddrContextKey, net.Addr(&net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 27123})))
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, req)
			return response
		}
		for _, invalid := range []struct{ csrf, origin string }{{"", origin}, {issued.CSRF, "http://untrusted.example"}} {
			response := request(http.MethodPost, "/api/v1/teams", `{"name":"blocked-team"}`, invalid.csrf, invalid.origin)
			require.Equal(t, http.StatusForbidden, response.Code, response.Body.String())
		}
		response := request(http.MethodPost, "/api/v1/teams", `{"name":"browser-team"}`, issued.CSRF, origin)
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())
		var created struct {
			Team db.Team `json:"team"`
		}
		require.NoError(t, json.Unmarshal(response.Body.Bytes(), &created))
		teamPath := "/api/v1/teams/" + created.Team.UID
		response = request(http.MethodPut, teamPath+"/members/member", "", issued.CSRF, origin)
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())
		path := fmt.Sprintf("/api/v1/projects/%d/access", f.public.ID)
		response = request(http.MethodPut, path, fmt.Sprintf(`{"visibility":"teams","team_uids":[%q]}`, created.Team.UID), issued.CSRF, origin)
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())
		policy, err := store.ProjectAccessPolicy(t.Context(), f.public.UID)
		require.NoError(t, err)
		require.Equal(t, []string{created.Team.UID}, policy.TeamUIDs)
		response = request(http.MethodGet, "/api/v1/ui/snapshot?view=all-open", "", "", origin)
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())
		var snapshot struct {
			Capabilities struct {
				Writable    bool `json:"writable"`
				AccessAdmin bool `json:"access_admin"`
			} `json:"capabilities"`
		}
		require.NoError(t, json.Unmarshal(response.Body.Bytes(), &snapshot))
		require.False(t, snapshot.Capabilities.Writable)
		require.True(t, snapshot.Capabilities.AccessAdmin)
		for _, path := range []string{fmt.Sprintf("/api/v1/projects/%d/issues", f.public.ID), "/api/v1/tokens", "/api/v1/federation/enrollments", "/api/v1/ui/proxy/api/v1/teams"} {
			response = request(http.MethodPost, path, `{"title":"forbidden","actor":"forged"}`, issued.CSRF, origin)
			require.Equal(t, http.StatusForbidden, response.Code, response.Body.String())
		}
		response = request(http.MethodDelete, teamPath+"/members/member", "", issued.CSRF, origin)
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())
		response = request(http.MethodDelete, teamPath, "", issued.CSRF, origin)
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())
		now = now.Add(25 * time.Hour)
		response = request(http.MethodPost, "/api/v1/teams", `{"name":"expired-team"}`, issued.CSRF, origin)
		require.Equal(t, http.StatusUnauthorized, response.Code, response.Body.String())
	})
}
