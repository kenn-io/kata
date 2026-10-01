package githubsync

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/issuesync"
)

func statusConfig() Config {
	return Config{Host: "github.com", Owner: "example-owner", Repo: "example-repo", RepoID: 123, StatusSync: "two-way"}
}
func statusIssueWire(state string) map[string]any {
	return map[string]any{"id": 456, "node_id": "I_example", "number": 7, "url": "https://api.github.com/repos/example-owner/example-repo/issues/7", "html_url": "https://github.com/example-owner/example-repo/issues/7", "title": "Editable title", "body": "Editable body", "state": state, "state_reason": nil, "created_at": "2026-09-01T00:00:00Z", "updated_at": "2026-09-02T00:00:00Z", "user": map[string]any{"login": "example-user"}, "labels": []any{}, "assignees": []any{}}
}
func TestGitHubStatusWriteUsesTrustedLocatorAndVerifiedReadback(t *testing.T) {
	state := "open"
	patches, admissions := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertHTTPFetcherHeaders(t, r)
		switch r.URL.Path {
		case "/repos/example-owner/example-repo":
			require.Equal(t, http.MethodGet, r.Method)
			require.NoError(t, json.MarshalWrite(w, map[string]any{"id": 123, "node_id": "R_example", "full_name": "example-owner/example-repo"}))
		case "/repos/example-owner/example-repo/issues/7":
			if r.Method == http.MethodPatch {
				patches++
				body, err := io.ReadAll(r.Body)
				require.NoError(t, err)
				require.JSONEq(t, `{"state":"closed"}`, string(body))
				state = "closed"
			}
			require.NoError(t, json.MarshalWrite(w, statusIssueWire(state)))
		default:
			t.Fatalf("unexpected endpoint %s", r.URL.Path)
		}
	}))
	defer server.Close()
	f := NewHTTPFetcher(HTTPFetcherConfig{Client: server.Client(), CredentialResolver: newStaticHTTPFetcherTestResolver("test-token"), RESTBaseURLOverride: server.URL})
	raw, err := f.ForBinding(t.Context(), statusConfig().Binding())
	require.NoError(t, err)
	s := raw.(StatusSession)
	observed, err := s.WriteStatus(t.Context(), statusConfig(), "issue-id:456", 7, "closed", func() error { admissions++; return nil })
	require.NoError(t, err)
	require.Equal(t, "closed", observed.Status)
	require.Equal(t, "closed", *observed.RawStatus)
	require.Equal(t, "7", observed.Locator)
	require.Equal(t, time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC), observed.Version)
	require.Equal(t, 1, patches)
	require.Equal(t, 1, admissions)
}

func TestGitHubAmbiguousStatusWriteIsNotBlindlyRetried(t *testing.T) {
	state, patches := "open", 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/repos/example-owner/example-repo" {
			require.NoError(t, json.MarshalWrite(w, map[string]any{"id": 123, "full_name": "example-owner/example-repo"}))
			return
		}
		if r.Method == http.MethodPatch {
			patches++
			state = "closed"
			w.WriteHeader(503)
			_, err := w.Write([]byte(`{"message":"test-secret private failure"}`))
			require.NoError(t, err)
			return
		}
		require.NoError(t, json.MarshalWrite(w, statusIssueWire(state)))
	}))
	defer server.Close()
	f := NewHTTPFetcher(HTTPFetcherConfig{Client: server.Client(), CredentialResolver: newStaticHTTPFetcherTestResolver("test-token"), RESTBaseURLOverride: server.URL})
	raw, err := f.ForBinding(t.Context(), statusConfig().Binding())
	require.NoError(t, err)
	s := raw.(StatusSession)
	_, err = s.WriteStatus(t.Context(), statusConfig(), "issue-id:456", 7, "closed", func() error { return nil })
	var delivery *issuesync.StatusError
	require.ErrorAs(t, err, &delivery)
	require.True(t, delivery.Ambiguous)
	require.NotContains(t, err.Error(), "test-secret")
	require.Equal(t, 1, patches)
	observed, err := s.WriteStatus(t.Context(), statusConfig(), "issue-id:456", 7, "closed", func() error { return nil })
	require.NoError(t, err)
	require.Equal(t, "closed", observed.Status)
	require.Equal(t, 1, patches)
}

func TestGitHubStatusRejectsWrongIdentityBeforeMutation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		repoID int
		change func(map[string]any)
	}{
		{"repository changed", 124, func(map[string]any) {}},
		{"different issue", 123, func(i map[string]any) { i["id"] = 457 }},
		{"different number", 123, func(i map[string]any) { i["number"] = 8 }},
		{"pull request", 123, func(i map[string]any) {
			i["pull_request"] = map[string]any{"url": "https://api.github.com/repos/example-owner/example-repo/pulls/7"}
		}},
		{"unknown state", 123, func(i map[string]any) { i["state"] = "future-state" }},
		{"moved issue URL", 123, func(i map[string]any) { i["url"] = "https://api.github.com/repos/example-owner/other-repo/issues/7" }},
		{"foreign issue URL", 123, func(i map[string]any) { i["url"] = "https://other.example/repos/example-owner/example-repo/issues/7" }},
		{"missing version", 123, func(i map[string]any) { delete(i, "updated_at") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				require.Equal(t, http.MethodGet, r.Method)
				if r.URL.Path == "/repos/example-owner/example-repo" {
					require.NoError(t, json.MarshalWrite(w, map[string]any{"id": tc.repoID, "full_name": "example-owner/example-repo"}))
					return
				}
				i := statusIssueWire("open")
				tc.change(i)
				require.NoError(t, json.MarshalWrite(w, i))
			}))
			defer server.Close()
			f := NewHTTPFetcher(HTTPFetcherConfig{Client: server.Client(), CredentialResolver: newStaticHTTPFetcherTestResolver("test-token"), RESTBaseURLOverride: server.URL})
			raw, err := f.ForBinding(t.Context(), statusConfig().Binding())
			require.NoError(t, err)
			_, err = raw.(StatusSession).WriteStatus(t.Context(), statusConfig(), "issue-id:456", 7, "closed", func() error { t.Fatal("invalid identity admitted"); return errors.New("invalid") })
			require.Error(t, err)
		})
	}
}

func TestGitHubStatusPreservesMatchingStateAndRejectsStaleAdmission(t *testing.T) {
	for _, matching := range []bool{false, true} {
		t.Run(map[bool]string{false: "admission rejected", true: "matching closed"}[matching], func(t *testing.T) {
			denied := errors.New("binding disabled")
			state := "open"
			if matching {
				state = "closed"
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				require.Equal(t, http.MethodGet, r.Method)
				if r.URL.Path == "/repos/example-owner/example-repo" {
					require.NoError(t, json.MarshalWrite(w, map[string]any{"id": 123, "full_name": "example-owner/example-repo"}))
					return
				}
				require.NoError(t, json.MarshalWrite(w, statusIssueWire(state)))
			}))
			defer server.Close()
			f := NewHTTPFetcher(HTTPFetcherConfig{Client: server.Client(), CredentialResolver: newStaticHTTPFetcherTestResolver("test-token"), RESTBaseURLOverride: server.URL})
			raw, err := f.ForBinding(t.Context(), statusConfig().Binding())
			require.NoError(t, err)
			admissions := 0
			observed, err := raw.(StatusSession).WriteStatus(t.Context(), statusConfig(), "issue-id:456", 7, "closed", func() error { admissions++; return denied })
			if matching {
				require.NoError(t, err)
				require.Equal(t, "closed", observed.Status)
				require.Zero(t, admissions)
			} else {
				require.ErrorIs(t, err, denied)
				require.Equal(t, 1, admissions)
			}
		})
	}
}

func TestGitHubStatusRejectsRedirectsAndSanitizesFailures(t *testing.T) {
	for _, code := range []int{302, 401, 403, 404, 409, 429, 503} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			foreign := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("redirect reached another origin") }))
			defer foreign.Close()
			patches := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPatch {
					patches++
					w.Header().Set("Location", foreign.URL)
					w.Header().Set("Retry-After", "7")
					w.WriteHeader(code)
					_, err := w.Write([]byte(`{"message":"test-secret private failure"}`))
					require.NoError(t, err)
					return
				}
				if r.URL.Path == "/repos/example-owner/example-repo" {
					require.NoError(t, json.MarshalWrite(w, map[string]any{"id": 123, "full_name": "example-owner/example-repo"}))
					return
				}
				require.NoError(t, json.MarshalWrite(w, statusIssueWire("open")))
			}))
			defer server.Close()
			client := server.Client()
			client.CheckRedirect = func(*http.Request, []*http.Request) error { return nil }
			f := NewHTTPFetcher(HTTPFetcherConfig{Client: client, CredentialResolver: newStaticHTTPFetcherTestResolver("test-token"), RESTBaseURLOverride: server.URL})
			raw, err := f.ForBinding(t.Context(), statusConfig().Binding())
			require.NoError(t, err)
			_, err = raw.(StatusSession).WriteStatus(t.Context(), statusConfig(), "issue-id:456", 7, "closed", func() error { return nil })
			var delivery *issuesync.StatusError
			require.ErrorAs(t, err, &delivery)
			require.Equal(t, 1, patches)
			require.Equal(t, code >= 500, delivery.Ambiguous)
			require.NotContains(t, err.Error(), "test-secret")
			require.NotContains(t, err.Error(), foreign.URL)
		})
	}
}

func TestGitHubStatusConfigModeRoundtripAndValidation(t *testing.T) {
	for _, mode := range []string{"", "one-way", "two-way"} {
		cfg := statusConfig()
		cfg.StatusSync = mode
		encoded, err := EncodeConfig(cfg)
		require.NoError(t, err)
		decoded, err := DecodeConfig(encoded)
		require.NoError(t, err)
		require.Equal(t, mode, decoded.StatusSync)
	}
	cfg := statusConfig()
	cfg.StatusSync = "unknown-mode"
	_, err := EncodeConfig(cfg)
	require.Error(t, err)
}

func TestGitHubStatusRateLimitIsSharedAcrossSessions(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if requests == 1 {
			w.Header().Set("Retry-After", "7")
			w.WriteHeader(429)
			_, err := w.Write([]byte(`{"message":"rate limit"}`))
			require.NoError(t, err)
			return
		}
		if r.URL.Path == "/repos/example-owner/example-repo" {
			require.NoError(t, json.MarshalWrite(w, map[string]any{"id": 123, "full_name": "example-owner/example-repo"}))
			return
		}
		require.NoError(t, json.MarshalWrite(w, statusIssueWire("open")))
	}))
	defer server.Close()
	f := NewHTTPFetcher(HTTPFetcherConfig{Client: server.Client(), CredentialResolver: newStaticHTTPFetcherTestResolver("test-token"), RESTBaseURLOverride: server.URL})
	now := time.Date(2026, 9, 3, 0, 0, 0, 0, time.UTC)
	f.graphQLNow = func() time.Time { return now }
	first, err := f.ForBinding(t.Context(), statusConfig().Binding())
	require.NoError(t, err)
	_, err = first.(StatusSession).ReadStatus(t.Context(), statusConfig(), "issue-id:456", 7)
	var rate *issuesync.StatusError
	require.ErrorAs(t, err, &rate)
	require.Equal(t, 7*time.Second, rate.RetryAfter)
	second, err := f.ForBinding(t.Context(), statusConfig().Binding())
	require.NoError(t, err)
	_, err = second.(StatusSession).ReadStatus(t.Context(), statusConfig(), "issue-id:456", 7)
	require.ErrorAs(t, err, &rate)
	require.Equal(t, 1, requests)
	now = now.Add(7 * time.Second)
	observed, err := second.(StatusSession).ReadStatus(t.Context(), statusConfig(), "issue-id:456", 7)
	require.NoError(t, err)
	require.Equal(t, "open", observed.Status)
	require.Equal(t, 3, requests)
}

func TestGitHubStatusRejectsMismatchedPatchIdentity(t *testing.T) {
	state := "open"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/repos/example-owner/example-repo" {
			require.NoError(t, json.MarshalWrite(w, map[string]any{"id": 123, "full_name": "example-owner/example-repo"}))
			return
		}
		row := statusIssueWire(state)
		if r.Method == http.MethodPatch {
			state = "closed"
			row = statusIssueWire(state)
			row["id"] = 457
		}
		require.NoError(t, json.MarshalWrite(w, row))
	}))
	defer server.Close()
	f := NewHTTPFetcher(HTTPFetcherConfig{Client: server.Client(), CredentialResolver: newStaticHTTPFetcherTestResolver("test-token"), RESTBaseURLOverride: server.URL})
	raw, err := f.ForBinding(t.Context(), statusConfig().Binding())
	require.NoError(t, err)
	_, err = raw.(StatusSession).WriteStatus(t.Context(), statusConfig(), "issue-id:456", 7, "closed", func() error { return nil })
	var delivery *issuesync.StatusError
	require.ErrorAs(t, err, &delivery)
	require.True(t, delivery.Ambiguous)
}

func TestGitHubStatusReadCarriesVerifiedCloseMetadata(t *testing.T) {
	closedAt := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/repos/example-owner/example-repo" {
			require.NoError(t, json.MarshalWrite(w, map[string]any{"id": 123, "full_name": "example-owner/example-repo"}))
			return
		}
		row := statusIssueWire("closed")
		row["state_reason"] = "not_planned"
		row["closed_at"] = closedAt.Format(time.RFC3339Nano)
		require.NoError(t, json.MarshalWrite(w, row))
	}))
	defer server.Close()
	f := NewHTTPFetcher(HTTPFetcherConfig{Client: server.Client(), CredentialResolver: newStaticHTTPFetcherTestResolver("test-token"), RESTBaseURLOverride: server.URL})
	raw, err := f.ForBinding(t.Context(), statusConfig().Binding())
	require.NoError(t, err)
	obs, err := raw.(StatusSession).ReadStatus(t.Context(), statusConfig(), "issue-id:456", 7)
	require.NoError(t, err)
	require.Equal(t, "wontfix", obs.ClosedReason)
	require.Equal(t, &closedAt, obs.ClosedAt)
}

func TestGitHubStatusCooldownAlsoFencesContentRESTAndGraphQL(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()
	f := NewHTTPFetcher(HTTPFetcherConfig{Client: server.Client(), CredentialResolver: newStaticHTTPFetcherTestResolver("test-token"), RESTBaseURLOverride: server.URL, GraphQLURLOverride: server.URL + "/graphql"})
	raw, err := f.ForBinding(t.Context(), statusConfig().Binding())
	require.NoError(t, err)
	_, err = raw.(StatusSession).ReadStatus(t.Context(), statusConfig(), "issue-id:456", 7)
	require.Error(t, err)
	require.Equal(t, 1, calls)
	_, err = raw.Repository(t.Context(), "github.com", "example-owner", "example-repo")
	var rate *issuesync.StatusError
	require.ErrorAs(t, err, &rate)
	require.Positive(t, rate.RetryAfter)
	require.Equal(t, 1, calls)
	_, err = raw.ParentData(t.Context(), statusConfig().Binding())
	require.ErrorAs(t, err, &rate)
	require.Positive(t, rate.RetryAfter)
	require.Equal(t, 1, calls)
}

func TestGitHubContentRateLimitFencesNextStatusRead(t *testing.T) {
	for _, graphql := range []bool{false, true} {
		t.Run(fmt.Sprint(graphql), func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls++
				w.Header().Set("Retry-After", "600")
				w.WriteHeader(http.StatusTooManyRequests)
			}))
			defer server.Close()
			f := NewHTTPFetcher(HTTPFetcherConfig{Client: server.Client(), CredentialResolver: newStaticHTTPFetcherTestResolver("test-token"), RESTBaseURLOverride: server.URL, GraphQLURLOverride: server.URL + "/graphql"})
			raw, err := f.ForBinding(t.Context(), statusConfig().Binding())
			require.NoError(t, err)
			if graphql {
				_, err = raw.ParentData(t.Context(), statusConfig().Binding())
			} else {
				_, err = raw.Repository(t.Context(), "github.com", "example-owner", "example-repo")
			}
			require.Error(t, err)
			require.Equal(t, 1, calls)
			_, err = raw.(StatusSession).ReadStatus(t.Context(), statusConfig(), "issue-id:456", 7)
			var rate *issuesync.StatusError
			require.ErrorAs(t, err, &rate)
			require.Positive(t, rate.RetryAfter)
			require.Equal(t, 1, calls)
		})
	}
}

func TestGitHubStatusWriteThatNeverConnectsIsNotAmbiguous(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/repos/example-owner/example-repo" {
			require.NoError(t, json.MarshalWrite(w, map[string]any{"id": 123, "full_name": "example-owner/example-repo"}))
			return
		}
		require.NoError(t, json.MarshalWrite(w, statusIssueWire("open")))
	}))
	defer server.Close()
	client := &http.Client{Transport: patchDialFailure{reads: server.Client().Transport}}
	f := NewHTTPFetcher(HTTPFetcherConfig{Client: client, CredentialResolver: newStaticHTTPFetcherTestResolver("test-token"), RESTBaseURLOverride: server.URL})
	raw, err := f.ForBinding(t.Context(), statusConfig().Binding())
	require.NoError(t, err)
	_, err = raw.(StatusSession).WriteStatus(t.Context(), statusConfig(), "issue-id:456", 7, "closed", func() error { return nil })
	var delivery *issuesync.StatusError
	require.ErrorAs(t, err, &delivery)
	require.False(t, delivery.Ambiguous, "a write that never connected was not sent")
}

// patchDialFailure serves reads normally and fails PATCH requests at dial time.
type patchDialFailure struct{ reads http.RoundTripper }

func (p patchDialFailure) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Method != http.MethodPatch {
		return p.reads.RoundTrip(r)
	}
	refused := &http.Transport{DialContext: func(context.Context, string, string) (net.Conn, error) {
		return nil, errors.New("connection refused")
	}}
	return refused.RoundTrip(r)
}

func TestGitHubRateLimitCooldownStaysWithItsRepository(t *testing.T) {
	limited := true
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if limited && strings.HasPrefix(r.URL.Path, "/repos/example-owner/example-repo") {
			limited = false
			w.Header().Set("Retry-After", "600")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		if r.URL.Path == "/repos/example-owner/other-repo" {
			require.NoError(t, json.MarshalWrite(w, map[string]any{"id": 124, "full_name": "example-owner/other-repo"}))
			return
		}
		row := statusIssueWire("open")
		row["url"] = strings.Replace(row["url"].(string), "example-repo", "other-repo", 1)
		require.NoError(t, json.MarshalWrite(w, row))
	}))
	defer server.Close()
	f := NewHTTPFetcher(HTTPFetcherConfig{Client: server.Client(), CredentialResolver: newStaticHTTPFetcherTestResolver("test-token"), RESTBaseURLOverride: server.URL})
	limitedSession, err := f.ForBinding(t.Context(), statusConfig().Binding())
	require.NoError(t, err)
	_, err = limitedSession.(StatusSession).ReadStatus(t.Context(), statusConfig(), "issue-id:456", 7)
	require.Error(t, err)

	other := statusConfig()
	other.Repo, other.RepoID = "other-repo", 124
	otherSession, err := f.ForBinding(t.Context(), other.Binding())
	require.NoError(t, err)
	_, err = otherSession.(StatusSession).ReadStatus(t.Context(), other, "issue-id:456", 7)
	require.NoError(t, err, "another repository's status reads are not paused")
	_, err = otherSession.Repository(t.Context(), "github.com", "example-owner", "other-repo")
	require.NoError(t, err, "another repository's content reads are not paused")
}
