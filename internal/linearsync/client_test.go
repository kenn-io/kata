package linearsync

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/issuesync"
)

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func response(code int, body string) *http.Response {
	return &http.Response{StatusCode: code, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}
}
func mockClient(t *testing.T, fn roundTrip) *Client {
	t.Helper()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	return NewClient(ClientConfig{LookupEnv: func(k string) (string, bool) { require.Equal(t, "KATA_LINEAR_TOKEN", k); return "example-key", true }, Transport: fn, Now: func() time.Time { return now }, Wait: func(ctx context.Context, d time.Duration) error { now = now.Add(d); return ctx.Err() }})
}
func sessionFor(t *testing.T, c *Client) Session {
	t.Helper()
	s, err := c.ForRun(context.Background(), testConfig())
	require.NoError(t, err)
	return s
}
func wireIssue() map[string]any {
	return map[string]any{"id": issueID, "team": map[string]any{"id": teamID}, "project": nil, "state": map[string]any{"id": stateID}, "identifier": "EX-1", "title": "Example task", "description": nil, "url": "https://linear.app/example-workspace/issue/EX-1/example-task", "priority": 0, "creator": nil, "assignee": nil, "createdAt": "2026-01-01T00:00:00Z", "updatedAt": "2026-01-02T00:00:00Z", "archivedAt": nil, "trashed": nil, "completedAt": nil, "canceledAt": nil}
}

func TestClientOmitsTrashedIssues(t *testing.T) {
	c := mockClient(t, func(*http.Request) (*http.Response, error) {
		i := wireIssue()
		i["trashed"] = true
		return dataResponse(map[string]any{"issues": map[string]any{"nodes": []any{i}, "pageInfo": map[string]any{"hasNextPage": false}}}), nil
	})
	items, err := sessionFor(t, c).Issues(t.Context(), testConfig())
	require.NoError(t, err)
	require.Empty(t, items, "a trashed source leaves its existing native copy untouched")
}

func TestClientRequiresSelectedTrashFlag(t *testing.T) {
	c := mockClient(t, func(*http.Request) (*http.Response, error) {
		i := wireIssue()
		delete(i, "trashed")
		return dataResponse(map[string]any{"issues": map[string]any{"nodes": []any{i}, "pageInfo": map[string]any{"hasNextPage": false}}}), nil
	})
	_, err := sessionFor(t, c).Issues(t.Context(), testConfig())
	require.Error(t, err, "an incomplete selected field cannot prove a source is live")
}

func TestClientRefusesTrashedProject(t *testing.T) {
	cfg := testConfig()
	cfg.ProjectID = projectID
	c := mockClient(t, func(r *http.Request) (*http.Response, error) {
		query, _ := requestBody(t, r)
		if strings.Contains(query, "KataLinearScope") {
			return dataResponse(map[string]any{"viewer": map[string]any{"organization": map[string]any{"id": workspaceID}}, "team": map[string]any{"id": teamID, "name": "Example team", "archivedAt": nil, "organization": map[string]any{"id": workspaceID}}}), nil
		}
		return dataResponse(map[string]any{"project": map[string]any{"id": projectID, "name": "Example project", "archivedAt": nil, "trashed": true, "teams": map[string]any{"nodes": []any{map[string]any{"id": teamID}}}}}), nil
	})
	s, err := c.ForRun(t.Context(), cfg)
	require.NoError(t, err)
	_, err = s.Scope(t.Context(), cfg)
	require.Error(t, err, "a trashed project cannot provide an available sync scope")
}
func dataResponse(data any) *http.Response {
	raw, _ := json.Marshal(map[string]any{"data": data})
	return response(200, string(raw))
}
func requestBody(t *testing.T, r *http.Request) (string, map[string]any) {
	t.Helper()
	raw, err := io.ReadAll(r.Body)
	require.NoError(t, err)
	var body struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}
	require.NoError(t, json.Unmarshal(raw, &body))
	return body.Query, body.Variables
}

func TestClientScopesPaginationAndNullableFields(t *testing.T) {
	calls := 0
	c := mockClient(t, func(r *http.Request) (*http.Response, error) {
		require.Equal(t, "https://api.linear.app/graphql", r.URL.String())
		require.Equal(t, "example-key", r.Header.Get("Authorization"))
		require.Equal(t, "POST", r.Method)
		query, v := requestBody(t, r)
		calls++
		if strings.Contains(query, "KataLinearScope") {
			require.Equal(t, teamID, v["team"])
			return dataResponse(map[string]any{"viewer": map[string]any{"organization": map[string]any{"id": workspaceID}}, "team": map[string]any{"id": teamID, "name": "Example team", "archivedAt": nil, "organization": map[string]any{"id": workspaceID}}}), nil
		}
		require.Contains(t, query, "KataLinearIssues")
		require.Equal(t, map[string]any{"team": map[string]any{"id": map[string]any{"eq": teamID}}}, v["filter"])
		if calls == 2 {
			return dataResponse(map[string]any{"issues": map[string]any{"nodes": []any{wireIssue()}, "pageInfo": map[string]any{"hasNextPage": true, "endCursor": "next"}}}), nil
		}
		require.Equal(t, "next", v["after"])
		return dataResponse(map[string]any{"issues": map[string]any{"nodes": []any{}, "pageInfo": map[string]any{"hasNextPage": false, "endCursor": nil}}}), nil
	})
	s := sessionFor(t, c)
	scope, err := s.Scope(context.Background(), testConfig())
	require.NoError(t, err)
	require.Equal(t, workspaceID, scope.WorkspaceID)
	items, err := s.Issues(context.Background(), testConfig())
	require.NoError(t, err)
	require.Len(t, items, 1)
	require.Empty(t, items[0].Description)
	require.Empty(t, items[0].AssigneeID)
	require.Equal(t, 3, calls)
}
func TestClientRejectsPartialErrorsAndScopeLeaks(t *testing.T) {
	for _, body := range []string{`{"data":{"issues":{"nodes":[]}},"errors":[{"message":"example-key private body"}]}`, `{"data":{"issues":{"nodes":[],"pageInfo":{"hasNextPage":true,"endCursor":null}}}}`, `{"data":{"issues":{"nodes":null,"pageInfo":{"hasNextPage":false}}}}`} {
		c := mockClient(t, func(*http.Request) (*http.Response, error) { return response(200, body), nil })
		_, err := sessionFor(t, c).Issues(context.Background(), testConfig())
		require.Error(t, err)
		require.NotContains(t, err.Error(), "example-key")
		require.NotContains(t, err.Error(), "private body")
	}
	for _, edit := range []func(map[string]any){func(i map[string]any) { i["team"] = map[string]any{"id": projectID} }, func(i map[string]any) { delete(i, "description") }, func(i map[string]any) { i["priority"] = 1.5 }} {
		c := mockClient(t, func(*http.Request) (*http.Response, error) {
			i := wireIssue()
			edit(i)
			return dataResponse(map[string]any{"issues": map[string]any{"nodes": []any{i}, "pageInfo": map[string]any{"hasNextPage": false}}}), nil
		})
		_, err := sessionFor(t, c).Issues(context.Background(), testConfig())
		require.Error(t, err)
	}
}
func TestClientAuthenticationAndRetryPacing(t *testing.T) {
	for _, auth := range []string{"api-key", "oauth"} {
		now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		var waits []time.Duration
		calls := 0
		c := NewClient(ClientConfig{Daemon: config.LinearSyncConfig{AuthType: auth}, LookupEnv: func(string) (string, bool) { return "example-token", true }, Now: func() time.Time { return now }, Wait: func(ctx context.Context, d time.Duration) error {
			waits = append(waits, d)
			now = now.Add(d)
			return ctx.Err()
		}, Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
			calls++
			want := "example-token"
			if auth == "oauth" {
				want = "Bearer " + want
			}
			require.Equal(t, want, r.Header.Get("Authorization"))
			if calls == 1 {
				res := response(400, `{"errors":[{"extensions":{"code":"RATELIMITED"}}]}`)
				res.Header.Set("Retry-After", "5")
				return res, nil
			}
			return dataResponse(map[string]any{"issues": map[string]any{"nodes": []any{}, "pageInfo": map[string]any{"hasNextPage": false}}}), nil
		})})
		s := sessionFor(t, c)
		_, err := s.Issues(context.Background(), testConfig())
		require.NoError(t, err)
		_, err = s.Issues(context.Background(), testConfig())
		require.NoError(t, err)
		require.Equal(t, []time.Duration{5 * time.Second, 1500 * time.Millisecond}, waits)
	}
	c := mockClient(t, func(*http.Request) (*http.Response, error) { return response(401, "example-key"), nil })
	_, err := sessionFor(t, c).Issues(context.Background(), testConfig())
	var classified *issuesync.StatusError
	require.ErrorAs(t, err, &classified)
	require.True(t, classified.Blocked)
	require.NotContains(t, err.Error(), "example-key")
}
func TestClientDetectsCursorCycleAndDuplicateIDs(t *testing.T) {
	for _, cycle := range []bool{false, true} {
		calls := 0
		c := mockClient(t, func(*http.Request) (*http.Response, error) {
			calls++
			return dataResponse(map[string]any{"issues": map[string]any{"nodes": []any{wireIssue()}, "pageInfo": map[string]any{"hasNextPage": cycle, "endCursor": "same"}}}), nil
		})
		_, err := sessionFor(t, c).Issues(context.Background(), testConfig())
		if cycle {
			require.Error(t, err)
			require.Equal(t, 2, calls)
		} else {
			require.NoError(t, err)
		}
	}
	c := mockClient(t, func(*http.Request) (*http.Response, error) {
		return dataResponse(map[string]any{"issues": map[string]any{"nodes": []any{wireIssue(), wireIssue()}, "pageInfo": map[string]any{"hasNextPage": false}}}), nil
	})
	_, err := sessionFor(t, c).Issues(context.Background(), testConfig())
	require.Error(t, err)
}

func TestClientProjectScopeAndSharedResetCooldown(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var waits []time.Duration
	calls := 0
	c := NewClient(ClientConfig{LookupEnv: func(string) (string, bool) { return "example-key", true }, Now: func() time.Time { return now }, Wait: func(ctx context.Context, d time.Duration) error {
		waits = append(waits, d)
		now = now.Add(d)
		return ctx.Err()
	}, Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		query, v := requestBody(t, r)
		calls++
		if strings.Contains(query, "KataLinearScope") {
			res := dataResponse(map[string]any{"viewer": map[string]any{"organization": map[string]any{"id": workspaceID}}, "team": map[string]any{"id": teamID, "name": "Example team", "archivedAt": nil, "organization": map[string]any{"id": workspaceID}}})
			if calls == 1 {
				res.Header.Set("X-RateLimit-Requests-Remaining", "0")
				res.Header.Set("X-RateLimit-Requests-Reset", fmt.Sprint(now.Add(10*time.Second).UnixMilli()))
			}
			return res, nil
		}
		require.Contains(t, query, "KataLinearProject")
		require.Equal(t, projectID, v["project"])
		require.Equal(t, teamID, v["team"])
		return dataResponse(map[string]any{"project": map[string]any{"id": projectID, "name": "Example project", "archivedAt": nil, "teams": map[string]any{"nodes": []any{map[string]any{"id": teamID}}, "pageInfo": map[string]any{"hasNextPage": false}}}}), nil
	})})
	cfg := testConfig()
	cfg.ProjectID = projectID
	s, err := c.ForRun(context.Background(), cfg)
	require.NoError(t, err)
	scope, err := s.Scope(context.Background(), cfg)
	require.NoError(t, err)
	require.Equal(t, projectID, scope.ProjectID)
	// A new run shares the same account client quota instead of bypassing cooldown.
	s, err = c.ForRun(context.Background(), cfg)
	require.NoError(t, err)
	_, err = s.Scope(context.Background(), cfg)
	require.NoError(t, err)
	require.Equal(t, []time.Duration{10 * time.Second, 1500 * time.Millisecond, 1500 * time.Millisecond}, waits)
}

// A rate-limited response waits for the exhausted bucket, not for an unrelated
// bucket that still has quota and resets much later.
func TestClientRateLimitWaitsForExhaustedBucket(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var waits []time.Duration
	calls := 0
	c := NewClient(ClientConfig{LookupEnv: func(string) (string, bool) { return "example-key", true }, Now: func() time.Time { return now }, Wait: func(ctx context.Context, d time.Duration) error {
		waits = append(waits, d)
		now = now.Add(d)
		return ctx.Err()
	}, Transport: roundTrip(func(*http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			res := response(400, `{"errors":[{"extensions":{"code":"RATELIMITED"}}]}`)
			res.Header.Set("X-RateLimit-Requests-Remaining", "0")
			res.Header.Set("X-RateLimit-Requests-Reset", fmt.Sprint(now.Add(10*time.Second).UnixMilli()))
			res.Header.Set("X-RateLimit-Complexity-Remaining", "250000")
			res.Header.Set("X-RateLimit-Complexity-Reset", fmt.Sprint(now.Add(time.Hour).UnixMilli()))
			return res, nil
		}
		return dataResponse(map[string]any{"issues": map[string]any{"nodes": []any{}, "pageInfo": map[string]any{"hasNextPage": false}}}), nil
	})})
	_, err := sessionFor(t, c).Issues(context.Background(), testConfig())
	require.NoError(t, err)
	require.Equal(t, []time.Duration{10 * time.Second}, waits)
}
