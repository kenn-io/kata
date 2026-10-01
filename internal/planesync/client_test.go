package planesync

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
)

func wireItem() map[string]any {
	return map[string]any{"id": testItemID, "project": testProjectID, "state": testStateID, "sequence_id": 42, "priority": "none", "name": "Source task", "description_html": "<p>Complete content</p>", "created_by": testUserID, "assignees": []string{testUserID}, "created_at": "2026-09-01T09:00:00Z", "updated_at": "2026-09-01T10:00:00Z"}
}

func wirePage(items any, next bool, cursor string) map[string]any {
	return map[string]any{"results": items, "next_page_results": next, "next_cursor": cursor}
}

func sendPlaneJSON(t *testing.T, w http.ResponseWriter, body any) {
	t.Helper()
	raw, err := json.Marshal(body)
	require.NoError(t, err)
	w.Header().Set("Content-Type", "application/json")
	_, err = w.Write(raw)
	require.NoError(t, err)
}

func planeTestClient(origin string) *Client {
	return NewClient(ClientConfig{Daemon: config.PlaneSyncConfig{APIOrigin: origin}, LookupEnv: func(string) (string, bool) { return "example-api-key", true }, Wait: func(ctx context.Context, _ time.Duration) error { return ctx.Err() }})
}

func TestClientReadOnlyAndCapturedOriginCredential(t *testing.T) {
	var requests atomic.Int32
	token := "first-key"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		require.Equal(t, "GET", r.Method)
		require.Equal(t, "first-key", r.Header.Get("X-API-Key"))
		require.Empty(t, r.Header.Get("Authorization"))
		require.Empty(t, r.Header.Get("Cookie"))
		prefix := "/api/v1/workspaces/example-workspace/projects/" + testProjectID + "/"
		switch r.URL.Path {
		case prefix:
			sendPlaneJSON(t, w, map[string]any{"id": testProjectID, "name": "Example project", "identifier": "EX"})
		case prefix + "states/":
			sendPlaneJSON(t, w, wirePage([]map[string]any{{"id": testStateID, "group": "started"}}, false, "last-cursor-is-allowed"))
		case prefix + "work-items/":
			require.Equal(t, "100", r.URL.Query().Get("per_page"))
			require.Equal(t, "sequence_id", r.URL.Query().Get("order_by"))
			require.Empty(t, r.URL.Query().Get("expand"))
			sendPlaneJSON(t, w, wirePage([]map[string]any{wireItem()}, false, ""))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	c := testConfig()
	c.APIOrigin = server.URL
	client := NewClient(ClientConfig{Daemon: config.PlaneSyncConfig{APIOrigin: server.URL}, LookupEnv: func(name string) (string, bool) { require.Equal(t, "KATA_PLANE_TOKEN", name); return token, true }, Wait: func(context.Context, time.Duration) error { return nil }})
	session, err := client.ForRun(context.Background(), c)
	require.NoError(t, err)
	token = "rotated-key"
	p, err := session.Project(context.Background(), c)
	require.NoError(t, err)
	require.Equal(t, testProjectID, p.ID)
	states, err := session.States(context.Background(), c)
	require.NoError(t, err)
	require.Equal(t, []State{{ID: testStateID, Group: "started"}}, states)
	items, err := session.WorkItems(context.Background(), c)
	require.NoError(t, err)
	require.Len(t, items, 1)
	require.Equal(t, "<p>Complete content</p>", items[0].DescriptionHTML)
	c.APIOrigin = "https://different.example"
	_, err = session.Project(context.Background(), c)
	require.Error(t, err)
	_, err = client.ForRun(context.Background(), c)
	require.Error(t, err)
	require.Equal(t, int32(3), requests.Load())
}

func TestClientNeverFollowsRedirectsOrLeaksErrors(t *testing.T) {
	var foreign atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) { foreign.Add(1) }))
	defer target.Close()
	for _, status := range []int{302, 401, 403, 500} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Location", target.URL)
				w.WriteHeader(status)
				_, _ = fmt.Fprint(w, "example-api-key private-response")
			}))
			defer server.Close()
			c := testConfig()
			c.APIOrigin = server.URL
			s, err := planeTestClient(server.URL).ForRun(context.Background(), c)
			require.NoError(t, err)
			_, err = s.Project(context.Background(), c)
			require.Error(t, err)
			require.NotContains(t, err.Error(), "example-api-key")
			require.NotContains(t, err.Error(), "private-response")
		})
	}
	require.Zero(t, foreign.Load())
}

func TestClientRejectsIncompletePaginationAndWireData(t *testing.T) {
	for _, kind := range []string{"missing results", "missing flag", "missing cursor", "repeated cursor", "duplicate ID", "missing description", "missing state", "expanded state", "missing assignees", "bad time", "wrong project", "response bound", "unknown group"} {
		t.Run(kind, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				item := wireItem()
				page := wirePage([]map[string]any{item}, false, "")
				switch kind {
				case "missing results":
					delete(page, "results")
				case "missing flag":
					delete(page, "next_page_results")
				case "missing cursor":
					page["next_page_results"] = true
				case "repeated cursor":
					page["next_page_results"] = true
					page["next_cursor"] = "same"
					item["id"] = testUserID
				case "duplicate ID":
					page["results"] = []map[string]any{item, item}
				case "missing description":
					delete(item, "description_html")
				case "missing state":
					delete(item, "state")
				case "expanded state":
					item["state"] = map[string]string{"id": testStateID}
				case "missing assignees":
					delete(item, "assignees")
				case "bad time":
					item["updated_at"] = "invalid"
				case "wrong project":
					item["project"] = testUserID
				case "response bound":
					_, _ = fmt.Fprint(w, strings.Repeat(" ", (8<<20)+1))
					return
				case "unknown group":
					page["results"] = []map[string]any{{"id": testStateID, "group": "unknown"}}
				}
				sendPlaneJSON(t, w, page)
			}))
			defer server.Close()
			c := testConfig()
			c.APIOrigin = server.URL
			s, err := planeTestClient(server.URL).ForRun(context.Background(), c)
			require.NoError(t, err)
			if kind == "unknown group" {
				_, err = s.States(context.Background(), c)
			} else {
				_, err = s.WorkItems(context.Background(), c)
			}
			require.Error(t, err)
		})
	}
}

func TestClientPaginationAndStateArrays(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "states/") {
			sendPlaneJSON(t, w, []map[string]any{{"id": testStateID, "group": "completed"}})
			return
		}
		requests.Add(1)
		item := wireItem()
		if r.URL.Query().Get("cursor") == "" {
			sendPlaneJSON(t, w, wirePage([]map[string]any{item}, true, "100:1:0"))
			return
		}
		require.Equal(t, "100:1:0", r.URL.Query().Get("cursor"))
		item["id"] = testUserID
		sendPlaneJSON(t, w, wirePage([]map[string]any{item}, false, "100:2:0"))
	}))
	defer server.Close()
	c := testConfig()
	c.APIOrigin = server.URL
	s, err := planeTestClient(server.URL).ForRun(context.Background(), c)
	require.NoError(t, err)
	states, err := s.States(context.Background(), c)
	require.NoError(t, err)
	require.Len(t, states, 1)
	items, err := s.WorkItems(context.Background(), c)
	require.NoError(t, err)
	require.Len(t, items, 2)
	require.Equal(t, int32(2), requests.Load())
}

func TestClientImportsPriority(t *testing.T) {
	for _, tc := range []struct {
		priority string
		want     *int64
	}{{"urgent", new(int64(0))}, {"high", new(int64(1))}, {"medium", new(int64(2))}, {"low", new(int64(3))}, {"none", nil}} {
		t.Run(tc.priority, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				item := wireItem()
				item["priority"] = tc.priority
				sendPlaneJSON(t, w, wirePage([]map[string]any{item}, false, ""))
			}))
			defer server.Close()
			c := testConfig()
			c.APIOrigin = server.URL
			session, err := planeTestClient(server.URL).ForRun(t.Context(), c)
			require.NoError(t, err)
			items, err := session.WorkItems(t.Context(), c)
			require.NoError(t, err)
			batch, err := BuildImportBatch(c.SourceKey(), c, Project{ID: testProjectID, Identifier: "EX"}, []State{{ID: testStateID, Group: "started"}}, items)
			require.NoError(t, err)
			require.Equal(t, tc.want, batch.Items[0].Priority)
		})
	}
}

func TestClientErrorsIdentifyWorkItem(t *testing.T) {
	for _, tc := range []struct {
		field string
		value any
	}{{"description_html", nil}, {"sequence_id", 0}, {"updated_at", "invalid"}, {"state", map[string]string{"id": testStateID}}, {"created_by", "invalid"}, {"priority", nil}, {"priority", "unknown"}} {
		t.Run(tc.field+fmt.Sprint(tc.value), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				item := wireItem()
				item[tc.field] = tc.value
				sendPlaneJSON(t, w, wirePage([]map[string]any{item}, false, ""))
			}))
			defer server.Close()
			c := testConfig()
			c.APIOrigin = server.URL
			session, err := planeTestClient(server.URL).ForRun(t.Context(), c)
			require.NoError(t, err)
			_, err = session.WorkItems(t.Context(), c)
			require.ErrorContains(t, err, testItemID)
		})
	}
}

func TestClientSharesPacingBetweenConcurrentSessions(t *testing.T) {
	var mu sync.Mutex
	var times []time.Time
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		times = append(times, time.Now())
		mu.Unlock()
		sendPlaneJSON(t, w, map[string]string{"id": testProjectID, "identifier": "EX", "name": "Example"})
	}))
	defer server.Close()
	c := testConfig()
	c.APIOrigin = server.URL
	client := NewClient(ClientConfig{Daemon: config.PlaneSyncConfig{APIOrigin: server.URL}, LookupEnv: func(string) (string, bool) { return "example-api-key", true }})
	var wg sync.WaitGroup
	for range 3 {
		wg.Go(func() {
			s, err := client.ForRun(context.Background(), c)
			if err == nil {
				_, err = s.Project(context.Background(), c)
			}
			if err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	require.Len(t, times, 3)
	for i := 1; i < len(times); i++ {
		require.GreaterOrEqual(t, times[i].Sub(times[i-1]), 950*time.Millisecond)
	}
}

func TestClientRetryAndCancellation(t *testing.T) {
	var requests atomic.Int32
	var waits []time.Duration
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if requests.Add(1) == 1 {
			w.Header().Set("Retry-After", "2")
			w.WriteHeader(429)
			return
		}
		sendPlaneJSON(t, w, map[string]string{"id": testProjectID, "identifier": "EX", "name": "Example"})
	}))
	defer server.Close()
	c := testConfig()
	c.APIOrigin = server.URL
	now := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	client := NewClient(ClientConfig{Daemon: config.PlaneSyncConfig{APIOrigin: server.URL}, LookupEnv: func(string) (string, bool) { return "example-api-key", true }, Now: func() time.Time { return now }, Wait: func(_ context.Context, d time.Duration) error { waits = append(waits, d); now = now.Add(d); return nil }})
	s, err := client.ForRun(context.Background(), c)
	require.NoError(t, err)
	_, err = s.Project(context.Background(), c)
	require.NoError(t, err)
	require.Equal(t, int32(2), requests.Load())
	require.Contains(t, waits, 2*time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = s.Project(ctx, c)
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, int32(2), requests.Load())
	bad := NewClient(ClientConfig{Daemon: config.PlaneSyncConfig{APIOrigin: server.URL}, LookupEnv: func(string) (string, bool) { return "", false }})
	_, err = bad.ForRun(context.Background(), c)
	require.Error(t, err)
	transport := NewClient(ClientConfig{Daemon: config.PlaneSyncConfig{APIOrigin: server.URL}, LookupEnv: func(string) (string, bool) { return "example-api-key", true }, Transport: planeRoundTrip(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("example-api-key private-transport")
	})})
	s, err = transport.ForRun(context.Background(), c)
	require.NoError(t, err)
	_, err = s.Project(context.Background(), c)
	require.Error(t, err)
	require.NotContains(t, err.Error(), "example-api-key")
}

func TestClientPreservesDeadlineFromRequestExecution(t *testing.T) {
	c := testConfig()
	client := NewClient(ClientConfig{
		Daemon:    config.PlaneSyncConfig{APIOrigin: c.APIOrigin},
		LookupEnv: func(string) (string, bool) { return "example-api-key", true },
		Transport: planeRoundTrip(func(*http.Request) (*http.Response, error) {
			return nil, context.DeadlineExceeded
		}),
	})
	s, err := client.ForRun(context.Background(), c)
	require.NoError(t, err)

	_, err = s.Project(context.Background(), c)
	require.ErrorIs(t, err, context.DeadlineExceeded)
}

func TestClientPreservesDeadlineFromResponseBodyRead(t *testing.T) {
	c := testConfig()
	client := NewClient(ClientConfig{
		Daemon:    config.PlaneSyncConfig{APIOrigin: c.APIOrigin},
		LookupEnv: func(string) (string, bool) { return "example-api-key", true },
		Transport: planeRoundTrip(func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(planeErrorReader{err: context.DeadlineExceeded}),
			}, nil
		}),
	})
	s, err := client.ForRun(context.Background(), c)
	require.NoError(t, err)

	_, err = s.Project(context.Background(), c)
	require.ErrorIs(t, err, context.DeadlineExceeded)
}

func TestClientCollectionBounds(t *testing.T) {
	for _, kind := range []string{"pages", "items"} {
		t.Run(kind, func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				count := requests.Add(1)
				if kind == "pages" {
					sendPlaneJSON(t, w, wirePage([]any{}, true, fmt.Sprint(count)))
				} else {
					sendPlaneJSON(t, w, wirePage(make([]any, maxItems+1), false, ""))
				}
			}))
			defer server.Close()
			c := testConfig()
			c.APIOrigin = server.URL
			s, err := planeTestClient(server.URL).ForRun(context.Background(), c)
			require.NoError(t, err)
			_, err = s.WorkItems(context.Background(), c)
			require.Error(t, err)
			if kind == "pages" {
				require.Equal(t, int32(1000), requests.Load())
			} else {
				require.Equal(t, int32(1), requests.Load())
			}
		})
	}
}

func TestClientBoundsAggregateCollectionMemory(t *testing.T) {
	var calls int
	c := testConfig()
	client := NewClient(ClientConfig{Daemon: config.PlaneSyncConfig{}, LookupEnv: func(string) (string, bool) { return "example-api-key", true }, Wait: func(context.Context, time.Duration) error { return nil }, Transport: planeRoundTrip(func(*http.Request) (*http.Response, error) {
		calls++
		raw, err := json.Marshal(wirePage([]any{}, true, fmt.Sprint(calls)))
		require.NoError(t, err)
		body := strings.Repeat(" ", (8<<20)-1024) + string(raw)
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
	})})
	s, err := client.ForRun(context.Background(), c)
	require.NoError(t, err)
	_, err = s.WorkItems(context.Background(), c)
	require.Error(t, err)
	require.Contains(t, err.Error(), "128 MiB")
	require.Equal(t, 17, calls)
}

type planeRoundTrip func(*http.Request) (*http.Response, error)

func (f planeRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type planeErrorReader struct{ err error }

func (r planeErrorReader) Read([]byte) (int, error) { return 0, r.err }

func TestClientAdmissionCancellationWhileAnotherCallerWaits(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	c := NewClient(ClientConfig{Wait: func(ctx context.Context, _ time.Duration) error {
		close(entered)
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}})
	c.next = time.Now().Add(time.Hour)
	done := make(chan error, 1)
	go func() { done <- c.admit(context.Background()) }()
	<-entered
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	canceled := make(chan error, 1)
	go func() { canceled <- c.admit(ctx) }()
	select {
	case err := <-canceled:
		require.ErrorIs(t, err, context.DeadlineExceeded)
	case <-time.After(time.Second):
		t.Error("a queued caller did not honor its deadline")
	}
	close(release)
	require.NoError(t, <-done)
}

func TestClientCapsSharedRetryAfterCooldown(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, value := range []string{"2147483647", now.Add(48 * time.Hour).Format(http.TimeFormat)} {
		c := NewClient(ClientConfig{Now: func() time.Time { return now }})
		c.deferRequests(value, 0)
		require.Equal(t, 20*time.Minute, c.cooldown.Sub(now))
	}
}
