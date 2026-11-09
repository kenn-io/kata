package daemon_test

import (
	"context"
	"encoding/json/jsontext"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	connectorclient "go.kenn.io/kata/internal/connector"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/rootbridge"
	"go.kenn.io/kata/pkg/connector"
)

type historicalBridgeFixture struct {
	store    *sqlitestore.Store
	target   db.Project
	issue    db.Issue
	path     string
	client   *daemonExternalRootClient
	service  *rootbridge.Service
	registry *rootbridge.Registry
	server   *httptest.Server
}

func newHistoricalBridgeFixture(t *testing.T) historicalBridgeFixture {
	t.Helper()
	database := openTestDB(t)
	source, err := database.db.CreateProject(t.Context(), "original-project")
	require.NoError(t, err)
	target, err := database.db.CreateProject(t.Context(), "current-project")
	require.NoError(t, err)
	issue, _, err := database.db.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: source.ID, Title: "Moved task", Author: "operator"})
	require.NoError(t, err)
	path := fmt.Sprintf("/api/v1/projects/%d/issues/%s/bridge", source.ID, issue.ShortID)
	moved, err := database.db.MoveIssueProject(t.Context(), db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: source.ID, ToProjectID: target.ID, IfMatchRev: issue.Revision, Actor: "operator"})
	require.NoError(t, err)
	now := time.Now().UTC().Truncate(time.Millisecond)
	client := &daemonExternalRootClient{
		description: connector.Description{ConnectorID: "example.connector", DisplayName: "Example connector", Protocol: connector.ProtocolVersion, AccountIdentity: "account-1", Capabilities: []connector.Capability{connector.CapabilityFields, connector.CapabilityConditionalFields, connector.CapabilityPublishComment}, SelfActorID: "self-1"},
		root:        connector.Root{Key: "root-1", IdentityKey: "account-1", Title: "External task", State: "open", Revision: "revision-1", UpdatedAt: now, ObservedAt: now},
		fields:      []connector.FieldDescriptor{{ID: "start-date", DisplayName: "Start date", AcceptedKinds: []string{"date"}, Nullable: true, Writable: true, SchemaRevision: "schema-1"}},
		fieldValues: map[string]connector.FieldValue{},
	}
	registry, err := rootbridge.NewRegistry(t.Context(), []config.ConnectorConfig{{ID: "example-connector", Command: testExternalRootCommand(t)}}, func(config.ConnectorConfig) connectorclient.Client { return client })
	require.NoError(t, err)
	reconciler := rootbridge.NewReconciler(database.db, registry, rootbridge.ReconcilerConfig{})
	service := rootbridge.NewService(database.db, registry, func(ctx context.Context, id int64, token string) ([]db.Event, error) {
		result, err := reconciler.Run(ctx, id, rootbridge.RunOptions{ClaimToken: token})
		return result.Events, err
	})
	server := startTestServer(t, daemon.ServerConfig{DB: database.db, ExternalRootService: service, ExternalRootRegistry: registry, ExternalRootReconciler: reconciler})
	return historicalBridgeFixture{store: database.db, target: target, issue: moved.Issue, path: path, client: client, service: service, registry: registry, server: server}
}

func (f historicalBridgeFixture) bind(t *testing.T) db.ExternalRootBinding {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Millisecond)
	binding, _, err := f.store.CreateExternalRootBinding(t.Context(), db.CreateExternalRootBindingParams{ProjectID: f.target.ID, IssueID: f.issue.ID, ConnectorInstance: "example-connector", ExternalRootKey: "root-1", ExternalAccountKey: "account-1", Actor: "operator", ReceiveCommentsAfter: now, PublishComments: true, PublishCommentsAfter: &now})
	require.NoError(t, err)
	return binding
}

func (f historicalBridgeFixture) pendingComment(t *testing.T, binding db.ExternalRootBinding) {
	t.Helper()
	comment, _, err := f.store.CreateComment(t.Context(), db.CreateCommentParams{IssueID: f.issue.ID, Author: "operator", Body: "Pending publication"})
	require.NoError(t, err)
	now := time.Now().UTC().Truncate(time.Millisecond)
	claimed, ok, err := f.store.ClaimExternalRootBindingForManualReconcile(t.Context(), binding.ID, "pending-seed", now, now.Add(-time.Minute))
	require.NoError(t, err)
	require.True(t, ok)
	_, err = f.store.SetPendingExternalComment(t.Context(), db.SetPendingExternalCommentParams{BindingID: binding.ID, ClaimToken: claimed.ClaimToken, CommentUID: comment.UID, At: now})
	require.NoError(t, err)
	_, err = f.store.ReleaseExternalRootClaim(t.Context(), binding.ID, claimed.ClaimToken)
	require.NoError(t, err)
}

func (f historicalBridgeFixture) fieldConflict(t *testing.T, binding db.ExternalRootBinding) {
	t.Helper()
	mapping, err := f.service.MapField(t.Context(), rootbridge.MapFieldParams{ConnectorInstance: "example-connector", KataField: "scheduled_on", ExternalField: "start-date"})
	require.NoError(t, err)
	_, err = f.store.PatchIssueMetadata(t.Context(), db.PatchIssueMetadataIn{IssueID: f.issue.ID, Actor: "operator", Patch: map[string]jsontext.Value{"scheduled_on": jsontext.Value(`"2026-08-21"`)}})
	require.NoError(t, err)
	f.client.fieldValues["start-date"] = connector.FieldValue{Kind: "date", Value: "2026-08-22"}
	now := time.Now().UTC().Truncate(time.Millisecond)
	claimed, ok, err := f.store.ClaimExternalRootBindingForManualReconcile(t.Context(), binding.ID, "field-seed", now, now.Add(-time.Minute))
	require.NoError(t, err)
	require.True(t, ok)
	_, _, err = f.store.UpsertExternalFieldState(t.Context(), db.ExternalFieldStateParams{BindingID: binding.ID, MappingID: mapping.ID, ClaimToken: claimed.ClaimToken, Baseline: jsontext.Value(`{"kind":"date","value":"2026-08-20"}`), ConflictKata: jsontext.Value(`{"kind":"date","value":"2026-08-21"}`), ConflictExternal: jsontext.Value(`{"kind":"date","value":"2026-08-22"}`), Conflicted: true, At: now, Actor: "connector:example-connector"})
	require.NoError(t, err)
	_, err = f.store.ReleaseExternalRootClaim(t.Context(), binding.ID, claimed.ClaimToken)
	require.NoError(t, err)
}

func TestExternalRootHistoricalRouteCreation(t *testing.T) {
	f := newHistoricalBridgeFixture(t)
	status, body := requestExternalRootJSON(t, f.server.URL, http.MethodPost, f.path, map[string]any{"actor": "operator", "connector": "example-connector", "external": "opaque-locator"})
	require.Equal(t, http.StatusOK, status, string(body))
	binding, err := f.store.ExternalRootBindingByIssue(t.Context(), f.issue.ID)
	require.NoError(t, err)
	assert.Equal(t, f.target.ID, binding.ProjectID)
}

func TestExternalRootHistoricalRouteActions(t *testing.T) {
	for _, tc := range []struct {
		action   string
		archived bool
	}{{action: "pause"}, {action: "resume"}, {action: "resolve-field"}, {action: "resolve-comment"}, {action: "unbind"}, {action: "resolve-comment", archived: true}, {action: "unbind", archived: true}} {
		t.Run(fmt.Sprintf("%s/archived=%t", tc.action, tc.archived), func(t *testing.T) {
			f := newHistoricalBridgeFixture(t)
			binding := f.bind(t)
			body := map[string]any{"actor": "operator"}
			method := http.MethodPost
			path := f.path + "/actions/" + tc.action
			switch tc.action {
			case "pause":
				body["reason"] = "operator_pause"
			case "resume":
				_, _, err := f.store.PauseExternalRootBinding(t.Context(), db.ExternalRootActionParams{BindingID: binding.ID, Actor: "operator", Reason: "operator_pause"})
				require.NoError(t, err)
			case "resolve-field":
				f.fieldConflict(t, binding)
				body["kata_field"] = "scheduled_on"
				body["use"] = "kata"
			case "resolve-comment":
				f.pendingComment(t, binding)
				body["action"] = "skip"
			case "unbind":
				method = http.MethodDelete
				path = f.path + "?actor=operator"
				body = nil
			}
			if tc.archived {
				_, _, err := f.store.RemoveProject(t.Context(), db.RemoveProjectParams{ProjectID: f.target.ID, Actor: "operator", Force: true})
				require.NoError(t, err)
			}
			status, raw := requestExternalRootJSON(t, f.server.URL, method, path, body)
			require.Equal(t, http.StatusOK, status, string(raw))
			current, err := f.store.ExternalRootBindingByID(t.Context(), binding.ID)
			require.NoError(t, err)
			assert.Equal(t, f.target.ID, current.ProjectID)
			switch tc.action {
			case "pause":
				assert.False(t, current.Enabled)
			case "resume":
				assert.True(t, current.Enabled)
			case "unbind":
				assert.False(t, current.Active)
			case "resolve-comment":
				assert.Empty(t, current.PendingCommentUID)
			case "resolve-field":
				states, err := f.store.ExternalFieldStates(t.Context(), binding.ID)
				require.NoError(t, err)
				require.Len(t, states, 1)
				assert.False(t, states[0].Conflicted)
				assert.Equal(t, "2026-08-21", f.client.fieldValues["start-date"].Value)
			}
		})
	}
}

func TestExternalRootHistoricalRouteCurrentProjectAuthorization(t *testing.T) {
	for _, action := range []string{"bind", "pause", "resolve-comment", "unbind"} {
		t.Run(action, func(t *testing.T) {
			f := newHistoricalBridgeFixture(t)
			var binding db.ExternalRootBinding
			if action != "bind" {
				binding = f.bind(t)
			}
			if action == "resolve-comment" {
				f.pendingComment(t, binding)
			}
			server := daemon.NewServer(daemon.ServerConfig{DB: f.store, ExternalRootService: f.service, ExternalRootRegistry: f.registry, HostAccess: commentProjectHostAccess{deniedProjectID: f.target.ID}})
			t.Cleanup(func() { require.NoError(t, server.Close()) })
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				ctx := daemon.WithPrincipal(r.Context(), daemon.Principal{Kind: daemon.PrincipalHost, Subject: "host-user", Actor: "operator"})
				server.Handler().ServeHTTP(w, r.WithContext(ctx))
			}))
			t.Cleanup(ts.Close)
			path := f.path
			method := http.MethodPost
			body := map[string]any{"actor": "operator"}
			switch action {
			case "bind":
				body["connector"] = "example-connector"
				body["external"] = "opaque-locator"
			case "pause":
				path += "/actions/pause"
			case "resolve-comment":
				path += "/actions/resolve-comment"
				body["action"] = "skip"
			case "unbind":
				method = http.MethodDelete
				path += "?actor=operator"
				body = nil
			}
			status, raw := requestExternalRootJSON(t, ts.URL, method, path, body)
			assertAPIError(t, status, raw, http.StatusNotFound, "not_found")
			current, err := f.store.ExternalRootBindingByIssue(t.Context(), f.issue.ID)
			if action == "bind" {
				assert.ErrorIs(t, err, db.ErrNotFound)
			} else {
				require.NoError(t, err)
				assert.True(t, current.Active)
				assert.True(t, current.Enabled)
				if action == "resolve-comment" {
					assert.NotEmpty(t, current.PendingCommentUID)
				}
			}
		})
	}
}
