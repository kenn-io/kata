package main

import (
	"fmt"

	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/embedding"
)

// reloadEmbeddingCredentials changes only credentials on the running provider.
// Applying a new provider's key to the existing origin would misroute secrets.
func reloadEmbeddingCredentials(initial config.EmbeddingsConfig, client *embedding.Client, wake func()) error {
	if client == nil {
		return nil
	}
	cfg, err := config.ReadDaemonConfig()
	if err != nil {
		return err
	}
	current := cfg.Search.Embeddings
	providerOnly := func(ec config.EmbeddingsConfig) config.EmbeddingsConfig {
		ec.APIKey = ""
		ec.APIKeyFile = ""
		ec.APIKeyEnv = ""
		return ec
	}
	if providerOnly(current) != providerOnly(initial) {
		return fmt.Errorf("embedding provider settings changed; restart the daemon to apply them (keeping the running credential)")
	}
	client.SetCredential(current.ResolveCredential())
	if wake != nil {
		wake()
	}
	return nil
}
