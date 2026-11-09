package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	internalclient "go.kenn.io/kata/internal/client"
)

func TestMCPServeRequiresTransactionalScopeSupport(t *testing.T) {
	for _, test := range []struct {
		name    string
		version string
		args    []string
		reject  bool
	}{
		{name: "bound old daemon", version: "0.34.0", reject: true},
		{name: "explicit project old daemon", version: "0.33.0", args: []string{"--project", "spoke-project"}, reject: true},
		{name: "allowlist old daemon", version: "0.33.0", args: []string{"--projects", "spoke-project"}, reject: true},
		{name: "bound current daemon", version: "0.35.0"},
		{name: "allowlist current daemon", version: "0.35.0", args: []string{"--projects", "spoke-project"}},
		{name: "all projects retains minimum", version: "0.11.0", args: []string{"--all"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			setupKataEnv(t)
			t.Setenv("KATA_AUTHOR", "example-agent")
			t.Chdir(t.TempDir())
			var otherRequests atomic.Int64
			daemon := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				writer.Header().Set("Content-Type", "application/json")
				if request.URL.Path == "/api/v1/health" {
					_, _ = fmt.Fprintf(writer, `{"ok":true,"api_schema_version":%q}`, test.version)
					return
				}
				otherRequests.Add(1)
				switch request.URL.Path {
				case "/api/v1/projects/resolve":
					_, _ = writer.Write([]byte(`{"project":{"id":42,"name":"spoke-project"}}`))
				case "/api/v1/projects":
					_, _ = writer.Write([]byte(`{"projects":[{"id":42,"uid":"01HAAAAAAAAAAAAAAAAAAAAAAA","name":"spoke-project","metadata":{},"revision":1,"created_at":"2026-08-11T00:00:00Z"}]}`))
				default:
					http.NotFound(writer, request)
				}
			}))
			t.Cleanup(daemon.Close)

			command := newRootCmd()
			command.SetIn(bytes.NewReader(nil))
			command.SetOut(io.Discard)
			command.SetErr(io.Discard)
			command.SetArgs(append([]string{"mcp", "serve"}, test.args...))
			command.SetContext(context.WithValue(t.Context(), internalclient.BaseURLKey{}, daemon.URL))

			err := command.Execute()
			if test.reject {
				require.ErrorContains(t, err, "requires daemon API 0.35.0 or newer")
				require.ErrorContains(t, err, "reports "+test.version)
				require.Zero(t, otherRequests.Load(), "reject before project resolution or MCP tool requests can reach an older daemon")
			} else {
				require.NoError(t, err)
			}
		})
	}
}
