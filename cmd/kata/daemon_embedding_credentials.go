package main

import (
	"context"
	"fmt"

	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/embedding"
)

// reloadEmbeddingCredentials changes only credentials on the running provider.
// Applying a new provider's key to the existing origin would misroute secrets.
func reloadEmbeddingCredentials(ctx context.Context, initial config.EmbeddingsConfig, dsn string, client *embedding.Client, wake func()) error {
	if client == nil {
		return nil
	}
	cfg, err := config.ReadDaemonConfig()
	if err != nil {
		return err
	}
	current, err := applySharedEmbeddingChoice(ctx, cfg.Search.Embeddings, dsn)
	if err != nil {
		return err
	}
	if embeddingProviderIdentity(current) != embeddingProviderIdentity(initial) {
		return fmt.Errorf("embedding provider settings changed; restart the daemon to apply them (keeping the running credential)")
	}
	client.SetCredential(current.ResolveCredential())
	if wake != nil {
		wake()
	}
	return nil
}

// embeddingProviderIdentity drops credentials and fills the built-in server's
// default width, so a saved local = true matches its effective choice.
func embeddingProviderIdentity(ec config.EmbeddingsConfig) config.EmbeddingsConfig {
	ec.APIKey = ""
	ec.APIKeyFile = ""
	ec.APIKeyEnv = ""
	if ec.UsesLocal() {
		if ec.Dims == 0 {
			ec.Dims = 768
		}
	}
	return ec
}
