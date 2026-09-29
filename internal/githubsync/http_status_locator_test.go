package githubsync

import (
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestGitHubStatusLocatorsUseBoundedIndependentPages(t *testing.T) {
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodGet, r.Method)
		if r.URL.Path == "/repos/example-owner/example-repo" {
			require.NoError(t, json.MarshalWrite(w, map[string]any{"id": 123, "full_name": "example-owner/example-repo"}))
			return
		}
		require.Equal(t, "/repos/example-owner/example-repo/issues", r.URL.Path)
		query := r.URL.Query()
		require.Equal(t, "all", query.Get("state"))
		require.Equal(t, "created", query.Get("sort"))
		require.Equal(t, "asc", query.Get("direction"))
		require.Equal(t, "100", query.Get("per_page"))
		require.Empty(t, query.Get("since"))
		switch query.Get("page") {
		case "1":
			w.Header().Set("Link", fmt.Sprintf(`<%s/repositories/123/issues?state=all&sort=created&direction=asc&per_page=100&page=2>; rel="next"`, server.URL))
			pr := statusIssueWire("open")
			pr["id"] = 457
			pr["number"] = 8
			pr["pull_request"] = map[string]any{"url": "https://api.github.com/repos/example-owner/example-repo/pulls/8"}
			require.NoError(t, json.MarshalWrite(w, []any{statusIssueWire("open"), pr}))
		case "2":
			issue := statusIssueWire("closed")
			issue["id"] = 458
			issue["number"] = 9
			issue["url"] = "https://api.github.com/repos/example-owner/example-repo/issues/9"
			require.NoError(t, json.MarshalWrite(w, []any{issue}))
		default:
			t.Errorf("unexpected page %s", query.Get("page"))
		}
	}))
	defer server.Close()
	f := NewHTTPFetcher(HTTPFetcherConfig{Client: server.Client(), CredentialResolver: newStaticHTTPFetcherTestResolver("test-token"), RESTBaseURLOverride: server.URL})
	raw, err := f.ForBinding(t.Context(), statusConfig().Binding())
	require.NoError(t, err)
	cfg := statusConfig()
	cfg.Since = "2026-09-03"
	session := raw.(StatusLocatorSession)
	locators, next, err := session.StatusLocators(t.Context(), cfg, 1)
	require.NoError(t, err)
	require.Equal(t, 2, next)
	require.Equal(t, []StatusLocator{{ExternalID: "issue-id:456", LegacyExternalIDs: []string{"issue:I_example", "issue-number:7"}, Number: 7}}, locators)
	locators, next, err = session.StatusLocators(t.Context(), cfg, next)
	require.NoError(t, err)
	require.Zero(t, next)
	require.Equal(t, []StatusLocator{{ExternalID: "issue-id:458", LegacyExternalIDs: []string{"issue:I_example", "issue-number:9"}, Number: 9}}, locators)
}

func TestGitHubStatusLocatorsRejectUntrustedPagination(t *testing.T) {
	for _, target := range []string{
		"https://other.example/repos/example-owner/example-repo/issues?page=2",
		"/repositories/124/issues?page=2",
		"/repos/example-owner/other-repo/issues?page=2",
		"/repos/example-owner/example-repo/issues?page=1",
		"/repos/example-owner/example-repo/issues?page=2&since=2026-09-03",
		"/repos/example-owner/example-repo/issues?page=2&per_page=1000",
	} {
		t.Run(target, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/repos/example-owner/example-repo" {
					require.NoError(t, json.MarshalWrite(w, map[string]any{"id": 123, "full_name": "example-owner/example-repo"}))
					return
				}
				w.Header().Set("Link", "<"+target+">; rel=\"next\"")
				require.NoError(t, json.MarshalWrite(w, []any{statusIssueWire("open")}))
			}))
			defer server.Close()
			f := NewHTTPFetcher(HTTPFetcherConfig{Client: server.Client(), CredentialResolver: newStaticHTTPFetcherTestResolver("test-token"), RESTBaseURLOverride: server.URL})
			raw, err := f.ForBinding(t.Context(), statusConfig().Binding())
			require.NoError(t, err)
			_, _, err = raw.(StatusLocatorSession).StatusLocators(t.Context(), statusConfig(), 1)
			require.Error(t, err)
		})
	}
}

func TestGitHubStatusLocatorsRejectMalformedRows(t *testing.T) {
	for _, tc := range []struct {
		name string
		rows func() any
	}{
		{"null list", func() any { return nil }},
		{"missing global ID", func() any { r := statusIssueWire("open"); delete(r, "id"); return []any{r} }},
		{"duplicate global ID", func() any { return []any{statusIssueWire("open"), statusIssueWire("open")} }},
		{"foreign issue", func() any {
			r := statusIssueWire("open")
			r["url"] = "https://api.github.com/repos/example-owner/other-repo/issues/7"
			return []any{r}
		}},
		{"unbounded list", func() any {
			rows := make([]any, 101)
			for i := range rows {
				rows[i] = statusIssueWire("open")
			}
			return rows
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/repos/example-owner/example-repo" {
					require.NoError(t, json.MarshalWrite(w, map[string]any{"id": 123, "full_name": "example-owner/example-repo"}))
					return
				}
				require.NoError(t, json.MarshalWrite(w, tc.rows()))
			}))
			defer server.Close()
			f := NewHTTPFetcher(HTTPFetcherConfig{Client: server.Client(), CredentialResolver: newStaticHTTPFetcherTestResolver("test-token"), RESTBaseURLOverride: server.URL})
			raw, err := f.ForBinding(t.Context(), statusConfig().Binding())
			require.NoError(t, err)
			_, _, err = raw.(StatusLocatorSession).StatusLocators(t.Context(), statusConfig(), 1)
			require.Error(t, err)
		})
	}
}
