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
	mcpserver "go.kenn.io/kata/internal/mcp"
)

func TestMCPServeBindsProjectAndUsesStdoutOnlyForProtocol(t *testing.T) {
	setupKataEnv(t)
	t.Setenv("KATA_AUTHOR", "example-agent")
	workspace := t.TempDir()
	var resolved bool
	daemon := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		require.Equal(t, "/api/v1/projects/resolve", request.URL.Path)
		resolved = true
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"project":{"id":42,"name":"spoke-project"}}`))
	}))
	t.Cleanup(daemon.Close)

	request := map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "server/discover",
		"params": map[string]any{
			"_meta": map[string]any{
				"io.modelcontextprotocol/protocolVersion":    mcpserver.ProtocolVersion,
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
	require.Empty(t, stderr.String())
	require.Empty(t, rest, "stdout must contain protocol messages only")
	var response struct {
		Result struct {
			SupportedVersions []string `json:"supportedVersions"`
		} `json:"result"`
	}
	require.NoError(t, json.Unmarshal(protocolLine, &response))
	require.Equal(t, []string{mcpserver.ProtocolVersion}, response.Result.SupportedVersions)
}
