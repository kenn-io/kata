package embedding

import (
	"context"
	"time"

	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kit/embedlocal"
	kitvec "go.kenn.io/kit/vector"
)

// ClientConfig converts daemon settings to client settings.
func ClientConfig(ec config.EmbeddingsConfig) Config {
	cfg := Config{
		API:                 ec.API,
		BaseURL:             ec.BaseURL,
		Model:               ec.Model,
		Credential:          ec.ResolveCredential(),
		Salt:                ec.FingerprintSalt,
		DocumentPrefix:      ec.DocumentPrefix,
		DocumentSuffix:      ec.DocumentSuffix,
		QueryPrefix:         ec.QueryPrefix,
		QuerySuffix:         ec.QuerySuffix,
		RequestDimensions:   ec.RequestDimensions,
		Dims:                ec.Dims,
		BatchSize:           ec.BatchSize,
		Timeout:             time.Duration(ec.TimeoutSeconds) * time.Second,
		TrustPrivateNetwork: ec.TrustPrivateNetwork,
	}
	if ec.UsesLocal() {
		if cfg.BatchSize == 0 {
			cfg.BatchSize = embedlocal.BatchItems
		}
		if cfg.Timeout == 0 {
			cfg.Timeout = embedlocal.RequestTimeout
		}
	}
	return cfg
}

// ValidateBatching checks the configured limits without contacting the provider.
func ValidateBatching(ctx context.Context, ec config.EmbeddingsConfig, client *Client) error {
	_, err := kitvec.EncodeBatched(ctx, client.EncodeFunc(), nil, BatchOptions(ec, client.BatchSize())...)
	return err
}

// BatchOptions applies the configured batch limits.
func BatchOptions(ec config.EmbeddingsConfig, batchSize int) []kitvec.BatchOption {
	options := []kitvec.BatchOption{kitvec.WithBatchSize(batchSize)}
	if ec.MaxBatchTokens != 0 || ec.ModelContextTokens != 0 {
		options = append(options,
			kitvec.WithBatchTokenBudget(ec.MaxBatchTokens, ec.ModelContextTokens))
	}
	return options
}
