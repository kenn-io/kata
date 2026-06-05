package client

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	internalclient "go.kenn.io/kata/internal/client"
	"go.kenn.io/kata/pkg/client/generated"
)

// Client is a typed kata daemon API client generated from the Huma OpenAPI
// contract.
type Client = generated.ClientWithResponses

// RequestEditorFn mutates generated requests before they are sent.
type RequestEditorFn = generated.RequestEditorFn

// TargetAuth is explicit per-target bearer configuration for clients that
// switch between multiple daemon endpoints in one process.
type TargetAuth struct {
	Token         string
	AllowInsecure bool
}

// TransportOptions controls the HTTP transport built by auth-aware
// constructors.
type TransportOptions struct {
	Timeout               time.Duration
	ResponseHeaderTimeout time.Duration
	AllowInsecure         bool
}

type options struct {
	httpClient     *http.Client
	transport      TransportOptions
	requestEditors []generated.RequestEditorFn
}

// Option customizes a typed kata client.
type Option func(*options)

// WithHTTPClient uses the supplied HTTP client. It is intended for tests or
// callers that have already configured transport and auth behavior.
func WithHTTPClient(httpClient *http.Client) Option {
	return func(opts *options) {
		opts.httpClient = httpClient
	}
}

// WithTransportOptions sets timeout and plaintext opt-out behavior for
// auth-aware constructors.
func WithTransportOptions(transport TransportOptions) Option {
	return func(opts *options) {
		opts.transport = transport
	}
}

// WithRequestEditor appends a generated request editor.
func WithRequestEditor(fn generated.RequestEditorFn) Option {
	return func(opts *options) {
		if fn != nil {
			opts.requestEditors = append(opts.requestEditors, fn)
		}
	}
}

// WithTrustedActor adds a trusted-proxy actor header to outgoing requests.
func WithTrustedActor(header, actor string) Option {
	return WithRequestEditor(func(_ context.Context, req *http.Request) error {
		header = strings.TrimSpace(header)
		if header == "" {
			return fmt.Errorf("trusted actor header is required")
		}
		req.Header.Set(header, strings.TrimSpace(actor))
		return nil
	})
}

// New creates a client using http.DefaultClient and no bearer-token
// configuration. Use NewWithGlobalAuth, NewWithBearer, or NewForTarget when
// the daemon endpoint requires kata auth.
func New(baseURL string, opts ...Option) (*Client, error) {
	return newGeneratedClient(baseURL, collectOptions(opts...))
}

// NewWithHTTPClient creates a client using the supplied HTTP client.
func NewWithHTTPClient(baseURL string, httpClient *http.Client, opts ...Option) (*Client, error) {
	merged := collectOptions(opts...)
	merged.httpClient = httpClient
	return newGeneratedClient(baseURL, merged)
}

// NewWithGlobalAuth creates a client using kata's global auth resolution:
// KATA_AUTH_TOKEN, [auth].token, trust_private_network, remote allow_insecure,
// and Unix-socket transport behavior match the first-party CLI/TUI path.
func NewWithGlobalAuth(ctx context.Context, baseURL string, opts ...Option) (*Client, error) {
	merged := collectOptions(opts...)
	httpClient, err := internalclient.NewHTTPClient(ctx, baseURL, internalOpts(merged.transport))
	if err != nil {
		return nil, err
	}
	merged.httpClient = httpClient
	return newGeneratedClient(baseURL, merged)
}

// NewWithBearer creates a client using an explicit bearer token while still
// honoring kata's configured trust_private_network and remote allow_insecure
// behavior.
func NewWithBearer(ctx context.Context, baseURL, token string, opts ...Option) (*Client, error) {
	merged := collectOptions(opts...)
	httpClient, err := internalclient.NewHTTPClientWithBearer(ctx, baseURL, token, internalOpts(merged.transport))
	if err != nil {
		return nil, err
	}
	merged.httpClient = httpClient
	return newGeneratedClient(baseURL, merged)
}

// NewForTarget creates a client for a fully resolved daemon target. Unlike
// NewWithGlobalAuth and NewWithBearer, it does not read global auth config;
// the supplied TargetAuth is the complete bearer policy for this client.
func NewForTarget(ctx context.Context, baseURL string, auth TargetAuth, opts ...Option) (*Client, error) {
	merged := collectOptions(opts...)
	httpClient, err := internalclient.NewHTTPClientForTarget(ctx, baseURL,
		internalclient.TargetAuth{Token: auth.Token, AllowInsecure: auth.AllowInsecure},
		internalOpts(merged.transport))
	if err != nil {
		return nil, err
	}
	merged.httpClient = httpClient
	return newGeneratedClient(baseURL, merged)
}

func collectOptions(opts ...Option) options {
	var out options
	for _, opt := range opts {
		if opt != nil {
			opt(&out)
		}
	}
	return out
}

func newGeneratedClient(baseURL string, opts options) (*Client, error) {
	if opts.httpClient == nil {
		opts.httpClient = http.DefaultClient
	}
	generatedOpts := []generated.ClientOption{generated.WithHTTPClient(opts.httpClient)}
	for _, editor := range opts.requestEditors {
		generatedOpts = append(generatedOpts, generated.WithRequestEditorFn(editor))
	}
	return generated.NewClientWithResponses(strings.TrimRight(baseURL, "/"), generatedOpts...)
}

func internalOpts(opts TransportOptions) internalclient.Opts {
	return internalclient.Opts{
		Timeout:               opts.Timeout,
		ResponseHeaderTimeout: opts.ResponseHeaderTimeout,
		AllowInsecure:         opts.AllowInsecure,
	}
}
