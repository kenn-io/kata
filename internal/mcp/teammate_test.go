package mcpserver

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
	kataclient "go.kenn.io/kata/pkg/client"
)

func TestMCPTeammateSharedCalls(t *testing.T) {
	var mu sync.Mutex
	requests := map[string]map[string]any{}
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v1/health" {
			writeJSON(w, map[string]any{"ok": true, "api_schema_version": "0.18.0"})
			return
		}
		var body map[string]any
		if r.Method == http.MethodPost {
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			mu.Lock()
			requests[r.Header.Get("Idempotency-Key")] = body
			mu.Unlock()
		}
		var response map[string]any
		require.NoError(t, json.Unmarshal(daemonResponse(r), &response))
		if strings.HasSuffix(r.URL.Path, "/comments") {
			comment := response["comment"].(map[string]any)
			comment["author"] = "coordinator"
			if c, ok := body["teammate"]; ok {
				comment["teammate"] = c
			}
		}
		writeJSON(w, response)
	}))
	t.Cleanup(daemon.Close)
	client, err := kataclient.NewWithHTTPClient(daemon.URL, daemon.Client())
	require.NoError(t, err)
	session := connectTestServerWithOptions(t, Options{Client: client, ProjectID: 42, ProjectName: "spoke-project", Actor: "coordinator", Version: "test"})
	var wg sync.WaitGroup
	for _, handle := range []string{"reviewer-7", "implementer-3", ""} {
		wg.Go(func() {
			for _, tool := range []string{"kata.comment", "kata.create"} {
				key := tool + handle
				args := map[string]any{"ref": "abc1", "body": "progress", "teammate": handle, "idempotency_key": key}
				if tool == "kata.create" {
					delete(args, "ref")
					args["title"] = "New work"
					args["metadata"] = map[string]any{"context": "keep"}
				}
				result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: tool, Arguments: args})
				require.NoError(t, err)
				require.False(t, result.IsError, "%s", mustJSON(t, result))
				if tool == "kata.comment" {
					comment := result.StructuredContent.(map[string]any)["comment"].(map[string]any)
					require.Equal(t, "coordinator", comment["author"])
					if handle != "" {
						require.Equal(t, handle, comment["teammate"])
					} else {
						require.NotContains(t, comment, "teammate")
					}
				}
			}
		})
	}
	wg.Wait()
	mu.Lock()
	require.Len(t, requests, 6)
	for key, body := range requests {
		require.Equal(t, "coordinator", body["actor"])
		handle := strings.TrimPrefix(strings.TrimPrefix(key, "kata.comment"), "kata.create")
		attribution := body
		if strings.HasPrefix(key, "kata.create") {
			attribution = body["metadata"].(map[string]any)
			require.Equal(t, "keep", attribution["context"])
		}
		if handle != "" {
			require.Equal(t, handle, attribution["teammate"])
		} else {
			require.NotContains(t, attribution, "teammate")
		}
	}
	mu.Unlock()
	result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "kata.comment", Arguments: map[string]any{"ref": "other-project#abc1", "body": "progress", "teammate": "reviewer-7", "idempotency_key": "outside"}})
	require.NoError(t, err)
	require.True(t, result.IsError)
	mu.Lock()
	require.Len(t, requests, 6)
	mu.Unlock()
}

func TestMCPTeammateRejectsOldDaemonBeforePost(t *testing.T) {
	var posts atomic.Int32
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v1/health" {
			writeJSON(w, map[string]any{"ok": true, "api_schema_version": "0.17.0"})
			return
		}
		if r.Method == http.MethodPost {
			posts.Add(1)
		}
		_, _ = w.Write(daemonResponse(r))
	}))
	t.Cleanup(daemon.Close)
	client, err := kataclient.NewWithHTTPClient(daemon.URL, daemon.Client())
	require.NoError(t, err)
	session := connectTestServerWithClient(t, client)
	for _, handle := range []string{"reviewer-7", "bad/handle"} {
		result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "kata.comment", Arguments: map[string]any{"ref": "abc1", "body": "progress", "teammate": handle, "idempotency_key": handle}})
		require.NoError(t, err)
		require.True(t, result.IsError)
		require.Contains(t, string(mustJSON(t, result)), "teammate")
	}
	require.Zero(t, posts.Load())
	result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "kata.comment", Arguments: map[string]any{"ref": "abc1", "body": "progress", "idempotency_key": "legacy"}})
	require.NoError(t, err)
	require.False(t, result.IsError, "%s", mustJSON(t, result))
	require.Equal(t, int32(1), posts.Load())
}

func TestMCPTeammateStartupFallbackAndMetadata(t *testing.T) {
	var mu sync.Mutex
	var requests []map[string]any
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v1/health" {
			writeJSON(w, map[string]any{"ok": true, "api_schema_version": "0.18.0"})
			return
		}
		var body map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		mu.Lock()
		requests = append(requests, body)
		mu.Unlock()
		_, _ = w.Write(daemonResponse(r))
	}))
	t.Cleanup(daemon.Close)
	client, err := kataclient.NewWithHTTPClient(daemon.URL, daemon.Client())
	require.NoError(t, err)
	session := connectTestServerWithOptions(t, Options{Client: client, ProjectID: 42, ProjectName: "spoke-project", Actor: "coordinator", Version: "test", Teammate: "default-1"})
	for _, tc := range []struct {
		name     string
		override any
		metadata map[string]any
		want     string
		fail     bool
	}{
		{"fallback", nil, nil, "default-1", false},
		{"override", "reviewer-7", nil, "reviewer-7", false},
		{"clear", "", nil, "", false},
		{"matching", nil, map[string]any{"teammate": "default-1"}, "default-1", false},
		{"conflict", nil, map[string]any{"teammate": "reviewer-7"}, "", true},
		{"explicit", "", map[string]any{"teammate": "reviewer-7"}, "reviewer-7", false},
		{"nonstring", "", map[string]any{"teammate": 3}, "", true},
		{"invalid", "", map[string]any{"teammate": "bad/handle"}, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := map[string]any{"title": "New issue", "idempotency_key": tc.name}
			if tc.override != nil {
				args["teammate"] = tc.override
			}
			if tc.metadata != nil {
				args["metadata"] = tc.metadata
			}
			mu.Lock()
			before := len(requests)
			mu.Unlock()
			result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "kata.create", Arguments: args})
			require.NoError(t, err)
			require.Equal(t, tc.fail, result.IsError, "%s", mustJSON(t, result))
			mu.Lock()
			defer mu.Unlock()
			if tc.fail {
				require.Len(t, requests, before)
				return
			}
			require.Len(t, requests, before+1)
			body := requests[len(requests)-1]
			require.Equal(t, "coordinator", body["actor"])
			metadata, _ := body["metadata"].(map[string]any)
			if tc.want == "" {
				require.NotContains(t, metadata, "teammate")
			} else {
				require.Equal(t, tc.want, metadata["teammate"])
			}
		})
	}
	for _, tc := range []struct {
		override any
		want     string
	}{{nil, "default-1"}, {"", ""}, {"reviewer-7", "reviewer-7"}, {nil, "default-1"}} {
		args := map[string]any{"ref": "abc1", "body": "progress", "idempotency_key": "comment"}
		if tc.override != nil {
			args["teammate"] = tc.override
		}
		result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "kata.comment", Arguments: args})
		require.NoError(t, err)
		require.False(t, result.IsError, "%s", mustJSON(t, result))
		mu.Lock()
		body := requests[len(requests)-1]
		mu.Unlock()
		require.Equal(t, "coordinator", body["actor"])
		if tc.want == "" {
			require.NotContains(t, body, "teammate")
		} else {
			require.Equal(t, tc.want, body["teammate"])
		}
	}
	metadata := map[string]any{"context": "keep"}
	h := toolHandlers{options: Options{Client: client, Scope: mustBoundTeammateScope(t), Actor: "coordinator", Teammate: "default-1"}}
	_, _, err = h.create(t.Context(), nil, CreateInput{Title: "Copied", IdempotencyKey: "copy", Metadata: metadata})
	require.NoError(t, err)
	require.Equal(t, map[string]any{"context": "keep"}, metadata)
}

func mustBoundTeammateScope(t *testing.T) *Scope {
	t.Helper()
	s, err := NewBoundScope(ProjectIdentity{ID: 42, Name: "spoke-project"})
	require.NoError(t, err)
	return s
}
