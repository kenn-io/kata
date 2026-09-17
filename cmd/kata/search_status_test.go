package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSearchStatusValidation(t *testing.T) {
	for _, status := range []string{"", " ", "OPEN", "all", "pending"} {
		t.Run(fmt.Sprintf("status=%q", status), func(t *testing.T) {
			_, _, err := executeRootCapture(t, context.Background(), "search", "work", "--status", status)
			var cliErr *cliError
			require.ErrorAs(t, err, &cliErr)
			assert.Equal(t, ExitValidation, cliErr.ExitCode)
			assert.Contains(t, err.Error(), "--status must be open or closed")
		})
	}
}

func TestSearchStatusDaemonCompatibility(t *testing.T) {
	for _, version := range []string{"", "nonsense", "0.18.0", "0.19.0", "0.20.0"} {
		t.Run(version, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v1/health":
					_, _ = fmt.Fprintf(w, `{"ok":true,"api_schema_version":%q}`, version)
				case "/api/v1/projects/resolve":
					_, _ = w.Write([]byte(`{"project":{"id":1,"name":"spoke-project"}}`))
				case "/api/v1/projects/1/search":
					calls.Add(1)
					assert.Equal(t, "open", r.URL.Query().Get("status"))
					_, _ = w.Write([]byte(`{"query":"work","mode":"lexical","results":[]}`))
				default:
					http.NotFound(w, r)
				}
			}))
			t.Cleanup(server.Close)
			_, _, err := executeRootCapture(t, contextWithBaseURL(context.Background(), server.URL), "--project", "spoke-project", "search", "work", "--status", "open")
			if version == "0.20.0" {
				require.NoError(t, err)
				assert.EqualValues(t, 1, calls.Load())
			} else {
				require.Error(t, err)
				assert.Contains(t, err.Error(), "requires daemon API 0.20.0 or newer")
				assert.Zero(t, calls.Load())
			}
		})
	}
}
