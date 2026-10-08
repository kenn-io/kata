package githubsync

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
)

var testParentAlias = regexp.MustCompile(`i([0-9]+):\s*issue\(number:\s*([0-9]+)\)`)

func TestIncrementalParentUnionAndBoundaryPaging(t *testing.T) {
	since := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	var pages, graphCalls int
	var queried []int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			pages++
			assert.Equal(t, "/repos/example-owner/example-repo/issues/events", r.URL.Path)
			assert.Equal(t, "100", r.URL.Query().Get("per_page"))
			switch pages {
			case 1:
				w.Header().Set("Link", `<?per_page=100&page=2>; rel="next"`)
				_, _ = fmt.Fprint(w, `[{"event":"parent_issue_removed","created_at":"2026-10-01T12:01:00Z","issue":{"number":3}}, {"event":"parent_issue_added","created_at":"2026-10-01T12:00:00Z","issue":{"number":4}}]`)
			case 2:
				w.Header().Set("Link", `<?per_page=100&page=3>; rel="next"`)
				_, _ = fmt.Fprint(w, `[{"event":"parent_issue_removed","created_at":"2026-10-01T12:00:00Z","issue":{"number":4}}, {"event":"parent_issue_added","created_at":"2026-10-01T11:59:59Z","issue":{"number":5}}]`)
			default:
				t.Error("read events older than cursor")
				_, _ = fmt.Fprint(w, `[]`)
			}
			return
		}
		graphCalls++
		var request parentGraphQLRequest
		require.NoError(t, json.UnmarshalRead(r.Body, &request))
		assert.NotContains(t, request.Query, "issues(first:")
		nodes := map[string]any{}
		for _, match := range testParentAlias.FindAllStringSubmatch(request.Query, -1) {
			n, _ := strconv.Atoi(match[2])
			queried = append(queried, n)
			nodes["i"+match[1]] = map[string]any{"number": n, "fullDatabaseId": 100 + n, "parent": nil}
		}
		require.NoError(t, json.MarshalWrite(w, map[string]any{"data": map[string]any{"repository": nodes}}))
	}))
	defer server.Close()
	f := newParentGraphQLTestFetcher(server.URL + "/graphql")
	f.restBaseURLOverride = server.URL + "/"
	data, err := f.ParentData(context.Background(), Binding{Host: "github.com", Owner: "example-owner", Repo: "example-repo"}, ParentRequest{Since: &since, IssueNumbers: []int{2, 2}})
	require.NoError(t, err)
	assert.Equal(t, 2, pages)
	assert.Equal(t, 1, graphCalls)
	assert.Equal(t, []int{2, 3, 4}, queried)
	assert.Equal(t, ParentScanIncremental, data.Scan)
	assert.Equal(t, map[int]int64{2: 102, 3: 103, 4: 104}, data.ScannedChildIDs)
	assert.Empty(t, data.ParentByChild)
}

func TestIncrementalParentIdleMakesNoGraphQLRequests(t *testing.T) {
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		assert.Equal(t, http.MethodGet, r.Method)
		_, _ = fmt.Fprint(w, `[]`)
	}))
	defer server.Close()
	f := newParentGraphQLTestFetcher(server.URL + "/graphql")
	f.restBaseURLOverride = server.URL + "/"
	since := time.Now()
	data, err := f.ParentData(t.Context(), Binding{Host: "github.com", Owner: "example-owner", Repo: "example-repo"}, ParentRequest{Since: &since})
	require.NoError(t, err)
	assert.Equal(t, 1, requests)
	assert.Equal(t, ParentScanIncremental, data.Scan)
	assert.Empty(t, data.ScannedChildIDs)
}

// GitHub stops the repository issue-event feed at page 300 with a full page
// and no next link, so a gap of more than 30,000 events never reaches the cursor.
func TestIncrementalParentTruncatedEventFeedFallsBackToFullScan(t *testing.T) {
	var eventPages int
	var queries []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			eventPages++
			events := make([]string, 100)
			for i := range events {
				events[i] = `{"event":"labeled","created_at":"2026-09-20T00:00:00Z","issue":{"number":9}}`
			}
			_, _ = fmt.Fprint(w, "["+strings.Join(events, ",")+"]")
			return
		}
		var request parentGraphQLRequest
		require.NoError(t, json.UnmarshalRead(r.Body, &request))
		queries = append(queries, request.Query)
		_, _ = fmt.Fprint(w, `{"data":{"repository":{"issues":{"pageInfo":{"hasNextPage":false,"endCursor":null},"nodes":[`+
			`{"number":7,"fullDatabaseId":107,"parent":null},`+
			`{"number":8,"fullDatabaseId":108,"parent":{"number":1,"fullDatabaseId":101}}]}}}}`)
	}))
	defer server.Close()
	f := newParentGraphQLTestFetcher(server.URL + "/graphql")
	f.restBaseURLOverride = server.URL + "/"
	since := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	data, err := f.ParentData(t.Context(), Binding{Host: "github.com", Owner: "example-owner", Repo: "example-repo"}, ParentRequest{Since: &since, IssueNumbers: []int{8}})
	require.NoError(t, err)
	assert.Equal(t, 1, eventPages)
	require.Len(t, queries, 1)
	assert.Contains(t, queries[0], "issues(first: 100")
	assert.Equal(t, ParentScanComplete, data.Scan)
	assert.Equal(t, map[int]int64{7: 107, 8: 108}, data.ScannedChildIDs)
	assert.Equal(t, map[int]int64{8: 101}, data.ParentByChild)
}

func TestIncrementalParentRechecksSameRepositoryChildrenOfNewParents(t *testing.T) {
	var subIssuePaths []string
	var batches [][]int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			if r.URL.Path == "/repos/example-owner/example-repo/issues/events" {
				_, _ = fmt.Fprint(w, `[]`)
				return
			}
			subIssuePaths = append(subIssuePaths, r.URL.Path)
			_, _ = fmt.Fprint(w, `[`+
				`{"number":1,"repository":{"full_name":"example-owner/example-repo"}},`+
				`{"number":7,"repository":{"full_name":"other-owner/other-repo"}}]`)
			return
		}
		var request parentGraphQLRequest
		require.NoError(t, json.UnmarshalRead(r.Body, &request))
		var batch []int
		nodes := map[string]any{}
		for _, match := range testParentAlias.FindAllStringSubmatch(request.Query, -1) {
			n, _ := strconv.Atoi(match[2])
			batch = append(batch, n)
			var parent any
			if n == 1 {
				parent = map[string]any{"number": 2, "fullDatabaseId": 102}
			}
			nodes["i"+match[1]] = map[string]any{"number": n, "fullDatabaseId": 100 + n, "parent": parent}
		}
		batches = append(batches, batch)
		require.NoError(t, json.MarshalWrite(w, map[string]any{"data": map[string]any{"repository": nodes}}))
	}))
	defer server.Close()
	f := newParentGraphQLTestFetcher(server.URL + "/graphql")
	f.restBaseURLOverride = server.URL + "/"
	since := time.Now()
	data, err := f.ParentData(t.Context(), Binding{Host: "github.com", Owner: "example-owner", Repo: "example-repo"}, ParentRequest{Since: &since, IssueNumbers: []int{2}, ChildrenOf: []int{2}})
	require.NoError(t, err)
	assert.Equal(t, []string{"/repos/example-owner/example-repo/issues/2/sub_issues"}, subIssuePaths)
	assert.Equal(t, [][]int{{2}, {1}}, batches)
	assert.Equal(t, ParentScanIncremental, data.Scan)
	assert.Equal(t, map[int]int64{1: 101, 2: 102}, data.ScannedChildIDs)
	assert.Equal(t, map[int]int64{1: 102}, data.ParentByChild)
}

func TestIncrementalParentUnsupportedHostSkipsSubIssueLookup(t *testing.T) {
	var subIssueRequests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			if r.URL.Path != "/repos/example-owner/example-repo/issues/events" {
				subIssueRequests++
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_, _ = fmt.Fprint(w, `[]`)
			return
		}
		_, _ = fmt.Fprint(w, `{"errors":[{"type":"undefinedField","message":"Field 'parent' doesn't exist on type 'Issue'"}]}`)
	}))
	defer server.Close()
	f := newParentGraphQLTestFetcher(server.URL + "/graphql")
	f.restBaseURLOverride = server.URL + "/"
	since := time.Now()
	data, err := f.ParentData(t.Context(), Binding{Host: "github.com", Owner: "example-owner", Repo: "example-repo"}, ParentRequest{Since: &since, IssueNumbers: []int{2}, ChildrenOf: []int{2}})
	require.NoError(t, err)
	assert.Equal(t, ParentScanUnsupported, data.Scan)
	assert.Zero(t, subIssueRequests)
}

func TestIncrementalParentBatchesBoundedSelection(t *testing.T) {
	var counts []int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = fmt.Fprint(w, `[]`)
			return
		}
		var request parentGraphQLRequest
		require.NoError(t, json.UnmarshalRead(r.Body, &request))
		nodes := map[string]any{}
		matches := testParentAlias.FindAllStringSubmatch(request.Query, -1)
		counts = append(counts, len(matches))
		for _, m := range matches {
			n, _ := strconv.Atoi(m[2])
			nodes["i"+m[1]] = map[string]any{"number": n, "fullDatabaseId": 1000 + n, "parent": map[string]any{"number": 999, "fullDatabaseId": 1999}}
		}
		require.NoError(t, json.MarshalWrite(w, map[string]any{"data": map[string]any{"repository": nodes}}))
	}))
	defer server.Close()
	f := newParentGraphQLTestFetcher(server.URL + "/graphql")
	f.restBaseURLOverride = server.URL + "/"
	numbers := make([]int, 205)
	for i := range numbers {
		numbers[i] = i + 1
	}
	since := time.Now()
	data, err := f.ParentData(t.Context(), Binding{Host: "github.com", Owner: "example-owner", Repo: "example-repo"}, ParentRequest{Since: &since, IssueNumbers: numbers})
	require.NoError(t, err)
	assert.Equal(t, []int{100, 100, 5}, counts)
	assert.Len(t, data.ScannedChildIDs, 205)
	assert.Len(t, data.ParentByChild, 205)
}

func TestIncrementalParentMalformedResponsesDoNotSupplyAuthority(t *testing.T) {
	for _, tc := range []struct{ name, events, graph string }{
		{"missing timestamp", `[{"event":"parent_issue_removed","issue":{"number":1}}]`, ``},
		{"missing child", `[{"event":"parent_issue_removed","created_at":"2026-10-01T12:00:00Z"}]`, ``},
		{"missing alias", `[]`, `{"data":{"repository":{}}}`},
		{"unexplained null", `[]`, `{"data":{"repository":{"i0":null}}}`},
		{"wrong child", `[]`, `{"data":{"repository":{"i0":{"number":2,"fullDatabaseId":102,"parent":null}}}}`},
		{"missing parent field", `[]`, `{"data":{"repository":{"i0":{"number":1,"fullDatabaseId":101}}}}`},
		{"missing child ID", `[]`, `{"data":{"repository":{"i0":{"number":1,"parent":null}}}}`},
		{"missing parent ID", `[]`, `{"data":{"repository":{"i0":{"number":1,"fullDatabaseId":101,"parent":{"number":2}}}}}`},
		{"unrelated not found", `[]`, `{"data":{"repository":{"i0":null}},"errors":[{"type":"NOT_FOUND","path":["repository","foreign"],"message":"missing"}]}`},
		{"valid data beside error", `[]`, `{"data":{"repository":{"i0":{"number":1,"fullDatabaseId":101,"parent":null}}},"errors":[{"type":"FORBIDDEN","message":"denied"}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					_, _ = fmt.Fprint(w, tc.events)
				} else {
					_, _ = fmt.Fprint(w, tc.graph)
				}
			}))
			defer server.Close()
			f := newParentGraphQLTestFetcher(server.URL + "/graphql")
			f.restBaseURLOverride = server.URL + "/"
			since := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
			data, err := f.ParentData(t.Context(), Binding{Host: "github.com", Owner: "example-owner", Repo: "example-repo"}, ParentRequest{Since: &since, IssueNumbers: []int{1}})
			require.Error(t, err)
			assert.Empty(t, data.ScannedChildIDs)
		})
	}
}

func TestIncrementalParentNotFoundChildPreservesValidSibling(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = fmt.Fprint(w, `[]`)
			return
		}
		_, _ = fmt.Fprint(w, `{"data":{"repository":{"i0":null,"i1":{"number":2,"fullDatabaseId":"102","parent":null}}},"errors":[{"type":"NOT_FOUND","path":["repository","i0"],"message":"Could not resolve issue"}]}`)
	}))
	defer server.Close()
	f := newParentGraphQLTestFetcher(server.URL + "/graphql")
	f.restBaseURLOverride = server.URL + "/"
	since := time.Now()
	data, err := f.ParentData(t.Context(), Binding{Host: "github.com", Owner: "example-owner", Repo: "example-repo"}, ParentRequest{Since: &since, IssueNumbers: []int{1, 2}})
	require.NoError(t, err)
	assert.False(t, data.ChildScanned(1))
	assert.Equal(t, map[int]int64{2: 102}, data.ScannedChildIDs)
}

func TestIncrementalParentEventPaginationCannotRepeat(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.Header().Set("Link", `<?per_page=100&page=2>; rel="next"`)
		_, _ = fmt.Fprint(w, `[]`)
	}))
	defer server.Close()
	f := newParentGraphQLTestFetcher(server.URL + "/graphql")
	f.restBaseURLOverride = server.URL + "/"
	since := time.Now()
	_, err := f.ParentData(t.Context(), Binding{Host: "github.com", Owner: "example-owner", Repo: "example-repo"}, ParentRequest{Since: &since})
	require.ErrorContains(t, err, "pagination did not advance")
	assert.Equal(t, 2, calls)
}

func TestIncrementalParentUsesScopedTransportForPaginationAndAliases(t *testing.T) {
	for _, host := range []string{"github.com", "github.example"} {
		t.Run(host, func(t *testing.T) {
			if host != "github.com" {
				allowGitHubEnterpriseHost(t, host)
			}
			for _, next := range []string{"numeric", "foreign-origin", "foreign-repo", "redirect"} {
				t.Run(next, func(t *testing.T) {
					tokenEnv := exampleGitHubTokenEnv()
					t.Setenv(tokenEnv, "test-token")
					var calls int
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						calls++
						assert.Equal(t, "Bearer test-token", r.Header.Get("Authorization"))
						prefix := ""
						if host != "github.com" {
							prefix = "/api/v3"
						}
						if r.Method == http.MethodGet {
							if calls == 1 {
								link := prefix + "/repositories/123/issues/events?per_page=100&page=2"
								switch next {
								case "foreign-origin":
									link = "https://foreign.example/repos/example-owner/example-repo/issues/events?page=2"
								case "foreign-repo":
									link = prefix + "/repos/example-owner/foreign-repo/issues/events?page=2"
								case "redirect":
									w.Header().Set("Location", "https://foreign.example/events")
									w.WriteHeader(http.StatusFound)
									return
								}
								w.Header().Set("Link", fmt.Sprintf(`<%s>; rel="next"`, link))
								_, _ = fmt.Fprint(w, `[]`)
								return
							}
							assert.Equal(t, prefix+"/repos/example-owner/example-repo/issues/events", r.URL.Path)
							_, _ = fmt.Fprint(w, `[]`)
							return
						}
						// This request crossed the real credential guard, then a test-only host rewrite.
						var request parentGraphQLRequest
						require.NoError(t, json.UnmarshalRead(r.Body, &request))
						assert.True(t, graphQLQueryMatchesParentQuery(request.Query))
						_, _ = fmt.Fprint(w, `{"data":{"repository":{"i0":{"number":1,"fullDatabaseId":101,"parent":null}}}}`)
					}))
					defer server.Close()
					target, err := url.Parse(server.URL)
					require.NoError(t, err)
					rewrite := &rewriteHostRoundTripper{target: target, next: server.Client().Transport}
					resolver := NewCredentialResolver(config.GitHubSyncConfig{TokenEnv: tokenEnv, TokenHost: host}, &fakeCommandRunner{})
					f := NewHTTPFetcher(HTTPFetcherConfig{Client: &http.Client{Transport: rewrite}, CredentialResolver: resolver})
					f.graphQLSleep = func(context.Context, time.Duration) error { return nil }
					since := time.Now()
					data, err := f.ParentData(t.Context(), Binding{Host: host, Owner: "example-owner", Repo: "example-repo"}, ParentRequest{Since: &since, IssueNumbers: []int{1}})
					if next == "numeric" {
						require.NoError(t, err)
						assert.Equal(t, 3, calls)
						assert.True(t, data.ChildScanned(1))
					} else {
						require.Error(t, err)
						assert.Equal(t, 1, calls)
						assert.Empty(t, data.ScannedChildIDs)
					}
				})
			}
		})
	}
}

func TestIncrementalParentSharesRetryBudgetAcrossEventsAndGraphQL(t *testing.T) {
	var events, graphs int
	var waits []time.Duration
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			events++
			if events < 3 {
				w.Header().Set("Retry-After", "25")
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = fmt.Fprint(w, `busy`)
				return
			}
			_, _ = fmt.Fprint(w, `[]`)
			return
		}
		graphs++
		w.Header().Set("Retry-After", "11")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = fmt.Fprint(w, `busy`)
	}))
	defer server.Close()
	f := newParentGraphQLTestFetcher(server.URL + "/graphql")
	f.restBaseURLOverride = server.URL + "/"
	now := time.Now()
	f.graphQLNow = func() time.Time { return now }
	f.graphQLSleep = func(_ context.Context, d time.Duration) error { now = now.Add(d); waits = append(waits, d); return nil }
	since := time.Now()
	_, err := f.ParentData(t.Context(), Binding{Host: "github.com", Owner: "example-owner", Repo: "example-repo"}, ParentRequest{Since: &since, IssueNumbers: []int{1}})
	require.ErrorContains(t, err, "max total sleep")
	assert.Equal(t, 3, events)
	assert.Equal(t, 1, graphs)
	assert.Equal(t, []time.Duration{25 * time.Second, 25 * time.Second}, waits)
}

func FuzzIncrementalParentUniqueCoverage(f *testing.F) {
	f.Add([]byte{0, 0, 0, 1, 0, 0, 0, 1, 255, 255, 255, 255, 0, 0, 0, 2})
	f.Add(bytes.Repeat([]byte{0, 0, 1, 0}, 120))
	f.Fuzz(func(t *testing.T, raw []byte) {
		// Cap materialization cost, retaining the full int32 value domain.
		raw = raw[:min(len(raw), 2048)]
		var numbers []int
		expected := map[int]int64{}
		for i := 0; i+4 <= len(raw); i += 4 {
			value := int64(binary.BigEndian.Uint32(raw[i : i+4]))
			if value >= 1<<31 {
				value -= 1 << 32
			}
			n := int(value)
			numbers = append(numbers, n)
			if n > 0 {
				expected[n] = int64(n) + 100
			}
		}
		seen := map[int]bool{}
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet {
				_, _ = fmt.Fprint(w, `[]`)
				return
			}
			var request parentGraphQLRequest
			require.NoError(t, json.UnmarshalRead(r.Body, &request))
			matches := testParentAlias.FindAllStringSubmatch(request.Query, -1)
			require.NotEmpty(t, matches)
			require.LessOrEqual(t, len(matches), 100)
			require.True(t, graphQLQueryMatchesParentQuery(request.Query))
			nodes := map[string]any{}
			for _, m := range matches {
				n, err := strconv.Atoi(m[2])
				require.NoError(t, err)
				require.False(t, seen[n], "child queried more than once")
				seen[n] = true
				nodes["i"+m[1]] = map[string]any{"number": n, "fullDatabaseId": int64(n) + 100, "parent": nil}
			}
			require.NoError(t, json.MarshalWrite(w, map[string]any{"data": map[string]any{"repository": nodes}}))
		}))
		defer server.Close()
		fetcher := newParentGraphQLTestFetcher(server.URL + "/graphql")
		fetcher.restBaseURLOverride = server.URL + "/"
		since := time.Now()
		data, err := fetcher.ParentData(t.Context(), Binding{Host: "github.com", Owner: "example-owner", Repo: "example-repo"}, ParentRequest{Since: &since, IssueNumbers: numbers})
		require.NoError(t, err)
		assert.Equal(t, expected, data.ScannedChildIDs)
	})
}

func TestIncrementalParentCredentialGuardRejectsOversizedQuery(t *testing.T) {
	numbers := make([]int, 101)
	for i := range numbers {
		numbers[i] = i + 1
	}
	assert.False(t, graphQLQueryMatchesParentQuery(targetedParentGraphQLQuery(numbers)))
}

func FuzzIncrementalParentCredentialGuardRejectsExtraSelection(f *testing.F) {
	f.Add([]byte{1, 2, 3})
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, raw []byte) {
		raw = raw[:min(len(raw), 1024)]
		numbers := make([]int, min(len(raw)+1, 100))
		for i := range numbers {
			if i < len(raw) {
				numbers[i] = int(raw[i]) + 1
			} else {
				numbers[i] = 1
			}
		}
		query := targetedParentGraphQLQuery(numbers)
		// An additional field in the repository selection must never gain a
		// credential, regardless of the generated issue count or field spelling.
		query = query[:len(query)-4] + " extra" + hex.EncodeToString(raw) + " } }"
		require.False(t, graphQLQueryMatchesParentQuery(query))
	})
}

func TestSelectedParentBootstrapBatchesWithoutEvents(t *testing.T) {
	var graphCalls int
	var queried []int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodPost, r.Method, "bootstrap must not read historical events")
		graphCalls++
		var request parentGraphQLRequest
		require.NoError(t, json.UnmarshalRead(r.Body, &request))
		assert.NotContains(t, request.Query, "issues(first:")
		nodes := map[string]any{}
		for _, match := range testParentAlias.FindAllStringSubmatch(request.Query, -1) {
			n, err := strconv.Atoi(match[2])
			require.NoError(t, err)
			queried = append(queried, n)
			nodes["i"+match[1]] = map[string]any{"number": n, "fullDatabaseId": 1000 + n, "parent": nil}
		}
		require.NoError(t, json.MarshalWrite(w, map[string]any{"data": map[string]any{"repository": nodes}}))
	}))
	defer server.Close()
	f := newParentGraphQLTestFetcher(server.URL + "/graphql")
	f.restBaseURLOverride = server.URL + "/"
	numbers := []int{0, -1, 1, 1}
	want := make([]int, 101)
	for i := range want {
		want[i] = i + 1
		numbers = append(numbers, i+1)
	}
	var progress [][2]int
	ctx := withProgressReporter(t.Context(), func(phase string, done, total int) {
		assert.Equal(t, "parents", phase)
		progress = append(progress, [2]int{done, total})
	})
	data, err := f.ParentData(ctx, Binding{Host: "github.com", Owner: "example-owner", Repo: "example-repo"}, ParentRequest{IssueNumbers: numbers})
	require.NoError(t, err)
	assert.Equal(t, 2, graphCalls)
	assert.Equal(t, want, queried)
	assert.Equal(t, ParentScanComplete, data.Scan)
	assert.Len(t, data.ScannedChildIDs, 101)
	assert.Equal(t, int64(1101), data.ScannedChildIDs[101])
	assert.Equal(t, [][2]int{{0, 101}, {100, 101}, {101, 101}}, progress)
}

func TestSelectedParentBootstrapEmptyMakesNoRequests(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("empty bootstrap made an HTTP request")
		_, _ = fmt.Fprint(w, `{"data":{"repository":{"issues":{"nodes":[],"pageInfo":{"hasNextPage":false}}}}}`)
	}))
	defer server.Close()
	f := newParentGraphQLTestFetcher(server.URL + "/graphql")
	var progress [][2]int
	ctx := withProgressReporter(t.Context(), func(phase string, done, total int) {
		assert.Equal(t, "parents", phase)
		progress = append(progress, [2]int{done, total})
	})
	data, err := f.ParentData(ctx, Binding{Host: "github.com", Owner: "example-owner", Repo: "example-repo"}, ParentRequest{IssueNumbers: []int{}})
	require.NoError(t, err)
	assert.Equal(t, ParentScanComplete, data.Scan)
	assert.Empty(t, data.ScannedChildIDs)
	assert.Equal(t, [][2]int{{0, 0}}, progress)
}
