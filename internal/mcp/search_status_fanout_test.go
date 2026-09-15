package mcpserver

import (
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
)

// statusSearchGateVersion satisfies the daemon API version gate that
// status-filtered search requires; the gate itself is covered by
// TestMCPSearchStatusCompatibilityAndForwarding.
const statusSearchGateVersion = "0.19.0"

func serveStatusSearchHealth(writer http.ResponseWriter, request *http.Request) bool {
	if request.URL.Path != "/api/v1/health" {
		return false
	}
	writeJSON(writer, map[string]any{"ok": true, "api_schema_version": statusSearchGateVersion})
	return true
}

func TestMCPSearchStatusFansOutToEveryProjectWhenProjectOmitted(t *testing.T) {
	for _, status := range []string{"open", "closed"} {
		t.Run(status, func(t *testing.T) {
			var mu sync.Mutex
			statusByProjectPath := map[string]string{}
			session, _ := connectMultiProjectServer(t, func(writer http.ResponseWriter, request *http.Request) bool {
				if serveStatusSearchHealth(writer, request) {
					return true
				}
				if !strings.HasSuffix(request.URL.Path, "/search") {
					return false
				}
				mu.Lock()
				statusByProjectPath[request.URL.Path] = request.URL.Query().Get("status")
				mu.Unlock()
				projectName, shortID := "spoke-project", "spk1"
				if strings.Contains(request.URL.Path, "/projects/2/") {
					projectName, shortID = "hub-project", "hbb1"
				}
				writeJSON(writer, map[string]any{
					"query": "shared", "mode": "lexical",
					"results": []any{map[string]any{"issue": issueJSON(1, projectName, shortID), "score": 2.0, "matched_in": []string{"title"}}},
				})
				return true
			})

			result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{
				Name: "kata.search", Arguments: map[string]any{"query": "shared", "status": status},
			})
			require.NoError(t, err)
			require.False(t, result.IsError, "%s", mustJSON(t, result))

			mu.Lock()
			defer mu.Unlock()
			require.Len(t, statusByProjectPath, 2, "status-filtered search must fan out to every project in scope")
			require.Equal(t, status, statusByProjectPath["/api/v1/projects/1/search"])
			require.Equal(t, status, statusByProjectPath["/api/v1/projects/2/search"])

			output := result.StructuredContent.(map[string]any)
			hits := output["results"].([]any)
			require.Len(t, hits, 2)
			require.ElementsMatch(t, []string{"hub-project#hbb1", "spoke-project#spk1"}, []string{
				hits[0].(map[string]any)["issue"].(map[string]any)["qualified_ref"].(string),
				hits[1].(map[string]any)["issue"].(map[string]any)["qualified_ref"].(string),
			})
			require.Len(t, output["projects"].([]any), 2)
		})
	}
}

func TestMCPSearchStatusFanoutKeepsGlobalLimitDegradedAndAtomicity(t *testing.T) {
	testCases := []struct {
		name      string
		arguments map[string]any
		respond   func(writer http.ResponseWriter, request *http.Request) bool
		verify    func(t *testing.T, result *sdkmcp.CallToolResult)
	}{
		{
			name:      "rank fuses projects then cuts to the global limit and names degraded projects",
			arguments: map[string]any{"query": "shared", "status": "open", "limit": 4},
			respond: func(writer http.ResponseWriter, request *http.Request) bool {
				if !strings.HasSuffix(request.URL.Path, "/search") {
					return false
				}
				projectName, shortPrefix := "spoke-project", "spk"
				if strings.Contains(request.URL.Path, "/projects/2/") {
					projectName, shortPrefix = "hub-project", "hbb"
				}
				results := make([]any, 0, 3)
				for index := range 3 {
					results = append(results, map[string]any{
						"issue":      issueJSON(1, projectName, fmt.Sprintf("%s%d", shortPrefix, index+1)),
						"score":      1.0,
						"matched_in": []string{"title"},
					})
				}
				response := map[string]any{"query": "shared", "mode": "lexical", "results": results}
				if projectName == "spoke-project" {
					response["degraded"] = true
					response["degraded_reason"] = "embedding store unavailable; lexical fallback"
				}
				writeJSON(writer, response)
				return true
			},
			verify: func(t *testing.T, result *sdkmcp.CallToolResult) {
				require.False(t, result.IsError, "%s", mustJSON(t, result))
				output := result.StructuredContent.(map[string]any)
				hits := output["results"].([]any)
				require.Len(t, hits, 4, "merged results must be cut to the requested global limit")
				require.Equal(t, true, output["truncated"])
				require.Equal(t, true, output["degraded"])
				require.Equal(t, "spoke-project: embedding store unavailable; lexical fallback", output["degraded_reason"])
			},
		},
		{
			name:      "one project failing fails the whole search with no partial results",
			arguments: map[string]any{"query": "shared", "status": "open"},
			respond: func(writer http.ResponseWriter, request *http.Request) bool {
				if !strings.HasSuffix(request.URL.Path, "/search") {
					return false
				}
				if strings.Contains(request.URL.Path, "/projects/2/") {
					writer.WriteHeader(http.StatusServiceUnavailable)
					writeJSON(writer, map[string]any{"status": 503, "error": map[string]any{"code": "unavailable", "message": "search unavailable"}})
					return true
				}
				writeJSON(writer, map[string]any{
					"query": "shared", "mode": "lexical",
					"results": []any{map[string]any{"issue": issueJSON(1, "spoke-project", "spk1"), "score": 2.0, "matched_in": []string{"title"}}},
				})
				return true
			},
			verify: func(t *testing.T, result *sdkmcp.CallToolResult) {
				require.True(t, result.IsError)
				require.Nil(t, result.StructuredContent, "no partial results may be published when any project search fails")
				require.Contains(t, string(mustJSON(t, result)), "hub-project")
			},
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			session, _ := connectMultiProjectServer(t, func(writer http.ResponseWriter, request *http.Request) bool {
				if serveStatusSearchHealth(writer, request) {
					return true
				}
				return testCase.respond(writer, request)
			})
			result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{
				Name: "kata.search", Arguments: testCase.arguments,
			})
			require.NoError(t, err)
			testCase.verify(t, result)
		})
	}
}
