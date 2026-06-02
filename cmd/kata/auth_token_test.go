package main

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/testenv"
)

// TestCLIInjectsAuthTokenAgainstProtectedDaemon covers roborev finding 19207:
// when the daemon enforces bearer auth, the CLI's client must attach
// an Authorization bearer header to every API call. Without the fix the
// `kata projects list` call below would get a 401 from the daemon's middleware
// and surface as a CLI error.
//
// testenv.WithAuthToken also exports KATA_AUTH_TOKEN into the test process,
// so client.NewHTTPClient — wired into httpClientFor via Opts — sees
// the same token the daemon enforces.
func TestCLIInjectsAuthTokenAgainstProtectedDaemon(t *testing.T) {
	resetFlags(t)
	env := testenv.New(t, testenv.WithAuthToken("cli-test-tok"))

	// Seed a project directly via the DB so this test does not depend on the
	// auth-aware test helpers (postJSON et al. issue raw http.Post calls
	// without auth headers; auth_token plumbing is what this test exercises,
	// not those helpers).
	_, err := env.DB.CreateProject(context.Background(), "alpha")
	require.NoError(t, err)

	out, err := runCmdOutput(t, env, "projects", "list")
	require.NoError(t, err,
		"projects list should succeed against a token-protected daemon when the CLI injects the bearer token")
	assert.Contains(t, out, "alpha")
}

// TestCLIRejectedWithoutAuthTokenEnv proves the test daemon enforces auth:
// strip the env var the CLI uses to resolve the token and the same command
// surfaces a daemon-side 401 instead of succeeding. Pairs with the success
// case above so a regression that bypassed bearer auth on the daemon side
// would also fail loudly here.
func TestCLIRejectedWithoutAuthTokenEnv(t *testing.T) {
	resetFlags(t)
	env := testenv.New(t, testenv.WithAuthToken("cli-test-tok"))
	t.Setenv("KATA_AUTH_TOKEN", "") // strip the testenv-supplied value

	_, err := runCmdOutput(t, env, "projects", "list")
	require.Error(t, err,
		"projects list without an auth token must surface a daemon error")
	assert.Contains(t, strings.ToLower(err.Error()), "authorization bearer required",
		"expected the daemon's bearer-auth rejection, got %v", err)
}
