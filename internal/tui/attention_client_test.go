package tui

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/notification"
)

func TestAttentionClientList(t *testing.T) {
	for _, all := range []bool{false, true} {
		t.Run(fmt.Sprint(all), func(t *testing.T) {
			key := notification.MetadataKey("alex/reviewer")
			c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				path := "/api/v1/projects/7/issues"
				if all {
					path = "/api/v1/issues"
				}
				require.Equal(t, path, r.URL.Path)
				require.Equal(t, "open", r.URL.Query().Get("status"))
				require.Equal(t, "0", r.URL.Query().Get("limit"))
				require.Equal(t, []string{key}, r.URL.Query()["meta"])
				rows := make([]map[string]any, 0, 205)
				for i := 200; i >= 0; i-- {
					rows = append(rows, map[string]any{"uid": fmt.Sprint(i), "short_id": fmt.Sprintf("%04d", i), "qualified_id": fmt.Sprintf("example-project#%04d", i), "status": "open", "project_id": 7, "metadata": map[string]any{key: map[string]any{"from": "sam", "message": "Review this"}}})
				}
				rows = append(rows, map[string]any{"status": "closed", "metadata": map[string]any{key: map[string]any{"from": "sam", "message": "Closed"}}}, map[string]any{"status": "open", "metadata": map[string]any{notification.MetadataKey("alex"): map[string]any{"from": "sam", "message": "Other recipient"}}})
				respondJSON(t, w, map[string]any{"issues": rows})
			})
			pid := new(int64(7))
			if all {
				pid = nil
			}
			rows, skipped, err := c.ListAttentionRequests(context.Background(), pid, "alex/reviewer")
			require.NoError(t, err)
			require.Zero(t, skipped)
			require.Len(t, rows, 201)
			require.Equal(t, "0000", rows[0].issue.ShortID)
			require.Equal(t, "0200", rows[200].issue.ShortID)
		})
	}
}

func TestAttentionClientDecode(t *testing.T) {
	key := notification.MetadataKey("alex")
	values := []string{`{"from":"sam","message":"Review","teammate":"worker"}`, `{"from":"sam","message":"Review","teammate":42}`, `{"from":"system","message":"Deadline reached: 2026-10-17"}`, `{"from":"system","teammate":"cron","message":"Review","extra":true}`, `{"from":"sam","message":" "}`, `{"from":2,"message":"Review"}`}
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		rows := []map[string]any{}
		for i, v := range values {
			work := []string{"needs-human", "stuck", "ok", "unknown", "", ""}[i]
			rows = append(rows, map[string]any{"short_id": fmt.Sprint(i), "status": "open", "metadata": map[string]any{key: jsontext.Value(v), "work.attention": work}})
		}
		respondJSON(t, w, map[string]any{"issues": rows})
	})
	rows, skipped, err := c.ListAttentionRequests(context.Background(), nil, "alex")
	require.NoError(t, err)
	require.Equal(t, 2, skipped)
	require.Len(t, rows, 4)
	require.Equal(t, "worker", rows[0].teammate)
	require.Empty(t, rows[1].teammate)
	require.Equal(t, "needs-human", rows[0].workAttention)
	require.Equal(t, "stuck", rows[1].workAttention)
	require.Equal(t, "ok", rows[2].workAttention)
	require.Empty(t, rows[3].workAttention)
	require.Equal(t, "cron", rows[3].teammate)
	require.JSONEq(t, values[3], string(rows[3].raw))
}

func TestAttentionClientNotificationContext(t *testing.T) {
	for _, tc := range []struct {
		name, fields string
		want         []string
	}{
		{"comment request", `"re":"01ARZ3NDEKTSV4RRFFQ69G5FAV"`, []string{"re 01ARZ3NDEKTSV4RRFFQ69G5FAV", "Inspect evidence"}},
		{"reply", `"re":"01ARZ3NDEKTSV4RRFFQ69G5FAV","kind":"refute"`, []string{"refute", "01ARZ3NDEKTSV4RRFFQ69G5FAV", "Inspect evidence"}},
		{"broadcast", `"broadcast":true,"re":"01ARZ3NDEKTSV4RRFFQ69G5FAV"`, []string{"broadcast", "re 01ARZ3NDEKTSV4RRFFQ69G5FAV", "Inspect evidence"}},
		{"malformed optional fields", `"re":42,"kind":false,"broadcast":"yes","teammate":42`, []string{"Inspect evidence", "sam"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, _ := attentionFixture(t)
			raw := jsontext.Value(`{"from":"sam","message":"Inspect evidence",` + tc.fields + `}`)
			m.api = newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				require.Equal(t, "/api/v1/projects/7/issues", r.URL.Path)
				respondJSON(t, w, map[string]any{"issues": []any{map[string]any{
					"uid": "01ARZ3NDEKTSV4RRFFQ69G5FAA", "short_id": "5faa", "project_id": 7,
					"status": "open", "title": "Decide on the release",
					"metadata": map[string]any{notification.MetadataKey("alex"): raw},
				}}})
			})
			m, cmd := m.transitionToAttention()
			m = loadAttention(m, cmd)
			require.NoError(t, m.attention.err)
			require.Len(t, m.attention.rows, 1)
			for _, want := range tc.want {
				require.Contains(t, stripANSI(m.View().Content), want)
			}
			require.Equal(t, string(raw), string(m.attention.rows[0].raw), "clearing must guard the complete original notification")
		})
	}
}

func TestAttentionClientClear(t *testing.T) {
	for _, version := range []string{"0.12.0", "", "bad", "0.13.0", "0.28.0", "failed"} {
		t.Run(version, func(t *testing.T) {
			posts := 0
			key := notification.MetadataKey("alex/reviewer")
			raw := jsontext.Value(`{ "from":"sam", "message":"Review", "extra":true }`)
			c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/health" {
					if version == "failed" {
						w.WriteHeader(503)
						return
					}
					respondJSON(t, w, map[string]any{"api_schema_version": version})
					return
				}
				posts++
				require.Equal(t, "POST", r.Method)
				require.Equal(t, "/api/v1/projects/7/issues/issue-uid/metadata", r.URL.Path)
				var body struct {
					Actor string                    `json:"actor"`
					Patch map[string]jsontext.Value `json:"patch"`
					Guard struct {
						Key     string `json:"key"`
						IfValue string `json:"if_value"`
					} `json:"guard"`
				}
				require.NoError(t, json.UnmarshalRead(r.Body, &body))
				require.Equal(t, "writer", body.Actor)
				require.Len(t, body.Patch, 1)
				require.Equal(t, "null", string(body.Patch[key]))
				require.Equal(t, key, body.Guard.Key)
				require.Equal(t, string(raw), body.Guard.IfValue)
				respondJSON(t, w, map[string]any{})
			})
			err := c.ClearAttentionRequest(context.Background(), 7, "issue-uid", key, raw, "writer")
			if version == "0.13.0" || version == "0.28.0" {
				require.NoError(t, err)
				require.Equal(t, 1, posts)
			} else {
				var preflight *mutationPreflightError
				require.ErrorAs(t, err, &preflight)
				require.Contains(t, err.Error(), "0.13.0")
				require.Zero(t, posts)
			}
		})
	}
}

func TestAttentionClientClearConflict(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/health" {
			respondJSON(t, w, map[string]any{"api_schema_version": "0.13.0"})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(409)
		_, _ = fmt.Fprint(w, `{"error":{"code":"metadata_guard_failed","message":"changed"}}`)
	})
	err := c.ClearAttentionRequest(context.Background(), 7, "abc4", "key", jsontext.Value(`{}`), "writer")
	var apiErr *APIError
	require.ErrorAs(t, err, &apiErr)
	require.Equal(t, 409, apiErr.Status)
	require.Equal(t, "metadata_guard_failed", apiErr.Code)
}

func TestAttentionUndoClientForwarding(t *testing.T) {
	var listRequests, clearRequests int
	key := notification.MetadataKey("alex")
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/health" {
			respondJSON(t, w, map[string]any{"api_schema_version": "0.13.0"})
		} else if r.Method == http.MethodGet {
			listRequests++
			require.Equal(t, "/api/v1/issues", r.URL.Path)
			respondJSON(t, w, map[string]any{"issues": []any{map[string]any{
				"uid": "01TEST-abc4", "short_id": "abc4", "status": "open",
				"metadata": map[string]any{key: map[string]any{"from": "sam", "message": "Review"}},
			}}})
		} else {
			clearRequests++
			require.Equal(t, http.MethodPost, r.Method)
			require.Equal(t, "/api/v1/projects/7/issues/abc4/metadata", r.URL.Path)
			respondJSON(t, w, map[string]any{})
		}
	})
	wrapped := newUndoClient(c)
	rows, skipped, err := wrapped.ListAttentionRequests(context.Background(), nil, "alex")
	require.NoError(t, err)
	require.Zero(t, skipped)
	require.Len(t, rows, 1)
	require.Equal(t, "abc4", rows[0].issue.ShortID)
	require.Equal(t, "Review", rows[0].message)
	require.Equal(t, 1, listRequests, "list must reach the wrapped client")
	require.NoError(t, wrapped.ClearAttentionRequest(context.Background(), 7, "abc4", "key", jsontext.Value(`{}`), "writer"))
	require.Equal(t, 1, clearRequests, "clear must reach the wrapped client")
	require.Empty(t, wrapped.pending)
	unavailable := newUndoClient(&struct{ KataAPI }{})
	_, _, err = unavailable.ListAttentionRequests(context.Background(), nil, "alex")
	require.Error(t, err)
	require.Error(t, unavailable.ClearAttentionRequest(context.Background(), 7, "abc4", "key", nil, "writer"))
}

// Exact recipients must stay distinct across arbitrary valid addresses.
func FuzzAttentionClientExactRecipient(f *testing.F) {
	f.Add("alex", "Review")
	f.Add("alex/reviewer", "Check this")
	f.Fuzz(func(t *testing.T, recipient, message string) {
		if !utf8.ValidString(message) {
			return
		}
		recipient, err := notification.NormalizeRecipient(recipient)
		if err != nil {
			return
		}
		key := notification.MetadataKey(recipient)
		other := notification.MetadataKey(recipient + "/other")
		raw, err := json.Marshal(map[string]string{"from": "sam", "message": message})
		require.NoError(t, err)
		c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
			respondJSON(t, w, map[string]any{"issues": []any{map[string]any{"status": "open", "short_id": "abc4", "metadata": map[string]any{key: jsontext.Value(raw)}}, map[string]any{"status": "open", "metadata": map[string]any{other: jsontext.Value(raw)}}}})
		})
		rows, skipped, err := c.ListAttentionRequests(context.Background(), nil, recipient)
		require.NoError(t, err)
		if strings.TrimSpace(message) == "" {
			require.Empty(t, rows)
			require.Equal(t, 1, skipped)
		} else {
			require.Equal(t, key, rows[0].key)
			require.Equal(t, message, rows[0].message)
			require.Len(t, rows, 1)
		}
	})
}
