package mcpserver

import (
	"encoding/json"
	"fmt"
	"maps"
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
	headers := map[string]string{}
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
			headers[r.Header.Get("Idempotency-Key")] = r.Header.Get("X-Kata-Teammate")
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
		require.Equal(t, handle, headers[key], "concurrent request contribution")
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

// All registered native issue mutations share the same optional contribution
// channel. Exercise the SDK schema and actual outgoing requests together.
func TestMCPDisplayContributionPerCall(t *testing.T) {
	var mu sync.Mutex
	var headers []string
	var leaseAttempts int
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v1/health" {
			writeJSON(w, map[string]any{"ok": true, "api_schema_version": "0.18.0"})
			return
		}
		if r.Method != http.MethodGet {
			headers = append(headers, r.Header.Get("X-Kata-Teammate"))
		} else {
			require.Empty(t, r.Header.Get("X-Kata-Teammate"))
		}
		if r.URL.Path == "/api/v1/projects" && r.Method == http.MethodGet {
			writeJSON(w, map[string]any{"projects": []any{
				map[string]any{"id": 42, "uid": "01ARZ3NDEKTSV4RRFFQ69G5FAA", "name": "spoke-project"},
				map[string]any{"id": 43, "uid": "01ARZ3NDEKTSV4RRFFQ69G5FAB", "name": "hub-project"},
			}})
			return
		}
		if strings.Contains(r.URL.Path, "/bridge") {
			if strings.HasSuffix(r.URL.Path, "/actions/reconcile") {
				writeJSON(w, externalRunFixture())
			} else {
				writeJSON(w, externalBridgeFixture())
			}
			return
		}
		if strings.Contains(r.URL.Path, "/lease/actions/") {
			granted := true
			if strings.HasSuffix(r.URL.Path, "/acquire") {
				leaseAttempts++
				granted = leaseAttempts > 1
			}
			writeJSON(w, map[string]any{"granted": granted, "holder": map[string]any{"holder": "coordinator"}})
			return
		}
		_, _ = w.Write(daemonResponse(r))
	}))
	defer daemon.Close()
	client, err := kataclient.NewWithHTTPClient(daemon.URL, daemon.Client())
	require.NoError(t, err)
	session := connectTestServerWithOptions(t, Options{Client: client, LongRunningClient: client, Scope: NewAllScope(), Actor: "coordinator", Version: "test", Teammate: "default-1"})
	cases := []struct {
		tool   string
		args   map[string]any
		writes int
	}{
		{"kata.create", map[string]any{"title": "New work", "idempotency_key": "create-display", "project": "spoke-project"}, 1},
		{"kata.comment", map[string]any{"body": "Progress", "idempotency_key": "comment-display"}, 1},
		{"kata.edit_comment", map[string]any{"body": "Updated progress", "comment_ref": "c:abc123"}, 1},
		{"kata.move", map[string]any{"to_project": "hub-project", "revision": 7, "single": true}, 1},
		{"kata.bridge_bind", map[string]any{"connector": "example-connector", "external": "root-locator"}, 1},
		{"kata.bridge_pause", nil, 1},
		{"kata.bridge_resume", nil, 1},
		{"kata.bridge_reconcile", nil, 1},
		{"kata.bridge_resolve_field", map[string]any{"kata_field": "scheduled_on", "use": "kata"}, 1},
		{"kata.bridge_resolve_comment", map[string]any{"action": "retry"}, 1},
		{"kata.bridge_unbind", nil, 1},
		{"kata.assign", map[string]any{"owner": "worker"}, 1},
		{"kata.unassign", nil, 1},
		{"kata.edit", map[string]any{"title": "Updated"}, 1},
		{"kata.claim", nil, 1},
		{"kata.set_label", map[string]any{"label": "ready", "present": true}, 1},
		{"kata.set_label", map[string]any{"label": "ready", "present": false}, 1},
		{"kata.set_metadata", map[string]any{"patch": map[string]any{"context": "keep"}}, 1},
		{"kata.set_deadline", map[string]any{"clear_deadline": true}, 1},
		{"kata.set_schedule", map[string]any{"clear_schedule": true}, 1},
		{"kata.close", map[string]any{"reason": "done", "message": "Completed the requested work and verified focused checks.", "evidence": []any{map[string]any{"type": "test", "command": "go test ./internal/example"}}}, 1},
		{"kata.reopen", nil, 1},
		{"kata.delete", map[string]any{"confirm": "DELETE spoke-project#abc1"}, 1},
		{"kata.restore", nil, 1},
		{"kata.lease", map[string]any{"action": "acquire"}, 1},
		{"kata.lease", map[string]any{"action": "renew"}, 1},
		{"kata.lease", map[string]any{"action": "release"}, 1},
		{"kata.lease_force_release", map[string]any{"reason": "Operator handoff"}, 1},
		{"kata.lease_steal", map[string]any{"reason": "Operator handoff"}, 3},
	}
	for _, tc := range cases {
		t.Run(tc.tool+fmt.Sprint(tc.args), func(t *testing.T) {
			for _, selection := range []struct {
				name    string
				value   any
				want    string
				invalid bool
			}{
				{"default", nil, "default-1", false}, {"override", "worker-2", "worker-2", false},
				{"empty", "", "", false}, {"invalid", "bad/handle", "", true},
			} {
				t.Run(selection.name, func(t *testing.T) {
					args := map[string]any{"ref": "spoke-project#abc1"}
					maps.Copy(args, tc.args)
					if tc.tool == "kata.create" {
						delete(args, "ref")
					}
					if selection.value != nil {
						args["teammate"] = selection.value
					}
					mu.Lock()
					headers = nil
					leaseAttempts = 0
					mu.Unlock()
					result, callErr := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: tc.tool, Arguments: args})
					require.NoError(t, callErr)
					require.Equal(t, selection.invalid, result.IsError, "%s", mustJSON(t, result))
					mu.Lock()
					got := append([]string(nil), headers...)
					mu.Unlock()
					if selection.invalid {
						require.Empty(t, got)
						return
					}
					require.Len(t, got, tc.writes)
					for _, header := range got {
						require.Equal(t, selection.want, header)
					}
				})
			}
		})
	}
}
