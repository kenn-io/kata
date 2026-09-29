package notionsync

import (
	"encoding/json/v2"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/issuesync"
)

const testPageID = "22222222-2222-4222-8222-222222222222"
const testUserID = "33333333-3333-4333-8333-333333333333"
const otherDatabaseID = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"

func pageWire() map[string]any {
	return map[string]any{"object": "page", "id": testPageID, "url": "https://www.notion.so/" + testPageID, "parent": map[string]any{"type": "data_source_id", "data_source_id": sourceID, "database_id": databaseID}, "created_time": "2026-01-01T00:00:00.000Z", "last_edited_time": "2026-01-02T00:00:00.001Z", "created_by": map[string]any{"object": "user", "id": testUserID}, "is_archived": false, "in_trash": false, "properties": map[string]any{"Workflow": map[string]any{"id": "s%3A1", "type": "status", "status": map[string]any{"id": "active", "name": "Doing"}}, "Task description": map[string]any{"id": "title", "type": "title", "title": []any{}}, "Responsible": map[string]any{"id": "p%2F1", "type": "people", "people": []any{}}}}
}
func observePageWire(t *testing.T, raw map[string]any, cfg Config) (Page, bool, error) {
	t.Helper()
	encoded, err := json.Marshal(raw)
	require.NoError(t, err)
	var wire wirePage
	require.NoError(t, json.Unmarshal(encoded, &wire))
	return wire.observation(cfg)
}
func queryWire(rows []any, more bool, cursor any) map[string]any {
	if rows == nil {
		rows = []any{}
	}
	return map[string]any{"object": "list", "results": rows, "has_more": more, "next_cursor": cursor, "request_status": map[string]any{"type": "complete"}, "new_additive_field": true}
}
func TestPagesCompleteness(t *testing.T) {
	t.Run("short pages and newest duplicate", func(t *testing.T) {
		calls := 0
		c, _ := testClient(t, func(r *http.Request) (*http.Response, error) {
			calls++
			require.Equal(t, "POST", r.Method)
			require.Equal(t, "/v1/data_sources/"+sourceID+"/query", r.URL.Path)
			var body map[string]any
			require.NoError(t, json.UnmarshalRead(r.Body, &body))
			require.Equal(t, "page", body["result_type"])
			require.Equal(t, false, body["is_archived"])
			require.Equal(t, float64(100), body["page_size"])
			require.Equal(t, []any{map[string]any{"timestamp": "last_edited_time", "direction": "ascending"}}, body["sorts"])
			require.Equal(t, map[string]any{"timestamp": "last_edited_time", "last_edited_time": map[string]any{"on_or_after": "2026-01-02T00:00:00Z"}}, body["filter"])
			row := pageWire()
			if calls == 1 {
				return jsonResponse(t, queryWire([]any{row}, true, "opaque/+?")), nil
			}
			require.Equal(t, "opaque/+?", body["start_cursor"])
			row["last_edited_time"] = "2026-01-03T00:00:00Z"
			return jsonResponse(t, queryWire([]any{row}, false, nil)), nil
		})
		lower := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
		var completed int
		ctx := issuesync.WithProgressReporter(t.Context(), func(phase string, n, total int) {
			require.Equal(t, "pages", phase)
			completed = n
			require.Zero(t, total)
		})
		pages, e := session(t, c).Pages(ctx, configFixture(t), &lower)
		require.NoError(t, e)
		require.Len(t, pages, 1)
		require.Equal(t, "2026-01-03T00:00:00Z", pages[0].UpdatedAt.Format(time.RFC3339))
		require.Equal(t, 1, completed)
		require.Equal(t, 2, calls)
	})
	for _, tc := range []struct {
		name   string
		mutate func(map[string]any)
	}{
		{"missing cursor", func(q map[string]any) { q["has_more"] = true }},
		{"incomplete final", func(q map[string]any) {
			q["request_status"] = map[string]any{"type": "incomplete", "incomplete_reason": "query_result_limit_reached"}
		}},
		{"unknown status", func(q map[string]any) { q["request_status"] = map[string]any{"type": "future"} }},
		{"wrong object", func(q map[string]any) { q["results"].([]any)[0].(map[string]any)["object"] = "data_source" }},
		{"invalid ID", func(q map[string]any) { q["results"].([]any)[0].(map[string]any)["id"] = "bad" }},
		{"invalid parent", func(q map[string]any) {
			q["results"].([]any)[0].(map[string]any)["parent"] = map[string]any{"type": "data_source_id", "data_source_id": "bad"}
		}},
		{"missing parent", func(q map[string]any) { delete(q["results"].([]any)[0].(map[string]any), "parent") }},
		{"bad timestamp", func(q map[string]any) { q["results"].([]any)[0].(map[string]any)["last_edited_time"] = "bad" }},
		{"inverted time", func(q map[string]any) {
			q["results"].([]any)[0].(map[string]any)["last_edited_time"] = "2025-01-01T00:00:00Z"
		}},
		{"conflicting duplicate", func(q map[string]any) {
			row := pageWire()
			row["url"] = "https://www.notion.so/other"
			q["results"] = append(q["results"].([]any), row)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q := queryWire([]any{pageWire()}, false, nil)
			tc.mutate(q)
			c, _ := testClient(t, func(*http.Request) (*http.Response, error) { return jsonResponse(t, q), nil })
			p, e := session(t, c).Pages(t.Context(), configFixture(t), nil)
			require.Error(t, e)
			require.Nil(t, p)
			if tc.name == "incomplete final" {
				require.ErrorContains(t, e, "query_result_limit_reached")
			}
		})
	}
	t.Run("repeated cursor", func(t *testing.T) {
		calls := 0
		c, _ := testClient(t, func(*http.Request) (*http.Response, error) {
			calls++
			return jsonResponse(t, queryWire(nil, true, "repeat")), nil
		})
		p, e := session(t, c).Pages(t.Context(), configFixture(t), nil)
		require.Error(t, e)
		require.Nil(t, p)
		require.Equal(t, 2, calls)
	})
	t.Run("skip unavailable moved and since", func(t *testing.T) {
		var rows []any
		for _, key := range []string{"is_archived", "in_trash"} {
			r := pageWire()
			r[key] = true
			rows = append(rows, r)
		}
		for _, kind := range []string{"data_source_id", "page_id"} {
			r := pageWire()
			r["parent"] = map[string]any{"type": kind, kind: databaseID}
			rows = append(rows, r)
		}
		rows = append(rows, pageWire())
		c, _ := testClient(t, func(*http.Request) (*http.Response, error) { return jsonResponse(t, queryWire(rows, false, nil)), nil })
		cfg := configFixture(t)
		cfg.Since = "2026-01-03"
		p, e := session(t, c).Pages(t.Context(), cfg, nil)
		require.NoError(t, e)
		require.Empty(t, p)
	})
}

func TestPagesSkipDatabaseParentMismatch(t *testing.T) {
	row := pageWire()
	row["parent"].(map[string]any)["database_id"] = otherDatabaseID
	c, _ := testClient(t, func(*http.Request) (*http.Response, error) {
		return jsonResponse(t, queryWire([]any{row}, false, nil)), nil
	})
	pages, err := session(t, c).Pages(t.Context(), configFixture(t), nil)
	require.NoError(t, err)
	require.Empty(t, pages)
}

func TestPagesRejectMissingDatabaseParent(t *testing.T) {
	row := pageWire()
	delete(row["parent"].(map[string]any), "database_id")
	c, _ := testClient(t, func(*http.Request) (*http.Response, error) {
		return jsonResponse(t, queryWire([]any{row}, false, nil)), nil
	})
	pages, err := session(t, c).Pages(t.Context(), configFixture(t), nil)
	require.ErrorContains(t, err, "page database parent")
	require.Nil(t, pages)
}

func TestSamePageIncludesDatabaseParent(t *testing.T) {
	cfg := configFixture(t)
	first, _, err := observePageWire(t, pageWire(), cfg)
	require.NoError(t, err)
	changed := pageWire()
	changed["parent"].(map[string]any)["database_id"] = otherDatabaseID
	second, _, err := observePageWire(t, changed, cfg)
	require.NoError(t, err)
	require.False(t, samePage(first, second))
}

func TestPagesBounds(t *testing.T) {
	t.Run("query pages", func(t *testing.T) {
		n := 0
		c, _ := testClient(t, func(*http.Request) (*http.Response, error) {
			n++
			return jsonResponse(t, queryWire(nil, true, fmt.Sprint(n))), nil
		})
		p, e := session(t, c).Pages(t.Context(), configFixture(t), nil)
		require.ErrorContains(t, e, "1000")
		require.Nil(t, p)
		require.Equal(t, 1000, n)
	})
	for _, count := range []int{10000, 10001} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			n := 0
			c, _ := testClient(t, func(*http.Request) (*http.Response, error) {
				var rows []any
				for range 100 {
					if n == count {
						break
					}
					row := pageWire()
					row["id"] = fmt.Sprintf("%08x-2222-4222-8222-222222222222", n)
					n++
					rows = append(rows, row)
				}
				var cursor any
				if n < count {
					cursor = fmt.Sprint(n)
				}
				return jsonResponse(t, queryWire(rows, n < count, cursor)), nil
			})
			p, e := session(t, c).Pages(t.Context(), configFixture(t), nil)
			if count == 10000 {
				require.NoError(t, e)
				require.Len(t, p, count)
			} else {
				require.ErrorContains(t, e, "10000")
				require.Nil(t, p)
			}
		})
	}
}

func TestPagesRejectMissingResults(t *testing.T) {
	c, _ := testClient(t, func(*http.Request) (*http.Response, error) {
		return response(200, `{"object":"list","has_more":false,"next_cursor":null}`), nil
	})
	pages, e := session(t, c).Pages(t.Context(), configFixture(t), nil)
	require.Error(t, e)
	require.Nil(t, pages)
}

func TestPagesRejectMissingPageFields(t *testing.T) {
	for _, field := range []string{"is_archived", "in_trash", "status"} {
		t.Run(field, func(t *testing.T) {
			row := pageWire()
			if field == "status" {
				delete(row["properties"].(map[string]any)["Workflow"].(map[string]any), field)
			} else {
				delete(row, field)
			}
			c, _ := testClient(t, func(*http.Request) (*http.Response, error) {
				return jsonResponse(t, queryWire([]any{row}, false, nil)), nil
			})
			pages, e := session(t, c).Pages(t.Context(), configFixture(t), nil)
			require.Error(t, e)
			require.Nil(t, pages)
		})
	}
}

func TestPagesRejectMalformedMovedObservation(t *testing.T) {
	row := pageWire()
	row["parent"] = map[string]any{"type": "page_id", "page_id": databaseID}
	row["last_edited_time"] = "invalid"
	c, _ := testClient(t, func(*http.Request) (*http.Response, error) {
		return jsonResponse(t, queryWire([]any{row}, false, nil)), nil
	})
	pages, e := session(t, c).Pages(t.Context(), configFixture(t), nil)
	require.Error(t, e)
	require.Nil(t, pages)
}
