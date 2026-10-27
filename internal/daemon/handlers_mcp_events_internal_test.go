package daemon

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var mcpEventsOperations = map[string]struct {
	method, path string
	allProjects  bool
}{
	"mcpEventsList":         {http.MethodPost, "/api/v1/mcp-events/list", false},
	"mcpEventsSubscribe":    {http.MethodPost, "/api/v1/mcp-events/subscribe", true},
	"mcpEventsUnsubscribe":  {http.MethodPost, "/api/v1/mcp-events/unsubscribe", false},
	"mcpEventSubscriptions": {http.MethodGet, "/api/v1/mcp-events/subscriptions", false},
}

func TestMCPEventsOperationsAreRegisteredWithPoliciesAndRules(t *testing.T) {
	srv := NewServer(ServerConfig{})
	t.Cleanup(func() { _ = srv.Close() })
	registered := registeredOperations(srv.API().OpenAPI())
	for id, want := range mcpEventsOperations {
		t.Run(id, func(t *testing.T) {
			route, ok := registered[id]
			require.True(t, ok, "operation is registered")
			assert.Equal(t, want.method, route.Method)
			assert.Equal(t, want.path, route.Path)

			policy, ok := hostOperationPolicy(id)
			require.True(t, ok, "host operation policy exists")
			assert.False(t, policy.Mutation, "subscriptions are daemon state, not project mutations")

			rule := hostAccessRuleFor(id)
			assert.Equal(t, want.allProjects, rule.RequiresAllProjects)
			assert.Equal(t, !want.allProjects, rule.NoProjectData)
			assert.False(t, rule.SelfAuthenticated)

			assert.True(t, issueScopedRouteAllowed(want.method, want.path), "scoped tokens may use the route")
		})
	}
}

func TestMCPEventsScopedRoutesAreExact(t *testing.T) {
	assert.False(t, issueScopedRouteAllowed(http.MethodGet, "/api/v1/mcp-events/list"))
	assert.False(t, issueScopedRouteAllowed(http.MethodPost, "/api/v1/mcp-events/subscriptions"))
	assert.False(t, issueScopedRouteAllowed(http.MethodPost, "/api/v1/mcp-events/subscribe/extra"))
	assert.False(t, issueScopedRouteAllowed(http.MethodPost, "/api/v1/mcp-events"))
}
