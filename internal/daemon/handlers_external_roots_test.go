package daemon

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/rootbridge"
)

func splitRoute(t *testing.T, route string) (string, string) {
	t.Helper()
	parts := strings.SplitN(route, " ", 2)
	require.Len(t, parts, 2)
	return parts[0], parts[1]
}

func operationForMethod(item *huma.PathItem, method string) *huma.Operation {
	switch method {
	case http.MethodGet:
		return item.Get
	case http.MethodPut:
		return item.Put
	case http.MethodPost:
		return item.Post
	case http.MethodDelete:
		return item.Delete
	default:
		return nil
	}
}

func TestExternalRootRoutesRegisterExactOperationIDs(t *testing.T) {
	srv := NewServer(ServerConfig{})
	t.Cleanup(func() { require.NoError(t, srv.Close()) })
	want := map[string]string{
		"GET /api/v1/connectors":                                                         "listConnectors",
		"GET /api/v1/connectors/{instance}":                                              "getConnectorStatus",
		"GET /api/v1/connectors/{instance}/fields":                                       "listConnectorFields",
		"PUT /api/v1/connectors/{instance}/fields/{kata_field}":                          "mapConnectorField",
		"DELETE /api/v1/connectors/{instance}/fields/{kata_field}":                       "unmapConnectorField",
		"POST /api/v1/projects/{project_id}/issues/{ref}/bridge":                         "bindExternalRoot",
		"GET /api/v1/projects/{project_id}/issues/{ref}/bridge":                          "getExternalRootBridge",
		"POST /api/v1/projects/{project_id}/issues/{ref}/bridge/actions/reconcile":       "reconcileExternalRootBridge",
		"POST /api/v1/connectors/{instance}/actions/reconcile-root":                      "reconcileExternalRootByKey",
		"POST /api/v1/projects/{project_id}/issues/{ref}/bridge/actions/pause":           "pauseExternalRootBridge",
		"POST /api/v1/projects/{project_id}/issues/{ref}/bridge/actions/resume":          "resumeExternalRootBridge",
		"POST /api/v1/projects/{project_id}/issues/{ref}/bridge/actions/resolve-field":   "resolveExternalField",
		"POST /api/v1/projects/{project_id}/issues/{ref}/bridge/actions/resolve-comment": "resolveExternalComment",
		"DELETE /api/v1/projects/{project_id}/issues/{ref}/bridge":                       "unbindExternalRoot",
	}
	for route, operationID := range want {
		method, path := splitRoute(t, route)
		item := srv.API().OpenAPI().Paths[path]
		require.NotNil(t, item, route)
		operation := operationForMethod(item, method)
		require.NotNil(t, operation, route)
		assert.Equal(t, operationID, operation.OperationID)
	}
}

func TestExternalRootOperationsAreRestrictedIntegrationAdministration(t *testing.T) {
	for _, operationID := range []string{
		"listConnectors", "getConnectorStatus", "listConnectorFields", "mapConnectorField", "unmapConnectorField",
		"bindExternalRoot", "getExternalRootBridge", "reconcileExternalRootBridge", "reconcileExternalRootByKey",
		"pauseExternalRootBridge", "resumeExternalRootBridge", "resolveExternalField", "resolveExternalComment", "unbindExternalRoot",
	} {
		policy, ok := hostOperationPolicy(operationID)
		require.True(t, ok, operationID)
		assert.Equal(t, hostOperationIntegrationAdministration, policy.Kind, operationID)
		assert.True(t, policy.restricted, operationID)
		if operationID != "listConnectors" && operationID != "getConnectorStatus" && operationID != "listConnectorFields" && operationID != "getExternalRootBridge" {
			assert.True(t, policy.Mutation, operationID)
		}
	}
}

func TestListConnectorsResponseDoesNotExposeExecutableConfiguration(t *testing.T) {
	srv := NewServer(ServerConfig{})
	t.Cleanup(func() { require.NoError(t, srv.Close()) })
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/v1/connectors", nil))
	assert.Equal(t, http.StatusOK, rr.Code)
	assert.NotContains(t, rr.Body.String(), "command")
	assert.NotContains(t, rr.Body.String(), "settings")
	assert.NotContains(t, rr.Body.String(), "environment")
}

func TestExternalRootAdministrationRejectsBrowserAndReadonlyPrincipals(t *testing.T) {
	for _, ctx := range []context.Context{
		withWebSession(context.Background(), Principal{Kind: PrincipalWebLocal, Actor: "operator"}),
		withWebSession(context.Background(), Principal{Kind: PrincipalDBToken, Actor: "operator", TokenID: 7}),
		WithPrincipal(context.Background(), Principal{Kind: PrincipalWebLocal, Actor: "operator"}),
		WithPrincipal(context.Background(), Principal{Kind: PrincipalDBToken, Actor: "operator", TokenID: 7}),
		WithPrincipal(context.Background(), Principal{Kind: PrincipalBootstrap}),
		WithPrincipal(context.Background(), Principal{Kind: PrincipalTrustedProxy, Actor: "operator"}),
		WithPrincipal(context.Background(), Principal{Kind: PrincipalTrustedProxyAbsent}),
		withInsecureReadonlyRequest(context.Background()),
	} {
		err := ensureExternalRootAdministrationAllowed(ctx)
		require.Error(t, err)
		var apiErr interface{ GetStatus() int }
		if errors.As(err, &apiErr) {
			assert.Equal(t, http.StatusForbidden, apiErr.GetStatus())
		}
	}
}

func TestExternalRootAdministrationAllowsOwnerAndIntegrationPrincipals(t *testing.T) {
	for _, ctx := range []context.Context{
		context.Background(),
		WithPrincipal(context.Background(), Principal{Kind: PrincipalStaticToken}),
		WithPrincipal(context.Background(), Principal{Kind: PrincipalHost, Subject: "example-service"}),
	} {
		require.NoError(t, ensureExternalRootAdministrationAllowed(ctx))
	}
}

func TestExternalRootHandlerErrorClassifiesExpectedConflicts(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
		code string
	}{
		{name: "issue already bound", err: db.ErrExternalRootIssueAlreadyBound, code: "external_root_conflict"},
		{name: "root already bound", err: db.ErrExternalRootAlreadyBound, code: "external_root_conflict"},
		{name: "issue sync owns content", err: db.ErrExternalRootIssueSyncConflict, code: "external_root_conflict"},
		{name: "bridge worker is active", err: db.ErrExternalRootClaimActive, code: "external_root_conflict"},
		{name: "connector identity changed", err: rootbridge.ErrConnectorIdentityChanged, code: "external_root_conflict"},
		{name: "fields capability unavailable", err: rootbridge.ErrFieldSynchronizationUnavailable, code: "external_root_conflict"},
		{name: "federated spoke is read only", err: db.ErrFederatedReadOnly, code: "federated_read_only"},
	} {
		t.Run(test.name, func(t *testing.T) {
			classified := externalRootHandlerError(errors.Join(test.err, errors.New("private cause")))
			var apiErr *api.APIError
			require.ErrorAs(t, classified, &apiErr)
			assert.Equal(t, http.StatusConflict, apiErr.Status)
			assert.Equal(t, test.code, apiErr.Code)
			assert.NotContains(t, apiErr.Message, "private cause")
		})
	}
}

func TestExternalRootAdministrationRejectsUnauthenticatedPrivateNetworkClient(t *testing.T) {
	srv := NewServer(ServerConfig{Auth: config.AuthConfig{AllowUnauthenticatedPrivateNetworkWrites: true}})
	t.Cleanup(func() { require.NoError(t, srv.Close()) })
	handler, err := srv.HandlerFor(ListenerPolicy{
		Kind: ListenerSharedTCP, Origin: "http://100.64.0.5:7777", BackendAuthority: "100.64.0.5:7777",
	})
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodGet, "http://100.64.0.5:7777/api/v1/connectors", nil)
	req.Host = "100.64.0.5:7777"
	req.RemoteAddr = "100.64.0.10:23456"
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusForbidden, rr.Code)
	assert.Contains(t, rr.Body.String(), `"integration_administration_forbidden"`)
}
