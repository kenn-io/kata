package daemon_test

import (
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

const requestProjectScopeHeader = "X-Kata-Project-Scope"

func requestScopeValue(t *testing.T, refs ...string) string {
	t.Helper()
	raw, err := json.Marshal(refs)
	require.NoError(t, err)
	return string(raw)
}

func TestRequestProjectScopeRejectsMalformedLimits(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newProjectAccessFixture(t, store)
		for _, limit := range []string{"", " ", ",", f.public.UID + ",", "invalid", "id:0", "id:-1", "id:missing", "[]", "null", "[null]", requestScopeValue(t, "invalid"), requestScopeValue(t, "id:0"), requestScopeValue(t, f.public.UID) + "," + requestScopeValue(t, f.private.UID)} {
			t.Run(fmt.Sprintf("%q", limit), func(t *testing.T) {
				status, _, _ := f.request(t, http.MethodGet, "/api/v1/projects", "admin", nil, map[string]string{requestProjectScopeHeader: limit})
				require.Equal(t, http.StatusBadRequest, status)
			})
		}
	})
}
func TestRequestProjectScopeOnlyNarrowsAuthority(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newProjectAccessFixture(t, store)
		for _, tc := range []struct {
			actor, limit    string
			visible, hidden db.Project
		}{
			{"admin", requestScopeValue(t, f.public.UID), f.public, f.private},
			{"admin", requestScopeValue(t, fmt.Sprintf("id:%d", f.public.ID)), f.public, f.private},
			{"nonmember", requestScopeValue(t, f.private.UID), db.Project{}, f.private},
		} {
			t.Run(tc.actor+tc.limit, func(t *testing.T) {
				status, _, raw := f.request(t, http.MethodGet, "/api/v1/projects", tc.actor, nil, map[string]string{requestProjectScopeHeader: tc.limit})
				require.Equal(t, http.StatusOK, status, string(raw))
				require.NotContains(t, string(raw), tc.hidden.UID)
				if tc.visible.ID > 0 {
					require.Contains(t, string(raw), tc.visible.UID)
				} else {
					require.NotContains(t, string(raw), f.public.UID)
				}
			})
		}
	})
}

func TestRequestProjectScopeIntersectsStackedLimits(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newProjectAccessFixture(t, store)
		request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, f.server.URL+"/api/v1/projects", nil)
		require.NoError(t, err)
		request.Header.Set("Authorization", "Bearer bootstrap-test-token")
		request.Header.Add(requestProjectScopeHeader, requestScopeValue(t, f.public.UID))
		request.Header.Add(requestProjectScopeHeader, requestScopeValue(t, f.private.UID, f.public.UID))
		response, err := f.server.Client().Do(request)
		require.NoError(t, err)
		defer func() { _ = response.Body.Close() }()
		raw, err := io.ReadAll(response.Body)
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, response.StatusCode, string(raw))
		require.Contains(t, string(raw), f.public.UID)
		require.NotContains(t, string(raw), f.private.UID)
	})
}
