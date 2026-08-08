package mcpserver

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"

	kataclient "go.kenn.io/kata/pkg/client"
)

func TestWireDiscoveryPublishesNegotiatedToolsCapability(t *testing.T) {
	connection, reader := rawServerConnection(t)
	writeWireRequest(t, connection, 1, "server/discover", map[string]any{"_meta": currentMeta()})

	response := readWireMap(t, reader)
	require.EqualValues(t, 1, response["id"])
	require.NotContains(t, response, "error")
	result, ok := response["result"].(map[string]any)
	require.True(t, ok)
	supportedVersions := result["supportedVersions"].([]any)
	require.Contains(t, supportedVersions, currentProtocolVersion)
	require.Contains(t, supportedVersions, legacyProtocolVersion)
	require.Equal(t, "private", result["cacheScope"])
	require.EqualValues(t, 5*time.Minute.Milliseconds(), result["ttlMs"])
	require.NotEmpty(t, result["resultType"])
	capabilities := result["capabilities"].(map[string]any)
	require.Equal(t, map[string]any{"tools": map[string]any{}}, capabilities)
	meta := result["_meta"].(map[string]any)
	serverInfo := meta["io.modelcontextprotocol/serverInfo"].(map[string]any)
	require.Equal(t, "kata", serverInfo["name"])
	require.Equal(t, "test-version", serverInfo["version"])
}

func TestWireNegotiatesLegacyInitialization(t *testing.T) {
	connection, reader := rawServerConnection(t)
	writeWireRequest(t, connection, 1, "initialize", map[string]any{
		"protocolVersion": legacyProtocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "legacy-client", "version": "1"},
	})

	response := readWireMap(t, reader)
	require.EqualValues(t, 1, response["id"])
	require.NotContains(t, response, "error")
	result, ok := response["result"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, legacyProtocolVersion, result["protocolVersion"])
	serverInfo := result["serverInfo"].(map[string]any)
	require.Equal(t, "kata", serverInfo["name"])
	require.Equal(t, "test-version", serverInfo["version"])

	writeWireNotification(t, connection, "notifications/initialized", map[string]any{})
	writeWireRequest(t, connection, 2, "tools/list", map[string]any{})
	toolsResponse := readWireMap(t, reader)
	require.EqualValues(t, 2, toolsResponse["id"])
	toolsResult := toolsResponse["result"].(map[string]any)
	require.NotEmpty(t, toolsResult["tools"])
}

func TestWireRejectsUnsupportedProtocolVersion(t *testing.T) {
	connection, reader := rawServerConnection(t)
	writeWireRequest(t, connection, 1, "server/discover", map[string]any{
		"_meta": map[string]any{
			"io.modelcontextprotocol/protocolVersion":    "2027-01-01",
			"io.modelcontextprotocol/clientCapabilities": map[string]any{},
		},
	})

	response := readWireResponse(t, reader)
	require.NotNil(t, response.Error)
	require.Equal(t, int64(sdkmcp.CodeUnsupportedProtocolVersion), response.Error.Code)
	var data map[string]any
	require.NoError(t, json.Unmarshal(response.Error.Data, &data))
	require.Equal(t, "2027-01-01", data["requested"])
	supported := data["supported"].([]any)
	require.Contains(t, supported, currentProtocolVersion)
	require.Contains(t, supported, legacyProtocolVersion)
}

func TestCurrentProtocolStillRequiresClientCapabilities(t *testing.T) {
	connection, reader := rawServerConnection(t)
	writeWireRequest(t, connection, 1, "server/discover", map[string]any{
		"_meta": map[string]any{
			"io.modelcontextprotocol/protocolVersion": currentProtocolVersion,
		},
	})

	response := readWireResponse(t, reader)
	require.NotNil(t, response.Error)
	require.Equal(t, int64(jsonrpc.CodeInvalidParams), response.Error.Code)
}

func TestInitializeNegotiatesHandshakeCompatibleVersion(t *testing.T) {
	connection, reader := rawServerConnection(t)
	writeWireRequest(t, connection, 1, "initialize", map[string]any{
		"protocolVersion": currentProtocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "test-client", "version": "1"},
	})

	response := readWireMap(t, reader)
	require.NotContains(t, response, "error")
	result := response["result"].(map[string]any)
	require.Equal(t, legacyProtocolVersion, result["protocolVersion"])
}

func TestWireCancellationSuppressesLateToolResponse(t *testing.T) {
	started := make(chan struct{})
	daemon := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		close(started)
		<-request.Context().Done()
	}))
	t.Cleanup(daemon.Close)
	apiClient, err := kataclient.NewWithHTTPClient(daemon.URL, daemon.Client())
	require.NoError(t, err)
	connection, reader := rawServerConnectionWithClient(t, apiClient)

	writeWireRequest(t, connection, 1, "tools/call", map[string]any{
		"name": "kata.search", "arguments": map[string]any{"query": "slow"}, "_meta": currentMeta(),
	})
	<-started
	writeWireNotification(t, connection, "notifications/cancelled", map[string]any{"requestId": 1})
	writeWireRequest(t, connection, 2, "server/discover", map[string]any{"_meta": currentMeta()})

	response := readWireResponse(t, reader)
	require.Nil(t, response.Error)
	require.EqualValues(t, 2, response.ID)
	require.NoError(t, connection.SetReadDeadline(time.Now().Add(100*time.Millisecond)))
	_, err = reader.ReadBytes('\n')
	var netError net.Error
	require.True(t, errors.As(err, &netError) && netError.Timeout(), err)
}

func TestWireInvalidMessagesDoNotPoisonFollowingDiscovery(t *testing.T) {
	tests := []struct {
		name     string
		line     string
		wantCode int64
	}{
		{name: "parse error", line: "{\n", wantCode: jsonrpc.CodeParseError},
		{name: "invalid request", line: "{}\n", wantCode: jsonrpc.CodeInvalidRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			connection, reader := rawServerConnection(t)
			_, err := connection.Write([]byte(tt.line))
			require.NoError(t, err)
			response := readWireResponse(t, reader)
			require.Equal(t, tt.wantCode, response.Error.Code)
			writeWireRequest(t, connection, 2, "server/discover", map[string]any{"_meta": currentMeta()})
			require.EqualValues(t, 2, readWireResponse(t, reader).ID)
		})
	}
}

func TestWireDropsForbiddenClientResponse(t *testing.T) {
	connection, reader := rawServerConnection(t)
	_, err := connection.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}` + "\n"))
	require.NoError(t, err)
	writeWireRequest(t, connection, 2, "server/discover", map[string]any{"_meta": currentMeta()})

	response := readWireResponse(t, reader)
	require.EqualValues(t, 2, response.ID)
}

func rawServerConnection(t *testing.T) (net.Conn, *bufio.Reader) {
	t.Helper()
	return rawServerConnectionWithClient(t, &kataclient.Client{})
}

func rawServerConnectionWithClient(t *testing.T, client *kataclient.Client) (net.Conn, *bufio.Reader) {
	t.Helper()
	server, err := New(Options{
		Client:      client,
		ProjectID:   42,
		ProjectName: "spoke-project",
		Actor:       "example-agent",
		Version:     "test-version",
	})
	require.NoError(t, err)
	serverConn, clientConn := net.Pipe()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- server.Run(ctx, NewStdioTransport(serverConn, serverConn)) }()
	require.NoError(t, clientConn.SetDeadline(time.Now().Add(2*time.Second)))
	t.Cleanup(func() {
		_ = clientConn.Close()
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("raw MCP server did not stop")
		}
	})
	return clientConn, bufio.NewReader(clientConn)
}

func writeWireNotification(t *testing.T, connection net.Conn, method string, params any) {
	t.Helper()
	data, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
	require.NoError(t, err)
	_, err = connection.Write(append(data, '\n'))
	require.NoError(t, err)
}

func writeWireRequest(t *testing.T, connection net.Conn, id int, method string, params any) {
	t.Helper()
	data, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": id, "method": method, "params": params,
	})
	require.NoError(t, err)
	_, err = connection.Write(append(data, '\n'))
	require.NoError(t, err)
}

func readWireResponse(t *testing.T, reader *bufio.Reader) wireResponse {
	t.Helper()
	line, err := reader.ReadBytes('\n')
	require.NoError(t, err)
	var response wireResponse
	require.NoError(t, json.Unmarshal(line, &response))
	return response
}

func readWireMap(t *testing.T, reader *bufio.Reader) map[string]any {
	t.Helper()
	line, err := reader.ReadBytes('\n')
	require.NoError(t, err)
	var response map[string]any
	require.NoError(t, json.Unmarshal(line, &response))
	return response
}
