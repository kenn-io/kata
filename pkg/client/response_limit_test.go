package client

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/pkg/client/generated"
)

func TestWithResponseLimitEscapesStringPathParametersOnce(t *testing.T) {
	tests := []struct {
		name string
		want string
		call func(*testing.T, *Client) error
	}{
		{
			name: "qualified issue reference",
			want: "/api/v1/projects/1/issues/spoke-project%23abc4",
			call: func(t *testing.T, client *Client) error {
				_, err := client.ShowIssue(t.Context(), &generated.ShowIssueRequestOptions{
					PathParams: &generated.ShowIssuePath{ProjectID: 1, Ref: "spoke-project#abc4"},
				})
				return err
			},
		},
		{
			name: "project name with spaces",
			want: "/api/v1/projects/name:spoke%20project/issues/abc4",
			call: func(t *testing.T, client *Client) error {
				_, err := client.EditIssue(t.Context(), &generated.EditIssueRequestOptions{
					PathParams: &generated.EditIssuePath{ProjectID: "name:spoke project", Ref: "abc4"},
					Body:       &generated.EditIssueBody{},
				})
				return err
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var escapedPath string
			server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				escapedPath = r.URL.EscapedPath()
			}))
			defer server.Close()

			client, err := NewWithHTTPClient(server.URL, server.Client())
			require.NoError(t, err)
			limited := client.WithResponseLimit(1024)
			require.NoError(t, tt.call(t, limited))
			require.Equal(t, tt.want, escapedPath)
		})
	}
}

type countedResponseBody struct {
	read   int
	closed bool
}

func (b *countedResponseBody) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = ' '
	}
	b.read += len(p)
	return len(p), nil
}
func (b *countedResponseBody) Close() error { b.closed = true; return nil }

func TestResponseLimitPreservesHTTPFailureStatus(t *testing.T) {
	for _, status := range []int{200, 401, 403, 429, 500, 503} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			body := new(countedResponseBody)
			transport := roundTripFunc(func(_ *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: status, Header: make(http.Header), Body: body}, nil
			})
			client, err := NewWithHTTPClient("https://daemon.example", &http.Client{Transport: transport})
			require.NoError(t, err)
			_, err = client.WithResponseLimit(16).ListProjects(t.Context(), &generated.ListProjectsRequestOptions{})
			require.ErrorIs(t, err, ErrResponseTooLarge)
			var statusError *ResponseTooLargeError
			require.True(t, errors.As(err, &statusError))
			require.Equal(t, status, statusError.StatusCode)
			require.Equal(t, 17, body.read)
			require.True(t, body.closed)
		})
	}
}

func TestResponseLimitReturnsBodyCloseError(t *testing.T) {
	closeErr := errors.New("body close failed")
	body := &closeErrorResponseBody{Reader: strings.NewReader(""), closeErr: closeErr}
	transport := roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: body}, nil
	})
	client, err := NewWithHTTPClient("https://daemon.example", &http.Client{Transport: transport})
	require.NoError(t, err)

	_, err = client.WithResponseLimit(128).ListIssues(t.Context(), &generated.ListIssuesRequestOptions{PathParams: &generated.ListIssuesPath{ProjectID: 1}})
	require.ErrorIs(t, err, closeErr)
	require.True(t, body.closed)
}

type closeErrorResponseBody struct {
	io.Reader
	closeErr error
	closed   bool
}

func (b *closeErrorResponseBody) Close() error {
	b.closed = true
	return b.closeErr
}
