package config

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type recordingRoundTripper struct {
	calls         int
	authorization string
}

func (r *recordingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	r.calls++
	r.authorization = req.Header.Get("Authorization")
	return &http.Response{
		StatusCode: http.StatusNoContent,
		Body:       http.NoBody,
		Request:    req,
	}, nil
}

func TestBearerTransportCanonicalOrigin(t *testing.T) {
	tests := []struct {
		name              string
		target            string
		wantBaseCalls     int
		wantAuthorization string
		wantErr           bool
	}{
		{
			name:              "equivalent canonical origin",
			target:            "https://daemon.example/resource",
			wantBaseCalls:     1,
			wantAuthorization: "Bearer test-token",
		},
		{
			name:    "different port",
			target:  "https://daemon.example:8443/resource",
			wantErr: true,
		},
		{
			name:    "different scheme",
			target:  "http://daemon.example/resource",
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			base := &recordingRoundTripper{}
			transport := BearerTransport(base, "test-token", "https://Daemon.Example:443")
			req, err := http.NewRequest(http.MethodGet, tt.target, nil)
			require.NoError(t, err)

			resp, err := transport.RoundTrip(req)
			if tt.wantErr {
				require.Error(t, err)
				assert.Nil(t, resp)
			} else {
				require.NoError(t, err)
				require.NotNil(t, resp)
				require.NoError(t, resp.Body.Close())
			}
			assert.Equal(t, tt.wantBaseCalls, base.calls)
			assert.Equal(t, tt.wantAuthorization, base.authorization)
		})
	}
}

func TestBearerTransportCanonicalIPv6ZoneOrigin(t *testing.T) {
	base := &recordingRoundTripper{}
	transport := BearerTransport(
		base,
		"test-token",
		"https://[FE80::ABCD%25EnABC]:00443/configured/path",
	)
	req, err := http.NewRequest(
		http.MethodGet,
		"https://[fe80::abcd%25EnABC]/api/v1/ping",
		nil,
	)
	require.NoError(t, err)

	resp, err := transport.RoundTrip(req)
	require.NoError(t, err)
	require.NotNil(t, resp)
	require.NoError(t, resp.Body.Close())
	assert.Equal(t, 1, base.calls)
	assert.Equal(t, "Bearer test-token", base.authorization)
}
