package embedding

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"

	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kit/embedclient"
	"go.kenn.io/kit/embedconfig"
	"go.kenn.io/kit/embedmodel"
	kitvec "go.kenn.io/kit/vector"
)

type Config struct {
	BaseURL             string
	Model               string
	Credential          config.EmbeddingCredential
	DocumentPrefix      string
	DocumentSuffix      string
	QueryPrefix         string
	QuerySuffix         string
	RequestDimensions   bool
	Salt                string
	Dims                int
	BatchSize           int
	Timeout             time.Duration
	TrustPrivateNetwork bool
}

type Client struct {
	options            embedclient.Options
	space              embedmodel.Descriptor
	generation         kitvec.Generation
	http               *http.Client
	mu                 sync.Mutex
	credential         config.EmbeddingCredential
	credentialRevision uint64
	credentialHealth   CredentialHealth
}

const (
	defaultDims      = 768
	defaultBatchSize = 64
	defaultTimeout   = 30 * time.Second
)

func New(cfg Config) (*Client, error) {
	dims := cfg.Dims
	if dims <= 0 {
		dims = defaultDims
	}
	batch := cfg.BatchSize
	if batch <= 0 {
		batch = defaultBatchSize
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	parts, err := (embedconfig.Embedder{
		BaseURL: cfg.BaseURL, Model: cfg.Model, Dims: dims,
		FingerprintSalt: cfg.Salt, BatchSize: batch,
		TrustPrivateNetwork: cfg.TrustPrivateNetwork,
	}).Parts()
	if err != nil {
		return nil, fmt.Errorf("embedding: configure client: %w", err)
	}
	parts.Model.RequestDimensions = cfg.RequestDimensions
	parts.Roles.DocumentPrefix = cfg.DocumentPrefix
	parts.Roles.DocumentSuffix = cfg.DocumentSuffix
	parts.Roles.QueryPrefix = cfg.QueryPrefix
	parts.Roles.QuerySuffix = cfg.QuerySuffix
	parts.Transport.Timeout = timeout
	options := embedclient.Options{
		Model: parts.Model, Roles: parts.Roles, Deployment: parts.Deployment,
		Batch: parts.Batch, Transport: parts.Transport,
	}
	if _, err := embedclient.New(options); err != nil {
		return nil, fmt.Errorf("embedding: configure client: %w", err)
	}
	params := map[string]string{"recipe": strconv.Itoa(RecipeVersion)}
	if cfg.Salt != "" {
		params["salt"] = cfg.Salt
	}
	for key, value := range map[string]string{
		"document_prefix": cfg.DocumentPrefix, "document_suffix": cfg.DocumentSuffix,
		"query_prefix": cfg.QueryPrefix, "query_suffix": cfg.QuerySuffix,
	} {
		if value != "" {
			params[key] = value
		}
	}
	if cfg.RequestDimensions {
		params["request_dimensions"] = "true"
	}
	generation := kitvec.Generation{Model: cfg.Model, Dimensions: dims, Params: params}
	space := embedmodel.Descriptor{
		Model: parts.Model, Roles: parts.Roles, Deployment: parts.Deployment,
		Input:  embedconfig.InputLimits{Recipe: "kata.issue/v" + strconv.Itoa(RecipeVersion)},
		Legacy: []string{generation.Fingerprint()},
	}
	if err := space.Validate(); err != nil {
		return nil, fmt.Errorf("embedding: configure identity: %w", err)
	}
	client := &Client{options: options, space: space, generation: generation, http: &http.Client{Timeout: timeout}}
	client.SetCredential(cfg.Credential)
	return client, nil
}

// Dims returns the configured vector dimensionality.
func (c *Client) Dims() int { return c.options.Model.Dimensions }

// BatchSize is the maximum number of inputs per request.
func (c *Client) BatchSize() int { return c.options.Batch.Items }

// Space describes the embedding model and its existing generation fingerprints.
func (c *Client) Space() embedmodel.Descriptor { return c.space }

// Generation preserves Kata's recipe-aware fingerprint. The endpoint is
// excluded so moving a host or port never forces re-embedding.
func (c *Client) Generation() kitvec.Generation { return c.generation }

func (c *Client) EncodeFunc() kitvec.EncodeFunc {
	return func(ctx context.Context, texts []string) (vectors [][]float32, err error) {
		defer func() {
			if recovered := recover(); recovered != nil {
				err = fmt.Errorf("embedding: encoder panic: %v", recovered)
			}
		}()
		return c.Embed(ctx, texts)
	}
}

// Embed applies document affixes to already prepared document text.
func (c *Client) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	return c.embed(ctx, embedconfig.RoleDocument, texts)
}

// EmbedQuery applies query affixes to already prepared query text.
func (c *Client) EmbedQuery(ctx context.Context, texts []string) ([][]float32, error) {
	return c.embed(ctx, embedconfig.RoleQuery, texts)
}

func (c *Client) embed(ctx context.Context, role embedconfig.Role, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return [][]float32{}, nil
	}
	credential, revision, err := c.requestCredential()
	if err != nil {
		return nil, err
	}
	options := c.options
	options.APIKey = credential.Key
	options.HTTP = c.http
	provider, err := embedclient.New(options)
	if err != nil {
		return nil, err
	}
	vectors, err := provider.EmbedTexts(ctx, role, texts)
	if err != nil {
		if rejected, ok := errors.AsType[*embedclient.APIError](err); ok && rejected.CredentialsRejected() {
			return nil, c.rejectCredential(credential, revision, rejected)
		}
		return nil, err
	}
	c.credentialSucceeded(revision)
	return vectors, nil
}
