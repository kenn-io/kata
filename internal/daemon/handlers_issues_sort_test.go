package daemon_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/doordash-oss/oapi-codegen-dd/v3/pkg/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
	"go.kenn.io/kata/pkg/client/generated"
)

func TestListIssuesSort(t *testing.T) {
	env := testenv.New(t)
	ctx := context.Background()
	project, err := env.DB.CreateProject(ctx, "list-sort-project")
	require.NoError(t, err)
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	_, _, err = env.DB.ImportBatch(ctx, db.ImportBatchParams{
		ProjectID: project.ID,
		Source:    "fixture",
		Actor:     "fixture",
		Items: []db.ImportItem{
			{ExternalID: "first", Title: "first issue", Author: "fixture", Status: "open", CreatedAt: base, UpdatedAt: base},
			{ExternalID: "tie-first", Title: "first tied issue", Author: "fixture", Status: "open", CreatedAt: base.Add(time.Hour), UpdatedAt: base.Add(time.Hour)},
			{ExternalID: "tie-second", Title: "second tied issue", Author: "fixture", Status: "open", CreatedAt: base.Add(time.Hour), UpdatedAt: base.Add(time.Hour)},
			{ExternalID: "latest", Title: "latest issue", Author: "fixture", Status: "open", CreatedAt: base.Add(2 * time.Hour), UpdatedAt: base.Add(2 * time.Hour)},
		},
	})
	require.NoError(t, err)

	client, err := generated.NewDefaultClient(env.URL, runtime.WithHTTPClient(contextHTTPClient{client: env.HTTP}))
	require.NoError(t, err)
	scoped := func(query *generated.ListIssuesQuery) []string {
		response, callErr := client.ListIssuesWithResponse(ctx, &generated.ListIssuesRequestOptions{
			PathParams: &generated.ListIssuesPath{ProjectID: project.ID}, Query: query,
		})
		require.NoError(t, callErr)
		require.Equal(t, http.StatusOK, response.StatusCode, string(response.Body))
		return listIssueTitles(t, response.Body)
	}
	all := func(query *generated.ListAllIssuesQuery) []string {
		response, callErr := client.ListAllIssuesWithResponse(ctx, &generated.ListAllIssuesRequestOptions{Query: query})
		require.NoError(t, callErr)
		require.Equal(t, http.StatusOK, response.StatusCode, string(response.Body))
		return listIssueTitles(t, response.Body)
	}
	wantDefault := []string{"latest issue", "second tied issue", "first tied issue", "first issue"}
	wantOldest := []string{"first issue", "first tied issue", "second tied issue", "latest issue"}

	for _, route := range []struct {
		name string
		call func(any) []string
	}{
		{name: "scoped", call: func(query any) []string { return scoped(query.(*generated.ListIssuesQuery)) }},
		{name: "global", call: func(query any) []string { return all(query.(*generated.ListAllIssuesQuery)) }},
	} {
		t.Run(route.name, func(t *testing.T) {
			var omitted, empty, oldest []string
			if route.name == "scoped" {
				omitted = route.call((*generated.ListIssuesQuery)(nil))
				empty = route.call(&generated.ListIssuesQuery{Sort: new(generated.ListIssuesQuerySort(""))})
				limit := int64(3)
				oldest = route.call(&generated.ListIssuesQuery{Sort: new(generated.ListIssuesQuerySort("oldest")), Limit: &limit})
			} else {
				omitted = route.call((*generated.ListAllIssuesQuery)(nil))
				empty = route.call(&generated.ListAllIssuesQuery{Sort: new(generated.ListAllIssuesQuerySort(""))})
				limit := int64(3)
				oldest = route.call(&generated.ListAllIssuesQuery{Sort: new(generated.ListAllIssuesQuerySort("oldest")), Limit: &limit})
			}
			assert.Equal(t, wantDefault, omitted)
			assert.Equal(t, wantDefault, empty)
			assert.Equal(t, wantOldest[:3], oldest)
		})
	}

	resp, body := envGetRaw(t, env,
		"/api/v1/projects/"+strconv.FormatInt(project.ID, 10)+"/issues?sort=newest")
	assertAPIError(t, resp.StatusCode, body, http.StatusBadRequest, "validation")
	assert.Contains(t, string(body), "sort")
}

type contextHTTPClient struct {
	client *http.Client
}

func (c contextHTTPClient) Do(ctx context.Context, req *http.Request) (*http.Response, error) {
	return c.client.Do(req.WithContext(ctx)) //nolint:gosec // test client targets the local daemon
}

func listIssueTitles(t *testing.T, body []byte) []string {
	t.Helper()
	var response struct {
		Issues []struct {
			Title string `json:"title"`
		} `json:"issues"`
	}
	require.NoError(t, json.Unmarshal(body, &response))
	titles := make([]string, len(response.Issues))
	for i, issue := range response.Issues {
		titles[i] = issue.Title
	}
	return titles
}
