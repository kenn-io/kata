package notionsync

import (
	"context"
	"encoding/json/v2"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type notionTransport func(*http.Request) (*http.Response, error)

func (f notionTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type responseReadErrorBody struct{ content string }

func (b *responseReadErrorBody) Read(p []byte) (int, error) {
	n := copy(p, b.content)
	b.content = b.content[n:]
	return n, errors.New("private response read failure")
}
func (*responseReadErrorBody) Close() error { return nil }

func response(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}
func jsonResponse(t *testing.T, body any) *http.Response {
	t.Helper()
	b, e := json.Marshal(body)
	require.NoError(t, e)
	return response(200, string(b))
}

type clientClock struct {
	mu    sync.Mutex
	now   time.Time
	waits []time.Duration
}

func (c *clientClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *clientClock) Wait(ctx context.Context, d time.Duration) error {
	if e := ctx.Err(); e != nil {
		return e
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.waits = append(c.waits, d)
	c.now = c.now.Add(d)
	return nil
}
func testClient(t *testing.T, transport notionTransport) (*Client, *clientClock) {
	t.Helper()
	clock := &clientClock{now: time.Now()}
	return NewClient(ClientConfig{LookupEnv: func(string) (string, bool) { return "test-secret", true }, Transport: transport, Now: clock.Now, Wait: clock.Wait}), clock
}
func session(t *testing.T, c *Client) Session {
	t.Helper()
	s, e := c.ForRun(context.Background())
	require.NoError(t, e)
	return s
}
func databaseWire() map[string]any {
	return map[string]any{"object": "database", "id": databaseID, "data_sources": []any{map[string]any{"id": sourceID, "name": "Example tasks"}}, "url": "https://attacker.example/ignored"}
}

func TestClientCredentialBoundary(t *testing.T) {
	token := " \tfirst-secret\r\n"
	calls := 0
	clock := &clientClock{now: time.Now()}
	c := NewClient(ClientConfig{TokenEnv: "EXAMPLE_TOKEN", LookupEnv: func(key string) (string, bool) { require.Equal(t, "EXAMPLE_TOKEN", key); return token, true }, Now: clock.Now, Wait: clock.Wait, Transport: notionTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		require.Equal(t, "https://api.notion.com", r.URL.Scheme+"://"+r.URL.Host)
		require.Equal(t, "/v1/databases/"+databaseID, r.URL.Path)
		require.Equal(t, "2026-03-11", r.Header.Get("Notion-Version"))
		require.Equal(t, http.MethodGet, r.Method)
		if calls <= 2 {
			require.Equal(t, "Bearer first-secret", r.Header.Get("Authorization"))
		} else {
			require.Equal(t, "Bearer second-secret", r.Header.Get("Authorization"))
		}
		return jsonResponse(t, databaseWire()), nil
	})})
	s := session(t, c)
	token = "second-secret\n"
	_, e := s.Database(t.Context(), databaseID)
	require.NoError(t, e)
	_, e = s.Database(t.Context(), databaseID)
	require.NoError(t, e)
	_, e = session(t, c).Database(t.Context(), databaseID)
	require.NoError(t, e)
	token = " \n "
	_, e = c.ForRun(t.Context())
	require.Error(t, e)
	require.Equal(t, 3, calls)
	for _, status := range []int{302, 307, 401, 403, 404, 400} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			n := 0
			c, _ := testClient(t, func(_ *http.Request) (*http.Response, error) {
				n++
				res := response(status, `{"code":"test-secret","message":"test-secret"}`)
				res.Header.Set("Location", "https://attacker.example/secret")
				return res, nil
			})
			_, e := session(t, c).Database(t.Context(), databaseID)
			require.Error(t, e)
			require.NotContains(t, e.Error(), "test-secret")
			require.Equal(t, 1, n)
		})
	}
}

func TestClientPreservesSafeStatusWhenResponseBodyReadFails(t *testing.T) {
	for _, tc := range []struct {
		status int
		code   string
	}{{http.StatusUnauthorized, "unauthorized"}, {http.StatusNotFound, "object_not_found"}} {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			body := `{"code":"` + tc.code + `","message":"private-token"}`
			c, _ := testClient(t, func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: tc.status, Header: make(http.Header), Body: &responseReadErrorBody{content: body}}, nil
			})
			_, err := session(t, c).Database(t.Context(), databaseID)
			require.Error(t, err)
			require.Contains(t, err.Error(), "notion HTTP "+strconv.Itoa(tc.status))
			require.Contains(t, err.Error(), tc.code)
			require.NotContains(t, err.Error(), "private-token")
			require.NotContains(t, err.Error(), "private response read failure")
		})
	}
}

func TestClientRetryAndSharedPacing(t *testing.T) {
	for _, status := range []int{429, 529, 500, 502, 503, 504, 0} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			n := 0
			var starts []time.Time
			var clock *clientClock
			c, k := testClient(t, func(_ *http.Request) (*http.Response, error) {
				n++
				starts = append(starts, clock.Now())
				if status == 0 {
					return nil, errors.New("test-secret network error")
				}
				res := response(status, `{"code":"rate_limited"}`)
				res.Header.Set("Retry-After", "3")
				return res, nil
			})
			clock = k
			_, e := session(t, c).Database(t.Context(), databaseID)
			require.Error(t, e)
			require.NotContains(t, e.Error(), "test-secret")
			require.Equal(t, 5, n)
			for i := 1; i < len(starts); i++ {
				minimum := time.Second * time.Duration(1<<(i-1))
				if status != 0 && minimum < 3*time.Second {
					minimum = 3 * time.Second
				}
				require.GreaterOrEqual(t, starts[i].Sub(starts[i-1]), minimum)
			}
		})
	}
	t.Run("blocked access", func(t *testing.T) {
		n := 0
		c, _ := testClient(t, func(*http.Request) (*http.Response, error) {
			n++
			return response(429, `{"code":"rate_limited","additional_data":{"rate_limit_reason":"public_api_request_blocked"}}`), nil
		})
		_, e := session(t, c).Database(t.Context(), databaseID)
		require.ErrorContains(t, e, "public_api_request_blocked")
		require.Equal(t, 1, n)
	})
	t.Run("shared cooldown exceeds deadline", func(t *testing.T) {
		n := 0
		c, clock := testClient(t, func(*http.Request) (*http.Response, error) {
			n++
			if n == 1 {
				r := response(429, `{"code":"rate_limited"}`)
				r.Header.Set("Retry-After", "120")
				return r, nil
			}
			return jsonResponse(t, databaseWire()), nil
		})
		start := clock.Now()
		ctx, cancel := context.WithDeadline(t.Context(), start.Add(time.Minute))
		defer cancel()
		_, e := session(t, c).Database(ctx, databaseID)
		require.ErrorContains(t, e, "rate_limited")
		require.ErrorContains(t, e, "retry")
		require.Equal(t, 1, n)
		_, e = session(t, c).Database(t.Context(), databaseID)
		require.NoError(t, e)
		require.GreaterOrEqual(t, clock.Now().Sub(start), 120*time.Second)
	})
	t.Run("paced sessions", func(t *testing.T) {
		c, clock := testClient(t, func(*http.Request) (*http.Response, error) { return jsonResponse(t, databaseWire()), nil })
		start := clock.Now()
		for range 4 {
			_, e := session(t, c).Database(t.Context(), databaseID)
			require.NoError(t, e)
		}
		require.GreaterOrEqual(t, clock.Now().Sub(start), time.Second)
	})
	t.Run("cancel wait", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		n := 0
		c := NewClient(ClientConfig{LookupEnv: func(string) (string, bool) { return "test-secret", true }, Transport: notionTransport(func(*http.Request) (*http.Response, error) { n++; cancel(); return response(503, `{}`), nil })})
		_, e := session(t, c).Database(ctx, databaseID)
		require.ErrorIs(t, e, context.Canceled)
		require.Equal(t, 1, n)
	})
}

func TestClientDatabaseAndSchema(t *testing.T) {
	c, _ := testClient(t, func(r *http.Request) (*http.Response, error) {
		require.Equal(t, "/v1/data_sources/"+sourceID, r.URL.Path)
		return jsonResponse(t, map[string]any{"object": "data_source", "id": sourceID, "parent": map[string]any{"type": "database_id", "database_id": databaseID}, "title": []any{map[string]any{"plain_text": "Example tasks"}}, "properties": map[string]any{"Workflow": map[string]any{"id": "s%3A1", "type": "status", "status": map[string]any{"options": []any{map[string]any{"id": "done", "name": "Done"}}}}}}), nil
	})
	ds, e := session(t, c).DataSource(t.Context(), sourceID)
	require.NoError(t, e)
	require.Equal(t, DataSource{ID: sourceID, DatabaseID: databaseID, Name: "Example tasks", Properties: []Property{{ID: "s%3A1", Name: "Workflow", Type: "status", Options: []Option{{ID: "done", Name: "Done"}}}}}, ds)
	for _, bad := range []string{`{"object":"page","id":"` + databaseID + `"}`, `{"object":"database","id":"` + sourceID + `"}`} {
		c, _ := testClient(t, func(*http.Request) (*http.Response, error) { return response(200, bad), nil })
		_, e := session(t, c).Database(t.Context(), databaseID)
		require.Error(t, e)
	}
}

func TestClientConcurrentSessions(t *testing.T) {
	var mu sync.Mutex
	var starts []time.Time
	var clock *clientClock
	c, k := testClient(t, func(*http.Request) (*http.Response, error) {
		mu.Lock()
		starts = append(starts, clock.Now())
		mu.Unlock()
		return jsonResponse(t, databaseWire()), nil
	})
	clock = k
	start := clock.Now()
	sessions := make([]Session, 12)
	for i := range sessions {
		sessions[i] = session(t, c)
	}
	var wg sync.WaitGroup
	errs := make(chan error, len(sessions))
	for _, s := range sessions {
		wg.Go(func() { _, e := s.Database(t.Context(), databaseID); errs <- e })
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		require.NoError(t, e)
	}
	require.Len(t, starts, 12)
	// Fake waits advance time immediately, so network starts can arrive out of
	// admission order; the elapsed admission budget must still cover every call.
	require.GreaterOrEqual(t, clock.Now().Sub(start), time.Duration(11)*time.Second/3)
}

func TestClientAttemptTimeoutAndQueryRetry(t *testing.T) {
	n := 0
	c, _ := testClient(t, func(r *http.Request) (*http.Response, error) {
		n++
		deadline, ok := r.Context().Deadline()
		require.True(t, ok)
		require.LessOrEqual(t, time.Until(deadline), 30*time.Second)
		require.Equal(t, http.MethodPost, r.Method)
		if n == 1 {
			res := response(529, `{"code":"service_overload"}`)
			res.Header.Set("Retry-After", time.Now().Add(time.Minute).UTC().Format(http.TimeFormat))
			return res, nil
		}
		return jsonResponse(t, queryWire(nil, false, nil)), nil
	})
	_, e := session(t, c).Pages(t.Context(), configFixture(t), nil)
	require.NoError(t, e)
	require.Equal(t, 2, n)
}

type brokenNotionBody struct{}

func (brokenNotionBody) Read([]byte) (int, error) { return 0, errors.New("test-secret body error") }
func (brokenNotionBody) Close() error             { return nil }
func TestClientDoesNotRetryRejectedRequestWithBrokenBody(t *testing.T) {
	for _, status := range []int{302, 400, 401, 403, 404} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			n := 0
			c, _ := testClient(t, func(*http.Request) (*http.Response, error) {
				n++
				return &http.Response{StatusCode: status, Header: make(http.Header), Body: brokenNotionBody{}}, nil
			})
			_, e := session(t, c).Database(t.Context(), databaseID)
			require.Error(t, e)
			require.NotContains(t, e.Error(), "test-secret")
			require.Equal(t, 1, n)
		})
	}
}

func TestClientCancellationDuringPacingWait(t *testing.T) {
	waiting := make(chan struct{})
	var calls int
	c := NewClient(ClientConfig{LookupEnv: func(key string) (string, bool) {
		require.Equal(t, "KATA_NOTION_TOKEN", key)
		return "test-secret", true
	}, Transport: notionTransport(func(*http.Request) (*http.Response, error) { calls++; return jsonResponse(t, databaseWire()), nil }), Wait: func(ctx context.Context, d time.Duration) error { close(waiting); return waitForNotion(ctx, d) }})
	s := session(t, c)
	_, e := s.Database(t.Context(), databaseID)
	require.NoError(t, e)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, e := s.Database(ctx, databaseID); done <- e }()
	<-waiting
	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
	require.Equal(t, 1, calls)
}

func TestClientRetryAfterBound(t *testing.T) {
	for _, retryAfter := range []string{"86400", "18446744073709551615", time.Now().Add(24 * time.Hour).UTC().Format(http.TimeFormat)} {
		t.Run(retryAfter, func(t *testing.T) {
			calls := 0
			c, clock := testClient(t, func(*http.Request) (*http.Response, error) {
				calls++
				if calls == 1 {
					res := response(429, `{"code":"rate_limited"}`)
					res.Header.Set("Retry-After", retryAfter)
					return res, nil
				}
				return jsonResponse(t, databaseWire()), nil
			})
			start := clock.Now()
			ctx, cancel := context.WithDeadline(t.Context(), start.Add(25*time.Minute))
			defer cancel()
			_, err := session(t, c).Database(ctx, databaseID)
			require.ErrorContains(t, err, "rate_limited")
			require.Equal(t, 1, calls)
			_, err = session(t, c).Database(t.Context(), databaseID)
			require.NoError(t, err)
			require.Equal(t, 25*time.Minute, clock.Now().Sub(start))
		})
	}
}
