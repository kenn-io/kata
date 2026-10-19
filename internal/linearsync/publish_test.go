package linearsync

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/issuesync"
)

func openPublishLinear(t *testing.T, c *Client, cfg Config) issuesync.PublishSession {
	t.Helper()
	raw, err := EncodeConfig(cfg)
	require.NoError(t, err)
	session, err := NewAdapter(nil, c).OpenPublish(t.Context(), db.IssueSyncBinding{Provider: "linear", SourceKey: cfg.SourceKey(), RemoteID: cfg.RemoteID(), Config: raw})
	require.NoError(t, err)
	return session
}
func publishLinearTransport(t *testing.T, _ Config, create func(map[string]any) (*http.Response, error), read func() map[string]any) roundTrip {
	t.Helper()
	return func(r *http.Request) (*http.Response, error) {
		require.Equal(t, endpoint, r.URL.String())
		require.Equal(t, "example-key", r.Header.Get("Authorization"))
		query, v := requestBody(t, r)
		switch {
		case strings.Contains(query, "KataLinearScope"):
			return dataResponse(map[string]any{"viewer": map[string]any{"organization": map[string]any{"id": workspaceID}}, "team": map[string]any{"id": teamID, "name": "Example team", "organization": map[string]any{"id": workspaceID}, "archivedAt": nil}}), nil
		case strings.Contains(query, "KataLinearProject"):
			return dataResponse(map[string]any{"project": map[string]any{"id": projectID, "name": "Example project", "archivedAt": nil, "trashed": false, "teams": map[string]any{"nodes": []any{map[string]any{"id": teamID}}}}}), nil
		case strings.Contains(query, "KataLinearStates"):
			return dataResponse(map[string]any{"workflowStates": map[string]any{"nodes": []any{map[string]any{"id": stateID, "type": "unstarted", "position": 0, "team": map[string]any{"id": teamID}, "archivedAt": nil}}, "pageInfo": map[string]any{"hasNextPage": false}}}), nil
		case strings.Contains(query, "KataLinearCreate"):
			return create(v)
		case strings.Contains(query, "KataLinearPublished"):
			require.Equal(t, issueID, v["id"])
			return dataResponse(map[string]any{"issue": read()}), nil
		default:
			t.Errorf("unexpected query %s", query)
			return response(400, `{}`), nil
		}
	}
}
func TestPublishLinearCreateAndRead(t *testing.T) {
	for _, withProject := range []bool{false, true} {
		for _, body := range []string{"", "**世界**\nBody"} {
			t.Run(body+map[bool]string{false: "team", true: "project"}[withProject], func(t *testing.T) {
				cfg := testConfig()
				if withProject {
					cfg.ProjectID = projectID
				}
				creates, admissions := 0, 0
				wire := func() map[string]any {
					i := wireIssue()
					if withProject {
						i["project"] = map[string]any{"id": projectID}
					}
					return i
				}
				c := mockClient(t, publishLinearTransport(t, cfg, func(v map[string]any) (*http.Response, error) {
					creates++
					require.Equal(t, 1, admissions)
					input := map[string]any{"teamId": teamID, "title": "Native 🐟", "description": body, "stateId": stateID}
					if withProject {
						input["projectId"] = projectID
					}
					require.Equal(t, map[string]any{"input": input}, v)
					return dataResponse(map[string]any{"issueCreate": map[string]any{"success": true, "issue": wire()}}), nil
				}, wire))
				s := openPublishLinear(t, c, cfg)
				payload, err := s.PrepareCreate(t.Context(), db.Issue{Title: "Native 🐟", Body: body, Status: "open"})
				require.NoError(t, err)
				remote, err := s.Create(t.Context(), payload, func(context.Context) error { admissions++; return nil })
				require.NoError(t, err)
				require.Equal(t, "issue:"+issueID, remote.ExternalID)
				require.Equal(t, "open", remote.Status)
				_, err = s.ReadCreated(t.Context(), remote)
				require.NoError(t, err)
				_, err = s.ReadCreated(t.Context(), db.IssuePublicationRemote{ExternalID: issueID})
				require.NoError(t, err)
				require.Equal(t, 1, creates)
			})
		}
	}
}
func TestPublishLinearOneSendAndIdentityFences(t *testing.T) {
	for _, scenario := range []string{"drop", "503", "false", "errors", "malformed", "redirect", "wrong_team", "wrong_project", "incomplete", "admission"} {
		t.Run(scenario, func(t *testing.T) {
			creates := 0
			cfg := testConfig()
			cfg.ProjectID = projectID
			wire := func() map[string]any {
				i := wireIssue()
				i["project"] = map[string]any{"id": projectID}
				switch scenario {
				case "wrong_team":
					i["team"] = map[string]any{"id": workspaceID}
				case "wrong_project":
					i["project"] = nil
				case "incomplete":
					delete(i, "updatedAt")
				}
				return i
			}
			c := mockClient(t, publishLinearTransport(t, cfg, func(map[string]any) (*http.Response, error) {
				creates++
				switch scenario {
				case "drop":
					return nil, errors.New("response lost")
				case "503":
					return response(503, `{}`), nil
				case "malformed":
					return response(200, `{"data":`), nil
				case "errors":
					return response(200, `{"errors":[{"message":"secret failure"}]}`), nil
				case "redirect":
					res := response(307, `{}`)
					res.Header.Set("Location", "https://untrusted.example")
					return res, nil
				}
				return dataResponse(map[string]any{"issueCreate": map[string]any{"success": scenario != "false", "issue": wire()}}), nil
			}, wire))
			s := openPublishLinear(t, c, cfg)
			payload, err := s.PrepareCreate(t.Context(), db.Issue{Title: "Native", Status: "open"})
			require.NoError(t, err)
			remote, err := s.Create(t.Context(), payload, func(context.Context) error {
				if scenario == "admission" {
					return errors.New("denied")
				}
				return nil
			})
			require.Error(t, err)
			require.NotContains(t, err.Error(), "secret")
			if scenario == "admission" {
				require.Zero(t, creates)
			} else {
				require.Equal(t, 1, creates)
			}
			if scenario == "incomplete" {
				require.Equal(t, "issue:"+issueID, remote.ExternalID)
			}
			if scenario == "wrong_team" || scenario == "wrong_project" {
				_, err = s.ReadCreated(t.Context(), db.IssuePublicationRemote{ExternalID: issueID})
				require.Error(t, err)
			}
		})
	}
}
func TestPublishLinearPreflight(t *testing.T) {
	for _, scenario := range []string{"no_open_state", "scope_denied", "oversize", "closed"} {
		t.Run(scenario, func(t *testing.T) {
			creates := 0
			cfg := testConfig()
			normal := publishLinearTransport(t, cfg, func(map[string]any) (*http.Response, error) { creates++; return response(500, `{}`), nil }, wireIssue)
			c := mockClient(t, func(r *http.Request) (*http.Response, error) {
				raw, err := io.ReadAll(r.Body)
				require.NoError(t, err)
				r.Body = io.NopCloser(bytes.NewReader(raw))
				if scenario == "no_open_state" && bytes.Contains(raw, []byte("KataLinearStates")) {
					return dataResponse(map[string]any{"workflowStates": map[string]any{"nodes": []any{map[string]any{"id": stateID, "type": "completed", "position": 0, "team": map[string]any{"id": teamID}}}, "pageInfo": map[string]any{"hasNextPage": false}}}), nil
				}
				if scenario == "scope_denied" {
					return response(403, `{}`), nil
				}
				return normal(r)
			})
			s := openPublishLinear(t, c, cfg)
			issue := db.Issue{Title: "Native", Status: "open"}
			if scenario == "oversize" {
				issue.Body = strings.Repeat("x", maxDescriptionBytes+1)
			}
			if scenario == "closed" {
				issue.Status = "closed"
			}
			_, err := s.PrepareCreate(t.Context(), issue)
			require.Error(t, err)
			require.Zero(t, creates)
		})
	}
}
