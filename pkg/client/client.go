// Package client exposes the generated kata daemon API client with constructors
// that match kata's existing daemon transport and authentication modes.
package client

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/doordash-oss/oapi-codegen-dd/v3/pkg/runtime"

	internalclient "go.kenn.io/kata/internal/client"
	"go.kenn.io/kata/pkg/client/generated"
)

// Client is a typed kata daemon API client generated from the Huma OpenAPI
// contract.
type Client struct {
	*generated.Client

	apiClient  runtime.APIClient
	httpClient *http.Client
}

// RequestEditorFn mutates generated requests before they are sent.
type RequestEditorFn = runtime.RequestEditorFn

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
	requestEditors []runtime.RequestEditorFn
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
func WithRequestEditor(fn runtime.RequestEditorFn) Option {
	return func(opts *options) {
		if fn != nil {
			opts.requestEditors = append(opts.requestEditors, fn)
		}
	}
}

// WithTrustedActor adds a trusted-proxy actor header to outgoing requests.
func WithTrustedActor(header, actor string) Option {
	header = strings.TrimSpace(header)
	actor = strings.TrimSpace(actor)
	return WithRequestEditor(func(_ context.Context, req *http.Request) error {
		if header == "" {
			return fmt.Errorf("trusted actor header is required")
		}
		req.Header.Set(header, actor)
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
	normalizedBaseURL := strings.TrimRight(baseURL, "/")
	generatedOpts := []runtime.APIClientOption{runtime.WithHTTPClient(contextDoer{client: opts.httpClient})}
	for _, editor := range opts.requestEditors {
		generatedOpts = append(generatedOpts, runtime.WithRequestEditorFn(editor))
	}
	apiClient, err := runtime.NewAPIClient(normalizedBaseURL, generatedOpts...)
	if err != nil {
		return nil, err
	}
	escapedAPIClient := generated.NewPathEscapingAPIClient(apiClient)
	return &Client{
		Client:     generated.NewClient(escapedAPIClient),
		apiClient:  escapedAPIClient,
		httpClient: opts.httpClient,
	}, nil
}

func internalOpts(opts TransportOptions) internalclient.Opts {
	return internalclient.Opts{
		Timeout:               opts.Timeout,
		ResponseHeaderTimeout: opts.ResponseHeaderTimeout,
		AllowInsecure:         opts.AllowInsecure,
	}
}

type contextDoer struct {
	client *http.Client
}

func (d contextDoer) Do(ctx context.Context, req *http.Request) (*http.Response, error) {
	client := d.client
	if client == nil {
		client = http.DefaultClient
	}
	return client.Do(req.WithContext(ctx)) //nolint:gosec // request URL is built by the generated client from the caller-selected base URL
}

// StreamEvents calls the generated buffered stream method with the daemon's
// required SSE Accept header. Prefer StreamEventsRaw for live streams.
func (c *Client) StreamEvents(ctx context.Context, options *generated.StreamEventsRequestOptions, reqEditors ...RequestEditorFn) (*generated.StreamEventsResponse, error) {
	if c == nil || c.Client == nil {
		return nil, fmt.Errorf("client is not initialized")
	}
	return c.Client.StreamEvents(ctx, options, withSSEAccept(reqEditors)...)
}

// StreamEventsWithResponse calls the generated buffered stream method with the
// daemon's required SSE Accept header. Prefer StreamEventsRaw for live streams.
func (c *Client) StreamEventsWithResponse(ctx context.Context, options *generated.StreamEventsRequestOptions, reqEditors ...RequestEditorFn) (*generated.StreamEventsResp, error) {
	if c == nil || c.Client == nil {
		return nil, fmt.Errorf("client is not initialized")
	}
	return c.Client.StreamEventsWithResponse(ctx, options, withSSEAccept(reqEditors)...)
}

// StreamEventsRaw opens the long-lived Server-Sent Events stream without
// buffering the response body. The generated StreamEvents method is still
// available for finite responses, but live streams need callers to consume the
// body incrementally and close it when done.
func (c *Client) StreamEventsRaw(ctx context.Context, options *generated.StreamEventsRequestOptions, reqEditors ...RequestEditorFn) (*http.Response, error) {
	if c == nil || c.apiClient == nil {
		return nil, fmt.Errorf("client is not initialized")
	}
	if options == nil {
		options = &generated.StreamEventsRequestOptions{}
	}
	req, err := c.apiClient.CreateRequest(ctx, runtime.RequestOptionsParameters{
		RequestURL: c.apiClient.GetBaseURL() + "/api/v1/events/stream",
		Method:     http.MethodGet,
		Options:    options,
	}, reqEditors...)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "text/event-stream")

	resp, err := c.httpClient.Do(req.WithContext(ctx)) //nolint:gosec // request URL is built by the generated client from the caller-selected base URL
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		if resp.Body != nil {
			_ = resp.Body.Close()
		}
		return nil, runtime.NewClientAPIError(fmt.Errorf("API error (status %d)", resp.StatusCode), runtime.WithStatusCode(resp.StatusCode))
	}
	return resp, nil
}

func withSSEAccept(reqEditors []RequestEditorFn) []RequestEditorFn {
	editors := make([]RequestEditorFn, 0, len(reqEditors)+1)
	editors = append(editors, reqEditors...)
	editors = append(editors, func(_ context.Context, req *http.Request) error {
		req.Header.Set("Accept", "text/event-stream")
		return nil
	})
	return editors
}
