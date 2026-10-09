package main

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/testenv"
)

// R3: existing token creation exposes the same optional bounded lifetime.
func TestTokensCreateCommand_ExpiringOrdinaryRelayParent(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("bootstrap-token"), testenv.WithRequireTokenIdentity())
	before := time.Now().UTC()
	out := requireCmdOutput(t, env, "tokens", "create", "--actor", "relay-member", "--expires-in", "1h")
	parent, err := env.DB.ResolveAPIToken(t.Context(), extractTokenPlaintext(t, out))
	require.NoError(t, err)
	require.Nil(t, parent.Scope)
	require.NotNil(t, parent.ExpiresAt)
	require.WithinDuration(t, before.Add(time.Hour), *parent.ExpiresAt, 2*time.Second)
	for _, lifetime := range []string{"0s", "-1s", "1ns", "invalid"} {
		_, err := runCmdOutput(t, env, "tokens", "create", "--actor", "relay-member", "--expires-in", lifetime)
		_ = requireCLIError(t, err, ExitValidation)
	}
}
