package client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/doordash-oss/oapi-codegen-dd/v3/pkg/runtime"
	"go.kenn.io/kata/pkg/client/generated"
)

// ErrResponseTooLarge permits page readers to reduce the candidate limit and
// retry the same cursor without ever decoding an oversized daemon response.
var ErrResponseTooLarge = errors.New("daemon response exceeds byte limit; retry a smaller page")

// ResponseTooLargeError preserves HTTP authority and availability status even
// when an oversized body cannot be decoded. Only successful pages may be
// retried with a smaller page size or treated as an explicit source gap.
type ResponseTooLargeError struct{ StatusCode int }

func (e *ResponseTooLargeError) Error() string { return ErrResponseTooLarge.Error() }
func (e *ResponseTooLargeError) Unwrap() error { return ErrResponseTooLarge }

// WithResponseLimit returns a client sharing the selected daemon transport and
// request editors, with a byte bound applied before generated JSON decoding.
// It does not mutate the original client or redirect its credentials.
func (c *Client) WithResponseLimit(maxBytes int64) *Client {
	bounded := boundedAPIClient{
		streamingAPIClient{APIClient: c.apiClient, client: c.httpClient},
		maxBytes,
	}
	return &Client{Client: generated.NewClient(bounded), apiClient: bounded, httpClient: c.httpClient}
}

type boundedAPIClient struct {
	streamingAPIClient
	maxBytes int64
}

func (c boundedAPIClient) ExecuteRequest(ctx context.Context, req *http.Request, path string) (result *runtime.Response, resultErr error) {
	response, err := c.streamingAPIClient.ExecuteRequest(ctx, req, path)
	if err != nil {
		return nil, err
	}
	defer func() {
		if closeErr := response.Raw.Body.Close(); resultErr == nil && closeErr != nil {
			result = nil
			resultErr = fmt.Errorf("close daemon response body: %w", closeErr)
		}
	}()
	if c.maxBytes <= 0 {
		return nil, &ResponseTooLargeError{StatusCode: response.StatusCode}
	}
	content, err := io.ReadAll(io.LimitReader(response.Raw.Body, c.maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(content)) > c.maxBytes {
		return nil, &ResponseTooLargeError{StatusCode: response.StatusCode}
	}
	response.Content = content
	return response, nil
}
