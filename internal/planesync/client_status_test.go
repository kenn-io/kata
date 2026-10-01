package planesync

import (
	"context"
	"encoding/json/v2"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/issuesync"
)

const completedStateID = "55555555-5555-4555-8555-555555555555"
const cancelledStateID = "66666666-6666-4666-8666-666666666666"
const unstartedStateID = "77777777-7777-4777-8777-777777777777"

type testStatusSession interface {
	ReadStatus(context.Context, Config, string) (issuesync.StatusObservation, error)
	WriteStatus(context.Context, Config, string, string, func() error) (issuesync.StatusObservation, error)
}

func statusConfig(t *testing.T, origin string) Config {
	t.Helper()
	c := testConfig()
	c.APIOrigin = origin
	raw, err := EncodeConfig(c)
	require.NoError(t, err)
	var fields map[string]any
	require.NoError(t, json.Unmarshal(raw, &fields))
	fields["status_sync"] = "two-way"
	raw, err = json.Marshal(fields)
	require.NoError(t, err)
	c, err = DecodeConfig(raw)
	require.NoError(t, err)
	return c
}
func statusSession(t *testing.T, c Config) testStatusSession {
	t.Helper()
	session, err := planeTestClient(c.APIOrigin).ForRun(context.Background(), c)
	require.NoError(t, err)
	status, ok := session.(testStatusSession)
	require.True(t, ok, "Plane session must support status-only operations")
	return status
}
func workflowRows() []map[string]any {
	return []map[string]any{{"id": testStateID, "group": "started", "sequence": 2}, {"id": completedStateID, "group": "completed", "sequence": 4}, {"id": cancelledStateID, "group": "cancelled", "sequence": 5}, {"id": unstartedStateID, "group": "unstarted", "sequence": 1}}
}
func TestPlaneStatusOnlyWriteReadbackAndPreservesSubstates(t *testing.T) {
	ctx := context.Background()
	state := testStateID
	patches := 0
	admissions := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "example-api-key", r.Header.Get("X-API-Key"))
		prefix := "/api/v1/workspaces/example-workspace/projects/" + testProjectID + "/"
		switch r.URL.Path {
		case prefix:
			sendPlaneJSON(t, w, map[string]any{"id": testProjectID, "name": "Example project", "identifier": "EX"})
		case prefix + "states/":
			sendPlaneJSON(t, w, wirePage(workflowRows(), false, ""))
		case prefix + "work-items/" + testItemID + "/":
			if r.Method == http.MethodPatch {
				require.Equal(t, patches+1, admissions, "fresh admission must precede dispatch")
				body, err := io.ReadAll(r.Body)
				require.NoError(t, err)
				var payload map[string]string
				require.NoError(t, json.Unmarshal(body, &payload))
				require.Len(t, payload, 1)
				state = payload["state"]
				patches++
			}
			// No title, body, assignee or creation metadata needed for status sync.
			sendPlaneJSON(t, w, map[string]any{"id": testItemID, "project": testProjectID, "state": state, "updated_at": "2026-09-29T10:00:00Z", "completed_at": "2026-09-29T09:00:00Z"})
		default:
			t.Errorf("unexpected status request %s", r.URL)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	c := statusConfig(t, server.URL)
	session := statusSession(t, c)
	observe, err := session.ReadStatus(ctx, c, testItemID)
	require.NoError(t, err)
	require.Equal(t, "open", observe.Status)
	observe, err = session.WriteStatus(ctx, c, testItemID, "closed", func() error { admissions++; return nil })
	require.NoError(t, err)
	require.Equal(t, completedStateID, *observe.RawStatus)
	require.Equal(t, "done", observe.ClosedReason)
	require.Equal(t, "2026-09-29T09:00:00Z", observe.ClosedAt.Format(time.RFC3339))
	require.Equal(t, 1, patches)
	state = cancelledStateID
	observe, err = session.WriteStatus(ctx, c, testItemID, "closed", func() error { t.Error("matching binary state must preserve cancellation"); return nil })
	require.NoError(t, err)
	require.Equal(t, cancelledStateID, *observe.RawStatus)
	require.Equal(t, "wontfix", observe.ClosedReason)
	require.Equal(t, 1, patches)
	observe, err = session.WriteStatus(ctx, c, testItemID, "open", func() error { admissions++; return nil })
	require.NoError(t, err)
	require.Equal(t, unstartedStateID, *observe.RawStatus)
	require.Equal(t, "open", observe.Status)
	require.Equal(t, 2, patches)
}

func TestPlaneStatusWriteRejectsStaleAdmissionAndNeverRetriesAmbiguousPatch(t *testing.T) {
	for _, reject := range []bool{true, false} {
		t.Run(map[bool]string{true: "fenced", false: "ambiguous"}[reject], func(t *testing.T) {
			patches := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodPatch:
					patches++
					w.Header().Set("Retry-After", "20")
					w.WriteHeader(503)
					_, _ = w.Write([]byte("private response"))
				case r.URL.Path[len(r.URL.Path)-7:] == "states/":
					sendPlaneJSON(t, w, wirePage(workflowRows(), false, ""))
				case r.URL.Path[len(r.URL.Path)-len(testItemID)-1:] == testItemID+"/":
					sendPlaneJSON(t, w, map[string]any{"id": testItemID, "project": testProjectID, "state": testStateID, "updated_at": "2026-09-29T10:00:00Z"})
				default:
					sendPlaneJSON(t, w, map[string]any{"id": testProjectID, "name": "Example project", "identifier": "EX"})
				}
			}))
			defer server.Close()
			c := statusConfig(t, server.URL)
			session := statusSession(t, c)
			fence := errors.New("intent changed")
			_, err := session.WriteStatus(context.Background(), c, testItemID, "closed", func() error {
				if reject {
					return fence
				}
				return nil
			})
			require.Error(t, err)
			require.NotContains(t, err.Error(), "private response")
			if reject {
				require.ErrorIs(t, err, fence)
				require.Equal(t, 0, patches)
			} else {
				var statusErr *issuesync.StatusError
				require.ErrorAs(t, err, &statusErr)
				require.True(t, statusErr.Ambiguous)
				require.Equal(t, 1, patches)
			}
		})
	}
}

func TestPlaneStatusRejectsUnavailableAndWrongIdentity(t *testing.T) {
	for _, kind := range []string{"wrong-item", "wrong-project", "archived", "deleted", "null-state", "unknown-state", "permission"} {
		t.Run(kind, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.URL.Path[len(r.URL.Path)-7:] == "states/":
					sendPlaneJSON(t, w, wirePage(workflowRows(), false, ""))
				case r.URL.Path[len(r.URL.Path)-len(testItemID)-1:] == testItemID+"/":
					row := map[string]any{"id": testItemID, "project": testProjectID, "state": testStateID, "updated_at": "2026-09-29T10:00:00Z"}
					switch kind {
					case "wrong-item":
						row["id"] = testUserID
					case "wrong-project":
						row["project"] = testUserID
					case "archived":
						row["archived_at"] = "2026-09-29T11:00:00Z"
					case "deleted":
						row["deleted_at"] = "2026-09-29T11:00:00Z"
					case "null-state":
						row["state"] = nil
					case "unknown-state":
						row["state"] = testUserID
					case "permission":
						w.WriteHeader(403)
						return
					}
					sendPlaneJSON(t, w, row)
				default:
					sendPlaneJSON(t, w, map[string]any{"id": testProjectID, "name": "Example project", "identifier": "EX"})
				}
			}))
			defer server.Close()
			c := statusConfig(t, server.URL)
			_, err := statusSession(t, c).ReadStatus(context.Background(), c, testItemID)
			require.Error(t, err)
			var statusErr *issuesync.StatusError
			require.ErrorAs(t, err, &statusErr)
			require.True(t, statusErr.Blocked)
		})
	}
}

type brokenStatusBody struct{}

func (brokenStatusBody) Read([]byte) (int, error) {
	return 0, errors.New("private transport diagnostics")
}
func (brokenStatusBody) Close() error { return nil }
func TestPlaneStatusReadBodyFailureIsTransient(t *testing.T) {
	client := planeTestClient("https://api.plane.so")
	client.http.Transport = planeRoundTrip(func(_ *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: brokenStatusBody{}, Header: http.Header{}}, nil
	})
	c := statusConfig(t, "https://api.plane.so")
	session, err := client.ForRun(t.Context(), c)
	require.NoError(t, err)
	_, err = session.(testStatusSession).ReadStatus(t.Context(), c, testItemID)
	var classified *issuesync.StatusError
	require.ErrorAs(t, err, &classified)
	require.False(t, classified.Blocked)
	require.NotContains(t, err.Error(), "private transport diagnostics")
}

func TestPlaneStatusWriteThatNeverConnectsIsNotAmbiguous(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path[len(r.URL.Path)-7:] == "states/":
			sendPlaneJSON(t, w, wirePage(workflowRows(), false, ""))
		case r.URL.Path[len(r.URL.Path)-len(testItemID)-1:] == testItemID+"/":
			sendPlaneJSON(t, w, map[string]any{"id": testItemID, "project": testProjectID, "state": testStateID, "updated_at": "2026-09-29T10:00:00Z"})
		default:
			sendPlaneJSON(t, w, map[string]any{"id": testProjectID, "name": "Example project", "identifier": "EX"})
		}
	}))
	defer server.Close()
	c := statusConfig(t, server.URL)
	client := NewClient(ClientConfig{Daemon: config.PlaneSyncConfig{APIOrigin: server.URL}, LookupEnv: func(string) (string, bool) { return "example-api-key", true }, Wait: func(ctx context.Context, _ time.Duration) error { return ctx.Err() }, Transport: patchDialFailure{reads: server.Client().Transport}})
	raw, err := client.ForRun(context.Background(), c)
	require.NoError(t, err)
	_, err = raw.(testStatusSession).WriteStatus(context.Background(), c, testItemID, "closed", func() error { return nil })
	var statusErr *issuesync.StatusError
	require.ErrorAs(t, err, &statusErr)
	require.False(t, statusErr.Ambiguous, "a write that never connected was not sent")
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
