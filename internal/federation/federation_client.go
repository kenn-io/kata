package federation

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"strings"

	"go.kenn.io/kata/internal/httpurl"

	"github.com/doordash-oss/oapi-codegen-dd/v3/pkg/runtime"
	"go.kenn.io/kata/internal/api"
	clientpkg "go.kenn.io/kata/internal/client"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/pkg/client/generated"
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
	canonicalBaseURL, err := httpurl.CanonicalHTTPBaseURL(baseURL)
	if err != nil {
		return nil, err
	}
	c, err := clientpkg.NewHTTPClientWithBearer(ctx, canonicalBaseURL, token, opts)
	if err != nil {
		return nil, err
	}
	return &Client{
		baseURL: canonicalBaseURL,
		client:  c,
	}, nil
}

// PollProjectEvents fetches hub project events strictly after afterID.
func (c *Client) PollProjectEvents(
	ctx context.Context, hubProjectID, afterID int64, limit int,
) (api.PollEventsBody, error) {
	apiClient, err := generated.NewDefaultClient(c.baseURL, runtime.WithHTTPClient(replicationDoer{c.client}))
	if err != nil {
		return api.PollEventsBody{}, err
	}
	query := &generated.PollFederationProjectEventsQuery{AfterID: &afterID}
	if limit > 0 {
		query.Limit = new(int64(limit))
	}
	response, callErr := apiClient.PollFederationProjectEventsWithResponse(ctx, &generated.PollFederationProjectEventsRequestOptions{PathParams: &generated.PollFederationProjectEventsPath{ProjectID: hubProjectID}, Query: query})
	var body api.PollEventsBody
	if response == nil {
		return body, callErr
	}
	err = decodeReplicationResponse(response.HTTPResponse, response.Body, &body)
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
	apiClient, err := generated.NewDefaultClient(c.baseURL, runtime.WithHTTPClient(replicationDoer{c.client}))
	if err != nil {
		return api.FederationIngestEventsBody{}, err
	}
	data, err := json.Marshal(api.FederationIngestEventsRequestBody{
		SchemaVersion: db.CurrentSchemaVersion(), AdoptionBaseline: opts.AdoptionBaseline,
		AdoptionBaselineEndEventID: opts.AdoptionBaselineEndEventID, Events: events,
	})
	if err != nil {
		return api.FederationIngestEventsBody{}, err
	}
	var payload generated.IngestFederationProjectEventsBody
	if err := json.Unmarshal(data, &payload); err != nil {
		return api.FederationIngestEventsBody{}, err
	}
	response, callErr := apiClient.IngestFederationProjectEventsWithResponse(ctx, &generated.IngestFederationProjectEventsRequestOptions{PathParams: &generated.IngestFederationProjectEventsPath{ProjectID: hubProjectID}, Body: &payload})
	var body api.FederationIngestEventsBody
	if response == nil {
		return body, callErr
	}
	err = decodeReplicationResponse(response.HTTPResponse, response.Body, &body)
	return body, err
}

// ProjectFederation fetches the hub metadata needed to bind a spoke replica.
func (c *Client) ProjectFederation(ctx context.Context, hubProjectID int64) (api.ProjectFederationBody, error) {
	apiClient, err := generated.NewDefaultClient(c.baseURL, runtime.WithHTTPClient(replicationDoer{c.client}))
	if err != nil {
		return api.ProjectFederationBody{}, err
	}
	response, callErr := apiClient.GetFederationProjectMetadataWithResponse(ctx, &generated.GetFederationProjectMetadataRequestOptions{PathParams: &generated.GetFederationProjectMetadataPath{ProjectID: hubProjectID}})
	var body api.ProjectFederationBody
	if response == nil {
		return body, callErr
	}
	err = decodeReplicationResponse(response.HTTPResponse, response.Body, &body)
	return body, err
}

// Keep the replication error body bounded before the generated runtime reads it.
type replicationDoer struct{ client *http.Client }

func (d replicationDoer) Do(ctx context.Context, req *http.Request) (*http.Response, error) {
	resp, err := d.client.Do(req.WithContext(ctx))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, &HubStatusError{Path: req.URL.Path, StatusCode: resp.StatusCode, Body: strings.TrimSpace(string(body))}
	}
	return resp, nil
}

func decodeReplicationResponse(resp *http.Response, body []byte, out any) error {
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("decode hub %s response: %w", resp.Request.URL.Path, err)
	}
	return nil
}
