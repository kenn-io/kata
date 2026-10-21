package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
	kataclient "go.kenn.io/kata/pkg/client"
)

func TestAttachmentTextTool(t *testing.T) {
	called := false
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/projects/42/issues/abc4/attachments/01JB3Z6V2K8M4Q9T1X5W7Y0RNA/text" {
			called = true
			require.Equal(t, "3", r.URL.Query().Get("max_bytes"))
			require.Equal(t, "ocr", r.URL.Query().Get("profile"))
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"state": "ready", "text": "OCR", "truncated": true})
			return
		}
		http.NotFound(w, r)
	}))
	defer httpServer.Close()
	client, err := kataclient.New(httpServer.URL)
	require.NoError(t, err)
	session := connectRawTestServerWithOptions(t, Options{Client: client, ProjectID: 42, ProjectName: "spoke-project", Actor: "operator", Version: "test-version"})
	result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "kata.load_attachments", Arguments: map[string]any{}})
	require.NoError(t, err)
	require.False(t, result.IsError, mustJSON(t, result))
	require.Contains(t, string(mustJSON(t, result)), "kata.attachment_text")
	result, err = session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "kata.attachment_text", Arguments: map[string]any{"issue": "abc4", "attachment": "01JB3Z6V2K8M4Q9T1X5W7Y0RNA", "max_bytes": 3, "profile": "ocr"}})
	require.NoError(t, err)
	require.False(t, result.IsError, mustJSON(t, result))
	require.Contains(t, string(mustJSON(t, result)), "OCR")
	require.Contains(t, string(mustJSON(t, result)), `"truncated":true`)
	require.True(t, called)
}

type attachmentTextTransport func(*http.Request) (*http.Response, error)

func (f attachmentTextTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestAttachmentTextUsesBoundedLongRunningClient(t *testing.T) {
	ordinary, err := kataclient.NewWithHTTPClient("http://daemon.example", &http.Client{Transport: attachmentTextTransport(func(_ *http.Request) (*http.Response, error) {
		return nil, fmt.Errorf("ordinary transport used for rendition read")
	})})
	require.NoError(t, err)
	var budget time.Duration
	long, err := kataclient.NewWithHTTPClient("http://daemon.example", &http.Client{Transport: attachmentTextTransport(func(r *http.Request) (*http.Response, error) {
		deadline, ok := r.Context().Deadline()
		require.True(t, ok, "rendition read must have a bounded context")
		budget = time.Until(deadline)
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"state":"ready","text":"OCR","truncated":false}`))}, nil
	})})
	require.NoError(t, err)
	scope, err := NewBoundScope(ProjectIdentity{ID: 42, Name: "spoke-project"})
	require.NoError(t, err)
	h := toolHandlers{options: Options{Client: ordinary, LongRunningClient: long, Scope: scope}}
	_, result, err := h.attachmentText(context.Background(), nil, AttachmentTextInput{Issue: "abc4", Attachment: "01JB3Z6V2K8M4Q9T1X5W7Y0RNA"})
	require.NoError(t, err)
	require.Equal(t, "OCR", *result.Text)
	require.Greater(t, budget, 35*time.Minute)
	require.LessOrEqual(t, budget, 36*time.Minute)
}

func TestAttachmentTextFitsStdioBudget(t *testing.T) {
	const sourceBytes = 9 << 20
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requested, err := strconv.ParseFloat(r.URL.Query().Get("max_bytes"), 64)
		require.NoError(t, err)
		limit := min(int(requested), sourceBytes)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"state": "ready", "text": strings.Repeat("\x00", limit), "truncated": limit < sourceBytes})
	}))
	defer server.Close()
	client, err := kataclient.New(server.URL)
	require.NoError(t, err)
	session := connectRawTestServerWithOptions(t, Options{Client: client, ProjectID: 42, ProjectName: "spoke-project", Actor: "operator", Version: "test-version"})
	_, err = session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "kata.load_attachments", Arguments: map[string]any{}})
	require.NoError(t, err)
	result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "kata.attachment_text", Arguments: map[string]any{"issue": "abc4", "attachment": "01JB3Z6V2K8M4Q9T1X5W7Y0RNA", "max_bytes": 16 << 20}})
	require.NoError(t, err)
	require.False(t, result.IsError, string(mustJSON(t, result)))
	encoded := mustJSON(t, result)
	require.Less(t, len(encoded), 8<<20)
	require.Contains(t, string(encoded), `"truncated":true`)
	require.Contains(t, string(encoded), `\u0000`)
}
