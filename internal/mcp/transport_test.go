package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
)

func TestStdioTransportSupportsOnlyCurrentProtocol(t *testing.T) {
	transport := NewStdioTransport(io.NopCloser(strings.NewReader("")), io.Discard)

	require.True(t, transport.SupportsProtocolVersion(ProtocolVersion))
	require.False(t, transport.SupportsProtocolVersion("2025-11-25"))
	require.False(t, transport.SupportsProtocolVersion("2027-01-01"))
}

func TestStdioConnectionAdmitsCurrentRequests(t *testing.T) {
	input := requestLine(t, 1, "tools/list", map[string]any{
		"_meta": currentMeta(),
	})
	var output bytes.Buffer
	conn := connectStdio(t, input, &output)

	message, err := conn.Read(t.Context())
	require.NoError(t, err)
	request, ok := message.(*jsonrpc.Request)
	require.True(t, ok)
	require.Equal(t, "tools/list", request.Method)
	require.Empty(t, output.String())
}

func TestStdioConnectionAcceptsWindowsLineEndings(t *testing.T) {
	input := strings.ReplaceAll(requestLine(t, 1, "tools/list", map[string]any{
		"_meta": currentMeta(),
	}), "\n", "\r\n")
	conn := connectStdio(t, input, io.Discard)

	message, err := conn.Read(t.Context())
	require.NoError(t, err)
	require.Equal(t, "tools/list", message.(*jsonrpc.Request).Method)
}

func TestStdioConnectionBoundsInputMessages(t *testing.T) {
	transport := NewStdioTransport(io.NopCloser(strings.NewReader(strings.Repeat("x", 256)+"\n")), io.Discard)
	transport.maxMessageBytes = 64
	conn, err := transport.Connect(t.Context())
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	_, err = conn.Read(t.Context())
	require.ErrorContains(t, err, "token too long")
}

func TestStdioConnectionForwardsCancellationWithoutRequestMetadata(t *testing.T) {
	input := requestLine(t, nil, "notifications/cancelled", map[string]any{"requestId": 7})
	conn := connectStdio(t, input, io.Discard)

	message, err := conn.Read(t.Context())
	require.NoError(t, err)
	request := message.(*jsonrpc.Request)
	require.Equal(t, "notifications/cancelled", request.Method)
	require.False(t, request.IsCall())
}

func TestStdioConnectionRejectsUnsupportedAndInvalidRequests(t *testing.T) {
	tests := []struct {
		name          string
		line          string
		wantCode      int64
		wantRequested string
	}{
		{
			name: "legacy initialize",
			line: requestLine(t, 1, "initialize", map[string]any{
				"protocolVersion": "2025-11-25",
				"capabilities":    map[string]any{},
				"clientInfo":      map[string]any{"name": "legacy-client", "version": "1"},
			}),
			wantCode:      sdkmcp.CodeUnsupportedProtocolVersion,
			wantRequested: "2025-11-25",
		},
		{
			name: "handshake removed at current version",
			line: requestLine(t, 6, "initialize", map[string]any{
				"protocolVersion": ProtocolVersion,
				"capabilities":    map[string]any{},
				"clientInfo":      map[string]any{"name": "legacy-client", "version": "1"},
			}),
			wantCode: jsonrpc.CodeMethodNotFound,
		},
		{
			name: "future version",
			line: requestLine(t, 2, "tools/list", map[string]any{
				"_meta": map[string]any{
					"io.modelcontextprotocol/protocolVersion":    "2027-01-01",
					"io.modelcontextprotocol/clientCapabilities": map[string]any{},
				},
			}),
			wantCode:      sdkmcp.CodeUnsupportedProtocolVersion,
			wantRequested: "2027-01-01",
		},
		{
			name:     "missing metadata",
			line:     requestLine(t, 3, "tools/list", map[string]any{}),
			wantCode: jsonrpc.CodeInvalidParams,
		},
		{
			name: "malformed version",
			line: requestLine(t, 4, "tools/list", map[string]any{
				"_meta": map[string]any{
					"io.modelcontextprotocol/protocolVersion":    20260728,
					"io.modelcontextprotocol/clientCapabilities": map[string]any{},
				},
			}),
			wantCode: jsonrpc.CodeInvalidParams,
		},
		{
			name:     "batch",
			line:     `[{"jsonrpc":"2.0","id":5,"method":"tools/list","params":{}}]` + "\n",
			wantCode: jsonrpc.CodeInvalidRequest,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var output bytes.Buffer
			conn := connectStdio(t, tt.line, &output)

			_, err := conn.Read(t.Context())
			require.ErrorIs(t, err, io.EOF)

			response := decodeResponse(t, output.String())
			wireErr := response.Error
			require.NotNil(t, wireErr)
			require.Equal(t, tt.wantCode, wireErr.Code)
			if tt.name == "batch" {
				var envelope map[string]any
				require.NoError(t, json.Unmarshal([]byte(strings.TrimSpace(output.String())), &envelope))
				id, present := envelope["id"]
				require.True(t, present, "JSON-RPC errors without a request ID must carry id:null")
				require.Nil(t, id)
			}
			if tt.wantRequested == "" {
				return
			}
			var data struct {
				Supported []string `json:"supported"`
				Requested string   `json:"requested"`
			}
			require.NoError(t, json.Unmarshal(wireErr.Data, &data))
			require.Equal(t, []string{ProtocolVersion}, data.Supported)
			require.Equal(t, tt.wantRequested, data.Requested)
		})
	}
}

func TestStdioConnectionDropsLegacyInitializedNotification(t *testing.T) {
	input := requestLine(t, nil, "notifications/initialized", map[string]any{})
	var output bytes.Buffer
	conn := connectStdio(t, input, &output)

	_, err := conn.Read(t.Context())
	require.ErrorIs(t, err, io.EOF)
	require.Empty(t, output.String())
}

func TestStdioConnectionRejectsInitializedCall(t *testing.T) {
	input := requestLine(t, 9, "notifications/initialized", map[string]any{})
	var output bytes.Buffer
	conn := connectStdio(t, input, &output)

	_, err := conn.Read(t.Context())
	require.ErrorIs(t, err, io.EOF)
	require.Equal(t, int64(jsonrpc.CodeMethodNotFound), decodeResponse(t, output.String()).Error.Code)
}

func TestStdioConnectionDropsCurrentInitializeNotification(t *testing.T) {
	input := requestLine(t, nil, "initialize", map[string]any{
		"protocolVersion": ProtocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "legacy-client", "version": "1"},
	})
	var output bytes.Buffer
	conn := connectStdio(t, input, &output)

	_, err := conn.Read(t.Context())
	require.ErrorIs(t, err, io.EOF)
	require.Empty(t, output.String())
}

func TestStdioConnectionRejectsCancellationCall(t *testing.T) {
	input := requestLine(t, 10, "notifications/cancelled", map[string]any{"requestId": 1})
	var output bytes.Buffer
	conn := connectStdio(t, input, &output)

	_, err := conn.Read(t.Context())
	require.ErrorIs(t, err, io.EOF)
	require.Equal(t, int64(jsonrpc.CodeInvalidRequest), decodeResponse(t, output.String()).Error.Code)
}

func TestStdioConnectionDistinguishesParseAndInvalidRequest(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		wantCode int64
	}{
		{name: "invalid JSON", input: "{\n", wantCode: jsonrpc.CodeParseError},
		{name: "malformed array", input: "[\n", wantCode: jsonrpc.CodeParseError},
		{name: "invalid JSON-RPC envelope", input: "{}\n", wantCode: jsonrpc.CodeInvalidRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var output bytes.Buffer
			conn := connectStdio(t, tt.input, &output)
			_, err := conn.Read(t.Context())
			require.ErrorIs(t, err, io.EOF)
			require.Equal(t, tt.wantCode, decodeResponse(t, output.String()).Error.Code)
		})
	}
}

func TestStdioConnectionDropsForbiddenClientResponses(t *testing.T) {
	var output bytes.Buffer
	conn := connectStdio(t, `{"jsonrpc":"2.0","id":1,"result":{}}`+"\n", &output)

	_, err := conn.Read(t.Context())
	require.ErrorIs(t, err, io.EOF)
	require.Empty(t, output.String())
}

func TestStdioConnectionSuppressesCancelledRequestResponse(t *testing.T) {
	input := requestLine(t, 1, "tools/list", map[string]any{"_meta": currentMeta()}) +
		requestLine(t, nil, "notifications/cancelled", map[string]any{"requestId": 1})
	var output bytes.Buffer
	conn := connectStdio(t, input, &output)

	_, err := conn.Read(t.Context())
	require.NoError(t, err)
	message, err := conn.Read(t.Context())
	require.NoError(t, err)
	require.Equal(t, "notifications/cancelled", message.(*jsonrpc.Request).Method)
	require.NoError(t, conn.Write(t.Context(), testResponse(t, 1, map[string]any{"late": true})))
	require.Empty(t, output.String())
}

func TestStdioConnectionDoesNotSuppressResponseForMalformedCancellation(t *testing.T) {
	input := requestLine(t, 1, "tools/list", map[string]any{"_meta": currentMeta()}) +
		requestLine(t, nil, "notifications/cancelled", map[string]any{"requestId": 1, "reason": 7})
	var output bytes.Buffer
	conn := connectStdio(t, input, &output)

	_, err := conn.Read(t.Context())
	require.NoError(t, err)
	_, err = conn.Read(t.Context())
	require.NoError(t, err)
	require.NoError(t, conn.Write(t.Context(), testResponse(t, 1, map[string]any{"ok": true})))
	require.NotEmpty(t, output.String())
}

func TestStdioConnectionChecksCancellationAfterWaitingForWriter(t *testing.T) {
	input := requestLine(t, 1, "tools/list", map[string]any{"_meta": currentMeta()}) +
		requestLine(t, nil, "notifications/cancelled", map[string]any{"requestId": 1})
	var output bytes.Buffer
	conn := connectStdio(t, input, &output)
	impl := conn.(*stdioConnection)

	_, err := conn.Read(t.Context())
	require.NoError(t, err)
	impl.writeMu.Lock()
	writeDone := make(chan error, 1)
	go func() {
		writeDone <- conn.Write(t.Context(), testResponse(t, 1, map[string]any{"late": true}))
	}()
	require.Eventually(t, func() bool {
		return impl.writeWaiting.Load() > 0
	}, time.Second, time.Millisecond)
	_, err = conn.Read(t.Context())
	require.NoError(t, err)
	impl.writeMu.Unlock()
	require.NoError(t, <-writeDone)
	require.Empty(t, output.String())
}

func TestStdioConnectionIgnoresUnsolicitedCancellationID(t *testing.T) {
	input := requestLine(t, nil, "notifications/cancelled", map[string]any{"requestId": 1})
	var output bytes.Buffer
	conn := connectStdio(t, input, &output)

	_, err := conn.Read(t.Context())
	require.NoError(t, err)
	require.NoError(t, conn.Write(t.Context(), testResponse(t, 1, map[string]any{"ok": true})))
	require.NotEmpty(t, output.String())
}

func TestStdioConnectionWritesCompactNewlineDelimitedJSON(t *testing.T) {
	var output bytes.Buffer
	conn := connectStdio(t, "", &output)
	response := testResponse(t, 1, map[string]any{"ok": true})

	require.NoError(t, conn.Write(t.Context(), response))
	require.Equal(t, 1, strings.Count(output.String(), "\n"))
	require.True(t, strings.HasSuffix(output.String(), "\n"))
	require.NotContains(t, strings.TrimSuffix(output.String(), "\n"), "\n")
}

func TestStdioConnectionReplacesOversizeResponseWithCorrelatedError(t *testing.T) {
	const maximum = 256
	var output bytes.Buffer
	transport := NewStdioTransport(io.NopCloser(strings.NewReader("")), &output)
	transport.maxMessageBytes = maximum
	conn, err := transport.Connect(t.Context())
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	require.NoError(t, conn.Write(t.Context(), testResponse(t, 17, map[string]any{
		"body": strings.Repeat("x", maximum),
	})))

	response := decodeResponse(t, output.String())
	require.Equal(t, float64(17), response.ID)
	require.NotNil(t, response.Error)
	require.Equal(t, int64(jsonrpc.CodeInternalError), response.Error.Code)
	require.Equal(t, "response exceeds maximum message size", response.Error.Message)
	require.LessOrEqual(t, len(strings.TrimSuffix(output.String(), "\n")), maximum)
}

func TestStdioConnectionReportsShortWrites(t *testing.T) {
	conn := connectStdio(t, "", shortWriter{})
	response := testResponse(t, 1, map[string]any{"ok": true})

	err := conn.Write(t.Context(), response)
	require.ErrorIs(t, err, io.ErrShortWrite)
}

func TestStdioConnectionRejectsServerCalls(t *testing.T) {
	conn := connectStdio(t, "", io.Discard)
	id, err := jsonrpc.MakeID(float64(1))
	require.NoError(t, err)
	params, err := json.Marshal(map[string]any{})
	require.NoError(t, err)
	request := &jsonrpc.Request{ID: id, Method: "ping", Params: params}

	err = conn.Write(t.Context(), request)
	require.ErrorContains(t, err, "server requests")
}

func TestStdioConnectionCloseUnblocksRead(t *testing.T) {
	reader, writer := io.Pipe()
	conn := connectStdioReader(t, reader, io.Discard)
	done := make(chan error, 1)
	go func() {
		_, err := conn.Read(context.Background())
		done <- err
	}()

	require.NoError(t, conn.Close())
	require.NoError(t, writer.Close())
	select {
	case err := <-done:
		require.True(t, errors.Is(err, sdkmcp.ErrConnectionClosed) || errors.Is(err, io.ErrClosedPipe), err)
	case <-time.After(time.Second):
		t.Fatal("Read did not unblock after Close")
	}
}

func TestStdioConnectionCloseUnblocksWrite(t *testing.T) {
	writer := newBlockingWriter()
	conn := connectStdio(t, "", writer)
	done := make(chan error, 1)
	go func() {
		done <- conn.Write(context.Background(), testResponse(t, 1, map[string]any{"ok": true}))
	}()
	<-writer.started

	require.NoError(t, conn.Close())
	select {
	case err := <-done:
		require.Error(t, err)
	case <-time.After(time.Second):
		t.Fatal("Write did not unblock after Close")
	}
	require.True(t, writer.closed.Load())
}

func TestStdioConnectionSerializesConcurrentWrites(t *testing.T) {
	var output lockedBuffer
	conn := connectStdio(t, "", &output)
	const count = 32
	var group sync.WaitGroup
	for i := range count {
		group.Add(1)
		go func() {
			defer group.Done()
			response := testResponse(t, i, map[string]any{"ok": true})
			require.NoError(t, conn.Write(t.Context(), response))
		}()
	}
	group.Wait()

	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	require.Len(t, lines, count)
	for _, line := range lines {
		_ = decodeResponse(t, line)
	}
}

func connectStdio(t *testing.T, input string, output io.Writer) sdkmcp.Connection {
	t.Helper()
	return connectStdioReader(t, io.NopCloser(strings.NewReader(input)), output)
}

func connectStdioReader(t *testing.T, input io.ReadCloser, output io.Writer) sdkmcp.Connection {
	t.Helper()
	transport := NewStdioTransport(input, output)
	conn, err := transport.Connect(t.Context())
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func requestLine(t *testing.T, id any, method string, params any) string {
	t.Helper()
	request := map[string]any{"jsonrpc": "2.0", "method": method, "params": params}
	if id != nil {
		request["id"] = id
	}
	data, err := json.Marshal(request)
	require.NoError(t, err)
	return string(data) + "\n"
}

func currentMeta() map[string]any {
	return map[string]any{
		"io.modelcontextprotocol/protocolVersion":    ProtocolVersion,
		"io.modelcontextprotocol/clientCapabilities": map[string]any{},
	}
}

type wireResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      any             `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *jsonrpc.Error  `json:"error,omitempty"`
}

func decodeResponse(t *testing.T, line string) *wireResponse {
	t.Helper()
	var response wireResponse
	err := json.Unmarshal([]byte(strings.TrimSpace(line)), &response)
	require.NoError(t, err)
	require.Equal(t, "2.0", response.JSONRPC)
	return &response
}

func testResponse(t *testing.T, idValue int, result any) *jsonrpc.Response {
	t.Helper()
	id, err := jsonrpc.MakeID(float64(idValue))
	require.NoError(t, err)
	encoded, err := json.Marshal(result)
	require.NoError(t, err)
	return &jsonrpc.Response{ID: id, Result: encoded}
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

type shortWriter struct{}

func (shortWriter) Write(data []byte) (int, error) {
	return len(data) - 1, nil
}

type blockingWriter struct {
	started chan struct{}
	unblock chan struct{}
	once    sync.Once
	closed  atomic.Bool
}

func newBlockingWriter() *blockingWriter {
	return &blockingWriter{started: make(chan struct{}), unblock: make(chan struct{})}
}

func (w *blockingWriter) Write(_ []byte) (int, error) {
	w.once.Do(func() { close(w.started) })
	<-w.unblock
	return 0, io.ErrClosedPipe
}

func (w *blockingWriter) Close() error {
	if w.closed.CompareAndSwap(false, true) {
		close(w.unblock)
	}
	return nil
}

func (b *lockedBuffer) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(data)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
