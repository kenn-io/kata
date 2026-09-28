package tui

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"sync"

	"go.kenn.io/kata/internal/client"
)

// A profile connection shares its endpoint snapshot across API and SSE. The
// callers can keep their original base URL while a verified runtime moves to
// another socket or loopback port.
type localProfileTransports struct {
	mu          sync.Mutex
	refreshGate chan struct{}
	target      daemonTarget
	origin      string
	generation  uint64
	api, sse    *http.Client
	apiRT       http.RoundTripper
	sseRT       http.RoundTripper
}

func shareLocalProfileTransports(target daemonTarget, api, sse *http.Client) *localProfileTransports {
	s := &localProfileTransports{target: target, origin: target.resolved.BaseURL, api: api, sse: sse, apiRT: api.Transport, sseRT: sse.Transport, refreshGate: make(chan struct{}, 1)}
	api.Transport = localProfileTransport{s, clientOptsNormal}
	sse.Transport = localProfileTransport{s, clientOptsSSE}
	return s
}

func (s *localProfileTransports) refresh(ctx context.Context, generation uint64) error {
	select {
	case s.refreshGate <- struct{}{}:
		defer func() { <-s.refreshGate }()
	case <-ctx.Done():
		return ctx.Err()
	}
	s.mu.Lock()
	if generation != s.generation {
		s.mu.Unlock()
		return nil // Another request already refreshed this failed endpoint.
	}
	target := s.target
	s.mu.Unlock()
	resolved, err := client.EnsureSameLocalProfile(ctx, target.resolved)
	if err != nil {
		return err
	}
	target.resolved = resolved
	api, err := newHTTPClientForTUI(ctx, resolved.BaseURL, target, clientOptsNormal)
	if err != nil {
		return err
	}
	sse, err := newHTTPClientForTUI(ctx, resolved.BaseURL, target, clientOptsSSE)
	if err != nil {
		api.CloseIdleConnections()
		return err
	}
	s.mu.Lock()
	oldTransports := []http.RoundTripper{s.apiRT, s.sseRT}
	s.target, s.apiRT, s.sseRT = target, api.Transport, sse.Transport
	s.generation++
	s.mu.Unlock()
	for _, transport := range oldTransports {
		if closer, ok := transport.(interface{ CloseIdleConnections() }); ok {
			closer.CloseIdleConnections()
		}
	}
	return nil
}

func (s *localProfileTransports) refreshAPI(ctx context.Context) (*http.Client, error) {
	s.mu.Lock()
	generation := s.generation
	s.mu.Unlock()
	if err := s.refresh(ctx, generation); err != nil {
		return nil, err
	}
	return s.api, nil
}

type localProfileTransport struct {
	state *localProfileTransports
	kind  clientOptsKind
}

func (t localProfileTransport) roundTrip(req *http.Request) (*http.Response, uint64, error) {
	t.state.mu.Lock()
	base, generation := t.state.target.resolved.BaseURL, t.state.generation
	transport := t.state.apiRT
	if t.kind == clientOptsSSE {
		transport = t.state.sseRT
	}
	t.state.mu.Unlock()
	endpoint, err := url.Parse(base)
	if err != nil {
		return nil, generation, err
	}
	clone := req.Clone(req.Context())
	clone.URL.Scheme, clone.URL.Host = endpoint.Scheme, endpoint.Host
	clone.Host = endpoint.Host
	resp, err := transport.RoundTrip(clone)
	return resp, generation, err
}

func (t localProfileTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Scheme+"://"+req.URL.Host != t.state.origin {
		return nil, fmt.Errorf("local profile request has an unexpected origin")
	}
	resp, generation, err := t.roundTrip(req)
	if err == nil || req.Context().Err() != nil {
		return resp, err
	}
	if refreshErr := t.state.refresh(req.Context(), generation); refreshErr != nil {
		return nil, refreshErr
	}
	if !canRetryLocalTransport(req.Method, map[string]string{"Idempotency-Key": req.Header.Get("Idempotency-Key")}) {
		return nil, err
	}
	clone := req.Clone(req.Context())
	if req.Body != nil {
		if req.GetBody == nil {
			return nil, err
		}
		clone.Body, err = req.GetBody()
		if err != nil {
			return nil, err
		}
	}
	resp, _, err = t.roundTrip(clone)
	return resp, err
}
