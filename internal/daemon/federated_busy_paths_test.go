package daemon_test

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
)

func TestFederatedDocbankBusy(t *testing.T) {
	hub, spoke, project, replica, issue, token := createClaimForwardingPair(t, "claim,push,pull")
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		busy := strings.Contains(r.URL.Path, "/federation/docbank/")
		if busy {
			w.Header().Set("Retry-After", "2")
			w.WriteHeader(429)
			_, _ = w.Write([]byte("Rate exceeded."))
			return
		}
		req := r.Clone(r.Context())
		req.RequestURI = ""
		req.URL.Scheme = "http"
		req.URL.Host = hub.URL[len("http://"):]
		req.Host = req.URL.Host
		resp, err := hub.HTTP.Do(req) //nolint:gosec // loopback fixture
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		defer func() { _ = resp.Body.Close() }()
		for k, values := range resp.Header {
			for _, value := range values {
				w.Header().Add(k, value)
			}
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}))
	t.Cleanup(proxy.Close)
	setClaimBindingHubURL(t, spoke, replica.ID, proxy.URL)
	require.NoError(t, config.WriteFederationCredential(replica.UID, config.FederationCredential{HubURL: proxy.URL, HubProjectID: project.ID, Token: token, Actor: "tester", Capabilities: "claim,push,pull"}))
	path := fmt.Sprintf("/api/v1/projects/%d/issues/%s/attachments/links", replica.ID, issue.ShortID)
	resp, raw := envDoRaw(t, spoke, http.MethodPost, path, map[string]string{"path": "/example.txt", "actor": "tester"}, nil)
	assertAPIError(t, resp.StatusCode, raw, http.StatusTooManyRequests, "server_busy")
}
