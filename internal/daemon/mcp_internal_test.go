package daemon

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
)

type testMCPService struct{}

func (testMCPService) Available(context.Context) (bool, error) { return true, nil }
func (testMCPService) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p, ok := PrincipalFromContext(r.Context())
	if ok && p.Kind == PrincipalStaticToken {
		w.WriteHeader(204)
		return
	}
	w.WriteHeader(500)
}

func TestMCPListenerAndBearerBoundary(t *testing.T) {
	s := NewServer(ServerConfig{Auth: config.AuthConfig{Token: "daemon-token"}, MCP: testMCPService{}})
	t.Cleanup(func() { _ = s.Close() })
	for _, kind := range []ListenerKind{ListenerSocket, ListenerBrowser, ListenerFederation, ListenerSharedTCP} {
		handler, err := s.HandlerFor(ListenerPolicy{Kind: kind, Origin: "https://daemon.example"})
		require.NoError(t, err)
		for _, token := range []string{"", "wrong", "daemon-token"} {
			r := httptest.NewRequest("POST", "https://daemon.example/mcp", nil)
			r.Header.Set("Content-Type", "application/json")
			if token != "" {
				r.Header.Set("Authorization", "Bearer "+token)
			}
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			want := 404
			if kind == ListenerSharedTCP {
				want = 204
				if token == "" {
					want = 401
				}
				if token == "wrong" {
					want = 403
				}
			}
			require.Equal(t, want, w.Code, "%s %s: %s", kind, token, w.Body.String())
			if kind == ListenerSharedTCP {
				require.Contains(t, w.Header().Get("Cache-Control"), "no-store")
			}
		}
	}
}
