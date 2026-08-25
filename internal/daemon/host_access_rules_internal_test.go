package daemon

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestHostAccessRulesReferenceRegisteredOperations catches a rule keyed by a
// misspelled or removed operation ID, which would otherwise silently apply to
// nothing.
func TestHostAccessRulesReferenceRegisteredOperations(t *testing.T) {
	srv := NewServer(ServerConfig{})
	t.Cleanup(func() { _ = srv.Close() })
	registered := registeredOperations(srv.API().OpenAPI())

	for operationID := range hostAccessRules {
		_, ok := registered[operationID]
		assert.Truef(t, ok, "host access rule references unregistered operation %q", operationID)
	}
}

func TestRegisterHostAccessRulesRejectsContradictoryRules(t *testing.T) {
	require.PanicsWithValue(t,
		"contradictory host access rule for purgeProject: an operation cannot both "+
			"touch no project data and require authority over every project",
		func() {
			registerHostAccessRules(map[string]hostAccessRule{},
				hostAccessRule{NoProjectData: true, RequiresAllProjects: true}, "purgeProject")
		})

	require.PanicsWithValue(t,
		"contradictory host access rule for acquireIssueLease: a federation bearer "+
			"cannot reach a route the bearer middleware pre-empts",
		func() {
			registerHostAccessRules(map[string]hostAccessRule{},
				hostAccessRule{AcceptsFederationBearer: true}, "acquireIssueLease")
		})
}

func TestRegisterHostAccessRulesRejectsDuplicateRegistration(t *testing.T) {
	rules := map[string]hostAccessRule{}
	registerHostAccessRules(rules, hostAccessRule{NoProjectData: true}, "ping")

	require.PanicsWithValue(t, "duplicate host access rule: ping", func() {
		registerHostAccessRules(rules, hostAccessRule{RequiresAllProjects: true}, "ping")
	})
}

// TestHostAccessRulesPreserveTheForceReleaseAsymmetry pins the one deliberate
// difference between the bearer-middleware bypass set and the set of routes a
// federation enrollment credential may authenticate. forceReleaseIssueLease
// passes allowEnrollment=false to resolveClaimPrincipal, so an enrollment
// token must not reach it even though the middleware defers to the handler.
func TestHostAccessRulesPreserveTheForceReleaseAsymmetry(t *testing.T) {
	forceRelease := hostAccessRuleFor("forceReleaseIssueLease")
	assert.True(t, forceRelease.SelfAuthenticated,
		"force release must stay out of the bearer middleware's pre-emption")
	assert.False(t, forceRelease.AcceptsFederationBearer,
		"force release must not accept a federation enrollment credential")

	acquire := hostAccessRuleFor("acquireIssueLease")
	assert.True(t, acquire.SelfAuthenticated)
	assert.True(t, acquire.AcceptsFederationBearer)
}

// TestHostAccessRulesPreserveHandlerResolvedScopes catches route-table drift
// that would authorize one of these operations against every project before
// its handler has resolved the projects it actually touches.
func TestHostAccessRulesPreserveHandlerResolvedScopes(t *testing.T) {
	for _, operationID := range []string{
		"readUIReferences",
		"readUILaunchTarget",
		"patchProjectMetadata",
	} {
		assert.Truef(t, hostAccessRuleFor(operationID).ResolvedByHandler,
			"%s must defer project-scope authorization to its handler", operationID)
	}
}
