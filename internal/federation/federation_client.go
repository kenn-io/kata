package federation

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"go.kenn.io/kata/internal/api"
	clientpkg "go.kenn.io/kata/internal/client"
	"go.kenn.io/kata/internal/db"
)

// HubStatusError reports a non-2xx response from the configured hub.
type HubStatusError struct {
	Path       string
	StatusCode int
	Body       string
}

func (e *HubStatusError) Error() string {
	return fmt.Sprintf("hub %s returned %d: %s", e.Path, e.StatusCode, e.Body)
}

// Client is the outbound hub client used by pull replication.
type Client struct {
	baseURL string
	client  *http.Client
}

// NewClient builds a bearer-pinned HTTP client for a trusted hub.
func NewClient(ctx context.Context, baseURL string, token string, opts clientpkg.Opts) (*Client, error) {
	c, err := clientpkg.NewHTTPClientWithBearer(ctx, baseURL, token, opts)
	if err != nil {
		return nil, err
	}
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		client:  c,
	}, nil
}

// PollProjectEvents fetches hub project events strictly after afterID.
func (c *Client) PollProjectEvents(
	ctx context.Context, hubProjectID, afterID int64, limit int,
) (api.PollEventsBody, error) {
	q := url.Values{}
	q.Set("after_id", strconv.FormatInt(afterID, 10))
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	var body api.PollEventsBody
	err := c.getJSON(ctx,
		fmt.Sprintf("/api/v1/projects/%d/federation/events?%s", hubProjectID, q.Encode()), &body)
	if body.Events == nil {
		body.Events = []api.EventEnvelope{}
	}
	return body, err
}

// IngestProjectEvents pushes local spoke events into the hub transport
// endpoint.
func (c *Client) IngestProjectEvents(
	ctx context.Context,
	hubProjectID int64,
	events []api.FederationIngestEventEnvelope,
) (api.FederationIngestEventsBody, error) {
	return c.IngestProjectEventsWithOptions(ctx, hubProjectID, events, IngestProjectEventsOptions{})
}

// IngestProjectEventsOptions carries optional metadata for federation ingest.
type IngestProjectEventsOptions struct {
	AdoptionBaseline           string
	AdoptionBaselineEndEventID int64
}

// IngestProjectEventsWithOptions pushes local spoke events with optional
// transport metadata used by chunked adoption baselines.
func (c *Client) IngestProjectEventsWithOptions(
	ctx context.Context,
	hubProjectID int64,
	events []api.FederationIngestEventEnvelope,
	opts IngestProjectEventsOptions,
) (api.FederationIngestEventsBody, error) {
	var body api.FederationIngestEventsBody
	err := c.postJSON(ctx,
		fmt.Sprintf("/api/v1/projects/%d/federation/events:ingest", hubProjectID),
		api.FederationIngestEventsRequestBody{
			SchemaVersion:              db.CurrentSchemaVersion(),
			AdoptionBaseline:           opts.AdoptionBaseline,
			AdoptionBaselineEndEventID: opts.AdoptionBaselineEndEventID,
			Events:                     events,
		}, &body)
	return body, err
}

// ProjectFederation fetches the hub metadata needed to bind a spoke replica.
func (c *Client) ProjectFederation(ctx context.Context, hubProjectID int64) (api.ProjectFederationBody, error) {
	var body api.ProjectFederationBody
	err := c.getJSON(ctx, fmt.Sprintf("/api/v1/projects/%d/federation/metadata", hubProjectID), &body)
	return body, err
}

func (c *Client) getJSON(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil) //nolint:gosec // baseURL is caller-supplied hub config.
	if err != nil {
		return err
	}
	resp, err := c.client.Do(req) //nolint:gosec // request target is built from explicit hub config.
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return &HubStatusError{Path: req.URL.Path, StatusCode: resp.StatusCode, Body: strings.TrimSpace(string(body))}
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decode hub %s response: %w", req.URL.Path, err)
	}
	return nil
}

func (c *Client) postJSON(ctx context.Context, path string, in, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return fmt.Errorf("marshal hub %s request: %w", path, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(body)) //nolint:gosec // baseURL is caller-supplied hub config.
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.client.Do(req) //nolint:gosec // request target is built from explicit hub config.
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return &HubStatusError{Path: req.URL.Path, StatusCode: resp.StatusCode, Body: strings.TrimSpace(string(body))}
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decode hub %s response: %w", req.URL.Path, err)
	}
	return nil
}
