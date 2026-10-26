package client

import (
	"context"
	"net/http"
)

type teammateKey struct{}

// WithTeammate attributes requests made with ctx to handle. An empty handle
// clears an inherited teammate; it does not alter the parent context.
func WithTeammate(ctx context.Context, handle string) context.Context {
	return context.WithValue(ctx, teammateKey{}, handle)
}

type teammateTransport struct{ http.RoundTripper }

func (t teammateTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if handle, ok := req.Context().Value(teammateKey{}).(string); ok {
		req = req.Clone(req.Context())
		req.Header.Del("X-Kata-Teammate")
		if handle != "" {
			req.Header.Set("X-Kata-Teammate", handle)
		}
	}
	return t.RoundTripper.RoundTrip(req)
}
