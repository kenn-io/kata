package main

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/embedding"
	"go.kenn.io/kata/internal/testenv"
)

func TestHealthCredentialWarningVisibleInHumanAndAgent(t *testing.T) {
	for _, args := range [][]string{{"health"}, {"--agent", "health"}, {"--json", "health"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			resetFlags(t)
			emb, err := embedding.New(embedding.Config{BaseURL: "http://127.0.0.1:9", Model: "m", Dims: 2,
				Credential: config.EmbeddingCredential{Source: "env:EXAMPLE_KEY", Reason: "no embedding API key (env EXAMPLE_KEY is unset)"},
			})
			require.NoError(t, err)
			env := testenv.New(t, func(cfg *daemon.ServerConfig) {
				cfg.Embedder = emb
				cfg.ReconcilerHealth = func() daemon.ReconcilerHealth {
					return daemon.ReconcilerHealth{Configured: true}
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
