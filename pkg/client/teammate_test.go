package client

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"
)

// Each child context overrides only its own requests, including empty clearing.
func TestPropertyTeammateRequestIsolation(t *testing.T) {
	var got string
	transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		got = req.Header.Get("X-Kata-Teammate")
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: http.NoBody}, nil
	})
	originalClient := &http.Client{Transport: transport}
	api, err := NewWithHTTPClient("http://daemon.example", originalClient)
	require.NoError(t, err)
	// Portable handles have 1–64 ASCII characters; only the first must be
	// alphanumeric. Generate the full valid domain without testing validation.
	const alphanumeric = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	drawHandle := func(ht *hegel.T) string {
		return hegel.Draw(ht, hegel.Text().Alphabet(alphanumeric).MinSize(1).MaxSize(1)) +
			hegel.Draw(ht, hegel.Text().Alphabet(alphanumeric+"._-").MaxSize(63))
	}
	hegel.Test(t, func(ht *hegel.T) {
		handle, override := drawHandle(ht), drawHandle(ht)
		parent := WithTeammate(context.Background(), handle)
		for _, tc := range []struct {
			ctx  context.Context
			want string
		}{
			{parent, handle}, {WithTeammate(parent, override), override},
			{WithTeammate(parent, ""), ""}, {parent, handle}, {context.Background(), ""},
		} {
			req, err := http.NewRequestWithContext(tc.ctx, http.MethodGet, "http://daemon.example/api/v1/ping", nil)
			require.NoError(t, err)
			resp, err := api.httpClient.Do(req)
			require.NoError(t, err)
			require.NoError(t, resp.Body.Close())
			if got != tc.want {
				ht.Fatalf("header=%q want=%q", got, tc.want)
			}
			require.Empty(t, req.Header.Get("X-Kata-Teammate"), "transport must not mutate original request")
		}
		req, err := http.NewRequestWithContext(parent, http.MethodGet, "http://daemon.example/api/v1/ping", nil)
		require.NoError(t, err)
		resp, err := originalClient.Do(req)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
		require.Empty(t, got, "constructor must not mutate caller's client")
	}, hegel.WithTestCases(100), hegel.WithDatabase(t.TempDir()))
}

func TestTeammateGeneratedAndStreamingRequests(t *testing.T) {
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "worker", r.Header.Get("X-Kata-Teammate"))
		if r.URL.Path == "/api/v1/events/stream" {
			w.Header().Set("Content-Type", "text/event-stream")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true,"service":"kata"}`)
	}))
	t.Cleanup(daemon.Close)
	api, err := NewWithHTTPClient(daemon.URL, daemon.Client())
	require.NoError(t, err)
	ctx := WithTeammate(t.Context(), "worker")
	_, err = api.PingWithResponse(ctx)
	require.NoError(t, err)
	resp, err := api.StreamEventsRaw(ctx, nil)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
}
