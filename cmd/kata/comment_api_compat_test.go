package main

import (
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCommentFeaturesRejectDaemonsWithoutCommentLinks(t *testing.T) {
	t.Setenv("KATA_TEAMMATE", "")
	for _, version := range []string{"0.26.0", "0.27.0"} {
		for _, tc := range []struct {
			name string
			args []string
		}{
			{"reply", []string{"comment", "abc1", "--reply", "c:abcdef", "--body", "Answer"}},
			{"thread", []string{"show", "abc1", "--thread", "c:abcdef"}},
		} {
			t.Run(version+"/"+tc.name, func(t *testing.T) {
				var featureCalls atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					switch r.URL.Path {
					case "/api/v1/health":
						_ = json.MarshalWrite(w, map[string]any{"ok": true, "api_schema_version": version})
					case "/api/v1/projects/resolve":
						_, _ = w.Write([]byte(`{"project":{"id":1,"name":"example-project","uid":"01HAAAAAAAAAAAAAAAAAAAAAAA"}}`))
					case "/api/v1/projects/1/issues/abc1":
						featureCalls.Add(1)
						_, _ = w.Write([]byte(`{"issue":{"uid":"01HBBBBBBBBBBBBBBBBBBBBBBB","short_id":"abc1"},"comments":[]}`))
					case "/api/v1/projects/name:example-project/issues/abc1/comments":
						featureCalls.Add(1)
						http.Error(w, "unknown field reply_to", http.StatusUnprocessableEntity)
					default:
						http.NotFound(w, r)
					}
				}))
				t.Cleanup(server.Close)

				args := append([]string{"--project", "example-project"}, tc.args...)
				_, _, err := executeRootCapture(t, contextWithBaseURL(t.Context(), server.URL), args...)
				var cliErr *cliError
				require.ErrorAs(t, err, &cliErr)
				assert.Equal(t, "daemon_api_too_old", cliErr.Code)
				assert.Zero(t, featureCalls.Load(), "reject unsupported comment requests before sending them")
			})
		}
	}
}
