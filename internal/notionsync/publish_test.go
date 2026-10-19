package notionsync

import (
	"context"
	"encoding/json/v2"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/issuesync"
)

func openPublishNotion(t *testing.T, c *Client, legacy bool) issuesync.PublishSession {
	t.Helper()
	cfg, err := ResolveConfig(groupSchema(), Selectors{}, "")
	require.NoError(t, err)
	if legacy {
		cfg = configFixture(t)
	}
	raw, err := EncodeConfig(cfg)
	require.NoError(t, err)
	session, err := NewAdapter(nil, c).OpenPublish(t.Context(), db.IssueSyncBinding{Provider: "notion", SourceKey: "notion:" + sourceID, RemoteID: sourceID, Config: raw})
	require.NoError(t, err)
	return session
}
func TestPublishNotionTitleMarkdownAndRecovery(t *testing.T) {
	for _, body := range []string{"", "# Title\n\n世界 **body**"} {
		t.Run(body, func(t *testing.T) {
			creates, admissions := 0, 0
			c, _ := testClient(t, func(r *http.Request) (*http.Response, error) {
				require.Equal(t, "api.notion.com", r.URL.Host)
				require.Equal(t, "Bearer test-secret", r.Header.Get("Authorization"))
				switch r.URL.Path {
				case "/v1/data_sources/" + sourceID:
					return jsonResponse(t, statusSchemaWire()), nil
				case "/v1/databases/" + databaseID:
					return jsonResponse(t, databaseWire()), nil
				case "/v1/pages":
					require.Equal(t, http.MethodPost, r.Method)
					creates++
					require.Equal(t, 1, admissions)
					var payload map[string]any
					require.NoError(t, json.UnmarshalRead(r.Body, &payload))
					require.Equal(t, map[string]any{"parent": map[string]any{"data_source_id": sourceID}, "properties": map[string]any{"title": map[string]any{"title": []any{map[string]any{"type": "text", "text": map[string]any{"content": "Native 🐟"}}}}, "s%3A1": map[string]any{"status": map[string]any{"id": "ready"}}}, "markdown": body}, payload)
					return jsonResponse(t, statusPageWire(new("ready"))), nil
				case "/v1/pages/" + testPageID:
					return jsonResponse(t, statusPageWire(new("ready"))), nil
				default:
					t.Errorf("unexpected path %s", r.URL.Path)
					return response(404, `{}`), nil
				}
			})
			s := openPublishNotion(t, c, false)
			payload, err := s.PrepareCreate(t.Context(), db.Issue{Title: "Native 🐟", Body: body, Status: "open"})
			require.NoError(t, err)
			remote, err := s.Create(t.Context(), payload, func(context.Context) error { admissions++; return nil })
			require.NoError(t, err)
			require.Equal(t, "page:"+testPageID, remote.ExternalID)
			require.Equal(t, "open", remote.Status)
			adopted, err := s.ReadCreated(t.Context(), db.IssuePublicationRemote{ExternalID: testPageID})
			require.NoError(t, err)
			require.Equal(t, remote.ExternalID, adopted.ExternalID)
			_, err = s.ReadCreated(t.Context(), remote)
			require.NoError(t, err)
			require.Equal(t, 1, creates)
		})
	}
}
func TestPublishNotionOneSendAndSourceFences(t *testing.T) {
	for _, scenario := range []string{"drop", "503", "malformed", "async", "redirect", "wrong_parent", "incomplete", "denied", "title_removed", "legacy", "admission", "oversize"} {
		t.Run(scenario, func(t *testing.T) {
			creates, admissions := 0, 0
			c, _ := testClient(t, func(r *http.Request) (*http.Response, error) {
				switch r.URL.Path {
				case "/v1/data_sources/" + sourceID:
					if scenario == "denied" {
						return response(403, `{}`), nil
					}
					wire := statusSchemaWire()
					if scenario == "title_removed" {
						delete(wire["properties"].(map[string]any), "Task description")
					}
					return jsonResponse(t, wire), nil
				case "/v1/databases/" + databaseID:
					return jsonResponse(t, databaseWire()), nil
				case "/v1/pages":
					creates++
					require.Equal(t, 1, admissions)
					switch scenario {
					case "drop":
						return nil, errors.New("connection lost")
					case "503":
						return response(503, `{"message":"secret"}`), nil
					case "malformed":
						return response(200, `{"id":`), nil
					case "async":
						return jsonResponse(t, map[string]any{"object": "async_task", "id": "task"}), nil
					case "redirect":
						res := response(307, `{}`)
						res.Header.Set("Location", "https://untrusted.example")
						return res, nil
					}
					wire := statusPageWire(new("ready"))
					if scenario == "wrong_parent" {
						wire["parent"].(map[string]any)["data_source_id"] = "99999999-9999-4999-8999-999999999999"
					}
					if scenario == "incomplete" {
						delete(wire, "last_edited_time")
					}
					return jsonResponse(t, wire), nil
				default:
					t.Errorf("unexpected path %s", r.URL.String())
					return response(404, `{}`), nil
				}
			})
			s := openPublishNotion(t, c, scenario == "legacy")
			body := "Body"
			if scenario == "oversize" {
				body = strings.Repeat("界", 200000)
			}
			payload, err := s.PrepareCreate(t.Context(), db.Issue{Title: "Native", Body: body, Status: "open"})
			if scenario == "denied" || scenario == "title_removed" || scenario == "legacy" || scenario == "oversize" {
				require.Error(t, err)
				require.Zero(t, creates)
				return
			}
			require.NoError(t, err)
			remote, err := s.Create(t.Context(), payload, func(context.Context) error {
				if scenario == "admission" {
					return errors.New("denied")
				}
				admissions++
				return nil
			})
			require.Error(t, err)
			if scenario == "admission" {
				require.Zero(t, creates)
			} else {
				require.Equal(t, 1, creates)
			}
			if scenario == "incomplete" {
				require.Equal(t, "page:"+testPageID, remote.ExternalID)
			}
		})
	}
}

func TestPublishNotionLongTitlePreservesText(t *testing.T) {
	title := strings.Repeat("界", 2001)
	c, _ := testClient(t, func(r *http.Request) (*http.Response, error) {
		if strings.HasPrefix(r.URL.Path, "/v1/data_sources/") {
			return jsonResponse(t, statusSchemaWire()), nil
		}
		return jsonResponse(t, databaseWire()), nil
	})
	s := openPublishNotion(t, c, false)
	payload, err := s.PrepareCreate(t.Context(), db.Issue{Title: title, Status: "open"})
	require.NoError(t, err)
	var wire struct {
		Properties map[string]struct {
			Title []struct {
				Text struct {
					Content string `json:"content"`
				} `json:"text"`
			} `json:"title"`
		} `json:"properties"`
	}
	require.NoError(t, json.Unmarshal(payload, &wire))
	var joined string
	for _, part := range wire.Properties["title"].Title {
		joined += part.Text.Content
	}
	require.Equal(t, title, joined)
	require.Len(t, wire.Properties["title"].Title, 2)
}
