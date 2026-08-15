package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
	"golang.org/x/time/rate"

	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
	kataclient "go.kenn.io/kata/pkg/client"
	"go.kenn.io/kata/pkg/client/generated"
)

var sectionLoaderNames = []string{
	"kata.load_activity",
	"kata.load_federation",
	"kata.load_import",
	"kata.load_issue_discovery",
	"kata.load_issue_lifecycle",
	"kata.load_issue_mutation",
	"kata.load_leases",
	"kata.load_projects",
	"kata.load_recurrence",
	"kata.load_storage",
	"kata.load_sync",
	"kata.load_system",
	"kata.load_tokens",
}

func TestServerPublishesOnlySectionLoadersInitially(t *testing.T) {
	session := connectRawTestServerWithOptions(t, Options{
		Client: &kataclient.Client{}, ProjectID: 42, ProjectName: "spoke-project",
		Actor: "example-agent", Version: "test-version",
	})

	require.True(t, session.InitializeResult().Capabilities.Tools.ListChanged)
	result, err := session.ListTools(t.Context(), nil)
	require.NoError(t, err)
	require.Equal(t, sectionLoaderNames, toolNames(result.Tools))
	for _, tool := range result.Tools {
		require.True(t, tool.Annotations.ReadOnlyHint, tool.Name)
		require.False(t, *tool.Annotations.DestructiveHint, tool.Name)
	}
}

func TestSectionLoaderActivatesOnlyItsTypedToolsAndIsIdempotent(t *testing.T) {
	session := connectRawTestServerWithOptions(t, Options{
		Client: &kataclient.Client{}, ProjectID: 42, ProjectName: "spoke-project",
		Actor: "example-agent", Version: "test-version",
	})

	for range 2 {
		result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{
			Name: "kata.load_activity", Arguments: map[string]any{},
		})
		require.NoError(t, err)
		require.False(t, result.IsError)
		var output ToolSectionOutput
		require.NoError(t, json.Unmarshal(mustJSON(t, result.StructuredContent), &output))
		require.Equal(t, ToolSectionOutput{
			Section: "activity", Available: true, Loaded: true,
			Tools: []string{"kata.digest", "kata.events"},
		}, output)
	}

	result, err := session.ListTools(t.Context(), nil)
	require.NoError(t, err)
	want := append(append([]string(nil), sectionLoaderNames...), "kata.digest", "kata.events")
	sort.Strings(want)
	require.Equal(t, want, toolNames(result.Tools))
}

func TestTokenSectionRequiresExplicitStartupCapability(t *testing.T) {
	session := connectRawTestServerWithOptions(t, Options{
		Client: &kataclient.Client{}, Scope: NewAllScope(),
		Actor: "example-agent", Version: "test-version",
	})

	result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{
		Name: "kata.load_tokens", Arguments: map[string]any{},
	})
	require.NoError(t, err)
	require.False(t, result.IsError)
	var output ToolSectionOutput
	require.NoError(t, json.Unmarshal(mustJSON(t, result.StructuredContent), &output))
	require.False(t, output.Available)
	require.False(t, output.Loaded)

	enabled := connectRawTestServerWithOptions(t, Options{
		Client: &kataclient.Client{}, Scope: NewAllScope(), EnableTokenAdmin: true,
		Actor: "example-agent", Version: "test-version",
	})
	result, err = enabled.CallTool(t.Context(), &sdkmcp.CallToolParams{
		Name: "kata.load_tokens", Arguments: map[string]any{},
	})
	require.NoError(t, err)
	require.False(t, result.IsError)
	require.NoError(t, json.Unmarshal(mustJSON(t, result.StructuredContent), &output))
	require.True(t, output.Available)
	require.True(t, output.Loaded)
}

func TestServerPublishesCurrentToolsOnly(t *testing.T) {
	session := connectTestServerWithOptions(t, Options{
		Client: &kataclient.Client{}, Scope: NewAllScope(), EnableTokenAdmin: true,
		Actor: "example-agent", Version: "test-version",
	})

	initialized := session.InitializeResult()
	require.Equal(t, currentProtocolVersion, initialized.ProtocolVersion)
	require.NotNil(t, initialized.ServerInfo)
	require.Equal(t, "kata", initialized.ServerInfo.Name)
	require.Equal(t, "test-version", initialized.ServerInfo.Version)
	require.NotNil(t, initialized.Capabilities.Tools)
	require.True(t, initialized.Capabilities.Tools.ListChanged)
	require.Nil(t, initialized.Capabilities.Prompts)
	require.Nil(t, initialized.Capabilities.Resources)
	capabilities := string(mustJSON(t, initialized.Capabilities))
	require.NotContains(t, capabilities, `"logging"`)

	result, err := session.ListTools(t.Context(), nil)
	require.NoError(t, err)
	require.Equal(t, 5*time.Minute.Milliseconds(), int64(result.TTLMs))
	require.Equal(t, "private", result.CacheScope)

	wantNames := []string{
		"kata.audit_closes",
		"kata.claim",
		"kata.close",
		"kata.comment",
		"kata.create",
		"kata.delete",
		"kata.digest",
		"kata.edit",
		"kata.edit_comment",
		"kata.events",
		"kata.federation_enrollment_revoke",
		"kata.federation_leave",
		"kata.federation_quarantine",
		"kata.federation_rebind",
		"kata.federation_status",
		"kata.graph",
		"kata.import_issues",
		"kata.labels",
		"kata.lease",
		"kata.lease_force_release",
		"kata.lease_status",
		"kata.lease_steal",
		"kata.list",
		"kata.move",
		"kata.next",
		"kata.project_create",
		"kata.project_merge",
		"kata.project_purge",
		"kata.project_remove",
		"kata.project_restore",
		"kata.project_update",
		"kata.projects",
		"kata.purge",
		"kata.ready",
		"kata.recurrence_delete",
		"kata.recurrence_update",
		"kata.recurrences",
		"kata.reopen",
		"kata.restore",
		"kata.search",
		"kata.set_deadline",
		"kata.set_label",
		"kata.set_metadata",
		"kata.set_schedule",
		"kata.show",
		"kata.sync_once",
		"kata.sync_status",
		"kata.sync_update",
		"kata.system",
		"kata.token_create",
		"kata.token_revoke",
		"kata.tokens",
		"kata.wait",
	}
	wantNames = append(wantNames, sectionLoaderNames...)
	sort.Strings(wantNames)
	gotNames := make([]string, 0, len(result.Tools))
	projectSelectors := map[string]bool{
		"kata.audit_closes": true,
		"kata.create":       true, "kata.digest": true, "kata.events": true,
		"kata.federation_leave": true, "kata.federation_quarantine": true, "kata.federation_rebind": true,
		"kata.import_issues": true, "kata.labels": true, "kata.list": true, "kata.next": true,
		"kata.project_purge": true, "kata.project_remove": true, "kata.project_restore": true,
		"kata.project_update": true, "kata.projects": true, "kata.ready": true,
		"kata.recurrence_delete": true, "kata.recurrence_update": true, "kata.recurrences": true,
		"kata.search": true, "kata.sync_once": true, "kata.sync_status": true, "kata.sync_update": true,
	}
	openWorld := map[string]bool{
		"kata.federation_enrollment_revoke": true,
		"kata.federation_leave":             true, "kata.federation_quarantine": true,
		"kata.federation_rebind": true, "kata.federation_status": true, "kata.import_issues": true,
		"kata.search": true, "kata.sync_once": true, "kata.sync_status": true, "kata.sync_update": true,
	}
	for _, tool := range result.Tools {
		gotNames = append(gotNames, tool.Name)
		require.NotEmpty(t, tool.Title, tool.Name)
		require.NotEmpty(t, tool.Description, tool.Name)
		require.NotNil(t, tool.OutputSchema, tool.Name)

		input := schemaObject(t, tool.InputSchema)
		require.Equal(t, "https://json-schema.org/draft/2020-12/schema", input["$schema"], tool.Name)
		require.Equal(t, "object", input["type"], tool.Name)
		require.Equal(t, false, input["additionalProperties"], tool.Name)
		properties, _ := input["properties"].(map[string]any)
		if tool.Name != "kata.audit_closes" {
			require.NotContains(t, properties, "actor", tool.Name)
		}
		require.Equal(t, projectSelectors[tool.Name], properties["project"] != nil, tool.Name)
		require.NotContains(t, properties, "project_id", tool.Name)
		require.NotContains(t, properties, "workspace", tool.Name)

		annotations := tool.Annotations
		require.NotNil(t, annotations, tool.Name)
		require.NotNil(t, annotations.OpenWorldHint, tool.Name)
		require.Equal(t, openWorld[tool.Name], *annotations.OpenWorldHint, tool.Name)
	}
	require.Equal(t, wantNames, gotNames)
	require.True(t, sort.StringsAreSorted(gotNames))
}

func TestServerToolAnnotationsMatchMutationRisk(t *testing.T) {
	session := connectTestServerWithOptions(t, Options{
		Client: &kataclient.Client{}, Scope: NewAllScope(), EnableTokenAdmin: true,
		Actor: "example-agent", Version: "test-version",
	})
	result, err := session.ListTools(t.Context(), nil)
	require.NoError(t, err)

	readOnly := map[string]bool{
		"kata.audit_closes":      true,
		"kata.digest":            true,
		"kata.events":            true,
		"kata.federation_status": true,
		"kata.graph":             true,
		"kata.labels":            true,
		"kata.lease_status":      true,
		"kata.list":              true,
		"kata.next":              true,
		"kata.projects":          true,
		"kata.ready":             true,
		"kata.recurrences":       true,
		"kata.search":            true,
		"kata.show":              true,
		"kata.sync_status":       true,
		"kata.system":            true,
		"kata.tokens":            true,
		"kata.wait":              true,
	}
	for _, name := range sectionLoaderNames {
		readOnly[name] = true
	}
	// Mixed-mode tools with any overwriting or removing input (forced
	// claims, lease release, recurrence patch) advertise destructive.
	additive := map[string]bool{
		"kata.comment": true, "kata.create": true,
		"kata.project_create": true, "kata.project_restore": true,
		"kata.restore": true, "kata.token_create": true,
	}
	nonIdempotentTools := map[string]bool{
		// Identical retries mint new records: no idempotency key or natural
		// unique key deduplicates these creations.
		"kata.token_create":      true,
		"kata.recurrence_update": true,
		// Identical retries produce additional effects: renew extends the
		// lease expiry again and a sync pass re-imports from the provider.
		"kata.lease":     true,
		"kata.sync_once": true,
	}
	for _, tool := range result.Tools {
		annotations := tool.Annotations
		require.Equal(t, readOnly[tool.Name], annotations.ReadOnlyHint, tool.Name)
		require.Equal(t, !nonIdempotentTools[tool.Name], annotations.IdempotentHint, tool.Name)
		require.NotNil(t, annotations.DestructiveHint, tool.Name)
		if readOnly[tool.Name] || additive[tool.Name] {
			require.False(t, *annotations.DestructiveHint, tool.Name)
		} else {
			require.True(t, *annotations.DestructiveHint, tool.Name)
		}
	}
}

func TestToolAdmissionMiddlewareBoundsCalls(t *testing.T) {
	t.Run("rate", func(t *testing.T) {
		var calls atomic.Int64
		handler := toolAdmissionMiddleware(rate.NewLimiter(1, 1), make(chan struct{}, 1))(
			func(context.Context, string, sdkmcp.Request) (sdkmcp.Result, error) {
				calls.Add(1)
				return nil, nil
			},
		)
		_, err := handler(t.Context(), "tools/call", nil)
		require.NoError(t, err)
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
		defer cancel()
		_, err = handler(ctx, "tools/call", nil)
		require.ErrorContains(t, err, "deadline")
		_, err = handler(t.Context(), "server/discover", nil)
		require.NoError(t, err)
		require.EqualValues(t, 2, calls.Load())
	})

	t.Run("concurrency", func(t *testing.T) {
		started := make(chan struct{})
		release := make(chan struct{})
		handler := toolAdmissionMiddleware(rate.NewLimiter(rate.Inf, 1), make(chan struct{}, 1))(
			func(context.Context, string, sdkmcp.Request) (sdkmcp.Result, error) {
				close(started)
				<-release
				return nil, nil
			},
		)
		firstDone := make(chan error, 1)
		go func() {
			_, err := handler(t.Context(), "tools/call", nil)
			firstDone <- err
		}()
		<-started
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
		defer cancel()
		_, err := handler(ctx, "tools/call", nil)
		require.ErrorIs(t, err, context.DeadlineExceeded)
		close(release)
		require.NoError(t, <-firstDone)
	})

	t.Run("concurrency wait does not consume rate permit", func(t *testing.T) {
		concurrent := make(chan struct{}, 1)
		concurrent <- struct{}{}
		handler := toolAdmissionMiddleware(rate.NewLimiter(1, 1), concurrent)(
			func(context.Context, string, sdkmcp.Request) (sdkmcp.Result, error) {
				return nil, nil
			},
		)
		blockedContext, cancelBlocked := context.WithTimeout(t.Context(), 10*time.Millisecond)
		defer cancelBlocked()
		_, err := handler(blockedContext, "tools/call", nil)
		require.ErrorIs(t, err, context.DeadlineExceeded)
		<-concurrent

		admittedContext, cancelAdmitted := context.WithTimeout(t.Context(), 50*time.Millisecond)
		defer cancelAdmitted()
		_, err = handler(admittedContext, "tools/call", nil)
		require.NoError(t, err)
	})
}

func TestLinkSummaryPublishesOnlyDaemonFields(t *testing.T) {
	encoded, err := json.Marshal(LinkSummary{
		Type:         "blocks",
		QualifiedRef: "spoke-project#abc1",
		Status:       "open",
	})
	require.NoError(t, err)
	require.JSONEq(t, `{
		"type":"blocks",
		"qualified_ref":"spoke-project#abc1",
		"status":"open"
	}`, string(encoded))
}

func TestCreateAndCommentRequireIdempotencyKeys(t *testing.T) {
	session := connectTestServer(t)
	result, err := session.ListTools(t.Context(), nil)
	require.NoError(t, err)

	byName := make(map[string]*sdkmcp.Tool, len(result.Tools))
	for _, tool := range result.Tools {
		byName[tool.Name] = tool
	}
	for _, name := range []string{"kata.create", "kata.comment"} {
		input := schemaObject(t, byName[name].InputSchema)
		required := input["required"].([]any)
		require.Contains(t, required, "idempotency_key", name)
		properties := input["properties"].(map[string]any)
		keySchema := properties["idempotency_key"].(map[string]any)
		require.EqualValues(t, 1, keySchema["minLength"], name)
	}

	searchProperties := schemaObject(t, byName["kata.search"].InputSchema)["properties"].(map[string]any)
	require.ElementsMatch(t, []any{"auto", "lexical", "hybrid", "semantic"}, searchProperties["mode"].(map[string]any)["enum"])
	require.EqualValues(t, 100, searchProperties["limit"].(map[string]any)["maximum"])

	closeProperties := schemaObject(t, byName["kata.close"].InputSchema)["properties"].(map[string]any)
	require.ElementsMatch(t, []any{"done", "wontfix", "duplicate", "superseded", "audit-no-change"}, closeProperties["reason"].(map[string]any)["enum"])
	evidenceItems := closeProperties["evidence"].(map[string]any)["items"].(map[string]any)
	require.Len(t, evidenceItems["oneOf"], 7)
	require.Len(t, schemaObject(t, byName["kata.close"].InputSchema)["oneOf"], 5)

	require.Len(t, schemaObject(t, byName["kata.list"].InputSchema)["allOf"], 1)
	require.Len(t, schemaObject(t, byName["kata.ready"].InputSchema)["allOf"], 1)
	require.Len(t, schemaObject(t, byName["kata.edit"].InputSchema)["allOf"], 5)

	labelProperties := schemaObject(t, byName["kata.set_label"].InputSchema)["properties"].(map[string]any)
	require.EqualValues(t, 64, labelProperties["label"].(map[string]any)["maxLength"])
	require.Equal(t, `^[a-z0-9._:-]+$`, labelProperties["label"].(map[string]any)["pattern"])

	reopenProperties := schemaObject(t, byName["kata.reopen"].InputSchema)["properties"].(map[string]any)
	require.NotContains(t, reopenProperties, "message")

	metadataProperties := schemaObject(t, byName["kata.set_metadata"].InputSchema)["properties"].(map[string]any)
	require.EqualValues(t, 0, metadataProperties["revision"].(map[string]any)["minimum"])
}

func TestCloseInputSchemaMirrorsDaemonEvidenceMatrix(t *testing.T) {
	schema, err := inputSchemaFor[CloseInput]("kata.close").Resolve(nil)
	require.NoError(t, err)
	message40 := strings.Repeat("a", 40)
	message60 := strings.Repeat("b", 60)
	tests := []struct {
		name    string
		input   map[string]any
		wantErr bool
	}{
		{name: "done with test", input: map[string]any{"ref": "abc1", "reason": "done", "message": message40, "evidence": []any{map[string]any{"type": "test", "command": "go test ./..."}}}},
		{name: "done without evidence", input: map[string]any{"ref": "abc1", "reason": "done", "message": message40, "evidence": []any{}}, wantErr: true},
		{name: "wontfix", input: map[string]any{"ref": "abc1", "reason": "wontfix", "message": message60, "evidence": []any{}}},
		{name: "wontfix with evidence", input: map[string]any{"ref": "abc1", "reason": "wontfix", "message": message60, "evidence": []any{map[string]any{"type": "test", "command": "go test ./..."}}}, wantErr: true},
		{name: "duplicate", input: map[string]any{"ref": "abc1", "reason": "duplicate", "message": message40, "evidence": []any{map[string]any{"type": "duplicate-of", "issue_ref": "abc2"}}}},
		{name: "duplicate with test", input: map[string]any{"ref": "abc1", "reason": "duplicate", "message": message40, "evidence": []any{map[string]any{"type": "test", "command": "go test ./..."}}}, wantErr: true},
		{name: "superseded", input: map[string]any{"ref": "abc1", "reason": "superseded", "message": message40, "evidence": []any{map[string]any{"type": "superseded-by", "issue_ref": "abc2"}}}},
		{name: "audit", input: map[string]any{"ref": "abc1", "reason": "audit-no-change", "message": message40, "evidence": []any{map[string]any{"type": "no-change-audit", "rationale": "No change is required."}}}},
		{name: "audit plus reviewed paths", input: map[string]any{"ref": "abc1", "reason": "audit-no-change", "message": message40, "evidence": []any{map[string]any{"type": "no-change-audit", "rationale": "No change is required."}, map[string]any{"type": "reviewed-paths", "paths": []any{"internal/mcp"}}}}},
		{name: "audit without rationale", input: map[string]any{"ref": "abc1", "reason": "audit-no-change", "message": message40, "evidence": []any{map[string]any{"type": "reviewed-paths", "paths": []any{"internal/mcp"}}}}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := schema.Validate(tt.input)
			if tt.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestSetDeadlineInputSchemaRequiresOneMutation(t *testing.T) {
	schema, err := inputSchemaFor[SetDeadlineInput]("kata.set_deadline").Resolve(nil)
	require.NoError(t, err)

	require.NoError(t, schema.Validate(map[string]any{"ref": "abc1", "deadline": "2026-09-01T09:30"}))
	require.NoError(t, schema.Validate(map[string]any{"ref": "abc1", "clear_deadline": true}))
	require.Error(t, schema.Validate(map[string]any{"ref": "abc1"}))
	require.Error(t, schema.Validate(map[string]any{"ref": "abc1", "deadline": "2026-09-01", "clear_deadline": true}))
	require.Error(t, schema.Validate(map[string]any{"ref": "abc1", "clear_deadline": false}))
}

func TestSetScheduleInputSchemaRequiresOneMutation(t *testing.T) {
	schema, err := inputSchemaFor[SetScheduleInput]("kata.set_schedule").Resolve(nil)
	require.NoError(t, err)

	require.NoError(t, schema.Validate(map[string]any{"ref": "abc1", "schedule": "2026-09-01T09:30"}))
	require.NoError(t, schema.Validate(map[string]any{"ref": "abc1", "clear_schedule": true}))
	require.Error(t, schema.Validate(map[string]any{"ref": "abc1"}))
	require.Error(t, schema.Validate(map[string]any{"ref": "abc1", "schedule": "2026-09-01", "clear_schedule": true}))
	require.Error(t, schema.Validate(map[string]any{"ref": "abc1", "clear_schedule": false}))
}

func TestRecurrencePatchSchemaRequiresRevision(t *testing.T) {
	schema, err := inputSchemaFor[RecurrenceUpdateInput]("kata.recurrence_update").Resolve(nil)
	require.NoError(t, err)
	require.Error(t, schema.Validate(map[string]any{"action": "patch", "uid": "recurrence-uid"}))
	require.NoError(t, schema.Validate(map[string]any{"action": "patch", "uid": "recurrence-uid", "revision": 1}))
	require.NoError(t, schema.Validate(map[string]any{"action": "create", "template": map[string]any{"title": "Review"}}))
}

func TestToolsUseBoundDaemonProjectAndActor(t *testing.T) {
	requests := make(chan capturedRequest, 20)
	daemon := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, err := io.ReadAll(request.Body)
		require.NoError(t, err)
		requests <- capturedRequest{
			Method: request.Method,
			Path:   request.URL.Path,
			Query:  request.URL.RawQuery,
			Header: request.Header.Clone(),
			Body:   body,
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write(daemonResponse(request))
	}))
	t.Cleanup(daemon.Close)
	apiClient, err := kataclient.NewWithHTTPClient(daemon.URL, daemon.Client())
	require.NoError(t, err)
	session := connectTestServerWithClient(t, apiClient)

	tests := []struct {
		name       string
		tool       string
		arguments  map[string]any
		wantMethod string
		wantPath   string
		wantQuery  []string
		wantActor  bool
		wantKey    string
		wantMatch  string
	}{
		{name: "search", tool: "kata.search", arguments: map[string]any{"query": "callback race", "limit": 3, "labels": []string{"bug"}}, wantMethod: http.MethodGet, wantPath: "/api/v1/projects/42/search", wantQuery: []string{"q=callback+race", "limit=4", "label=bug"}, wantMatch: "abc1"},
		{name: "list", tool: "kata.list", arguments: map[string]any{"status": "open", "limit": 3, "unowned": true}, wantMethod: http.MethodGet, wantPath: "/api/v1/projects/42/issues", wantQuery: []string{"status=open", "limit=4", "unowned=true"}, wantMatch: "abc1"},
		{name: "show", tool: "kata.show", arguments: map[string]any{"ref": "abc1", "comment_limit": 1}, wantMethod: http.MethodGet, wantPath: "/api/v1/projects/42/issues/abc1", wantMatch: "Issue title"},
		{name: "ready", tool: "kata.ready", arguments: map[string]any{"limit": 3, "unowned": true}, wantMethod: http.MethodGet, wantPath: "/api/v1/projects/42/ready", wantQuery: []string{"limit=4", "unowned=true"}, wantMatch: "abc1"},
		{name: "labels", tool: "kata.labels", arguments: map[string]any{}, wantMethod: http.MethodGet, wantPath: "/api/v1/projects/42/labels", wantMatch: "bug"},
		{name: "create", tool: "kata.create", arguments: map[string]any{"title": "New issue", "idempotency_key": "new-issue-1", "labels": []string{"bug"}}, wantMethod: http.MethodPost, wantPath: "/api/v1/projects/42/issues", wantActor: true, wantKey: "new-issue-1", wantMatch: "abc1"},
		{name: "edit", tool: "kata.edit", arguments: map[string]any{"ref": "abc1", "title": "Updated title", "add_related": []string{"spoke-project#def2"}}, wantMethod: http.MethodPatch, wantPath: "/api/v1/projects/42/issues/abc1", wantActor: true, wantMatch: "abc1"},
		{name: "comment", tool: "kata.comment", arguments: map[string]any{"ref": "abc1", "body": "Progress note", "idempotency_key": "comment-1"}, wantMethod: http.MethodPost, wantPath: "/api/v1/projects/42/issues/abc1/comments", wantActor: true, wantKey: "comment-1", wantMatch: "Progress note"},
		{name: "claim", tool: "kata.claim", arguments: map[string]any{"ref": "abc1"}, wantMethod: http.MethodPost, wantPath: "/api/v1/projects/42/issues/abc1/actions/claim", wantActor: true, wantMatch: "abc1"},
		{name: "set deadline", tool: "kata.set_deadline", arguments: map[string]any{"ref": "abc1", "deadline": "2026-09-01T09:30", "revision": 7}, wantMethod: http.MethodPost, wantPath: "/api/v1/projects/42/issues/abc1/metadata", wantActor: true, wantMatch: "abc1"},
		{name: "set schedule", tool: "kata.set_schedule", arguments: map[string]any{"ref": "abc1", "schedule": "2026-09-01T09:30", "revision": 7}, wantMethod: http.MethodPost, wantPath: "/api/v1/projects/42/issues/abc1/metadata", wantActor: true, wantMatch: "abc1"},
		{name: "add label", tool: "kata.set_label", arguments: map[string]any{"ref": "abc1", "label": "bug", "present": true}, wantMethod: http.MethodPost, wantPath: "/api/v1/projects/42/issues/abc1/labels", wantActor: true, wantMatch: "abc1"},
		{name: "patch metadata", tool: "kata.set_metadata", arguments: map[string]any{"ref": "abc1", "patch": map[string]any{"work.attention": "ok"}, "revision": 7}, wantMethod: http.MethodPost, wantPath: "/api/v1/projects/42/issues/abc1/metadata", wantActor: true, wantMatch: "abc1"},
		{name: "close", tool: "kata.close", arguments: map[string]any{"ref": "abc1", "reason": "done", "message": "Completed the requested work and verified its behavior.", "evidence": []map[string]any{{"type": "test", "command": "go test ./..."}}}, wantMethod: http.MethodPost, wantPath: "/api/v1/projects/42/issues/abc1/actions/close", wantActor: true, wantMatch: "abc1"},
		{name: "reopen", tool: "kata.reopen", arguments: map[string]any{"ref": "abc1"}, wantMethod: http.MethodPost, wantPath: "/api/v1/projects/42/issues/abc1/actions/reopen", wantActor: true, wantMatch: "abc1"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{
				Name:      tt.tool,
				Arguments: tt.arguments,
			})
			require.NoError(t, err)
			require.False(t, result.IsError, result.Content)
			require.Empty(t, result.Content, "latest clients use structured content only")
			structured, ok := result.StructuredContent.(map[string]any)
			require.True(t, ok, "%T", result.StructuredContent)
			project := structured["project"].(map[string]any)
			require.Equal(t, "spoke-project", project["name"])
			require.Contains(t, string(mustJSON(t, structured)), tt.wantMatch)
			assertSummaryHydration(t, tt.tool, structured)
			if tt.tool == "kata.edit" {
				require.Len(t, structured["events"], 2)
				changes := structured["changes"].(map[string]any)
				require.Len(t, changes["related_added"], 1)
			}

			if tt.tool == "kata.edit" {
				resolution := <-requests
				require.Equal(t, http.MethodGet, resolution.Method)
				require.Equal(t, "/api/v1/projects/42/issues/def2", resolution.Path)
			}
			request := <-requests
			require.Equal(t, tt.wantMethod, request.Method)
			require.Equal(t, tt.wantPath, request.Path)
			for _, query := range tt.wantQuery {
				require.Contains(t, request.Query, query)
			}
			if tt.wantKey != "" {
				require.Equal(t, tt.wantKey, request.Header.Get("Idempotency-Key"))
			}
			if tt.tool == "kata.set_metadata" || tt.tool == "kata.set_deadline" || tt.tool == "kata.set_schedule" {
				require.Equal(t, `"rev-7"`, request.Header.Get("If-Match"))
			}
			if tt.wantActor {
				var body map[string]any
				require.NoError(t, json.Unmarshal(request.Body, &body))
				require.Equal(t, "example-agent", body["actor"])
				require.NotContains(t, body, "project")
				if tt.tool == "kata.edit" {
					links := body["links_delta"].(map[string]any)
					require.Equal(t, []any{"01ARZ3NDEKTSV4RRFFQ69G5FAV"}, links["add_related"])
					require.Equal(t, map[string]any{
						"01ARZ3NDEKTSV4RRFFQ69G5FAV": "01ARZ3NDEKTSV4RRFFQ69G5FAA",
					}, links["expected_project_uids"])
				}
				if tt.tool == "kata.set_deadline" {
					require.Equal(t, map[string]any{"deadline_on": "2026-09-01T09:30"}, body["patch"])
				}
				if tt.tool == "kata.set_schedule" {
					require.Equal(t, map[string]any{"scheduled_on": "2026-09-01T09:30"}, body["patch"])
				}
			}
			if tt.tool == "kata.edit" {
				scopeRequest := <-requests
				require.Equal(t, http.MethodGet, scopeRequest.Method)
				require.Equal(t, "/api/v1/issues/01ARZ3NDEKTSV4RRFFQ69G5FAW", scopeRequest.Path)
			}
		})
	}
}

func TestToolsRoundTripAgainstRealDaemon(t *testing.T) {
	store, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "kata.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	project, err := store.CreateProject(t.Context(), "spoke-project")
	require.NoError(t, err)
	foreignProject, err := store.CreateProject(t.Context(), "other-project")
	require.NoError(t, err)
	foreignIssue, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{
		ProjectID: foreignProject.ID, Title: "Foreign issue", Author: "example-agent",
	})
	require.NoError(t, err)
	daemonServer := daemon.NewServer(daemon.ServerConfig{DB: store, StartedAt: time.Now().UTC()})
	t.Cleanup(func() { require.NoError(t, daemonServer.Close()) })
	httpServer := httptest.NewServer(daemonServer.Handler())
	t.Cleanup(httpServer.Close)
	apiClient, err := kataclient.NewWithHTTPClient(httpServer.URL, httpServer.Client())
	require.NoError(t, err)
	session := connectTestServerWithOptions(t, Options{
		Client: apiClient, ProjectID: project.ID, ProjectName: project.Name,
		Actor: "example-agent", Version: "test-version",
	})

	created, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{
		Name: "kata.create",
		Arguments: map[string]any{
			"title": "Round-trip issue", "labels": []string{"bug"},
			"idempotency_key": "round-trip-create-1",
		},
	})
	require.NoError(t, err)
	require.False(t, created.IsError)
	createdIssue := created.StructuredContent.(map[string]any)["issue"].(map[string]any)
	require.NotContains(t, createdIssue, "labels")
	ref := createdIssue["ref"].(string)
	escaped, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{
		Name: "kata.edit", Arguments: map[string]any{
			"ref": ref, "add_related": []string{"spoke-project#" + strings.ToLower(foreignIssue.UID)},
		},
	})
	require.NoError(t, err)
	require.True(t, escaped.IsError, "qualified full-length short IDs must not resolve globally")
	reused, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{
		Name: "kata.create",
		Arguments: map[string]any{
			"title": "Round-trip issue", "labels": []string{"bug"},
			"idempotency_key": "round-trip-create-1",
		},
	})
	require.NoError(t, err)
	require.Equal(t, true, reused.StructuredContent.(map[string]any)["reused"])

	labeled, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{
		Name: "kata.set_label", Arguments: map[string]any{"ref": ref, "label": "urgent", "present": true},
	})
	require.NoError(t, err)
	require.False(t, labeled.IsError)
	require.NotContains(t, labeled.StructuredContent.(map[string]any)["issue"], "labels")

	shown, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{
		Name: "kata.show", Arguments: map[string]any{"ref": ref},
	})
	require.NoError(t, err)
	require.False(t, shown.IsError)
	shownIssue := shown.StructuredContent.(map[string]any)["issue"].(map[string]any)
	require.ElementsMatch(t, []any{"bug", "urgent"}, shownIssue["labels"])
	require.NotContains(t, shownIssue, "blocked")

	revision := shownIssue["revision"]
	patched, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{
		Name: "kata.set_metadata", Arguments: map[string]any{
			"ref": ref, "patch": map[string]any{"work.attention": "ok"}, "revision": revision,
		},
	})
	require.NoError(t, err)
	require.False(t, patched.IsError, patched.Content)

	conflict, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{
		Name: "kata.set_metadata", Arguments: map[string]any{
			"ref": ref, "patch": map[string]any{"work.attention": "stale"}, "revision": revision,
		},
	})
	require.NoError(t, err)
	require.True(t, conflict.IsError)
	require.Contains(t, string(mustJSON(t, conflict.Content)), "revision")

	deadlineSet, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{
		Name: "kata.set_deadline", Arguments: map[string]any{"ref": ref, "deadline": "2026-09-01T16:30:00Z"},
	})
	require.NoError(t, err)
	require.False(t, deadlineSet.IsError, deadlineSet.Content)

	shown, err = session.CallTool(t.Context(), &sdkmcp.CallToolParams{
		Name: "kata.show", Arguments: map[string]any{"ref": ref},
	})
	require.NoError(t, err)
	require.Equal(t, "2026-09-01T16:30:00Z",
		shown.StructuredContent.(map[string]any)["issue"].(map[string]any)["metadata"].(map[string]any)["deadline_on"])

	deadlineCleared, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{
		Name: "kata.set_deadline", Arguments: map[string]any{"ref": ref, "clear_deadline": true},
	})
	require.NoError(t, err)
	require.False(t, deadlineCleared.IsError, deadlineCleared.Content)

	shown, err = session.CallTool(t.Context(), &sdkmcp.CallToolParams{
		Name: "kata.show", Arguments: map[string]any{"ref": ref},
	})
	require.NoError(t, err)
	require.NotContains(t,
		shown.StructuredContent.(map[string]any)["issue"].(map[string]any)["metadata"].(map[string]any), "deadline_on")

	scheduleSet, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{
		Name: "kata.set_schedule", Arguments: map[string]any{"ref": ref, "schedule": "2026-09-02T09:30"},
	})
	require.NoError(t, err)
	require.False(t, scheduleSet.IsError, scheduleSet.Content)

	shown, err = session.CallTool(t.Context(), &sdkmcp.CallToolParams{
		Name: "kata.show", Arguments: map[string]any{"ref": ref},
	})
	require.NoError(t, err)
	require.Equal(t, "2026-09-02T09:30",
		shown.StructuredContent.(map[string]any)["issue"].(map[string]any)["metadata"].(map[string]any)["scheduled_on"])

	scheduleCleared, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{
		Name: "kata.set_schedule", Arguments: map[string]any{"ref": ref, "clear_schedule": true},
	})
	require.NoError(t, err)
	require.False(t, scheduleCleared.IsError, scheduleCleared.Content)

	shown, err = session.CallTool(t.Context(), &sdkmcp.CallToolParams{
		Name: "kata.show", Arguments: map[string]any{"ref": ref},
	})
	require.NoError(t, err)
	require.NotContains(t,
		shown.StructuredContent.(map[string]any)["issue"].(map[string]any)["metadata"].(map[string]any), "scheduled_on")
}

func assertSummaryHydration(t *testing.T, tool string, structured map[string]any) {
	t.Helper()
	switch tool {
	case "kata.list", "kata.ready":
		issue := structured["issues"].([]any)[0].(map[string]any)
		require.Contains(t, issue, "labels")
		require.Contains(t, issue, "blocked")
	case "kata.search":
		issue := structured["results"].([]any)[0].(map[string]any)["issue"].(map[string]any)
		require.NotContains(t, issue, "labels")
		require.NotContains(t, issue, "blocked")
	case "kata.show":
		issue := structured["issue"].(map[string]any)
		require.Contains(t, issue, "labels")
		require.NotContains(t, issue, "blocked")
	case "kata.labels":
		return
	default:
		issue := structured["issue"].(map[string]any)
		require.NotContains(t, issue, "labels")
		require.NotContains(t, issue, "blocked")
		require.NotContains(t, structured, "reused")
	}
}

func TestListLikeToolsProbeOneExtraResult(t *testing.T) {
	tests := []struct {
		name      string
		tool      string
		arguments map[string]any
		path      string
		arrayKey  string
	}{
		{name: "search", tool: "kata.search", arguments: map[string]any{"query": "work", "limit": 2}, path: "/api/v1/projects/42/search", arrayKey: "results"},
		{name: "list", tool: "kata.list", arguments: map[string]any{"limit": 2}, path: "/api/v1/projects/42/issues", arrayKey: "issues"},
		{name: "ready", tool: "kata.ready", arguments: map[string]any{"limit": 2}, path: "/api/v1/projects/42/ready", arrayKey: "issues"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			daemon := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				require.Equal(t, tt.path, request.URL.Path)
				require.Equal(t, "3", request.URL.Query().Get("limit"))
				writer.Header().Set("Content-Type", "application/json")
				_, _ = writer.Write(listLikeResponse(tt.tool, 3))
			}))
			t.Cleanup(daemon.Close)
			apiClient, err := kataclient.NewWithHTTPClient(daemon.URL, daemon.Client())
			require.NoError(t, err)
			session := connectTestServerWithClient(t, apiClient)

			result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: tt.tool, Arguments: tt.arguments})
			require.NoError(t, err)
			require.False(t, result.IsError)
			structured := result.StructuredContent.(map[string]any)
			require.True(t, structured["truncated"].(bool))
			require.Len(t, structured[tt.arrayKey], 2)
		})
	}
}

func TestLinkSummaryReversesIncomingDirectionalLinks(t *testing.T) {
	issueUID := "01ARZ3NDEKTSV4RRFFQ69G5FAV"
	peer := generated.LinkPeer{UID: "01ARZ3NDEKTSV4RRFFQ69G5FAW", QualifiedID: "spoke-project#def2", Status: "open"}

	blockedBy := linkSummary(issueUID, generated.LinkOut{
		Type: "blocks",
		From: peer,
		To:   generated.LinkPeer{UID: issueUID},
	})
	require.Equal(t, "blocked_by", blockedBy.Type)

	child := linkSummary(issueUID, generated.LinkOut{
		Type: "parent",
		From: peer,
		To:   generated.LinkPeer{UID: issueUID},
	})
	require.Equal(t, "child", child.Type)
}

func TestBoundRelationshipRefCanonicalizesQualifiedRef(t *testing.T) {
	handlers := toolHandlers{options: Options{ProjectName: "spoke-project"}}

	ref, err := handlers.boundRelationshipRef("spoke-project#abc1")
	require.NoError(t, err)
	require.Equal(t, "abc1", ref)

	ref, err = handlers.boundRelationshipRef("abc1")
	require.NoError(t, err)
	require.Equal(t, "abc1", ref)

	_, err = handlers.boundRelationshipRef("spoke-project#other-project#def2")
	require.ErrorContains(t, err, "outside bound project")

	_, err = handlers.boundRelationshipRef("spoke-project#01arz3ndektsv4rrffq69g5fav")
	require.ErrorContains(t, err, "full-length")
}

func TestToolValidationStopsBeforeDaemonRequest(t *testing.T) {
	var calls atomic.Int64
	daemon := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		http.Error(writer, "unexpected request", http.StatusInternalServerError)
	}))
	t.Cleanup(daemon.Close)
	apiClient, err := kataclient.NewWithHTTPClient(daemon.URL, daemon.Client())
	require.NoError(t, err)
	session := connectTestServerWithClient(t, apiClient)

	tests := []struct {
		name      string
		tool      string
		arguments map[string]any
	}{
		{name: "unknown argument", tool: "kata.show", arguments: map[string]any{"ref": "abc1", "actor": "other"}},
		{name: "cross project subject", tool: "kata.show", arguments: map[string]any{"ref": "other-project#abc1"}},
		{name: "cross project relationship", tool: "kata.create", arguments: map[string]any{"title": "Work", "idempotency_key": "work-1", "blocked_by": []string{"other-project#abc1"}}},
		{name: "unscoped UID relationship", tool: "kata.create", arguments: map[string]any{"title": "Work", "idempotency_key": "work-2", "blocked_by": []string{"01ARZ3NDEKTSV4RRFFQ69G5FAV"}}},
		{name: "oversize limit", tool: "kata.list", arguments: map[string]any{"limit": 101}},
		{name: "empty idempotency key", tool: "kata.comment", arguments: map[string]any{"ref": "abc1", "body": "note", "idempotency_key": " "}},
		{name: "edit conflict", tool: "kata.edit", arguments: map[string]any{"ref": "abc1", "priority": 1, "clear_priority": true}},
		{name: "missing deadline mutation", tool: "kata.set_deadline", arguments: map[string]any{"ref": "abc1"}},
		{name: "conflicting deadline mutation", tool: "kata.set_deadline", arguments: map[string]any{"ref": "abc1", "deadline": "2026-09-01", "clear_deadline": true}},
		{name: "missing schedule mutation", tool: "kata.set_schedule", arguments: map[string]any{"ref": "abc1"}},
		{name: "conflicting schedule mutation", tool: "kata.set_schedule", arguments: map[string]any{"ref": "abc1", "schedule": "2026-09-01", "clear_schedule": true}},
		{name: "empty metadata patch", tool: "kata.set_metadata", arguments: map[string]any{"ref": "abc1", "patch": map[string]any{}}},
		{name: "invalid close reason", tool: "kata.close", arguments: map[string]any{"ref": "abc1", "reason": "finished", "message": "done", "evidence": []any{}}},
		{name: "incomplete close evidence", tool: "kata.close", arguments: map[string]any{"ref": "abc1", "reason": "done", "message": "done", "evidence": []any{map[string]any{"type": "commit"}}}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: tt.tool, Arguments: tt.arguments})
			require.NoError(t, err)
			require.True(t, result.IsError)
		})
	}
	require.Zero(t, calls.Load())
}

func TestUnknownToolIsProtocolErrorAndBadArgumentsAreToolError(t *testing.T) {
	session := connectTestServer(t)

	result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "kata.missing", Arguments: map[string]any{}})
	require.Error(t, err)
	require.Nil(t, result)

	result, err = session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "kata.show", Arguments: map[string]any{}})
	require.NoError(t, err)
	require.True(t, result.IsError)
}

func TestSetLabelAbsentUsesDeleteWithBoundActor(t *testing.T) {
	requestSeen := make(chan capturedRequest, 1)
	daemon := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		requestSeen <- capturedRequest{Method: request.Method, Path: request.URL.Path, Query: request.URL.RawQuery, Body: body}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write(daemonResponse(request))
	}))
	t.Cleanup(daemon.Close)
	apiClient, err := kataclient.NewWithHTTPClient(daemon.URL, daemon.Client())
	require.NoError(t, err)
	session := connectTestServerWithClient(t, apiClient)

	result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{
		Name: "kata.set_label",
		Arguments: map[string]any{
			"ref": "abc1", "label": "bug", "present": false,
		},
	})
	require.NoError(t, err)
	require.False(t, result.IsError)
	request := <-requestSeen
	require.Equal(t, http.MethodDelete, request.Method)
	require.Equal(t, "/api/v1/projects/42/issues/abc1/labels/bug", request.Path)
	require.Equal(t, "actor=example-agent", request.Query)
	require.Empty(t, request.Body)
}

func TestDaemonErrorsBecomeToolExecutionErrors(t *testing.T) {
	daemon := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusConflict)
		_, _ = writer.Write([]byte(`{"status":409,"error":{"code":"owner_conflict","message":"issue is owned by another actor"}}`))
	}))
	t.Cleanup(daemon.Close)
	apiClient, err := kataclient.NewWithHTTPClient(daemon.URL, daemon.Client())
	require.NoError(t, err)
	session := connectTestServerWithClient(t, apiClient)

	result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{
		Name: "kata.claim", Arguments: map[string]any{"ref": "abc1"},
	})
	require.NoError(t, err)
	require.True(t, result.IsError)
	require.NotEmpty(t, result.Content)
}

func TestToolCancellationStopsDaemonRequest(t *testing.T) {
	started := make(chan struct{})
	cancelled := make(chan struct{})
	daemon := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		close(started)
		<-request.Context().Done()
		close(cancelled)
	}))
	t.Cleanup(daemon.Close)
	apiClient, err := kataclient.NewWithHTTPClient(daemon.URL, daemon.Client())
	require.NoError(t, err)
	session := connectTestServerWithClient(t, apiClient)

	ctx, cancel := context.WithCancel(t.Context())
	callDone := make(chan error, 1)
	go func() {
		_, err := session.CallTool(ctx, &sdkmcp.CallToolParams{
			Name: "kata.search", Arguments: map[string]any{"query": "slow request"},
		})
		callDone <- err
	}()
	<-started
	cancel()
	require.ErrorIs(t, <-callDone, context.Canceled)
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("daemon request was not cancelled")
	}
}

func connectTestServer(t *testing.T) *sdkmcp.ClientSession {
	t.Helper()
	return connectTestServerWithClient(t, &kataclient.Client{})
}

func connectTestServerWithClient(t *testing.T, apiClient *kataclient.Client) *sdkmcp.ClientSession {
	t.Helper()
	return connectTestServerWithOptions(t, Options{
		Client:      apiClient,
		ProjectID:   42,
		ProjectName: "spoke-project",
		Actor:       "example-agent",
		Version:     "test-version",
	})
}

func connectTestServerWithOptions(t *testing.T, options Options) *sdkmcp.ClientSession {
	t.Helper()
	session := connectRawTestServerWithOptions(t, options)
	for _, name := range sectionLoaderNames {
		result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: name, Arguments: map[string]any{}})
		require.NoError(t, err)
		require.False(t, result.IsError, "%s: %s", name, mustJSON(t, result))
	}
	return session
}

func connectRawTestServerWithOptions(t *testing.T, options Options) *sdkmcp.ClientSession {
	t.Helper()
	server, err := New(options)
	require.NoError(t, err)

	serverConn, clientConn := net.Pipe()
	ctx, cancel := context.WithCancel(t.Context())
	serverDone := make(chan error, 1)
	go func() {
		serverDone <- server.Run(ctx, NewStdioTransport(serverConn, serverConn))
	}()

	client := sdkmcp.NewClient(&sdkmcp.Implementation{
		Name:    "test-client",
		Version: "1",
	}, nil)
	session, err := client.Connect(ctx, &sdkmcp.IOTransport{
		Reader: clientConn,
		Writer: clientConn,
	}, nil)
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = session.Close()
		cancel()
		select {
		case <-serverDone:
		case <-time.After(time.Second):
			t.Error("MCP server did not stop")
		}
	})
	return session
}

type capturedRequest struct {
	Method string
	Path   string
	Query  string
	Header http.Header
	Body   []byte
}

func daemonResponse(request *http.Request) []byte {
	issue := map[string]any{
		"id": 1, "uid": "01ARZ3NDEKTSV4RRFFQ69G5FAV", "project_id": 42,
		"project_uid": "01ARZ3NDEKTSV4RRFFQ69G5FAA", "short_id": "abc1",
		"qualified_id": "spoke-project#abc1", "title": "Issue title", "body": "Issue body",
		"status": "open", "author": "example-agent", "owner": "example-agent",
		"labels": []string{"bug"}, "metadata": map[string]any{"work.attention": "ok"},
		"revision": 7, "blocked": false,
		"created_at": "2026-08-08T00:00:00Z", "updated_at": "2026-08-08T00:00:00Z",
	}
	event := map[string]any{
		"id": 9, "uid": "01ARZ3NDEKTSV4RRFFQ69G5FAW", "type": "issue.updated",
		"actor": "example-agent", "content_hash": "hash", "created_at": "2026-08-08T00:00:00Z",
		"origin_instance_uid": "01ARZ3NDEKTSV4RRFFQ69G5FAX", "payload": "{}",
		"project_id": 42, "project_name": "spoke-project", "project_uid": "01ARZ3NDEKTSV4RRFFQ69G5FAA",
	}
	path := request.URL.Path
	switch {
	case strings.HasSuffix(path, "/search"):
		return mustJSONBytes(map[string]any{"query": "callback race", "mode": "lexical", "results": []any{map[string]any{"issue": issue, "score": 1.5, "matched_in": []string{"title"}}}})
	case strings.HasSuffix(path, "/ready"):
		return mustJSONBytes(map[string]any{"issues": []any{issue}})
	case strings.HasSuffix(path, "/labels") && request.Method == http.MethodGet:
		return mustJSONBytes(map[string]any{"labels": []any{map[string]any{"label": "bug", "count": 1}}})
	case strings.HasSuffix(path, "/comments"):
		return mustJSONBytes(map[string]any{"changed": true, "issue": issue, "event": event, "comment": map[string]any{"id": 2, "uid": "01ARZ3NDEKTSV4RRFFQ69G5FAY", "issue_id": 1, "author": "example-agent", "body": "Progress note", "created_at": "2026-08-08T00:00:00Z"}})
	case strings.HasSuffix(path, "/actions/claim"):
		return mustJSONBytes(map[string]any{"changed": true, "issue": issue, "event": event})
	case strings.HasSuffix(path, "/metadata"):
		return mustJSONBytes(map[string]any{"changed": true, "issue": issue, "event": event})
	case strings.HasSuffix(path, "/labels") && request.Method == http.MethodPost:
		return mustJSONBytes(map[string]any{"changed": true, "issue": issue, "event": event, "label": map[string]any{"issue_id": 1, "label": "bug", "author": "example-agent", "created_at": "2026-08-08T00:00:00Z"}})
	case strings.Contains(path, "/actions/"):
		return mustJSONBytes(map[string]any{"changed": true, "issue": issue, "event": event})
	case strings.HasSuffix(path, "/issues") && request.Method == http.MethodGet:
		return mustJSONBytes(map[string]any{"issues": []any{issue}})
	case strings.HasSuffix(path, "/issues") && request.Method == http.MethodPost:
		return mustJSONBytes(map[string]any{"changed": true, "issue": issue, "event": event})
	case request.Method == http.MethodPatch:
		peer := map[string]any{
			"project": "spoke-project", "qualified_id": "spoke-project#def2",
			"short_id": "def2", "status": "open", "uid": "01ARZ3NDEKTSV4RRFFQ69G5FAW",
		}
		return mustJSONBytes(map[string]any{
			"changed": true, "issue": issue, "event": event, "events": []any{event, event},
			"changes": map[string]any{"related_added": []any{peer}},
		})
	case request.Method == http.MethodGet:
		return mustJSONBytes(map[string]any{"issue": issue, "labels": []any{map[string]any{"issue_id": 1, "label": "bug", "author": "example-agent", "created_at": "2026-08-08T00:00:00Z"}}, "comments": []any{map[string]any{"id": 2, "uid": "01ARZ3NDEKTSV4RRFFQ69G5FAY", "issue_id": 1, "author": "example-agent", "body": "Progress note", "created_at": "2026-08-08T00:00:00Z"}}, "links": []any{}})
	default:
		return mustJSONBytes(map[string]any{"changed": true, "issue": issue, "event": event})
	}
}

func listLikeResponse(tool string, count int) []byte {
	issues := make([]any, 0, count)
	results := make([]any, 0, count)
	for index := range count {
		shortID := fmt.Sprintf("abc%d", index)
		issue := map[string]any{
			"id": index + 1, "uid": fmt.Sprintf("01ARZ3NDEKTSV4RRFFQ69G5FA%d", index),
			"short_id": shortID, "qualified_id": "spoke-project#" + shortID,
			"project_id": 42, "title": "Issue title", "body": "Issue body",
			"author": "example-agent", "status": "open", "revision": 7,
			"metadata": map[string]any{}, "labels": []string{"bug"}, "blocked": false,
			"created_at": "2026-08-08T00:00:00Z", "updated_at": "2026-08-08T00:00:00Z",
		}
		issues = append(issues, issue)
		results = append(results, map[string]any{"issue": issue, "score": 1.0, "matched_in": []string{"title"}})
	}
	if tool == "kata.search" {
		return mustJSONBytes(map[string]any{"query": "work", "mode": "lexical", "results": results})
	}
	return mustJSONBytes(map[string]any{"issues": issues})
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	require.NoError(t, err)
	return data
}

func mustJSONBytes(value any) []byte {
	var buffer bytes.Buffer
	_ = json.NewEncoder(&buffer).Encode(value)
	return buffer.Bytes()
}

func schemaObject(t *testing.T, schema any) map[string]any {
	t.Helper()
	object, ok := schema.(map[string]any)
	require.True(t, ok, "schema is %T", schema)
	return object
}
