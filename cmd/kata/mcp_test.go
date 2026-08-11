package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	internalclient "go.kenn.io/kata/internal/client"
	"go.kenn.io/kata/internal/version"
)

func TestMCPServeProjectModesRejectFlagConflicts(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{name: "allowlist with project", args: []string{"--project", "spoke-project", "mcp", "serve", "--projects", "hub-project"}},
		{name: "allowlist with workspace", args: []string{"--workspace", t.TempDir(), "mcp", "serve", "--projects", "spoke-project"}},
		{name: "token administration with project", args: []string{"--project", "spoke-project", "mcp", "serve", "--enable-token-admin"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			command := newRootCmd()
			command.SetArgs(tt.args)
			err := command.Execute()
			require.ErrorContains(t, err, "cannot be combined")
		})
	}
}

func TestMCPServeDefaultsToDaemonWideScope(t *testing.T) {
	setupKataEnv(t)
	t.Setenv("KATA_AUTHOR", "example-agent")
	var requests int
	daemon := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		requests++
		http.Error(writer, "unexpected project resolution", http.StatusInternalServerError)
	}))
	t.Cleanup(daemon.Close)

	command := newRootCmd()
	command.SetIn(bytes.NewReader(nil))
	command.SetOut(io.Discard)
	command.SetErr(io.Discard)
	command.SetArgs([]string{"mcp", "serve"})
	command.SetContext(context.WithValue(t.Context(), internalclient.BaseURLKey{}, daemon.URL))

	require.NoError(t, command.Execute())
	require.Zero(t, requests, "daemon-wide startup must not resolve the current workspace")
}

func TestMCPServeDoesNotExposeAllProjectsFlag(t *testing.T) {
	command := newMCPServeCmd()
	require.Nil(t, command.Flags().Lookup("all-projects"))
}

func TestParseMCPStorageTargets(t *testing.T) {
	targets, err := parseMCPStorageTargets([]string{"restore=restore.db", "archive=postgres://db.example/kata"})
	require.NoError(t, err)
	require.Equal(t, "restore.db", targets["restore"])
	require.Equal(t, "postgres://db.example/kata", targets["archive"])

	_, err = parseMCPStorageTargets([]string{"restore=one.db", "restore=two.db"})
	require.ErrorContains(t, err, "duplicate")
	_, err = parseMCPStorageTargets([]string{"missing-separator"})
	require.Error(t, err)
}

const currentMCPProtocolVersion = "2026-07-28"

func TestMCPServeBindsProjectAndUsesStdoutOnlyForProtocol(t *testing.T) {
	setupKataEnv(t)
	t.Setenv("KATA_AUTHOR", "example-agent")
	workspace := t.TempDir()
	var resolved bool
	var cataloged bool
	daemon := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/api/v1/projects/resolve":
			resolved = true
			_, _ = writer.Write([]byte(`{"project":{"id":42,"name":"spoke-project"}}`))
		case "/api/v1/projects":
			cataloged = true
			_, _ = writer.Write([]byte(`{"projects":[{"id":42,"uid":"01HAAAAAAAAAAAAAAAAAAAAAAA","name":"spoke-project","metadata":{},"revision":1,"created_at":"2026-08-11T00:00:00Z"}]}`))
		default:
			http.NotFound(writer, request)
		}
	}))
	t.Cleanup(daemon.Close)

	request := map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "server/discover",
		"params": map[string]any{
			"_meta": map[string]any{
				"io.modelcontextprotocol/protocolVersion":    currentMCPProtocolVersion,
				"io.modelcontextprotocol/clientCapabilities": map[string]any{},
			},
		},
	}
	requestBytes, err := json.Marshal(request)
	require.NoError(t, err)

	command := newRootCmd()
	inputReader, inputWriter := io.Pipe()
	outputReader, outputWriter := io.Pipe()
	var stderr bytes.Buffer
	command.SetIn(inputReader)
	command.SetOut(outputWriter)
	command.SetErr(&stderr)
	command.SetArgs([]string{"--workspace", workspace, "mcp", "serve"})
	command.SetContext(context.WithValue(t.Context(), internalclient.BaseURLKey{}, daemon.URL))

	done := make(chan error, 1)
	go func() { done <- command.Execute() }()
	_, err = inputWriter.Write(append(requestBytes, '\n'))
	require.NoError(t, err)
	protocolLine, err := bufio.NewReader(outputReader).ReadBytes('\n')
	require.NoError(t, err)
	require.NoError(t, inputWriter.Close())
	require.NoError(t, <-done)
	rest, err := io.ReadAll(outputReader)
	require.NoError(t, err)
	require.True(t, resolved)
	require.True(t, cataloged)
	require.Empty(t, stderr.String())
	require.Empty(t, rest, "stdout must contain protocol messages only")
	var response struct {
		Result struct {
			SupportedVersions []string `json:"supportedVersions"`
			Meta              struct {
				ServerInfo struct {
					Name    string `json:"name"`
					Version string `json:"version"`
				} `json:"io.modelcontextprotocol/serverInfo"`
			} `json:"_meta"`
		} `json:"result"`
	}
	require.NoError(t, json.Unmarshal(protocolLine, &response))
	require.NotEmpty(t, response.Result.SupportedVersions)
	require.Contains(t, response.Result.SupportedVersions, currentMCPProtocolVersion)
	require.Equal(t, "kata", response.Result.Meta.ServerInfo.Name)
	require.Equal(t, version.Version, response.Result.Meta.ServerInfo.Version)
}
