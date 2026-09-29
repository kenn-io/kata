package notionsync

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func propertyWire(id, kind string, values []any, more bool, cursor any) map[string]any {
	items := make([]any, 0, len(values))
	for _, v := range values {
		items = append(items, map[string]any{"object": "property_item", "id": id, "type": kind, kind: v})
	}
	return map[string]any{"object": "list", "type": "property_item", "results": items, "has_more": more, "next_cursor": cursor, "property_item": map[string]any{"id": id, "type": kind, kind: map[string]any{}, "next_url": "https://attacker.example/ignored"}}
}
func markdownWire(text string) map[string]any {
	return map[string]any{"object": "page_markdown", "id": testPageID, "markdown": text, "truncated": false, "unknown_block_ids": []any{}}
}
func contentSession(t *testing.T, override func(*http.Request) (*http.Response, bool)) (Session, Page) {
	t.Helper()
	c, _ := testClient(t, func(r *http.Request) (*http.Response, error) {
		require.Equal(t, "https://api.notion.com", r.URL.Scheme+"://"+r.URL.Host)
		if res, ok := override(r); ok {
			return res, nil
		}
		switch r.URL.EscapedPath() {
		case "/v1/data_sources/" + sourceID + "/query":
			return jsonResponse(t, queryWire([]any{pageWire()}, false, nil)), nil
		case "/v1/pages/" + testPageID + "/properties/title":
			return jsonResponse(t, propertyWire("title", "title", []any{map[string]any{"plain_text": "Complete title"}}, false, nil)), nil
		case "/v1/pages/" + testPageID + "/properties/p%2F1":
			return jsonResponse(t, propertyWire("p%2F1", "people", nil, false, nil)), nil
		case "/v1/pages/" + testPageID + "/markdown":
			return jsonResponse(t, markdownWire("Body")), nil
		case "/v1/pages/" + testPageID:
			return jsonResponse(t, pageWire()), nil
		default:
			t.Errorf("unexpected outbound path %s", r.URL.EscapedPath())
			return response(400, `{}`), nil
		}
	})
	s := session(t, c)
	pages, e := s.Pages(t.Context(), configFixture(t), nil)
	require.NoError(t, e)
	require.Len(t, pages, 1)
	return s, pages[0]
}

func TestContentCompleteAndStable(t *testing.T) {
	t.Run("all people pages and partial user", func(t *testing.T) {
		calls := 0
		s, p := contentSession(t, func(r *http.Request) (*http.Response, bool) {
			if strings.HasSuffix(r.URL.EscapedPath(), "/properties/p%2F1") {
				calls++
				var values []any
				if calls%2 == 1 {
					for range 25 {
						values = append(values, map[string]any{"object": "group", "id": databaseID})
					}
					return jsonResponse(t, propertyWire("p%2F1", "people", values, true, "opaque/+?")), true
				}
				require.Equal(t, "opaque/+?", r.URL.Query().Get("start_cursor"))
				return jsonResponse(t, propertyWire("p%2F1", "people", []any{map[string]any{"object": "user", "id": testUserID}}, false, nil)), true
			}
			return nil, false
		})
		got, e := s.Content(t.Context(), configFixture(t), p)
		require.NoError(t, e)
		require.Equal(t, "Complete title", got.Title)
		require.Equal(t, "Body", got.Markdown)
		require.NotNil(t, got.OwnerID)
		require.Equal(t, testUserID, *got.OwnerID)
		require.Equal(t, 4, calls)
	})
	t.Run("empty title", func(t *testing.T) {
		s, p := contentSession(t, func(r *http.Request) (*http.Response, bool) {
			if strings.HasSuffix(r.URL.Path, "/properties/title") {
				return jsonResponse(t, propertyWire("title", "title", nil, false, nil)), true
			}
			return nil, false
		})
		got, e := s.Content(t.Context(), configFixture(t), p)
		require.NoError(t, e)
		require.Empty(t, got.Title)
		require.Nil(t, got.OwnerID)
	})
	for _, changes := range []int{1, 2} {
		t.Run(fmt.Sprintf("%d edits", changes), func(t *testing.T) {
			reads, bodies := 0, 0
			s, p := contentSession(t, func(r *http.Request) (*http.Response, bool) {
				if strings.HasSuffix(r.URL.Path, "/markdown") {
					bodies++
					return jsonResponse(t, markdownWire(fmt.Sprint(bodies))), true
				}
				if r.URL.Path == "/v1/pages/"+testPageID {
					reads++
					row := pageWire()
					version := min((reads+1)/2, changes)
					row["last_edited_time"] = fmt.Sprintf("2026-01-0%dT00:00:00Z", version+2)
					return jsonResponse(t, row), true
				}
				return nil, false
			})
			got, e := s.Content(t.Context(), configFixture(t), p)
			require.Equal(t, 4, reads)
			require.Equal(t, 2, bodies)
			if changes == 1 {
				require.NoError(t, e)
				require.Equal(t, "2", got.Markdown)
				require.Equal(t, 3, got.Page.UpdatedAt.Day())
			} else {
				require.ErrorContains(t, e, "changed")
			}
		})
	}
	for _, marker := range []string{"is_archived", "in_trash", "unavailable", "moved", "wrong ID", "status"} {
		t.Run(marker, func(t *testing.T) {
			s, p := contentSession(t, func(r *http.Request) (*http.Response, bool) {
				if r.URL.Path == "/v1/pages/"+testPageID {
					if marker == "unavailable" {
						return response(404, `{"code":"object_not_found"}`), true
					}
					row := pageWire()
					switch marker {
					case "moved":
						row["parent"] = map[string]any{"type": "page_id", "page_id": databaseID}
					case "wrong ID":
						row["id"] = databaseID
					case "status":
						row["properties"].(map[string]any)["Workflow"].(map[string]any)["status"] = map[string]any{"id": "complete-b"}
					default:
						row[marker] = true
					}
					return jsonResponse(t, row), true
				}
				return nil, false
			})
			got, e := s.Content(t.Context(), configFixture(t), p)
			if marker == "status" {
				require.NoError(t, e)
				require.Equal(t, "complete-b", *got.Page.StatusID)
			} else {
				require.Error(t, e)
			}
		})
	}
}

func TestContentRetriesWhenSelectedPropertiesChangeDuringRead(t *testing.T) {
	const otherUserID = "44444444-4444-4444-8444-444444444444"
	for _, changed := range []string{"title", "people"} {
		t.Run(changed, func(t *testing.T) {
			titleReads, peopleReads, markdownReads := 0, 0, 0
			s, p := contentSession(t, func(r *http.Request) (*http.Response, bool) {
				switch r.URL.EscapedPath() {
				case "/v1/pages/" + testPageID + "/properties/title":
					titleReads++
					title := "Current title"
					if changed == "title" && titleReads == 1 {
						title = "Stale title"
					}
					return jsonResponse(t, propertyWire("title", "title", []any{map[string]any{"plain_text": title}}, false, nil)), true
				case "/v1/pages/" + testPageID + "/properties/p%2F1":
					peopleReads++
					ownerID := testUserID
					if changed == "people" && peopleReads == 1 {
						ownerID = otherUserID
					}
					person := map[string]any{"object": "user", "id": ownerID, "type": "person"}
					return jsonResponse(t, propertyWire("p%2F1", "people", []any{person}, false, nil)), true
				case "/v1/pages/" + testPageID + "/markdown":
					markdownReads++
					return jsonResponse(t, markdownWire(fmt.Sprintf("Body %d", markdownReads))), true
				}
				return nil, false
			})

			got, err := s.Content(t.Context(), configFixture(t), p)
			require.NoError(t, err)
			require.Equal(t, "Current title", got.Title)
			require.Equal(t, fmt.Sprintf("Body %d", markdownReads), got.Markdown)
			require.NotNil(t, got.OwnerID)
			require.Equal(t, testUserID, *got.OwnerID)
			require.Equal(t, 4, titleReads)
			require.Equal(t, 4, peopleReads)
			require.Equal(t, 2, markdownReads)
		})
	}
}

func TestContentRejectsUnstableSelectedProperties(t *testing.T) {
	titleReads, peopleReads := 0, 0
	s, p := contentSession(t, func(r *http.Request) (*http.Response, bool) {
		switch r.URL.EscapedPath() {
		case "/v1/pages/" + testPageID + "/properties/title":
			titleReads++
			title := "Before"
			if titleReads%2 == 0 {
				title = "After"
			}
			return jsonResponse(t, propertyWire("title", "title", []any{map[string]any{"plain_text": title}}, false, nil)), true
		case "/v1/pages/" + testPageID + "/properties/p%2F1":
			peopleReads++
			person := map[string]any{"object": "user", "id": testUserID, "type": "person"}
			return jsonResponse(t, propertyWire("p%2F1", "people", []any{person}, false, nil)), true
		}
		return nil, false
	})

	_, err := s.Content(t.Context(), configFixture(t), p)
	require.ErrorContains(t, err, "changed")
	require.Equal(t, 4, titleReads)
	require.Equal(t, 4, peopleReads)
}

func TestContentRetriesWhenPageChangesDuringPropertyVerification(t *testing.T) {
	titleReads, peopleReads, markdownReads, pageReads := 0, 0, 0, 0
	metadataVersion := 1
	s, p := contentSession(t, func(r *http.Request) (*http.Response, bool) {
		switch r.URL.EscapedPath() {
		case "/v1/pages/" + testPageID + "/properties/title":
			titleReads++
			return jsonResponse(t, propertyWire("title", "title", []any{map[string]any{"plain_text": "Stable title"}}, false, nil)), true
		case "/v1/pages/" + testPageID + "/properties/p%2F1":
			peopleReads++
			if peopleReads == 2 {
				metadataVersion = 2
			}
			person := map[string]any{"object": "user", "id": testUserID, "type": "person"}
			return jsonResponse(t, propertyWire("p%2F1", "people", []any{person}, false, nil)), true
		case "/v1/pages/" + testPageID + "/markdown":
			markdownReads++
			return jsonResponse(t, markdownWire(fmt.Sprintf("Body %d", markdownReads))), true
		case "/v1/pages/" + testPageID:
			pageReads++
			row := pageWire()
			if metadataVersion == 2 {
				row["last_edited_time"] = "2026-01-03T00:00:00Z"
			}
			return jsonResponse(t, row), true
		}
		return nil, false
	})

	got, err := s.Content(t.Context(), configFixture(t), p)
	require.NoError(t, err)
	require.Equal(t, "Body 2", got.Markdown)
	require.Equal(t, 4, titleReads)
	require.Equal(t, 4, peopleReads)
	require.Equal(t, 4, pageReads)
	require.Equal(t, 3, got.Page.UpdatedAt.Day())
}

func TestContentRejectsDatabaseParentChange(t *testing.T) {
	s, p := contentSession(t, func(r *http.Request) (*http.Response, bool) {
		if r.URL.Path != "/v1/pages/"+testPageID {
			return nil, false
		}
		row := pageWire()
		row["parent"].(map[string]any)["database_id"] = otherDatabaseID
		return jsonResponse(t, row), true
	})
	_, err := s.Content(t.Context(), configFixture(t), p)
	require.ErrorContains(t, err, "unavailable or moved")
}

func TestContentLimits(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mutate  func(map[string]any)
		wantErr bool
	}{
		{"exact markdown", func(m map[string]any) { m["markdown"] = strings.Repeat("x", 1<<20) }, false},
		{"over markdown", func(m map[string]any) { m["markdown"] = strings.Repeat("x", (1<<20)+1) }, true},
		{"truncated", func(m map[string]any) { m["truncated"] = true }, true},
		{"unknown blocks", func(m map[string]any) { m["unknown_block_ids"] = []string{databaseID} }, true},
		{"supported placeholders", func(m map[string]any) {
			m["markdown"] = `<unknown url="https://attacker.example/subtree"/> ![file](https://attacker.example/file)`
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, p := contentSession(t, func(r *http.Request) (*http.Response, bool) {
				if strings.HasSuffix(r.URL.Path, "/markdown") {
					m := markdownWire("Body")
					tc.mutate(m)
					return jsonResponse(t, m), true
				}
				return nil, false
			})
			_, e := s.Content(t.Context(), configFixture(t), p)
			if tc.wantErr {
				require.Error(t, e)
			} else {
				require.NoError(t, e)
			}
		})
	}
	for _, size := range []int{8 << 20, (8 << 20) + 1} {
		t.Run(fmt.Sprintf("response %d", size), func(t *testing.T) {
			raw := `{"object":"database","id":"` + databaseID + `","data_sources":[]}`
			raw += strings.Repeat(" ", size-len(raw))
			c, _ := testClient(t, func(*http.Request) (*http.Response, error) { return response(200, raw), nil })
			_, e := session(t, c).Database(t.Context(), databaseID)
			if size == 8<<20 {
				require.NoError(t, e)
			} else {
				require.ErrorContains(t, e, "8 MiB")
			}
		})
	}
	for _, tc := range []struct {
		name          string
		pages, values int
		wantErr       bool
	}{{"exact property", 100, 100, false}, {"over pages", 101, 0, true}, {"over values", 100, 101, true}} {
		t.Run(tc.name, func(t *testing.T) {
			n := 0
			s, p := contentSession(t, func(r *http.Request) (*http.Response, bool) {
				if strings.HasSuffix(r.URL.Path, "/properties/title") {
					n++
					page := 1
					if cursor := r.URL.Query().Get("start_cursor"); cursor != "" {
						var err error
						page, err = strconv.Atoi(cursor)
						require.NoError(t, err)
					}
					values := make([]any, tc.values)
					for i := range values {
						values[i] = map[string]any{"plain_text": "x"}
					}
					var cursor any
					more := page < tc.pages
					if more {
						cursor = fmt.Sprint(page + 1)
					}
					return jsonResponse(t, propertyWire("title", "title", values, more, cursor)), true
				}
				return nil, false
			})
			got, e := s.Content(t.Context(), configFixture(t), p)
			if tc.wantErr {
				require.Error(t, e)
				require.LessOrEqual(t, n, 100)
			} else {
				require.NoError(t, e)
				require.Len(t, got.Title, 10000)
			}
		})
	}
	for _, kind := range []string{"repeated", "missing", "wrong property"} {
		t.Run(kind, func(t *testing.T) {
			s, p := contentSession(t, func(r *http.Request) (*http.Response, bool) {
				if strings.HasSuffix(r.URL.Path, "/properties/title") {
					var cursor any = "same"
					id := "title"
					if kind == "missing" {
						cursor = nil
					}
					if kind == "wrong property" {
						id = "other"
					}
					return jsonResponse(t, propertyWire(id, "title", nil, true, cursor)), true
				}
				return nil, false
			})
			_, e := s.Content(t.Context(), configFixture(t), p)
			require.Error(t, e)
		})
	}
}

func TestContentPreservesEncodedPropertyID(t *testing.T) {
	cfg := configFixture(t)
	cfg.TitlePropertyID = "t%3A1"
	calls := 0
	s, p := contentSession(t, func(r *http.Request) (*http.Response, bool) {
		if strings.Contains(r.URL.Path, "/properties/t:") {
			calls++
			require.Equal(t, "/v1/pages/"+testPageID+"/properties/t%3A1", r.URL.EscapedPath())
			return jsonResponse(t, propertyWire("t%3A1", "title", nil, false, nil)), true
		}
		return nil, false
	})
	_, e := s.Content(t.Context(), cfg, p)
	require.NoError(t, e)
	require.Equal(t, 2, calls)
}

func TestContentRejectsIncompleteEnvelope(t *testing.T) {
	for _, field := range []string{"results", "plain_text", "unknown_block_ids"} {
		t.Run(field, func(t *testing.T) {
			s, p := contentSession(t, func(r *http.Request) (*http.Response, bool) {
				if field == "unknown_block_ids" && strings.HasSuffix(r.URL.Path, "/markdown") {
					wire := markdownWire("Body")
					delete(wire, field)
					return jsonResponse(t, wire), true
				}
				if field != "unknown_block_ids" && strings.HasSuffix(r.URL.Path, "/properties/title") {
					wire := propertyWire("title", "title", []any{map[string]any{}}, false, nil)
					if field == "results" {
						delete(wire, field)
					}
					return jsonResponse(t, wire), true
				}
				return nil, false
			})
			_, e := s.Content(t.Context(), configFixture(t), p)
			require.Error(t, e)
		})
	}
}

func TestContentSelectsFirstUserRegardlessOfSubtype(t *testing.T) {
	const botID = "44444444-4444-4444-8444-444444444444"
	for _, tc := range []struct {
		name   string
		values []any
	}{
		{"bot only", []any{map[string]any{"object": "user", "id": botID, "type": "bot", "bot": map[string]any{}}}},
		{"group then bot then person", []any{map[string]any{"object": "group", "id": databaseID}, map[string]any{"object": "user", "id": botID, "type": "bot", "bot": map[string]any{}}, map[string]any{"object": "user", "id": testUserID, "type": "person", "person": map[string]any{}}}},
		{"partial first user", []any{map[string]any{"object": "group", "id": databaseID}, map[string]any{"object": "user", "id": botID}, map[string]any{"object": "user", "id": testUserID, "type": "person", "person": map[string]any{}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, p := contentSession(t, func(r *http.Request) (*http.Response, bool) {
				if strings.HasSuffix(r.URL.EscapedPath(), "/properties/p%2F1") {
					return jsonResponse(t, propertyWire("p%2F1", "people", tc.values, false, nil)), true
				}
				return nil, false
			})
			got, err := s.Content(t.Context(), configFixture(t), p)
			require.NoError(t, err)
			require.NotNil(t, got.OwnerID)
			require.Equal(t, botID, *got.OwnerID)
		})
	}
}
