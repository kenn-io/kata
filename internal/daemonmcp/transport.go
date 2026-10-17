package daemonmcp

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
)

// requestTransport calls the local REST stack without a network hop. Only
// explicitly admitted credentials cross from the MCP request into REST calls.
type requestTransport struct {
	handler http.Handler
	headers http.Header
}

func (t requestTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.URL.Scheme != "http" || request.URL.Host != "mcp.internal" || !strings.HasPrefix(request.URL.Path, "/api/v1/") {
		return nil, errors.New("MCP REST target is not the local API")
	}
	ctx, cancel := context.WithCancel(request.Context())
	r := request.Clone(ctx)
	r.RequestURI = r.URL.RequestURI()
	r.Header = make(http.Header)
	for _, key := range []string{"Accept", "Content-Type", "If-Match", "Idempotency-Key", "X-Kata-Confirm"} {
		r.Header[key] = request.Header.Values(key)
	}
	for key, values := range t.headers {
		r.Header[key] = append([]string(nil), values...)
	}
	reader, writer := io.Pipe()
	stop := context.AfterFunc(ctx, func() {
		_ = writer.CloseWithError(ctx.Err())
		_ = reader.CloseWithError(ctx.Err())
	})
	body := &responseBody{PipeReader: reader, close: func() { cancel(); stop() }}
	response := make(chan *http.Response, 1)
	w := &streamWriter{header: make(http.Header), writer: writer}
	w.send = func(status int) {
		response <- &http.Response{StatusCode: status, Header: w.header.Clone(), Body: body, Request: request}
	}
	go func() {
		defer func() {
			if recover() != nil {
				w.WriteHeader(http.StatusInternalServerError)
				_ = writer.CloseWithError(errors.New("MCP REST handler panicked"))
			}
		}()
		t.handler.ServeHTTP(w, r)
		w.WriteHeader(http.StatusOK)
		_ = writer.Close()
	}()
	select {
	case out := <-response:
		return out, nil
	case <-ctx.Done():
		_ = body.Close()
		return nil, ctx.Err()
	}
}

type responseBody struct {
	*io.PipeReader
	close func()
}

func (b *responseBody) Close() error { b.close(); return b.PipeReader.Close() }

type streamWriter struct {
	header http.Header
	writer *io.PipeWriter
	once   sync.Once
	send   func(int)
}

func (w *streamWriter) Header() http.Header    { return w.header }
func (w *streamWriter) WriteHeader(status int) { w.once.Do(func() { w.send(status) }) }
func (w *streamWriter) Write(data []byte) (int, error) {
	w.WriteHeader(http.StatusOK)
	return w.writer.Write(data)
}
func (w *streamWriter) Flush() { w.WriteHeader(http.StatusOK) }
