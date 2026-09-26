package daemon

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/internal/db"
)

// TestFederationTransportOperationsMatchTheSelfAuthenticatedRoutes ties the two
// registries together: a route a federation enrollment can authenticate must
// have transport facts, and every route with transport facts must be one the
// bearer middleware defers on. Either half going missing is the drift the
// eleven hand-written literals allowed.
func TestFederationTransportOperationsMatchTheSelfAuthenticatedRoutes(t *testing.T) {
	srv := NewServer(ServerConfig{})
	t.Cleanup(func() { _ = srv.Close() })

	selfAuthenticated := make(map[string]struct{})
	for operationID := range registeredOperations(srv.API().OpenAPI()) {
		if hostAccessRuleFor(operationID).SelfAuthenticated {
			selfAuthenticated[operationID] = struct{}{}
		}
	}
	require.NotEmpty(t, selfAuthenticated)

	for operationID, operation := range federationTransportOperations {
		assert.Equalf(t, operationID, operation.ID,
			"transport table key %q carries ID %q", operationID, operation.ID)
		_, hasPolicy := hostOperationPolicy(operationID)
		assert.Truef(t, hasPolicy,
			"federation transport operation %q has no host operation policy", operationID)
		_, isSelfAuthenticated := selfAuthenticated[operationID]
		assert.Truef(t, isSelfAuthenticated,
			"federation transport operation %q is not a self-authenticated route", operationID)
		delete(selfAuthenticated, operationID)
	}

	for operationID := range selfAuthenticated {
		t.Errorf("self-authenticated route %q has no federation transport facts", operationID)
	}
}

func TestFederationTransportOperationRejectsUnknownID(t *testing.T) {
	require.PanicsWithValue(t, "unknown federation transport operation: listIssues", func() {
		federationTransportOperation("listIssues")
	})
}

// TestFederationTransportFencesGETsThatWrite pins the two deliberate
// divergences from hostOperationPolicies. Both routes are declared reads but
// write while serving: handleClaimStatus expires timed claims and emits events,
// and projectFederationBody can refresh the federation baseline. Deriving the
// transport table from the policy registry would drop their transaction fence.
func TestFederationTransportFencesGETsThatWrite(t *testing.T) {
	for _, operationID := range []string{"getIssueLeaseStatus", "getFederationProjectMetadata"} {
		policy, ok := hostOperationPolicy(operationID)
		require.Truef(t, ok, "%s has no host operation policy", operationID)
		assert.Falsef(t, policy.Mutation,
			"%s is declared a read by the policy registry", operationID)
		assert.Truef(t, federationTransportOperation(operationID).Mutation,
			"%s must stay transaction-fenced on the federation path", operationID)
	}
}

func TestFederationTransportPollIsNotFenced(t *testing.T) {
	assert.False(t, federationTransportOperation("pollFederationProjectEvents").Mutation,
		"a pure read must not demand a transaction fence")
}

// TestNativeFederationFencePreservesRetryableSQLState guards the retry contract
// for request-scoped fenced writes. A transient database failure inside the
// native federation fence must stay recognizable as retryable so the storage
// layer retries the transaction instead of answering a hard 503.
func TestNativeFederationFencePreservesRetryableSQLState(t *testing.T) {
	deadlock := &pgconn.PgError{Code: "40P01"}
	fence := sanitizeNativeFederationTransactionFence(
		func(context.Context, db.Transaction) error { return deadlock })
	err := fence(t.Context(), nil)
	require.ErrorIs(t, err, errHostFederationAccessUnavailable)
	var state interface{ SQLState() string }
	require.ErrorAs(t, err, &state)
	assert.Equal(t, "40P01", state.SQLState())
}
