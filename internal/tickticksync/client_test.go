package tickticksync

import (
	"context"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/issuesync"
)

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func reply(code int, body string) *http.Response {
	return &http.Response{StatusCode: code, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}
}
func testClient(t *testing.T, fn transportFunc) (*Client, *time.Time) {
	t.Helper()
	now := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	c := NewClient(ClientConfig{LookupEnv: func(string) (string, bool) { return "example-secret", true }, Transport: fn, Now: func() time.Time { return now }, Wait: func(_ context.Context, d time.Duration) error { now = now.Add(d); return nil }})
	return c, &now
}
func clientConfig() Config { return Config{ProjectID: "project-1", StatusSync: "two-way"} }

// Contract: credentials reach only the fixed public API, including on redirects.
func TestClientPinnedCredentialsAndScopedReads(t *testing.T) {
	c, _ := testClient(t, func(r *http.Request) (*http.Response, error) {
		require.Equal(t, "https", r.URL.Scheme)
		require.Equal(t, "api.ticktick.com", r.URL.Host)
		require.Equal(t, "Bearer example-secret", r.Header.Get("Authorization"))
		switch r.URL.Path {
		case "/open/v1/project/project-1":
			return reply(200, `{"id":"project-1","kind":"TASK","permission":"write"}`), nil
		case "/open/v1/project/project-1/data":
			return reply(200, `{"project":{"id":"project-1","kind":"TASK"},"tasks":[{"id":"task-1","projectId":"project-1","status":0}]}`), nil
		default:
			return nil, errors.New("unexpected request")
		}
	})
	s, err := c.ForRun(context.Background(), clientConfig())
	require.NoError(t, err)
	_, err = s.Project(context.Background())
	require.NoError(t, err)
	data, err := s.Data(context.Background())
	require.NoError(t, err)
	require.Len(t, data.Tasks, 1)
	for _, body := range []string{`{"project":{"id":"other-project","kind":"TASK"}}`, `{"project":{"id":"project-1","kind":"TASK","closed":true}}`, `{"project":{"id":"project-1","kind":"TASK"},"tasks":[{"id":"task-1","projectId":"other-project","status":0}]}`, `{"project":{"id":"project-1","kind":"TASK"},"tasks":[{"id":"task-1","projectId":"project-1"}]}`, `null`} {
		bad, _ := testClient(t, func(*http.Request) (*http.Response, error) { return reply(200, body), nil })
		s, err := bad.ForRun(context.Background(), clientConfig())
		require.NoError(t, err)
		_, err = s.Data(context.Background())
		require.Error(t, err)
	}
	calls := 0
	redirect, _ := testClient(t, func(*http.Request) (*http.Response, error) {
		calls++
		r := reply(302, "example-secret")
		r.Header.Set("Location", "https://foreign.example/")
		return r, nil
	})
	s, err = redirect.ForRun(context.Background(), clientConfig())
	require.NoError(t, err)
	_, err = s.Project(context.Background())
	require.Error(t, err)
	require.NotContains(t, err.Error(), "example-secret")
	require.Equal(t, 1, calls)
}

func TestClientRetryCooldownAuthAndBounds(t *testing.T) {
	calls := 0
	c, now := testClient(t, func(*http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			r := reply(429, "secret remote body")
			r.Header.Set("Retry-After", "12")
			return r, nil
		}
		return reply(200, `{"id":"project-1","kind":"TASK"}`), nil
	})
	before := *now
	s, err := c.ForRun(context.Background(), clientConfig())
	require.NoError(t, err)
	_, err = s.Project(context.Background())
	require.NoError(t, err)
	require.Equal(t, 2, calls)
	require.GreaterOrEqual(t, now.Sub(before), 12*time.Second)
	for _, code := range []int{401, 403, 404} {
		calls = 0
		c, _ = testClient(t, func(*http.Request) (*http.Response, error) { calls++; return reply(code, "secret remote body"), nil })
		s, err = c.ForRun(context.Background(), clientConfig())
		require.NoError(t, err)
		_, err = s.Project(context.Background())
		require.Error(t, err)
		require.NotContains(t, err.Error(), "secret")
		require.Equal(t, 1, calls)
	}
	c, _ = testClient(t, func(*http.Request) (*http.Response, error) { return reply(200, strings.Repeat("x", (8<<20)+1)), nil })
	s, err = c.ForRun(context.Background(), clientConfig())
	require.NoError(t, err)
	_, err = s.Project(context.Background())
	require.Error(t, err)
	c = NewClient(ClientConfig{LookupEnv: func(string) (string, bool) { return "", false }})
	_, err = c.ForRun(context.Background(), clientConfig())
	require.Error(t, err)
}

func TestClientCompletionVerifiedAndUnsafeWritesBlocked(t *testing.T) {
	status := 0
	posts := 0
	admitted := false
	recurring := false
	c, _ := testClient(t, func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "/complete") {
			require.True(t, admitted)
			require.Equal(t, "POST", r.Method)
			if r.Body != nil {
				b, _ := io.ReadAll(r.Body)
				require.Empty(t, b)
			}
			posts++
			status = 2
			return reply(200, ""), nil
		}
		if r.URL.Path == "/open/v1/project/project-1" {
			return reply(200, `{"id":"project-1","kind":"TASK","permission":"write"}`), nil
		}
		repeat := ""
		if recurring {
			repeat = "RRULE:FREQ=DAILY"
		}
		return reply(200, fmt.Sprintf(`{"id":"task-1","projectId":"project-1","status":%d,"repeatFlag":%q}`, status, repeat)), nil
	})
	s, err := c.ForRun(context.Background(), clientConfig())
	require.NoError(t, err)
	obs, err := s.(StatusSession).WriteStatus(context.Background(), "task-1", "closed", func() error { admitted = true; return nil })
	require.NoError(t, err)
	require.Equal(t, "closed", obs.Status)
	require.Equal(t, 1, posts)
	_, err = s.(StatusSession).WriteStatus(context.Background(), "task-1", "closed", func() error { return errors.New("must not dispatch") })
	require.NoError(t, err)
	require.Equal(t, 1, posts)
	_, err = s.(StatusSession).WriteStatus(context.Background(), "task-1", "open", func() error { return nil })
	require.Error(t, err)
	require.True(t, err.(*issuesync.StatusError).Blocked)
	require.Equal(t, 1, posts)
	status = 0
	recurring = true
	_, err = s.(StatusSession).WriteStatus(context.Background(), "task-1", "closed", func() error { return nil })
	require.Error(t, err)
	require.Equal(t, 1, posts)
	recurring = false
	_, err = s.(StatusSession).WriteStatus(context.Background(), "task-1", "closed", func() error { return errors.New("new intent replaced it") })
	require.ErrorContains(t, err, "new intent")
	require.Equal(t, 1, posts)
}

func TestStatusSyncIgnoresImportContentValidationFailures(t *testing.T) {
	oversizedTitle := strings.Repeat("x", (1<<20)+1)
	status := 0
	posts := 0
	c, _ := testClient(t, func(r *http.Request) (*http.Response, error) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/complete"):
			posts++
			status = 2
			return reply(http.StatusOK, ""), nil
		case r.URL.Path == "/open/v1/project/project-1":
			return reply(http.StatusOK, `{"id":"project-1","kind":"TASK","permission":"write"}`), nil
		case r.URL.Path == "/open/v1/project/project-1/task/task-1":
			return reply(http.StatusOK, fmt.Sprintf(`{"id":"task-1","projectId":"project-1","status":%d,"kind":"TEXT","title":%q}`, status, oversizedTitle)), nil
		default:
			return nil, fmt.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	})
	s, err := c.ForRun(context.Background(), clientConfig())
	require.NoError(t, err)
	statusSession := s.(StatusSession)

	obs, err := statusSession.ReadStatus(context.Background(), "task-1")
	require.NoError(t, err)
	require.Equal(t, "open", obs.Status)

	obs, err = statusSession.WriteStatus(context.Background(), "task-1", "closed", func() error { return nil })
	require.NoError(t, err)
	require.Equal(t, "closed", obs.Status)
	require.Equal(t, 1, posts)
}

func TestClientNeverBlindlyRetriesCompletion(t *testing.T) {
	for _, code := range []int{429, 500, 401} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			posts := 0
			c, now := testClient(t, func(r *http.Request) (*http.Response, error) {
				if r.Method == "POST" {
					posts++
					res := reply(code, "secret")
					res.Header.Set("Retry-After", "10")
					return res, nil
				}
				if strings.HasSuffix(r.URL.Path, "task-1") {
					return reply(200, `{"id":"task-1","projectId":"project-1","status":0}`), nil
				}
				return reply(200, `{"id":"project-1","kind":"TASK"}`), nil
			})
			s, err := c.ForRun(context.Background(), clientConfig())
			require.NoError(t, err)
			_, err = s.(StatusSession).WriteStatus(context.Background(), "task-1", "closed", func() error { return nil })
			require.Error(t, err)
			var se *issuesync.StatusError
			require.ErrorAs(t, err, &se)
			require.Equal(t, code, se.HTTPStatus)
			require.Equal(t, code >= 500, se.Ambiguous)
			require.Equal(t, 1, posts)
			require.NotContains(t, err.Error(), "secret")
			if code == 429 {
				before := *now
				_, err = s.Project(context.Background())
				require.NoError(t, err)
				require.GreaterOrEqual(t, now.Sub(before), 10*time.Second)
			}
		})
	}
}

// Project policy must remain live for writes inside a status session. A task
// scope check cannot detect a project archived or made read-only after the
// session opened. Reads skip the recheck because they cannot change TickTick.
func TestStatusSessionRechecksLiveProjectPolicy(t *testing.T) {
	for _, project := range []string{`{"id":"project-1","kind":"TASK","closed":true}`, `{"id":"project-1","kind":"TASK","permission":"read"}`} {
		t.Run(project, func(t *testing.T) {
			changed := false
			taskReads, posts := 0, 0
			c, _ := testClient(t, func(r *http.Request) (*http.Response, error) {
				if r.Method == "POST" {
					posts++
					return reply(200, ""), nil
				}
				if r.URL.Path == "/open/v1/project/project-1" {
					if changed {
						return reply(200, project), nil
					}
					return reply(200, `{"id":"project-1","kind":"TASK","permission":"write"}`), nil
				}
				taskReads++
				return reply(200, `{"id":"task-1","projectId":"project-1","status":0}`), nil
			})
			session, err := c.ForRun(context.Background(), clientConfig())
			require.NoError(t, err)
			status := session.(StatusSession)
			_, err = status.ReadStatus(context.Background(), "task-1")
			require.NoError(t, err)
			changed = true
			_, err = status.ReadStatus(context.Background(), "task-1")
			require.NoError(t, err)
			_, err = status.WriteStatus(context.Background(), "task-1", "closed", func() error { return nil })
			require.Error(t, err)
			require.Equal(t, 2, taskReads)
			require.Zero(t, posts)
		})
	}
}

// An extreme Retry-After must not stall every binding until daemon restart.
func TestClientCapsRetryAfterCooldown(t *testing.T) {
	for _, header := range []string{"2000000000", "Fri, 01 Jan 2100 00:00:00 GMT"} {
		t.Run(header, func(t *testing.T) {
			c, _ := testClient(t, func(*http.Request) (*http.Response, error) {
				r := reply(429, "")
				r.Header.Set("Retry-After", header)
				return r, nil
			})
			s, err := c.ForRun(context.Background(), clientConfig())
			require.NoError(t, err)
			_, err = s.Project(context.Background())
			se, ok := errors.AsType[*issuesync.StatusError](err)
			require.True(t, ok)
			require.LessOrEqual(t, se.RetryAfter, maxCooldown)
		})
	}
}

// Status reads cost one task request each; the project policy is checked once
// when the status pass opens.
func TestStatusPassChecksProjectOncePerRun(t *testing.T) {
	projectReads := 0
	archived := false
	c, _ := testClient(t, func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/open/v1/project/project-1" {
			projectReads++
			if archived {
				return reply(200, `{"id":"project-1","kind":"TASK","closed":true}`), nil
			}
			return reply(200, `{"id":"project-1","kind":"TASK","permission":"write"}`), nil
		}
		return reply(200, `{"id":"task-1","projectId":"project-1","status":0}`), nil
	})
	b := db.IssueSyncBinding{Provider: "ticktick", SourceKey: "ticktick:project-1", RemoteID: "project-1", Config: jsontext.Value(`{"project_id":"project-1","status_sync":"two-way"}`)}
	adapter := NewAdapter(nil, c)
	run, err := adapter.OpenStatus(context.Background(), b, time.Time{})
	require.NoError(t, err)
	mapping := db.IssueStatusMapping{Mapping: db.ImportMapping{ExternalID: "task:task-1"}}
	for range 3 {
		_, err = run.ReadStatus(context.Background(), mapping)
		require.NoError(t, err)
	}
	require.Equal(t, 1, projectReads)
	archived = true
	_, err = adapter.OpenStatus(context.Background(), b, time.Time{})
	require.True(t, issuesync.IsBlockedStatusWarning(err))
}
