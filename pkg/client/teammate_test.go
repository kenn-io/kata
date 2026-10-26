package client

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/pkg/client/generated"
)

// A display selection must stay per-call, apply only to writes, and never
// escape to a different origin even if a generated request editor changes it.
func TestTeammateRequestIsolation(t *testing.T) {
	var mu sync.Mutex
	got := map[string]string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got[r.Method+" "+r.Header.Get("Idempotency-Key")] = r.Header.Get("X-Kata-Teammate")
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"issue":{},"changed":true}`))
	}))
	defer server.Close()
	c, err := New(server.URL)
	require.NoError(t, err)
	parent := WithTeammate(context.Background(), "default-1")
	for _, handle := range []string{"worker-1", "worker-2", ""} {
		ctx := WithTeammate(parent, handle)
		_, err = c.ReopenIssue(ctx, &generated.ReopenIssueRequestOptions{PathParams: &generated.ReopenIssuePath{ProjectID: 1, Ref: "abc1"}, Body: &generated.ReopenIssueBody{Actor: new("coordinator")}}, func(_ context.Context, r *http.Request) error { r.Header.Set("Idempotency-Key", handle); return nil })
		require.NoError(t, err)
	}
	_, err = c.Health(parent)
	require.NoError(t, err)
	for _, handle := range []string{"worker-1", "bad/handle"} {
		_, err = c.ResolveProject(WithTeammate(parent, handle), &generated.ResolveProjectRequestOptions{
			Body: &generated.ResolveProjectBody{Name: new("example-project")},
		}, func(_ context.Context, r *http.Request) error {
			r.Header.Set("Idempotency-Key", "resolve")
			return nil
		})
		require.NoError(t, err, "POST project lookup is used by reads")
	}
	require.Equal(t, map[string]string{"POST worker-1": "worker-1", "POST worker-2": "worker-2", "POST ": "", "GET ": "", "POST resolve": ""}, got)
}

func TestTeammateOriginAndValidation(t *testing.T) {
	seen := make(chan string, 4)
	foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Header.Get("X-Kata-Teammate")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"issue":{},"changed":true}`))
	}))
	defer foreign.Close()
	selected := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Header.Get("X-Kata-Teammate")
		http.Redirect(w, r, foreign.URL+"/redirected", http.StatusTemporaryRedirect)
	}))
	defer selected.Close()
	c, err := New(selected.URL)
	require.NoError(t, err)
	options := &generated.ReopenIssueRequestOptions{PathParams: &generated.ReopenIssuePath{ProjectID: 1, Ref: "abc1"}, Body: &generated.ReopenIssueBody{Actor: new("coordinator")}}
	_, err = c.ReopenIssue(WithTeammate(t.Context(), "worker-1"), options)
	require.NoError(t, err)
	require.Equal(t, "worker-1", <-seen)
	require.Empty(t, <-seen, "redirects must not leak contribution")
	_, err = c.ReopenIssue(WithTeammate(t.Context(), "bad/handle"), options)
	require.ErrorContains(t, err, "teammate")
	select {
	case value := <-seen:
		t.Fatalf("invalid selection sent request %q", value)
	default:
	}
	_, err = c.ReopenIssue(WithTeammate(t.Context(), "worker-1"), options, func(_ context.Context, r *http.Request) error {
		r.URL.Scheme = "http"
		r.URL.Host = strings.TrimPrefix(foreign.URL, "http://")
		return nil
	})
	require.NoError(t, err)
	require.Empty(t, <-seen, "per-call editors cannot retarget contribution")
}

func TestTeammateCallerRedirectPolicy(t *testing.T) {
	for _, reject := range []bool{false, true} {
		t.Run(fmt.Sprint(reject), func(t *testing.T) {
			var seen []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				seen = append(seen, r.Header.Get("X-Kata-Teammate"))
				if r.URL.Path != "/final" {
					http.Redirect(w, r, "/final", http.StatusTemporaryRedirect)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"issue":{},"changed":true}`))
			}))
			defer server.Close()
			redirects := 0
			policyErr := errors.New("caller refused redirect")
			caller := server.Client()
			originalTransport := caller.Transport
			caller.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
				redirects++
				if reject {
					return policyErr
				}
				return nil
			}
			c, err := NewWithHTTPClient(server.URL, caller)
			require.NoError(t, err)
			require.Same(t, originalTransport, caller.Transport)
			_, err = c.ReopenIssue(WithTeammate(t.Context(), "worker-1"), &generated.ReopenIssueRequestOptions{PathParams: &generated.ReopenIssuePath{ProjectID: 1, Ref: "abc1"}, Body: &generated.ReopenIssueBody{Actor: new("coordinator")}})
			require.Equal(t, 1, redirects)
			if reject {
				require.ErrorIs(t, err, policyErr)
				require.Equal(t, []string{"worker-1"}, seen)
			} else {
				require.NoError(t, err)
				require.Equal(t, []string{"worker-1", "worker-1"}, seen)
			}
		})
	}
}
