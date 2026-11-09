package client

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"net/http"

	"github.com/doordash-oss/oapi-codegen-dd/v3/pkg/runtime"
	"go.kenn.io/kata/internal/projectscope"
	"go.kenn.io/kata/pkg/client/generated"
)

// WithProjectScope returns an independent client limited to projectRefs. Use
// immutable project UIDs; id:N supports callers with a legacy numeric binding.
// It retains the original client's transport/authentication. Further bindings
// intersect the existing limits, and the original client remains unchanged.
func (c *Client) WithProjectScope(projectRefs []string) (*Client, error) {
	if c == nil || c.apiClient == nil {
		return nil, fmt.Errorf("client is not initialized")
	}
	raw, err := json.Marshal(projectRefs)
	if err != nil {
		return nil, err
	}
	if _, err := projectscope.ParseHeader(string(raw)); err != nil {
		return nil, err
	}
	scoped := projectScopedAPIClient{APIClient: c.apiClient, limit: string(raw)}
	return &Client{Client: generated.NewClient(scoped), apiClient: scoped, httpClient: c.httpClient}, nil
}

type projectScopedAPIClient struct {
	runtime.APIClient
	limit string
}

func (c projectScopedAPIClient) CreateRequest(ctx context.Context, params runtime.RequestOptionsParameters, editors ...runtime.RequestEditorFn) (*http.Request, error) {
	request, err := c.APIClient.CreateRequest(ctx, params, editors...)
	if err != nil {
		return nil, err
	}
	request.Header.Add(projectscope.Header, c.limit)
	return request, nil
}
