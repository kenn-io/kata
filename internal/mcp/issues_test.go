package mcpserver

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"

	kataclient "go.kenn.io/kata/pkg/client"
)

func TestMultiProjectListWithoutProjectReturnsBothProjects(t *testing.T) {
	session, _ := connectMultiProjectServer(t, nil)

	result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "kata.list", Arguments: map[string]any{"limit": 20}})
	require.NoError(t, err)
	require.False(t, result.IsError, "%s", mustJSON(t, result))
	issues := result.StructuredContent.(map[string]any)["issues"].([]any)
	require.Equal(t, []string{"hub-project#hbb1", "spoke-project#spk1"}, []string{
		issues[0].(map[string]any)["qualified_ref"].(string),
		issues[1].(map[string]any)["qualified_ref"].(string),
	})
}

func TestMultiProjectReadyWithoutProjectReturnsBothProjects(t *testing.T) {
	session, _ := connectMultiProjectServer(t, nil)

	result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "kata.ready", Arguments: map[string]any{"limit": 20}})
	require.NoError(t, err)
	require.False(t, result.IsError, "%s", mustJSON(t, result))
	issues := result.StructuredContent.(map[string]any)["issues"].([]any)
	require.Len(t, issues, 2)
}

func TestMultiProjectIssueWritesRequireQualifiedReference(t *testing.T) {
	session, requests := connectMultiProjectServer(t, nil)

	result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "kata.claim", Arguments: map[string]any{"ref": "spk1"}})
	require.NoError(t, err)
	require.True(t, result.IsError)
	require.Empty(t, *requests)
}

func TestMultiProjectCreateRequiresExplicitInScopeProject(t *testing.T) {
	session, requests := connectMultiProjectServer(t, nil)

	for _, arguments := range []map[string]any{
		{"title": "Unscoped", "idempotency_key": "unscoped"},
		{"project": "spoke-project", "title": "Bad relationship", "blocked_by": []any{"other-project#abcd"}, "idempotency_key": "outside"},
	} {
		result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "kata.create", Arguments: arguments})
		require.NoError(t, err)
		require.True(t, result.IsError)
	}
	require.Empty(t, *requests)
}

func TestMultiProjectShowRoutesQualifiedReference(t *testing.T) {
	session, requests := connectMultiProjectServer(t, nil)

	result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "kata.show", Arguments: map[string]any{"ref": "hub-project#hbb1"}})
	require.NoError(t, err)
	require.False(t, result.IsError, "%s", mustJSON(t, result))
	require.Contains(t, *requests, "/api/v1/projects/2/issues/hbb1")
}

func TestSearchFanoutUsesStableGlobalOrdering(t *testing.T) {
	session, _ := connectMultiProjectServer(t, func(writer http.ResponseWriter, request *http.Request) bool {
		if !strings.HasSuffix(request.URL.Path, "/search") {
			return false
		}
		projectName := "spoke-project"
		shortID := "spk1"
		if strings.Contains(request.URL.Path, "/projects/2/") {
			projectName = "hub-project"
			shortID = "hbb1"
		}
		writeJSON(writer, map[string]any{
			"query": "shared", "mode": "lexical",
			"results": []any{map[string]any{"issue": issueJSON(1, projectName, shortID), "score": 2.0, "matched_in": []string{"title"}}},
		})
		return true
	})

	result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "kata.search", Arguments: map[string]any{"query": "shared", "limit": 20}})
	require.NoError(t, err)
	require.False(t, result.IsError)
	hits := result.StructuredContent.(map[string]any)["results"].([]any)
	require.Equal(t, "hub-project#hbb1", hits[0].(map[string]any)["issue"].(map[string]any)["qualified_ref"])
	require.Equal(t, "spoke-project#spk1", hits[1].(map[string]any)["issue"].(map[string]any)["qualified_ref"])
}

func TestSearchFanoutReturnsNoPartialResultWhenOneProjectFails(t *testing.T) {
	session, _ := connectMultiProjectServer(t, func(writer http.ResponseWriter, request *http.Request) bool {
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
	})

	result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "kata.search", Arguments: map[string]any{"query": "shared"}})
	require.NoError(t, err)
	require.True(t, result.IsError)
	require.Nil(t, result.StructuredContent)
}

func TestSchedulingAndForceInputsAreFirstClass(t *testing.T) {
	create := CreateInput{
		ScheduledOn: "2026-09-01T15:00", Timezone: "America/Los_Angeles", ForceNew: true,
	}
	edit := EditInput{
		ScheduledOn: stringPointer("2026-09-01T15:00:30"), Timezone: stringPointer("America/Los_Angeles"),
		ClearScheduledOn: false, ClearTimezone: false,
	}
	claim := ClaimInput{Force: true}
	require.True(t, create.ForceNew)
	require.NotNil(t, edit.ScheduledOn)
	require.True(t, claim.Force)
}

func TestSchedulingMetadataAcceptsCivilTimeFormatsAndSomeday(t *testing.T) {
	for _, scheduledOn := range []string{
		"2026-09-01",
		"2026-09-01T15:00",
		"2026-09-01T15:00:30",
		"2026-09-01T22:00:00Z",
	} {
		metadata, err := createMetadata(CreateInput{
			ScheduledOn: scheduledOn,
			Timezone:    "America/Los_Angeles",
			Metadata:    map[string]any{"someday": true},
		})
		require.NoError(t, err, scheduledOn)
		require.Equal(t, true, metadata["someday"])
	}

	patch, err := editMetadata(EditInput{Metadata: map[string]any{"someday": nil}, ClearScheduledOn: true})
	require.NoError(t, err)
	require.Contains(t, patch, "someday")
	require.Nil(t, patch["someday"])
	require.Nil(t, patch["scheduled_on"])
}

func TestEditSchedulingRejectsSetClearAndMetadataCollisions(t *testing.T) {
	value := "2026-09-01"
	zone := "UTC"
	for _, input := range []EditInput{
		{ScheduledOn: &value, ClearScheduledOn: true},
		{Timezone: &zone, ClearTimezone: true},
		{ScheduledOn: &value, Metadata: map[string]any{"scheduled_on": "2026-09-02"}},
	} {
		_, err := editMetadata(input)
		require.Error(t, err)
	}
}

func TestCreateRejectsNumericScheduleOffsetAndDedicatedCollision(t *testing.T) {
	session := connectTestServer(t)
	for _, arguments := range []map[string]any{
		{"title": "Bad offset", "scheduled_on": "2026-09-01T15:00:00-07:00", "idempotency_key": "bad-offset"},
		{"title": "Collision", "scheduled_on": "2026-09-01", "metadata": map[string]any{"scheduled_on": "2026-09-02"}, "idempotency_key": "collision"},
		{"title": "Bad zone", "timezone": "Not/AZone", "idempotency_key": "bad-zone"},
	} {
		result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "kata.create", Arguments: arguments})
		require.NoError(t, err)
		require.True(t, result.IsError, "%s", mustJSON(t, result))
	}
}

func TestCreateSendsSchedulingSomedayAndForceNew(t *testing.T) {
	requestSeen := make(chan capturedRequest, 1)
	daemon := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		requestSeen <- capturedRequest{Path: request.URL.Path, Body: body}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write(daemonResponse(request))
	}))
	t.Cleanup(daemon.Close)
	client, err := kataclient.NewWithHTTPClient(daemon.URL, daemon.Client())
	require.NoError(t, err)
	session := connectTestServerWithClient(t, client)

	result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "kata.create", Arguments: map[string]any{
		"title": "Timed work", "scheduled_on": "2026-09-01T15:00", "timezone": "America/Los_Angeles",
		"metadata": map[string]any{"someday": true}, "force_new": true, "idempotency_key": "timed-work",
	}})
	require.NoError(t, err)
	require.False(t, result.IsError, "%s", mustJSON(t, result))
	request := <-requestSeen
	var body map[string]any
	require.NoError(t, json.Unmarshal(request.Body, &body))
	require.Equal(t, true, body["force_new"])
	require.Equal(t, map[string]any{
		"scheduled_on": "2026-09-01T15:00", "timezone": "America/Los_Angeles", "someday": true,
	}, body["metadata"])
}

func TestEditSchedulingClearUsesOneMetadataPatch(t *testing.T) {
	requestSeen := make(chan capturedRequest, 1)
	daemon := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		requestSeen <- capturedRequest{Path: request.URL.Path, Body: body}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write(daemonResponse(request))
	}))
	t.Cleanup(daemon.Close)
	client, err := kataclient.NewWithHTTPClient(daemon.URL, daemon.Client())
	require.NoError(t, err)
	session := connectTestServerWithClient(t, client)

	result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "kata.edit", Arguments: map[string]any{
		"ref": "abc1", "scheduled_on": "2026-09-01T22:00:00Z", "clear_timezone": true,
	}})
	require.NoError(t, err)
	require.False(t, result.IsError, "%s", mustJSON(t, result))
	request := <-requestSeen
	require.Equal(t, "/api/v1/projects/42/issues/abc1/metadata", request.Path)
	var body struct {
		Patch map[string]any `json:"patch"`
	}
	require.NoError(t, json.Unmarshal(request.Body, &body))
	require.Equal(t, map[string]any{"scheduled_on": "2026-09-01T22:00:00Z", "timezone": nil}, body.Patch)
}

func TestForceClaimReportsPreviousOwner(t *testing.T) {
	daemon := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		response := map[string]any{}
		require.NoError(t, json.Unmarshal(daemonResponse(request), &response))
		response["previous_owner"] = "other-agent"
		writeJSON(writer, response)
	}))
	t.Cleanup(daemon.Close)
	client, err := kataclient.NewWithHTTPClient(daemon.URL, daemon.Client())
	require.NoError(t, err)
	session := connectTestServerWithClient(t, client)

	result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "kata.claim", Arguments: map[string]any{"ref": "abc1", "force": true}})
	require.NoError(t, err)
	require.False(t, result.IsError, "%s", mustJSON(t, result))
	require.Equal(t, "other-agent", result.StructuredContent.(map[string]any)["previous_owner"])
}

func stringPointer(value string) *string { return &value }

func connectMultiProjectServer(t *testing.T, override func(http.ResponseWriter, *http.Request) bool) (*sdkmcp.ClientSession, *[]string) {
	t.Helper()
	var mu sync.Mutex
	requests := []string{}
	daemon := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/api/v1/projects" {
			mu.Lock()
			requests = append(requests, request.URL.Path)
			mu.Unlock()
		}
		writer.Header().Set("Content-Type", "application/json")
		if override != nil && override(writer, request) {
			return
		}
		switch request.URL.Path {
		case "/api/v1/projects":
			writeJSON(writer, map[string]any{"projects": []any{
				projectJSON(2, "01HBBBBBBBBBBBBBBBBBBBBBBB", "hub-project"),
				projectJSON(1, "01HAAAAAAAAAAAAAAAAAAAAAAA", "spoke-project"),
			}})
		case "/api/v1/issues", "/api/v1/ready":
			writeJSON(writer, map[string]any{"issues": []any{
				globalIssueJSON(2, "01HBBBBBBBBBBBBBBBBBBBBBBB", "hub-project", "hbb1"),
				globalIssueJSON(1, "01HAAAAAAAAAAAAAAAAAAAAAAA", "spoke-project", "spk1"),
			}})
		case "/api/v1/projects/1/issues", "/api/v1/projects/1/ready":
			writeJSON(writer, map[string]any{"issues": []any{issueJSON(1, "spoke-project", "spk1")}})
		case "/api/v1/projects/2/issues", "/api/v1/projects/2/ready":
			issue := issueJSON(2, "hub-project", "hbb1")
			issue["project_uid"] = "01HBBBBBBBBBBBBBBBBBBBBBBB"
			writeJSON(writer, map[string]any{"issues": []any{issue}})
		case "/api/v1/projects/2/issues/hbb1":
			writeJSON(writer, map[string]any{"issue": issueJSON(2, "hub-project", "hbb1"), "labels": []any{}, "links": []any{}, "comments": []any{}})
		default:
			http.Error(writer, `{"status":404,"error":{"code":"not_found","message":"not found"}}`, http.StatusNotFound)
		}
	}))
	t.Cleanup(daemon.Close)
	client, err := kataclient.NewWithHTTPClient(daemon.URL, daemon.Client())
	require.NoError(t, err)
	scope, err := NewAllowlistScope([]ProjectIdentity{
		{ID: 1, UID: "01HAAAAAAAAAAAAAAAAAAAAAAA", Name: "spoke-project"},
		{ID: 2, UID: "01HBBBBBBBBBBBBBBBBBBBBBBB", Name: "hub-project"},
	})
	require.NoError(t, err)
	return connectTestServerWithOptions(t, Options{Client: client, Scope: scope, Actor: "example-agent", Version: "test-version"}), &requests
}

func projectJSON(id int64, uid, name string) map[string]any {
	return map[string]any{"id": id, "uid": uid, "name": name, "metadata": map[string]any{}, "revision": 1, "created_at": "2026-08-11T00:00:00Z"}
}

func issueJSON(projectID int64, projectName, shortID string) map[string]any {
	return map[string]any{
		"id": projectID, "uid": "01ARZ3NDEKTSV4RRFFQ69G5FAV", "project_id": projectID,
		"project_uid": "01HAAAAAAAAAAAAAAAAAAAAAAA", "short_id": shortID,
		"qualified_id": projectName + "#" + shortID, "title": "Shared issue", "body": "Issue body",
		"status": "open", "author": "example-agent", "metadata": map[string]any{}, "revision": 1,
		"created_at": "2026-08-11T00:00:00Z", "updated_at": "2026-08-11T00:00:00Z",
	}
}

func globalIssueJSON(projectID int64, projectUID, projectName, shortID string) map[string]any {
	issue := issueJSON(projectID, projectName, shortID)
	issue["project_uid"] = projectUID
	issue["project_name"] = projectName
	issue["labels"] = []any{}
	return issue
}

func writeJSON(writer http.ResponseWriter, value any) {
	_ = json.NewEncoder(writer).Encode(value)
}
