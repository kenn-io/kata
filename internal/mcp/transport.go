package mcpserver

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"sync"
	"sync/atomic"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

const defaultMaxMessageBytes = 8 << 20

// StdioTransport carries one compact JSON-RPC message per line. It enforces
// Kata's input and output bounds while leaving protocol negotiation to the SDK.
type StdioTransport struct {
	reader          io.ReadCloser
	writer          io.Writer
	maxMessageBytes int
}

// NewStdioTransport creates a bounded stdio transport. A writer that implements
// io.Closer is closed with the connection so a blocked write cannot outlive the
// protocol stream.
func NewStdioTransport(reader io.ReadCloser, writer io.Writer) *StdioTransport {
	return &StdioTransport{
		reader:          reader,
		writer:          writer,
		maxMessageBytes: defaultMaxMessageBytes,
	}
}

// Connect implements mcp.Transport.
func (t *StdioTransport) Connect(context.Context) (sdkmcp.Connection, error) {
	if t == nil || t.reader == nil {
		return nil, errors.New("MCP stdio reader is required")
	}
	if t.writer == nil {
		return nil, errors.New("MCP stdio writer is required")
	}
	conn := &stdioConnection{
		reader:          t.reader,
		writer:          t.writer,
		reads:           make(chan readResult, 1),
		closed:          make(chan struct{}),
		active:          make(map[requestIDKey]struct{}),
		cancelled:       make(map[requestIDKey]struct{}),
		maxMessageBytes: t.maxMessageBytes,
	}
	if closer, ok := t.writer.(io.Closer); ok && !sameResource(t.reader, closer) {
		conn.writerCloser = closer
	}
	go conn.scan(t.maxMessageBytes)
	return conn, nil
}

type readResult struct {
	line []byte
	err  error
}

type stdioConnection struct {
	reader       io.ReadCloser
	writer       io.Writer
	writerCloser io.Closer
	reads        chan readResult
	closed       chan struct{}

	writeMu         sync.Mutex
	writeWaiting    atomic.Int64
	stateMu         sync.Mutex
	closeMu         sync.Mutex
	isClosed        bool
	active          map[requestIDKey]struct{}
	cancelled       map[requestIDKey]struct{}
	maxMessageBytes int
}

type requestIDKey struct {
	kind  string
	value string
}

func (c *stdioConnection) scan(maxMessageBytes int) {
	defer close(c.reads)
	scanner := bufio.NewScanner(c.reader)
	initialBufferBytes := min(maxMessageBytes, 64<<10)
	scanner.Buffer(make([]byte, initialBufferBytes), maxMessageBytes)
	for scanner.Scan() {
		line := bytes.TrimSuffix(scanner.Bytes(), []byte{'\r'})
		copyOfLine := append([]byte(nil), line...)
		select {
		case c.reads <- readResult{line: copyOfLine}:
		case <-c.closed:
			return
		}
	}
	if err := scanner.Err(); err != nil {
		select {
		case c.reads <- readResult{err: err}:
		case <-c.closed:
		}
		return
	}
	select {
	case c.reads <- readResult{err: io.EOF}:
	case <-c.closed:
	}
}

// Read returns the next admitted request. Rejected calls receive a correlated
// JSON-RPC error and are not dispatched to the SDK.
func (c *stdioConnection) Read(ctx context.Context) (jsonrpc.Message, error) {
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-c.closed:
			return nil, sdkmcp.ErrConnectionClosed
		case item, ok := <-c.reads:
			if !ok {
				select {
				case <-c.closed:
					return nil, sdkmcp.ErrConnectionClosed
				default:
				}
				return nil, io.EOF
			}
			if item.err != nil {
				return nil, item.err
			}
			message, admitted, err := c.admit(item.line)
			if err != nil {
				return nil, err
			}
			if admitted {
				return message, nil
			}
		}
	}
}

func (c *stdioConnection) admit(line []byte) (jsonrpc.Message, bool, error) {
	trimmed := bytes.TrimSpace(line)
	if len(trimmed) == 0 {
		return nil, false, c.writeError(context.Background(), jsonrpc.ID{}, jsonrpc.CodeParseError, "parse error", nil)
	}
	if trimmed[0] == '[' && json.Valid(trimmed) {
		return nil, false, c.writeError(context.Background(), jsonrpc.ID{}, jsonrpc.CodeInvalidRequest, "JSON-RPC batches are not supported", nil)
	}

	message, err := jsonrpc.DecodeMessage(trimmed)
	if err != nil {
		code := int64(jsonrpc.CodeInvalidRequest)
		message := "invalid request"
		if !json.Valid(trimmed) {
			code = jsonrpc.CodeParseError
			message = "parse error"
		}
		return nil, false, c.writeError(context.Background(), jsonrpc.ID{}, code, message, nil)
	}
	request, ok := message.(*jsonrpc.Request)
	if !ok {
		return nil, false, nil
	}

	if request.Method == "notifications/cancelled" {
		if request.IsCall() {
			return nil, false, c.writeError(context.Background(), request.ID, jsonrpc.CodeInvalidRequest, "cancellation must be a notification", nil)
		}
		c.recordCancellation(request.Params)
	}
	if request.IsCall() {
		c.recordActive(request.ID)
	}
	return request, true, nil
}

// Write implements mcp.Connection. Kata never sends calls to an MCP client.
func (c *stdioConnection) Write(ctx context.Context, message jsonrpc.Message) error {
	if request, ok := message.(*jsonrpc.Request); ok && request.IsCall() {
		return errors.New("MCP server requests are not supported")
	}
	encoded, err := jsonrpc.EncodeMessage(message)
	if err != nil {
		return fmt.Errorf("encode MCP message: %w", err)
	}
	if c.maxMessageBytes > 0 && len(encoded) > c.maxMessageBytes {
		response, ok := message.(*jsonrpc.Response)
		if !ok {
			return errors.New("encoded MCP message exceeds maximum message size")
		}
		encoded, err = jsonrpc.EncodeMessage(&jsonrpc.Response{
			ID: response.ID,
			Error: &jsonrpc.Error{
				Code:    jsonrpc.CodeInternalError,
				Message: "response exceeds maximum message size",
			},
		})
		if err != nil {
			return fmt.Errorf("encode oversized MCP response error: %w", err)
		}
		if len(encoded) > c.maxMessageBytes {
			return errors.New("encoded MCP response error exceeds maximum message size")
		}
	}
	c.writeWaiting.Add(1)
	c.writeMu.Lock()
	c.writeWaiting.Add(-1)
	defer c.writeMu.Unlock()
	if response, ok := message.(*jsonrpc.Response); ok && c.consumeCancellation(response.ID) {
		return nil
	}
	return c.writeLineLocked(ctx, encoded)
}

func (c *stdioConnection) recordCancellation(params json.RawMessage) {
	var value sdkmcp.CancelledParams
	if json.Unmarshal(params, &value) != nil {
		return
	}
	id, err := jsonrpc.MakeID(value.RequestID)
	if err != nil || !id.IsValid() {
		return
	}
	c.stateMu.Lock()
	key := idKey(id)
	if _, ok := c.active[key]; ok {
		c.cancelled[key] = struct{}{}
	}
	c.stateMu.Unlock()
}

func (c *stdioConnection) recordActive(id jsonrpc.ID) {
	c.stateMu.Lock()
	c.active[idKey(id)] = struct{}{}
	c.stateMu.Unlock()
}

func (c *stdioConnection) consumeCancellation(id jsonrpc.ID) bool {
	key := idKey(id)
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	delete(c.active, key)
	if _, ok := c.cancelled[key]; !ok {
		return false
	}
	delete(c.cancelled, key)
	return true
}

func idKey(id jsonrpc.ID) requestIDKey {
	raw := id.Raw()
	return requestIDKey{kind: fmt.Sprintf("%T", raw), value: fmt.Sprint(raw)}
}

func (c *stdioConnection) writeError(
	ctx context.Context,
	id jsonrpc.ID,
	code int64,
	message string,
	data json.RawMessage,
) error {
	wireError := &jsonrpc.Error{
		Code:    code,
		Message: message,
		Data:    data,
	}
	if !id.IsValid() {
		encoded, err := json.Marshal(struct {
			JSONRPC string         `json:"jsonrpc"`
			ID      any            `json:"id"`
			Error   *jsonrpc.Error `json:"error"`
		}{JSONRPC: "2.0", ID: nil, Error: wireError})
		if err != nil {
			return fmt.Errorf("encode MCP error: %w", err)
		}
		return c.writeLine(ctx, encoded)
	}
	response := &jsonrpc.Response{
		ID:    id,
		Error: wireError,
	}
	return c.Write(ctx, response)
}

func (c *stdioConnection) writeLine(ctx context.Context, encoded []byte) error {
	c.writeWaiting.Add(1)
	c.writeMu.Lock()
	c.writeWaiting.Add(-1)
	defer c.writeMu.Unlock()
	return c.writeLineLocked(ctx, encoded)
}

func (c *stdioConnection) writeLineLocked(ctx context.Context, encoded []byte) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	select {
	case <-c.closed:
		return sdkmcp.ErrConnectionClosed
	default:
	}
	line := append(encoded, '\n')
	written, err := c.writer.Write(line)
	if err != nil {
		select {
		case <-c.closed:
			return sdkmcp.ErrConnectionClosed
		default:
		}
		return fmt.Errorf("write MCP message: %w", err)
	}
	if written != len(line) {
		return fmt.Errorf("write MCP message: %w", io.ErrShortWrite)
	}
	return nil
}

// Close implements mcp.Connection and unblocks Read.
func (c *stdioConnection) Close() error {
	c.closeMu.Lock()
	defer c.closeMu.Unlock()
	if c.isClosed {
		return nil
	}
	c.isClosed = true
	close(c.closed)
	readerErr := c.reader.Close()
	var writerErr error
	if c.writerCloser != nil {
		writerErr = c.writerCloser.Close()
	}
	return errors.Join(readerErr, writerErr)
}

func sameResource(left, right any) bool {
	leftType := reflect.TypeOf(left)
	return leftType != nil && leftType == reflect.TypeOf(right) && leftType.Comparable() && left == right
}

// SessionID implements mcp.Connection. Stdio does not need a session ID.
func (*stdioConnection) SessionID() string { return "" }

var _ sdkmcp.Transport = (*StdioTransport)(nil)
var _ sdkmcp.Connection = (*stdioConnection)(nil)
