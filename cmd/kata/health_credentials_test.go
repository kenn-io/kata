package main

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/testenv"
)

func TestHealthCredentialWarningVisibleInHumanAndAgent(t *testing.T) {
	for _, args := range [][]string{{"health"}, {"--agent", "health"}, {"--json", "health"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			resetFlags(t)
			env := testenv.New(t, func(cfg *daemon.ServerConfig) {
				cfg.ReconcilerHealth = func() daemon.ReconcilerHealth {
					return daemon.ReconcilerHealth{Configured: true, Credential: "missing", CredentialSource: "env:EXAMPLE_KEY", CredentialReason: "no embedding API key (env EXAMPLE_KEY is unset)"} //nolint:gosec // G101: diagnostic fixture state and source names, not credential values.
				}
			})
			out, stderr, err := executeRootCapture(t, contextWithBaseURL(context.Background(), env.URL), args...)
			require.NoError(t, err)
			if args[0] != "--json" {
				assert.Contains(t, stderr, "EXAMPLE_KEY")
				assert.Contains(t, stderr, "semantic search disabled")
				assert.Contains(t, stderr, "lexical only")
				assert.NotContains(t, out, "warning:")
			} else {
				assert.Empty(t, stderr)
				assert.Contains(t, out, "EXAMPLE_KEY")
				assert.Contains(t, out, `"credential":"missing"`)
			}
		})
	}
}
