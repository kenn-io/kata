package daemon_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

type discoveryPage struct {
	Issues  []db.Issue `json:"issues"`
	Results []struct {
		Issue db.Issue `json:"issue"`
	} `json:"results"`
	NextCursor string `json:"next_cursor"`
	Complete   bool   `json:"complete"`
	Truncated  bool   `json:"truncated"`
	Total      *int64 `json:"total"`
}

func paginationFixture(t *testing.T, n int) (*testenv.Env, int64, []db.Issue) {
	t.Helper()
	env := testenv.New(t)
	ctx := t.Context()
	p, err := env.DB.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	items := make([]db.ImportItem, n)
	for i := range items {
		items[i] = db.ImportItem{ExternalID: fmt.Sprint(i), Title: "matching needle", Author: "example-agent", Status: "open", CreatedAt: base, UpdatedAt: base}
		if i > 0 && i < 6 {
			items[i].Priority = new(int64(i - 1))
		}
	}
	_, _, err = env.DB.ImportBatch(ctx, db.ImportBatchParams{ProjectID: p.ID, Source: "fixture", Actor: "example-agent", Items: items})
	require.NoError(t, err)
	issues, err := env.DB.ListIssues(ctx, db.ListIssuesParams{ProjectID: p.ID, OldestFirst: true})
	require.NoError(t, err)
	return env, p.ID, issues
}

func getDiscoveryPage(t *testing.T, env *testenv.Env, path string) discoveryPage {
	t.Helper()
	resp, body := envGetRaw(t, env, path)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	var p discoveryPage
	require.NoError(t, json.Unmarshal(body, &p))
	return p
}

// Rows edited during a limit=1 scan retain their place in both HTTP lists.
func TestHTTPListPaginationSurvivesEdits(t *testing.T) {
	for _, global := range []bool{false, true} {
		for _, sort := range []string{"oldest", "created"} {
			t.Run(fmt.Sprintf("global=%v/%s", global, sort), func(t *testing.T) {
				env, pid, issues := paginationFixture(t, 6)
				path := fmt.Sprintf("/api/v1/projects/%d/issues?sort=%s&limit=1&include_total=true", pid, sort)
				if global {
					path = fmt.Sprintf("/api/v1/issues?project_ids=%d&sort=%s&limit=1&include_total=true", pid, sort)
				}
				cursor := ""
				seen := map[int64]bool{}
				for i := range 6 {
					page := getDiscoveryPage(t, env, path+"&cursor="+url.QueryEscape(cursor))
					require.Len(t, page.Issues, 1)
					require.NotNil(t, page.Total)
					require.Equal(t, int64(6), *page.Total)
					issue := page.Issues[0]
					require.False(t, seen[issue.ID])
					seen[issue.ID] = true
					require.Equal(t, i == 5, page.Complete)
					require.Equal(t, i < 5, page.Truncated)
					if i < 5 {
						require.NotEmpty(t, page.NextCursor)
					} else {
						require.Empty(t, page.NextCursor)
					}
					cursor = page.NextCursor
					for _, row := range issues {
						_, _, _, err := env.DB.EditIssue(t.Context(), db.EditIssueParams{IssueID: row.ID, Body: new(fmt.Sprintf("updated %d", i)), Actor: "example-agent"})
						require.NoError(t, err)
					}
				}
				require.Len(t, seen, 6)
			})
		}
	}
}

func TestHTTPListPaginationUsesImportedTimestampInstants(t *testing.T) {
	env := testenv.New(t)
	project, err := env.DB.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)

	type importItem struct {
		ExternalID string    `json:"external_id"`
		Title      string    `json:"title"`
		Author     string    `json:"author"`
		Status     string    `json:"status"`
		CreatedAt  time.Time `json:"created_at"`
		UpdatedAt  time.Time `json:"updated_at"`
	}
	items := []importItem{
		{
			ExternalID: "offset-later",
			Title:      "offset-later",
			Author:     "example-agent",
			Status:     "open",
			CreatedAt:  time.Date(2026, 10, 1, 9, 0, 0, 123456789, time.FixedZone("", -7*60*60)),
			UpdatedAt:  time.Date(2026, 10, 1, 9, 0, 0, 123456789, time.FixedZone("", -7*60*60)),
		},
		{
			ExternalID: "utc-earlier",
			Title:      "utc-earlier",
			Author:     "example-agent",
			Status:     "open",
			CreatedAt:  time.Date(2026, 10, 1, 16, 0, 0, 123456788, time.UTC),
			UpdatedAt:  time.Date(2026, 10, 1, 16, 0, 0, 123456788, time.UTC),
		},
	}
	requestBody, err := json.Marshal(struct {
		Actor  string       `json:"actor"`
		Source string       `json:"source"`
		Items  []importItem `json:"items"`
	}{Actor: "example-agent", Source: "example", Items: items})
	require.NoError(t, err)
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
		fmt.Sprintf("%s/api/v1/projects/%d/imports", env.URL, project.ID), bytes.NewReader(requestBody))
	require.NoError(t, err)
	request.Header.Set("Content-Type", "application/json")
	response, err := env.HTTP.Do(request)
	require.NoError(t, err)
	defer func() { _ = response.Body.Close() }()
	responseBody, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.Equalf(t, http.StatusOK, response.StatusCode, string(responseBody))

	for _, tc := range []struct {
		sort string
		want []string
	}{
		{sort: "oldest", want: []string{"utc-earlier", "offset-later"}},
		{sort: "created", want: []string{"offset-later", "utc-earlier"}},
	} {
		t.Run(tc.sort, func(t *testing.T) {
			path := fmt.Sprintf("/api/v1/projects/%d/issues?sort=%s&limit=1", project.ID, tc.sort)
			cursor := ""
			for i, title := range tc.want {
				page := getDiscoveryPage(t, env, path+"&cursor="+url.QueryEscape(cursor))
				require.Len(t, page.Issues, 1)
				require.Equal(t, title, page.Issues[0].Title)
				if i == len(tc.want)-1 {
					require.True(t, page.Complete)
					require.Empty(t, page.NextCursor)
				} else {
					require.False(t, page.Complete)
					require.NotEmpty(t, page.NextCursor)
				}
				cursor = page.NextCursor
			}
		})
	}
}

func TestHTTPListPriorityPartitionsAndCursorValidation(t *testing.T) {
	env, pid, _ := paginationFixture(t, 6)
	for _, path := range []string{fmt.Sprintf("/api/v1/projects/%d/issues", pid), fmt.Sprintf("/api/v1/issues?project_ids=%d", pid)} {
		sep := "?"
		if path[len(path)-1] != 's' {
			sep = "&"
		}
		base := path + sep + "sort=oldest&limit=1&include_total=true"
		first := getDiscoveryPage(t, env, base)
		require.False(t, first.Complete)
		require.NotEmpty(t, first.NextCursor)
		var sum int64
		for _, priority := range []string{"none", "0", "1", "2", "3", "4"} {
			page := getDiscoveryPage(t, env, base+"&priority="+priority)
			require.NotNil(t, page.Total)
			require.Equal(t, int64(1), *page.Total)
			require.True(t, page.Complete)
			sum += *page.Total
		}
		require.Equal(t, *first.Total, sum)
		for _, suffix := range []string{"&priority=none&max_priority=4", "&cursor=bad", "&status=closed&cursor=" + url.QueryEscape(first.NextCursor)} {
			resp, body := envGetRaw(t, env, base+suffix)
			require.Equal(t, http.StatusBadRequest, resp.StatusCode, string(body))
		}
		resumed := getDiscoveryPage(t, env, path+sep+"sort=oldest&limit=0&cursor="+url.QueryEscape(first.NextCursor))
		require.Len(t, resumed.Issues, 5)
		require.True(t, resumed.Complete)
		require.Nil(t, resumed.Total)
	}
	for _, q := range []string{"project_ids=", "project_ids=0", "project_ids=-1", fmt.Sprintf("project_id=%d&project_ids=%d", pid, pid)} {
		resp, body := envGetRaw(t, env, "/api/v1/issues?"+q)
		require.Equal(t, http.StatusBadRequest, resp.StatusCode, string(body))
	}
}

func TestHTTPLexicalPaginationBeyondCandidateCap(t *testing.T) {
	env, pid, _ := paginationFixture(t, 205)
	base := fmt.Sprintf("/api/v1/projects/%d/search?q=needle&mode=lexical&sort=oldest&limit=200", pid)
	first := getDiscoveryPage(t, env, base)
	require.Len(t, first.Results, 200)
	require.False(t, first.Complete)
	require.NotEmpty(t, first.NextCursor)
	second := getDiscoveryPage(t, env, base+"&cursor="+url.QueryEscape(first.NextCursor))
	require.Len(t, second.Results, 5)
	require.True(t, second.Complete)
	seen := map[int64]bool{}
	for _, hit := range append(first.Results, second.Results...) {
		require.False(t, seen[hit.Issue.ID])
		seen[hit.Issue.ID] = true
	}
	require.Len(t, seen, 205)
	for _, q := range []string{"q=needle&mode=hybrid&sort=oldest", "q=needle&mode=lexical&cursor=" + url.QueryEscape(first.NextCursor), "q=changed&mode=lexical&sort=oldest&cursor=" + url.QueryEscape(first.NextCursor)} {
		resp, body := envGetRaw(t, env, fmt.Sprintf("/api/v1/projects/%d/search?%s", pid, q))
		require.Equal(t, http.StatusBadRequest, resp.StatusCode, string(body))
	}
}

// Counts and cursors must retain the full token scope, not just the project.
func TestHTTPPaginationAuthorizationScope(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("bootstrap-token"), testenv.WithRequireTokenIdentity())
	p, err := env.DB.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	root := createScopedHTTPTestIssue(t, env, p.ID, "matching needle", nil)
	createScopedHTTPTestIssue(t, env, p.ID, "matching needle child", &root)
	hidden := createScopedHTTPTestIssue(t, env, p.ID, "matching needle hidden", nil)
	expires := time.Now().UTC().Add(time.Hour)
	for _, tc := range []struct{ token, root string }{{"root-token", root.UID}, {"hidden-token", hidden.UID}} {
		_, _, err := env.DB.CreateAPIToken(t.Context(), db.CreateAPITokenParams{PlaintextToken: tc.token, Actor: "example-agent", AdminActor: db.BootstrapActor, ExpiresAt: &expires, Scope: &db.APITokenScope{Kind: db.APITokenScopeIssueSubtree, ProjectUID: p.UID, RootIssueUID: tc.root}})
		require.NoError(t, err)
	}
	get := func(path, token string) (int, discoveryPage) {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, env.URL+path, nil)
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := env.HTTP.Do(req)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		var page discoveryPage
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&page))
		return resp.StatusCode, page
	}
	for _, path := range []string{fmt.Sprintf("/api/v1/projects/%d/issues?sort=oldest&limit=1&include_total=true", p.ID), fmt.Sprintf("/api/v1/issues?project_ids=%d&sort=oldest&limit=1&include_total=true", p.ID)} {
		status, page := get(path, "root-token")
		require.Equal(t, http.StatusOK, status)
		require.Equal(t, int64(2), *page.Total)
		require.False(t, page.Complete)
		require.NotEmpty(t, page.NextCursor)
		status, _ = get(path+"&cursor="+url.QueryEscape(page.NextCursor), "hidden-token")
		require.Equal(t, http.StatusBadRequest, status)
	}
}
