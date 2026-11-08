package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/testenv"
)

// New daemons must receive the same CLI holder tuple used by lease acquire;
// legacy local daemons must not receive a new, unsupported JSON member.
func TestDocbankCLIForwardedLeaseKind(t *testing.T) {
	for _, version := range []string{"0.41.0", "0.42.0"} {
		for _, operation := range []string{"link", "close"} {
			t.Run(version+"/"+operation, func(t *testing.T) {
				env, vault, ref := attachmentCLIEnv(t, testenv.WithAuthToken("fixture-proxy-token"))
				seedCLILink(vault)
				target, err := url.Parse(env.URL)
				require.NoError(t, err)
				proxy := httputil.NewSingleHostReverseProxy(target)
				var bodies []map[string]any
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/api/v1/health" {
						_, _ = io.WriteString(w, `{"ok":true,"api_schema_version":"`+version+`"}`)
						return
					}
					if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/attachments/links") {
						data, readErr := io.ReadAll(io.LimitReader(r.Body, 64<<10))
						if readErr != nil {
							http.Error(w, "fixture read", 500)
							return
						}
						var body map[string]any
						if json.Unmarshal(data, &body) != nil {
							http.Error(w, "fixture JSON", 500)
							return
						}
						bodies = append(bodies, body)
						r.Body = io.NopCloser(bytes.NewReader(data))
					}
					r.Host = target.Host
					proxy.ServeHTTP(w, r)
				}))
				t.Cleanup(server.Close)
				if operation == "link" {
					_, err = executeAttachmentCLI(t, server.URL, "link", ref, "42", "--json")
				} else {
					_, err = executeCloseAttachmentCLI(t, server.URL, "close", ref, "--done", "-m", attachmentCloseMessage, "--docbank", "42", "--json")
				}
				require.NoError(t, err)
				require.Len(t, bodies, 1)
				if version == "0.42.0" {
					require.Equal(t, "cli", bodies[0]["client_kind"])
				} else {
					require.NotContains(t, bodies[0], "client_kind")
				}
			})
		}
	}
}
