package planesync

import (
	"context"
	"encoding/json/v2"
	"errors"
	"html"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/issuesync"
)

func openPublishPlane(t *testing.T, origin string) issuesync.PublishSession {
	t.Helper()
	cfg := testConfig()
	cfg.APIOrigin = origin
	raw, err := EncodeConfig(cfg)
	require.NoError(t, err)
	session, err := NewAdapter(nil, planeTestClient(origin)).OpenPublish(t.Context(), db.IssueSyncBinding{Provider: "plane", SourceKey: cfg.SourceKey(), RemoteID: cfg.RemoteID(), Config: raw})
	require.NoError(t, err)
	return session
}
func publishPlanePrefix() string {
	return "/api/v1/workspaces/example-workspace/projects/" + testProjectID + "/"
}
func TestPublishPlaneEscapedBodyAndRecovery(t *testing.T) {
	for _, body := range []string{"", "# 世界\n<script>alert('x')</script> & **text**"} {
		t.Run(body, func(t *testing.T) {
			creates, admissions := 0, 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				require.Equal(t, "example-api-key", r.Header.Get("X-API-Key"))
				switch r.URL.Path {
				case publishPlanePrefix():
					sendPlaneJSON(t, w, map[string]any{"id": testProjectID, "name": "Example project", "identifier": "EX"})
				case publishPlanePrefix() + "states/":
					sendPlaneJSON(t, w, wirePage(workflowRows(), false, ""))
				case publishPlanePrefix() + "work-items/":
					require.Equal(t, http.MethodPost, r.Method)
					creates++
					require.Equal(t, 1, admissions)
					var payload map[string]any
					require.NoError(t, json.UnmarshalRead(r.Body, &payload))
					wantHTML := ""
					if body != "" {
						wantHTML = "<pre>" + html.EscapeString(body) + "</pre>"
					}
					require.Equal(t, map[string]any{"name": "Native 🐟", "description_html": wantHTML, "state": unstartedStateID}, payload)
					w.WriteHeader(201)
					sendPlaneJSON(t, w, wireItem())
				case publishPlanePrefix() + "work-items/" + testItemID + "/":
					require.Equal(t, http.MethodGet, r.Method)
					item := wireItem()
					item["state"] = unstartedStateID
					sendPlaneJSON(t, w, item)
				default:
					t.Errorf("unexpected path %s", r.URL.Path)
					w.WriteHeader(404)
				}
			}))
			defer server.Close()
			s := openPublishPlane(t, server.URL)
			payload, err := s.PrepareCreate(t.Context(), db.Issue{Title: "Native 🐟", Body: body, Status: "open"})
			require.NoError(t, err)
			remote, err := s.Create(t.Context(), payload, func(context.Context) error { admissions++; return nil })
			require.NoError(t, err)
			require.Equal(t, "work-item:"+testItemID, remote.ExternalID)
			require.Equal(t, "open", remote.Status)
			_, err = s.ReadCreated(t.Context(), remote)
			require.NoError(t, err)
			_, err = s.ReadCreated(t.Context(), db.IssuePublicationRemote{ExternalID: testItemID})
			require.NoError(t, err)
			require.Equal(t, 1, creates)
		})
	}
}
func TestPublishPlaneOneSendAndPreflight(t *testing.T) {
	for _, scenario := range []string{"drop", "503", "malformed", "redirect", "wrong_project", "read_failure", "archived", "no_open_state", "denied", "long_title", "long_body", "admission"} {
		t.Run(scenario, func(t *testing.T) {
			creates := 0
			readbacks := 0
			foreign := 0
			target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { foreign++ }))
			defer target.Close()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case publishPlanePrefix():
					if scenario == "denied" {
						w.WriteHeader(403)
						return
					}
					wire := map[string]any{"id": testProjectID, "identifier": "EX"}
					if scenario == "archived" {
						wire["archived_at"] = "2026-01-01"
					}
					sendPlaneJSON(t, w, wire)
				case publishPlanePrefix() + "states/":
					rows := workflowRows()
					if scenario == "no_open_state" {
						rows = rows[:3]
					}
					sendPlaneJSON(t, w, wirePage(rows, false, ""))
				case publishPlanePrefix() + "work-items/":
					creates++
					switch scenario {
					case "drop":
						conn, _, err := w.(http.Hijacker).Hijack()
						require.NoError(t, err)
						require.NoError(t, conn.Close())
						return
					case "503":
						w.WriteHeader(503)
						return
					case "malformed":
						w.WriteHeader(201)
						_, err := w.Write([]byte(`{"id":`))
						require.NoError(t, err)
						return
					case "redirect":
						http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
						return
					}
					wire := wireItem()
					if scenario == "wrong_project" {
						wire["project"] = testUserID
					}
					sendPlaneJSON(t, w, wire)
				case publishPlanePrefix() + "work-items/" + testItemID + "/":
					readbacks++
					if scenario == "read_failure" {
						w.WriteHeader(404)
						return
					}
					wire := wireItem()
					wire["project"] = testUserID
					sendPlaneJSON(t, w, wire)
				default:
					t.Errorf("unexpected path %s", r.URL.Path)
					w.WriteHeader(404)
				}
			}))
			defer server.Close()
			s := openPublishPlane(t, server.URL)
			issue := db.Issue{Title: "Native", Status: "open"}
			if scenario == "long_title" {
				issue.Title = strings.Repeat("界", 256)
			}
			if scenario == "long_body" {
				issue.Body = strings.Repeat("&", maxDescriptionBytes/2)
			}
			payload, err := s.PrepareCreate(t.Context(), issue)
			if scenario == "archived" || scenario == "no_open_state" || scenario == "denied" || scenario == "long_title" || scenario == "long_body" {
				require.Error(t, err)
				require.Zero(t, creates)
				return
			}
			require.NoError(t, err)
			remote, err := s.Create(t.Context(), payload, func(context.Context) error {
				if scenario == "admission" {
					return errors.New("denied")
				}
				return nil
			})
			require.Error(t, err)
			if scenario == "admission" {
				require.Zero(t, creates)
			} else {
				require.Equal(t, 1, creates)
			}
			require.Zero(t, foreign)
			if scenario == "read_failure" {
				require.Equal(t, "work-item:"+testItemID, remote.ExternalID)
			}
			if scenario == "wrong_project" {
				require.Empty(t, remote.ExternalID, "a foreign creation response cannot supply a recoverable identity")
				require.Zero(t, readbacks, "reject a foreign creation response before readback")
				_, err = s.ReadCreated(t.Context(), db.IssuePublicationRemote{ExternalID: testItemID})
				require.Error(t, err)
			}
		})
	}
}
