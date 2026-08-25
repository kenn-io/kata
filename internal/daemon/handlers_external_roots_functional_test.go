package daemon_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/internal/config"
	connectorclient "go.kenn.io/kata/internal/connector"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/rootbridge"
	"go.kenn.io/kata/pkg/connector"
)

func testExternalRootCommand(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "example-connector")
}

func TestExternalRootHandlersFullLifecycleAndDelivery(t *testing.T) {
	database := openTestDB(t)
	project, err := database.db.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	issue, _, err := database.db.CreateIssue(t.Context(), db.CreateIssueParams{
		ProjectID: project.ID, Title: "Local title", Body: "Local body", Author: "tester",
	})
	require.NoError(t, err)
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	client := &daemonExternalRootClient{
		description: connector.Description{
			ConnectorID: "example.connector", DisplayName: "Example connector",
			Protocol: connector.ProtocolVersion, AccountIdentity: "account-1",
			Capabilities: []connector.Capability{
				connector.CapabilityPublishComment,
				connector.CapabilityFields,
				connector.CapabilityConditionalFields,
			},
			SelfActorID: "self-1",
		},
		root: connector.Root{
			Key: "root-1", IdentityKey: "account-1", Title: "External title", Body: "External body",
			State: "open", Revision: "revision-1", UpdatedAt: now, ObservedAt: now,
		},
		fields: []connector.FieldDescriptor{{
			ID: "start-date", DisplayName: "Start date", AcceptedKinds: []string{"date"},
			Nullable: true, Writable: true, SchemaRevision: "schema-1",
		}},
		fieldValues: map[string]connector.FieldValue{},
	}
	registry, err := rootbridge.NewRegistry(t.Context(), []config.ConnectorConfig{{
		ID: "example-connector", Command: testExternalRootCommand(t), Args: []string{"--mode", "private-arg"},
		Settings: map[string]any{"token": "private-setting"}, Env: map[string]string{"TOKEN": "PRIVATE_ENV"},
	}}, func(config.ConnectorConfig) connectorclient.Client { return client })
	require.NoError(t, err)
	reconciler := rootbridge.NewReconciler(database.db, registry, rootbridge.ReconcilerConfig{Now: func() time.Time { return now }})
	broadcaster := daemon.NewEventBroadcaster()
	sub := broadcaster.Subscribe(daemon.SubFilter{ProjectID: project.ID})
	t.Cleanup(sub.Unsub)
	hookSink := &recordingSink{}
	eventSink := func(event db.Event) {
		broadcaster.Broadcast(daemon.StreamMsg{Kind: "event", Event: &event, ProjectID: event.ProjectID})
		hookSink.Enqueue(event)
	}
	service := rootbridge.NewServiceWithEventSink(
		database.db,
		registry,
		func(ctx context.Context, bindingID int64, claimToken string) ([]db.Event, error) {
			result, err := reconciler.Run(ctx, bindingID, rootbridge.RunOptions{
				ClaimToken: claimToken,
				EventSink:  eventSink,
			})
			return result.Events, err
		},
		eventSink,
	)
	var wakeMu sync.Mutex
	var wakes []int64
	committedAtWake := true
	wake := func(bindingID int64) {
		wakeMu.Lock()
		defer wakeMu.Unlock()
		wakes = append(wakes, bindingID)
		binding, readErr := database.db.ExternalRootBindingByID(t.Context(), bindingID)
		if readErr != nil || binding.ClaimToken != "" {
			committedAtWake = false
		}
	}
	ts := startTestServer(t, daemon.ServerConfig{
		DB: database.db, StartedAt: now, Broadcaster: broadcaster, Hooks: hookSink,
		ExternalRootRegistry: registry, ExternalRootService: service,
		ExternalRootReconciler: reconciler, ExternalRootWake: wake,
	})

	status, body := requestExternalRootJSON(t, ts.URL, http.MethodGet, "/api/v1/connectors", nil)
	require.Equal(t, http.StatusOK, status, string(body))
	assert.Contains(t, string(body), `"instance_id":"example-connector"`)
	assert.Contains(t, string(body), `"connector_id":"example.connector"`)
	assert.Contains(t, string(body), `"healthy":true`)
	assert.NotContains(t, string(body), "private-setting")
	assert.NotContains(t, string(body), "private-arg")
	assert.NotContains(t, string(body), "PRIVATE_ENV")
	status, body = requestExternalRootJSON(t, ts.URL, http.MethodGet, "/api/v1/connectors/example-connector", nil)
	require.Equal(t, http.StatusOK, status, string(body))
	assert.Contains(t, string(body), `"connector_id":"example.connector"`)
	assert.NotContains(t, string(body), "private-setting")
	assert.NotContains(t, string(body), "private-arg")
	assert.NotContains(t, string(body), "PRIVATE_ENV")
	status, body = requestExternalRootJSON(t, ts.URL, http.MethodGet, "/api/v1/connectors/example-connector/fields", nil)
	require.Equal(t, http.StatusOK, status, string(body))
	assert.Contains(t, string(body), `"id":"start-date"`)

	status, body = requestExternalRootJSON(t, ts.URL, http.MethodPut, "/api/v1/connectors/example-connector/fields/scheduled_on", map[string]any{"external_field": "start-date"})
	require.Equal(t, http.StatusOK, status, string(body))
	status, body = requestExternalRootJSON(t, ts.URL, http.MethodDelete, "/api/v1/connectors/example-connector/fields/scheduled_on", nil)
	require.Equal(t, http.StatusOK, status, string(body))

	bridgePath := fmt.Sprintf("/api/v1/projects/%d/issues/%s/bridge", project.ID, issue.ShortID)
	status, body = requestExternalRootJSON(t, ts.URL, http.MethodPost, bridgePath, map[string]any{
		"connector": "example-connector", "external": "opaque-locator", "actor": "operator", "publish_comments": true,
	})
	require.Equal(t, http.StatusOK, status, string(body))
	assert.NotContains(t, string(body), "account-1")
	assert.NotContains(t, string(body), "root-1")
	binding, err := database.db.ExternalRootBindingByIssue(t.Context(), issue.ID)
	require.NoError(t, err)
	wakeMu.Lock()
	assert.Contains(t, wakes, binding.ID)
	assert.True(t, committedAtWake)
	wakeMu.Unlock()
	assert.Contains(t, eventTypesFromSink(hookSink.snapshot()), "issue.external_root_bound")
	require.Eventually(t, func() bool {
		select {
		case msg := <-sub.Ch:
			return msg.Event != nil && msg.Event.Type == "issue.external_root_bound"
		default:
			return false
		}
	}, time.Second, 10*time.Millisecond)

	status, body = requestExternalRootJSON(t, ts.URL, http.MethodGet, bridgePath, nil)
	require.Equal(t, http.StatusOK, status, string(body))
	client.readStarted = make(chan struct{})
	client.readRelease = make(chan struct{})
	type manualResponse struct {
		status int
		body   []byte
		err    error
	}
	manualDone := make(chan manualResponse, 1)
	go func() {
		encoded := bytes.NewBufferString(`{"actor":"operator"}`)
		request, requestErr := http.NewRequestWithContext(t.Context(), http.MethodPost, ts.URL+bridgePath+"/actions/reconcile", encoded)
		if requestErr != nil {
			manualDone <- manualResponse{err: requestErr}
			return
		}
		request.Header.Set("Content-Type", "application/json")
		response, requestErr := http.DefaultClient.Do(request)
		if requestErr != nil {
			manualDone <- manualResponse{err: requestErr}
			return
		}
		defer func() { _ = response.Body.Close() }()
		responseBody, readErr := io.ReadAll(response.Body)
		manualDone <- manualResponse{status: response.StatusCode, body: responseBody, err: readErr}
	}()
	select {
	case <-client.readStarted:
	case <-time.After(time.Second):
		t.Fatal("manual reconcile did not reach connector read")
	}
	select {
	case early := <-manualDone:
		t.Fatalf("long-running manual reconcile returned before connector completed: %#v", early)
	case <-time.After(100 * time.Millisecond):
	}
	close(client.readRelease)
	manual := <-manualDone
	require.NoError(t, manual.err)
	require.Equal(t, http.StatusOK, manual.status, string(manual.body))
	client.readStarted = nil
	client.readRelease = nil
	status, body = requestExternalRootJSON(t, ts.URL, http.MethodPost, "/api/v1/connectors/example-connector/actions/reconcile-root", map[string]any{"root_key": "root-1"})
	require.Equal(t, http.StatusOK, status, string(body))
	status, missingBody := requestExternalRootJSON(t, ts.URL, http.MethodPost, "/api/v1/connectors/other/actions/reconcile-root", map[string]any{"root_key": "root-1"})
	require.Equal(t, http.StatusNotFound, status, string(missingBody))
	status, otherMissingBody := requestExternalRootJSON(t, ts.URL, http.MethodPost, "/api/v1/connectors/example-connector/actions/reconcile-root", map[string]any{"root_key": "missing-root"})
	require.Equal(t, http.StatusNotFound, status, string(otherMissingBody))
	assert.Equal(t, string(missingBody), string(otherMissingBody))

	status, body = requestExternalRootJSON(t, ts.URL, http.MethodPut, "/api/v1/connectors/example-connector/fields/scheduled_on", map[string]any{"external_field": "start-date"})
	require.Equal(t, http.StatusOK, status, string(body))
	beforeRetained := len(hookSink.snapshot())
	client.root.Title = "External title after retry"
	client.root.Revision = "revision-2"
	client.listFieldsErr = errors.New("opaque connector diagnostic")
	status, body = requestExternalRootJSON(t, ts.URL, http.MethodPost, bridgePath+"/actions/reconcile", map[string]any{"actor": "operator"})
	require.Equal(t, http.StatusBadGateway, status, string(body))
	assert.NotContains(t, string(body), "opaque connector diagnostic")
	require.Greater(t, len(hookSink.snapshot()), beforeRetained, "committed projection events must survive a later connector error")
	client.listFieldsErr = nil
	_, err = database.db.PatchIssueMetadata(t.Context(), db.PatchIssueMetadataIn{
		IssueID: issue.ID, Actor: "operator", Patch: map[string]json.RawMessage{"scheduled_on": json.RawMessage(`"2026-08-21"`)},
	})
	require.NoError(t, err)
	client.fieldValues["start-date"] = connector.FieldValue{Kind: "date", Value: "2026-08-22"}
	mappings, err := database.db.ListExternalFieldMappings(t.Context(), "example-connector")
	require.NoError(t, err)
	activeMapping := mappings[len(mappings)-1]
	claimed, ok, err := database.db.ClaimExternalRootBindingForManualReconcile(t.Context(), binding.ID, "field-claim", now, now.Add(-5*time.Minute))
	require.NoError(t, err)
	require.True(t, ok)
	_, _, err = database.db.UpsertExternalFieldState(t.Context(), db.ExternalFieldStateParams{
		BindingID: binding.ID, MappingID: activeMapping.ID, ClaimToken: claimed.ClaimToken,
		Baseline:         json.RawMessage(`{"kind":"date","value":"2026-08-20"}`),
		ConflictKata:     json.RawMessage(`{"kind":"date","value":"2026-08-21"}`),
		ConflictExternal: json.RawMessage(`{"kind":"date","value":"2026-08-22"}`),
		Conflicted:       true, At: now, Actor: "connector:example-connector",
	})
	require.NoError(t, err)
	_, err = database.db.ReleaseExternalRootClaim(t.Context(), binding.ID, claimed.ClaimToken)
	require.NoError(t, err)
	status, body = requestExternalRootJSON(t, ts.URL, http.MethodGet, bridgePath, nil)
	require.Equal(t, http.StatusOK, status, string(body))
	assert.Contains(t, string(body), `"kata_candidate":{"kind":"date","value":"2026-08-21"}`)
	assert.Contains(t, string(body), `"external_candidate":{"kind":"date","value":"2026-08-22"}`)
	status, body = requestExternalRootJSON(t, ts.URL, http.MethodDelete, "/api/v1/connectors/example-connector/fields/scheduled_on", nil)
	require.Equal(t, http.StatusOK, status, string(body))
	status, body = requestExternalRootJSON(t, ts.URL, http.MethodGet, bridgePath, nil)
	require.Equal(t, http.StatusOK, status, string(body))
	assert.NotContains(t, string(body), `"field_conflicts"`, "unmapping must hide historical conflicts")
	status, body = requestExternalRootJSON(t, ts.URL, http.MethodPut, "/api/v1/connectors/example-connector/fields/scheduled_on", map[string]any{"external_field": "start-date"})
	require.Equal(t, http.StatusOK, status, string(body))
	status, body = requestExternalRootJSON(t, ts.URL, http.MethodGet, bridgePath, nil)
	require.Equal(t, http.StatusOK, status, string(body))
	assert.NotContains(t, string(body), `"field_conflicts"`, "remapping must not expose a prior revision's conflict")
	mappings, err = database.db.ListExternalFieldMappings(t.Context(), "example-connector")
	require.NoError(t, err)
	activeMapping = mappings[len(mappings)-1]
	claimed, ok, err = database.db.ClaimExternalRootBindingForManualReconcile(t.Context(), binding.ID, "remapped-field-claim", now, now.Add(-5*time.Minute))
	require.NoError(t, err)
	require.True(t, ok)
	_, _, err = database.db.UpsertExternalFieldState(t.Context(), db.ExternalFieldStateParams{
		BindingID: binding.ID, MappingID: activeMapping.ID, ClaimToken: claimed.ClaimToken,
		Baseline:         json.RawMessage(`{"kind":"date","value":"2026-08-20"}`),
		ConflictKata:     json.RawMessage(`{"kind":"date","value":"2026-08-21"}`),
		ConflictExternal: json.RawMessage(`{"kind":"date","value":"2026-08-22"}`),
		Conflicted:       true, At: now, Actor: "connector:example-connector",
	})
	require.NoError(t, err)
	_, err = database.db.ReleaseExternalRootClaim(t.Context(), binding.ID, claimed.ClaimToken)
	require.NoError(t, err)
	status, body = requestExternalRootJSON(t, ts.URL, http.MethodPost, bridgePath+"/actions/resolve-field", map[string]any{
		"kata_field": "scheduled_on", "use": "kata", "actor": "operator",
	})
	require.Equal(t, http.StatusOK, status, string(body))
	assert.Equal(t, "2026-08-21", client.fieldValues["start-date"].Value)

	local, _, err := database.db.CreateComment(t.Context(), db.CreateCommentParams{IssueID: issue.ID, Author: "operator", Body: "Adopt me"})
	require.NoError(t, err)
	claimed, ok, err = database.db.ClaimExternalRootBindingForManualReconcile(t.Context(), binding.ID, "pending-claim", now, now.Add(-5*time.Minute))
	require.NoError(t, err)
	require.True(t, ok)
	_, err = database.db.SetPendingExternalComment(t.Context(), db.SetPendingExternalCommentParams{
		BindingID: binding.ID, ClaimToken: claimed.ClaimToken, CommentUID: local.UID, At: now,
	})
	require.NoError(t, err)
	_, _, err = database.db.PauseExternalRootBinding(t.Context(), db.ExternalRootActionParams{BindingID: binding.ID, Actor: "operator", Reason: "operator_pause"})
	require.NoError(t, err)
	client.comments = []connector.Comment{{
		ID: "external-comment-1", Revision: "comment-revision-1", Body: local.Body,
		Author:    connector.Actor{ID: "self-1"},
		CreatedAt: now.Add(time.Second), UpdatedAt: now.Add(time.Second),
	}}
	status, body = requestExternalRootJSON(t, ts.URL, http.MethodPost, bridgePath+"/actions/resolve-comment", map[string]any{
		"action": "adopt", "external_comment_id": "external-comment-1", "actor": "operator",
	})
	require.Equal(t, http.StatusOK, status, string(body))
	assert.Contains(t, string(body), `"enabled":false`)
	status, body = requestExternalRootJSON(t, ts.URL, http.MethodPost, bridgePath+"/actions/resume", map[string]any{"actor": "operator"})
	require.Equal(t, http.StatusOK, status, string(body))
	status, body = requestExternalRootJSON(t, ts.URL, http.MethodPost, bridgePath+"/actions/pause", map[string]any{"actor": "operator", "reason": "operator_pause"})
	require.Equal(t, http.StatusOK, status, string(body))
	status, body = requestExternalRootJSON(t, ts.URL, http.MethodPost, bridgePath+"/actions/resume", map[string]any{"actor": "operator"})
	require.Equal(t, http.StatusOK, status, string(body))
	_, _, err = database.db.RemoveProject(t.Context(), db.RemoveProjectParams{
		ProjectID: project.ID, Actor: "operator", Force: true,
	})
	require.NoError(t, err)
	status, body = requestExternalRootJSON(t, ts.URL, http.MethodDelete, bridgePath+"?actor=operator", nil)
	require.Equal(t, http.StatusOK, status, string(body))
	assert.Contains(t, string(body), `"active":false`)
}

func TestExternalRootHandlersClassifyFieldAndPublishingErrors(t *testing.T) {
	database := openTestDB(t)
	project, err := database.db.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	issue, _, err := database.db.CreateIssue(t.Context(), db.CreateIssueParams{
		ProjectID: project.ID, Title: "Local title", Author: "tester",
	})
	require.NoError(t, err)
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	client := &daemonExternalRootClient{
		description: connector.Description{
			ConnectorID: "example.connector", DisplayName: "Example connector",
			Protocol: connector.ProtocolVersion, AccountIdentity: "account-1",
			Capabilities: []connector.Capability{
				connector.CapabilityPublishComment,
				connector.CapabilityFields,
				connector.CapabilityConditionalFields,
			},
			SelfActorID: "self-1",
		},
		root: connector.Root{
			Key: "root-1", IdentityKey: "account-1", Title: "External title", State: "open",
			Revision: "revision-1", UpdatedAt: now, ObservedAt: now,
		},
		fields: []connector.FieldDescriptor{
			{ID: "start-date", DisplayName: "Start date", AcceptedKinds: []string{"date"}, Nullable: true, Writable: true, SchemaRevision: "schema-1"},
			{ID: "readonly-text", DisplayName: "Read only", AcceptedKinds: []string{"string"}, Nullable: false, Writable: false, SchemaRevision: "schema-1"},
		},
		fieldValues: map[string]connector.FieldValue{},
	}
	registry, err := rootbridge.NewRegistry(t.Context(), []config.ConnectorConfig{{
		ID: "example-connector", Command: testExternalRootCommand(t),
	}}, func(config.ConnectorConfig) connectorclient.Client { return client })
	require.NoError(t, err)
	reconciler := rootbridge.NewReconciler(database.db, registry, rootbridge.ReconcilerConfig{Now: func() time.Time { return now }})
	service := rootbridge.NewService(database.db, registry,
		func(ctx context.Context, bindingID int64, claimToken string) ([]db.Event, error) {
			result, err := reconciler.Run(ctx, bindingID, rootbridge.RunOptions{ClaimToken: claimToken})
			return result.Events, err
		})
	ts := startTestServer(t, daemon.ServerConfig{
		DB: database.db, StartedAt: now, ExternalRootRegistry: registry,
		ExternalRootService: service, ExternalRootReconciler: reconciler,
	})

	status, body := requestExternalRootJSON(t, ts.URL, http.MethodPut,
		"/api/v1/connectors/example-connector/fields/scheduled_on", map[string]any{"external_field": "missing-private-selector"})
	assert.Equal(t, http.StatusNotFound, status, string(body))
	assert.Contains(t, string(body), `"code":"external_field_not_found"`)
	assert.NotContains(t, string(body), "missing-private-selector")

	status, body = requestExternalRootJSON(t, ts.URL, http.MethodPut,
		"/api/v1/connectors/example-connector/fields/unsupported_field", map[string]any{"external_field": "start-date"})
	assert.Equal(t, http.StatusBadRequest, status, string(body))
	assert.Contains(t, string(body), `"code":"validation"`)
	assert.NotContains(t, string(body), "unsupported_field")

	status, body = requestExternalRootJSON(t, ts.URL, http.MethodPut,
		"/api/v1/connectors/example-connector/fields/scheduled_on", map[string]any{"external_field": "readonly-text"})
	assert.Equal(t, http.StatusBadRequest, status, string(body))
	assert.Contains(t, string(body), `"code":"validation"`)
	assert.NotContains(t, string(body), "readonly-text")

	bridgePath := fmt.Sprintf("/api/v1/projects/%d/issues/%s/bridge", project.ID, issue.ShortID)
	client.description.Capabilities = []connector.Capability{
		connector.CapabilityFields,
		connector.CapabilityConditionalFields,
	}
	status, body = requestExternalRootJSON(t, ts.URL, http.MethodPost, bridgePath, map[string]any{
		"connector": "example-connector", "external": "opaque-private-locator", "actor": "operator", "publish_comments": true,
	})
	assert.Equal(t, http.StatusConflict, status, string(body))
	assert.Contains(t, string(body), `"code":"external_root_conflict"`)
	assert.NotContains(t, string(body), "opaque-private-locator")

	client.description.Capabilities = []connector.Capability{
		connector.CapabilityPublishComment,
		connector.CapabilityFields,
		connector.CapabilityConditionalFields,
	}
	status, body = requestExternalRootJSON(t, ts.URL, http.MethodPost, bridgePath, map[string]any{
		"connector": "example-connector", "external": "opaque-locator", "actor": "operator", "publish_comments": true,
	})
	require.Equal(t, http.StatusOK, status, string(body))
	status, body = requestExternalRootJSON(t, ts.URL, http.MethodPost, bridgePath+"/actions/pause", map[string]any{
		"actor": "operator", "reason": "operator_pause",
	})
	require.Equal(t, http.StatusOK, status, string(body))
	client.description.Capabilities = []connector.Capability{
		connector.CapabilityFields,
		connector.CapabilityConditionalFields,
	}
	status, body = requestExternalRootJSON(t, ts.URL, http.MethodPost, bridgePath+"/actions/resume", map[string]any{"actor": "operator"})
	assert.Equal(t, http.StatusConflict, status, string(body))
	assert.Contains(t, string(body), `"code":"external_root_conflict"`)
}

func TestExternalRootHandlersClassifyConnectorAndInternalFailures(t *testing.T) {
	const rawDiagnostic = "opaque child path stderr credential detail"
	for _, tc := range []struct {
		name    string
		err     error
		message string
	}{
		{
			name:    "typed connector error",
			err:     &connector.Error{Code: "remote_failed", Message: rawDiagnostic},
			message: "external connector request failed",
		},
		{
			name:    "process execution error",
			err:     errors.Join(connectorclient.ErrProcessFailure, errors.New(rawDiagnostic)),
			message: "external connector process failed",
		},
		{
			name:    "unsupported protocol",
			err:     errors.Join(connectorclient.ErrProtocolFailure, errors.New(rawDiagnostic)),
			message: "external connector protocol failed",
		},
		{
			name:    "child timeout",
			err:     errors.Join(connectorclient.ErrRequestTimeout, context.DeadlineExceeded, errors.New(rawDiagnostic)),
			message: "external connector request timed out",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database := openTestDB(t)
			client := &daemonExternalRootClient{
				description: connector.Description{
					ConnectorID: "example.connector", DisplayName: "Example connector",
					Protocol: connector.ProtocolVersion, AccountIdentity: "account-1",
					Capabilities: []connector.Capability{
						connector.CapabilityFields,
						connector.CapabilityConditionalFields,
					},
				},
				listFieldsErr: tc.err,
			}
			registry, err := rootbridge.NewRegistry(t.Context(), []config.ConnectorConfig{{
				ID: "example-connector", Command: testExternalRootCommand(t),
			}}, func(config.ConnectorConfig) connectorclient.Client { return client })
			require.NoError(t, err)
			ts := startTestServer(t, daemon.ServerConfig{
				DB: database.db, StartedAt: database.now, ExternalRootRegistry: registry,
			})

			status, body := requestExternalRootJSON(t, ts.URL, http.MethodGet, "/api/v1/connectors/example-connector/fields", nil)
			assertAPIError(t, status, body, http.StatusBadGateway, "connector_error")
			assert.Contains(t, string(body), `"message":"`+tc.message+`"`)
			assert.NotContains(t, string(body), rawDiagnostic)
			assert.NotContains(t, string(body), "opaque")
		})
	}

	for _, tc := range []struct {
		name      string
		states    []db.ExternalFieldState
		statesErr error
		private   string
	}{
		{name: "projection storage error", statesErr: errors.New("opaque storage detail"), private: "opaque storage detail"},
		{name: "context cancellation", statesErr: context.Canceled},
		{
			name: "malformed persisted field candidate",
			states: []db.ExternalFieldState{{
				BindingID: 1, MappingID: 1, Conflicted: true,
				ConflictKata: json.RawMessage(`{"kind":`),
			}},
			private: `{"kind":`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database := openTestDB(t)
			project, err := database.db.CreateProject(t.Context(), "example-project")
			require.NoError(t, err)
			issue, _, err := database.db.CreateIssue(t.Context(), db.CreateIssueParams{
				ProjectID: project.ID, Title: "Local title", Author: "tester",
			})
			require.NoError(t, err)
			binding, _, err := database.db.CreateExternalRootBinding(t.Context(), db.CreateExternalRootBindingParams{
				ProjectID: project.ID, IssueID: issue.ID, ConnectorInstance: "example-connector",
				ExternalRootKey: "root-1", ExternalAccountKey: "account-1", Actor: "tester",
				ReceiveCommentsAfter: database.now,
			})
			require.NoError(t, err)
			states := append([]db.ExternalFieldState(nil), tc.states...)
			if len(states) > 0 {
				mapping, mapErr := database.db.UpsertExternalFieldMapping(t.Context(), db.ExternalFieldMappingParams{
					ConnectorInstance: "example-connector", KataField: "scheduled_on",
					ExternalFieldID: "schedule", ExternalFieldName: "Schedule",
					AcceptedKinds: []string{"date"}, Nullable: true, Writable: true, SchemaRevision: "schema-1",
				})
				require.NoError(t, mapErr)
				for i := range states {
					states[i].MappingID = mapping.ID
				}
			}
			for i := range states {
				states[i].BindingID = binding.ID
			}
			store := &externalRootProjectionFailureStore{
				Storage: database.db, states: states, err: tc.statesErr,
			}
			ts := startTestServer(t, daemon.ServerConfig{DB: store, StartedAt: database.now})
			path := fmt.Sprintf("/api/v1/projects/%d/issues/%s/bridge", project.ID, issue.ShortID)

			status, body := requestExternalRootJSON(t, ts.URL, http.MethodGet, path, nil)
			assertAPIError(t, status, body, http.StatusInternalServerError, "internal")
			if tc.private != "" {
				assert.NotContains(t, string(body), tc.private)
			}
		})
	}
}

func requestExternalRootJSON(t *testing.T, baseURL, method, path string, body any) (int, []byte) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		require.NoError(t, err)
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(t.Context(), method, baseURL+path, reader)
	require.NoError(t, err)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := http.DefaultClient.Do(request)
	require.NoError(t, err)
	defer func() { require.NoError(t, response.Body.Close()) }()
	responseBody, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	return response.StatusCode, responseBody
}

func eventTypesFromSink(events []db.Event) []string {
	types := make([]string, len(events))
	for i := range events {
		types[i] = events[i].Type
	}
	return types
}

type daemonExternalRootClient struct {
	description   connector.Description
	root          connector.Root
	comments      []connector.Comment
	fields        []connector.FieldDescriptor
	fieldValues   map[string]connector.FieldValue
	listFieldsErr error
	readStarted   chan struct{}
	readRelease   chan struct{}
	readOnce      sync.Once
}

type externalRootProjectionFailureStore struct {
	db.Storage
	states []db.ExternalFieldState
	err    error
}

func (s *externalRootProjectionFailureStore) ExternalFieldStates(
	context.Context,
	int64,
) ([]db.ExternalFieldState, error) {
	return append([]db.ExternalFieldState(nil), s.states...), s.err
}

func (c *daemonExternalRootClient) Describe(context.Context) (connector.Description, error) {
	return c.description, nil
}
func (c *daemonExternalRootClient) ResolveRoot(context.Context, connector.ResolveRootParams) (connector.Root, error) {
	return c.root, nil
}

func (c *daemonExternalRootClient) ReadRoot(ctx context.Context, _ connector.ReadRootParams) (connector.Root, error) {
	if c.readRelease != nil {
		c.readOnce.Do(func() {
			if c.readStarted != nil {
				close(c.readStarted)
			}
		})
		select {
		case <-c.readRelease:
		case <-ctx.Done():
			return connector.Root{}, ctx.Err()
		}
	}
	return c.root, nil
}
func (c *daemonExternalRootClient) ListComments(context.Context, connector.ListCommentsParams) (connector.ListCommentsResult, error) {
	return connector.ListCommentsResult{Comments: c.comments}, nil
}
func (c *daemonExternalRootClient) CompleteRoot(context.Context, connector.CompleteRootParams) (connector.Root, error) {
	c.root.State = "complete"
	return c.root, nil
}
func (c *daemonExternalRootClient) PublishComment(context.Context, connector.PublishCommentParams) (connector.Comment, error) {
	return connector.Comment{}, nil
}
func (c *daemonExternalRootClient) ListFields(context.Context) (connector.ListFieldsResult, error) {
	return connector.ListFieldsResult{Fields: c.fields}, c.listFieldsErr
}
func (c *daemonExternalRootClient) ReadFields(_ context.Context, params connector.ReadFieldsParams) (connector.ReadFieldsResult, error) {
	result := connector.ReadFieldsResult{Fields: map[string]connector.FieldValue{}}
	for _, id := range params.FieldIDs {
		if value, ok := c.fieldValues[id]; ok {
			result.Fields[id] = value
		}
	}
	return result, nil
}
func (c *daemonExternalRootClient) WriteFields(_ context.Context, params connector.WriteFieldsParams) (connector.WriteFieldsResult, error) {
	maps.Copy(c.fieldValues, params.Fields)
	return connector.WriteFieldsResult{Fields: params.Fields}, nil
}

// TestArchivedProjectPendingCommentCleanupCoversPurge exercises the full
// cleanup chain after project archival: a pending publication must be
// resolvable, the bridge unbindable, and the project purgeable, all without
// restoring the project.
func TestArchivedProjectPendingCommentCleanupCoversPurge(t *testing.T) {
	database := openTestDB(t)
	project, err := database.db.CreateProject(t.Context(), "archived-cleanup")
	require.NoError(t, err)
	issue, _, err := database.db.CreateIssue(t.Context(), db.CreateIssueParams{
		ProjectID: project.ID, Title: "Archived cleanup", Author: "tester",
	})
	require.NoError(t, err)
	comment, _, err := database.db.CreateComment(t.Context(), db.CreateCommentParams{
		IssueID: issue.ID, Author: "tester", Body: "Pending publication",
	})
	require.NoError(t, err)
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	client := &daemonExternalRootClient{
		description: connector.Description{
			ConnectorID: "example.connector", DisplayName: "Example connector",
			Protocol: connector.ProtocolVersion, AccountIdentity: "account-1",
			Capabilities: []connector.Capability{connector.CapabilityPublishComment},
			SelfActorID:  "self-1",
		},
	}
	registry, err := rootbridge.NewRegistry(t.Context(), []config.ConnectorConfig{{
		ID: "example-connector", Command: testExternalRootCommand(t),
	}}, func(config.ConnectorConfig) connectorclient.Client { return client })
	require.NoError(t, err)
	service := rootbridge.NewServiceWithEventSink(
		database.db,
		registry,
		func(_ context.Context, _ int64, _ string) ([]db.Event, error) {
			return nil, nil
		},
		func(db.Event) {},
	)
	ts := startTestServer(t, daemon.ServerConfig{
		DB: database.db, StartedAt: now,
		ExternalRootRegistry: registry, ExternalRootService: service,
	})

	binding, _, err := database.db.CreateExternalRootBinding(t.Context(), db.CreateExternalRootBindingParams{
		ProjectID: project.ID, IssueID: issue.ID,
		ConnectorInstance: "example-connector",
		ExternalRootKey:   "root-1", ExternalAccountKey: "account-1",
		Actor: "tester", ReceiveCommentsAfter: now,
		PublishComments: true, PublishCommentsAfter: &now,
	})
	require.NoError(t, err)
	claimed, ok, err := database.db.ClaimExternalRootBinding(
		t.Context(), binding.ID, "cleanup-claim", now, now.Add(-time.Minute),
	)
	require.NoError(t, err)
	require.True(t, ok)
	_, err = database.db.SetPendingExternalComment(t.Context(), db.SetPendingExternalCommentParams{
		BindingID: binding.ID, ClaimToken: claimed.ClaimToken,
		CommentUID: comment.UID, At: now,
	})
	require.NoError(t, err)

	_, _, err = database.db.RemoveProject(t.Context(), db.RemoveProjectParams{
		ProjectID: project.ID, Actor: "tester", Force: true,
	})
	require.NoError(t, err)

	resolvePath := fmt.Sprintf("/api/v1/projects/%d/issues/%s/bridge/actions/resolve-comment",
		project.ID, issue.ShortID)
	status, body := requestExternalRootJSON(t, ts.URL, http.MethodPost, resolvePath, map[string]any{
		"action": "skip", "actor": "tester",
	})
	require.Equalf(t, http.StatusOK, status, "archived resolve-comment: %s", string(body))

	unbindPath := fmt.Sprintf("/api/v1/projects/%d/issues/%s/bridge", project.ID, issue.ShortID)
	status, body = requestExternalRootJSON(t, ts.URL, http.MethodDelete, unbindPath+"?actor=tester", nil)
	require.Equalf(t, http.StatusOK, status, "archived unbind: %s", string(body))

	purgePath := fmt.Sprintf("/api/v1/projects/%d/actions/purge", project.ID)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, ts.URL+purgePath,
		strings.NewReader(`{"actor":"tester"}`))
	req.Header.Set("Content-Type", "application/json")
	require.NoError(t, err)
	req.Header.Set("X-Kata-Confirm", "PURGE archived-cleanup")
	resp, err := ts.Client().Do(req) //nolint:gosec // G704: test request to httptest server URL
	require.NoError(t, err)
	purgeBody, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Equalf(t, http.StatusOK, resp.StatusCode, "archived purge: %s", string(purgeBody))
}
