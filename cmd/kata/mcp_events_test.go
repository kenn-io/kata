package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	internalclient "go.kenn.io/kata/internal/client"
)

func kataHomeFiles(t *testing.T, home string) []string {
	t.Helper()
	var entries []string
	require.NoError(t, filepath.WalkDir(home, func(path string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		entries = append(entries, path)
		return nil
	}))
	return entries
}

func TestMCPServeForwardsEventsWithoutWritingState(t *testing.T) {
	home := setupKataEnv(t)
	t.Setenv("KATA_AUTHOR", "example-agent")
	var forwarded []string
	daemon := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if serveMCPTestHealth(writer, request) {
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		forwarded = append(forwarded, request.URL.Path)
		switch request.URL.Path {
		case "/api/v1/mcp-events/subscribe":
			_, _ = writer.Write([]byte(`{"id":"sub_fixture","refresh_before":null,"cursor":"c1.x","truncated":false,"delivery_status":{"active":true}}`))
		default:
			http.NotFound(writer, request)
		}
	}))
	t.Cleanup(daemon.Close)
	before := kataHomeFiles(t, home)

	params := map[string]any{
		"_meta":     map[string]any{"io.modelcontextprotocol/protocolVersion": currentMCPProtocolVersion, "io.modelcontextprotocol/clientCapabilities": map[string]any{}},
		"name":      "kata.inbox.changed",
		"arguments": map[string]any{"for": "example-agent/worker"},
		"delivery":  map[string]any{"mode": "webhook", "url": "https://receiver.example/inbox", "secret": "whsec_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="},
	}
	request, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "events/subscribe", "params": params})
	require.NoError(t, err)

	command := newRootCmd()
	inputReader, inputWriter := io.Pipe()
	outputReader, outputWriter := io.Pipe()
	var stderr bytes.Buffer
	command.SetIn(inputReader)
	command.SetOut(outputWriter)
	command.SetErr(&stderr)
	command.SetArgs([]string{"mcp", "serve", "--all"})
	command.SetContext(context.WithValue(t.Context(), internalclient.BaseURLKey{}, daemon.URL))
	done := make(chan error, 1)
	go func() { done <- command.Execute() }()
	_, err = inputWriter.Write(append(request, '\n'))
	require.NoError(t, err)
	line, err := bufio.NewReader(outputReader).ReadBytes('\n')
	require.NoError(t, err)
	require.NoError(t, inputWriter.Close())
	require.NoError(t, <-done)

	var response struct {
		Result struct {
			ID         string `json:"id"`
			ResultType string `json:"resultType"`
		} `json:"result"`
		Error json.RawMessage `json:"error"`
	}
	require.NoError(t, json.Unmarshal(line, &response), string(line))
	require.Empty(t, response.Error, string(line))
	assert.Equal(t, "sub_fixture", response.Result.ID)
	assert.Equal(t, "complete", response.Result.ResultType)
	assert.Contains(t, forwarded, "/api/v1/mcp-events/subscribe")
	assert.Equal(t, before, kataHomeFiles(t, home), "the forwarder keeps no Events state on disk")
}
