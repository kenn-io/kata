package daemon

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/internal/api"
)

// TestPrincipalKind_TrustedProxyConstants locks the values of the two
// trusted-proxy principal kinds. They must be distinct from each other
// and from the kinds defined by the identity layer so logs and audit
// reads can tell them apart.
func TestPrincipalKind_TrustedProxyConstants(t *testing.T) {
	assert.NotEqual(t, PrincipalTrustedProxy, PrincipalTrustedProxyAbsent)
	assert.NotEqual(t, PrincipalTrustedProxy, PrincipalDBToken)
	assert.NotEqual(t, PrincipalTrustedProxy, PrincipalStaticToken)
	assert.NotEqual(t, PrincipalTrustedProxy, PrincipalBootstrap)
	assert.NotEqual(t, PrincipalTrustedProxyAbsent, PrincipalDBToken)
	assert.NotEqual(t, PrincipalTrustedProxyAbsent, PrincipalStaticToken)
	assert.NotEqual(t, PrincipalTrustedProxyAbsent, PrincipalBootstrap)

	// PrincipalKind is a string named type. Lock the snake_case string
	// values so they match the existing convention ("db_token",
	// "bootstrap", "static_token") and stay stable for log/audit
	// consumers.
	assert.Equal(t, PrincipalKind("trusted_proxy"), PrincipalTrustedProxy)
	assert.Equal(t, PrincipalKind("trusted_proxy_absent"), PrincipalTrustedProxyAbsent)
}

// TestActorFor_TrustedProxy verifies that a PrincipalTrustedProxy principal's
// Actor field is honored as the resolved actor, overriding any
// request-supplied actor string. The trusted-proxy header value is set by
// the listener-trust middleware and must win over client-claimed actors.
func TestActorFor_TrustedProxy(t *testing.T) {
	ctx := WithPrincipal(context.Background(), Principal{
		Kind:  PrincipalTrustedProxy,
		Actor: "proxy-user",
	})
	got := actorFor(ctx, "client-claim")
	assert.Equal(t, "proxy-user", got, "trusted-proxy principal wins over supplied actor")
}

// TestEnsureAttributedWriteAllowed_TrustedProxyAbsent locks the rejection
// contract for trusted-listener requests that did not carry the configured
// actor header. The error envelope must be 400 actor_header_required so
// clients can distinguish "trusted but unattributed" from generic 403s.
func TestEnsureAttributedWriteAllowed_TrustedProxyAbsent(t *testing.T) {
	ctx := WithPrincipal(context.Background(), Principal{
		Kind: PrincipalTrustedProxyAbsent,
	})
	err := ensureAttributedWriteAllowed(ctx)
	require.Error(t, err)

	var apiErr *api.APIError
	require.ErrorAs(t, err, &apiErr)
	assert.Equal(t, 400, apiErr.Status)
	assert.Equal(t, "actor_header_required", apiErr.Code)
}

// TestEnsureAttributedWriteAllowed_TrustedProxyAllowed confirms the positive
// path: when the trusted-proxy middleware sets a PrincipalTrustedProxy with
// an Actor value, attributed writes proceed.
func TestEnsureAttributedWriteAllowed_TrustedProxyAllowed(t *testing.T) {
	ctx := WithPrincipal(context.Background(), Principal{
		Kind:  PrincipalTrustedProxy,
		Actor: "proxy-user",
	})
	require.NoError(t, ensureAttributedWriteAllowed(ctx),
		"PrincipalTrustedProxy must be allowed to do attributed writes")
}

func TestPrincipalWebLocalAllowsOrdinaryWritesButNotTokenAdministration(t *testing.T) {
	ctx := WithPrincipal(context.Background(), Principal{Kind: PrincipalWebLocal})
	require.NoError(t, ensureAttributedWriteAllowed(ctx))

	err := ensureTokenAdminAllowed(ctx)
	require.Error(t, err)
	var apiErr *api.APIError
	require.ErrorAs(t, err, &apiErr)
	assert.Equal(t, 403, apiErr.Status)
	assert.Equal(t, "token_admin_forbidden", apiErr.Code)
}
