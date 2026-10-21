package daemon_test

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
)

type notificationReadHostAccess struct{ commentProjectHostAccess }

func (a notificationReadHostAccess) Authorize(ctx context.Context, request daemon.HostAccessRequest) (daemon.HostAccessDecision, error) {
	decision, err := a.commentProjectHostAccess.Authorize(ctx, request)
	decision.Revalidate = func(context.Context) error { return nil }
	return decision, err
}

func TestNotificationReferencesFollowProjectAccess(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	for _, authority := range []string{"project", "host"} {
		t.Run(authority, func(t *testing.T) {
			projectAccessBackends(t, func(t *testing.T, store db.Storage) {
				f := newProjectAccessFixture(t, store)
				actor := "nonmember"
				if authority == "host" {
					actor = "member" // Native project access permits the target; the host denies it.
					d := daemon.NewServer(daemon.ServerConfig{DB: store, HostAccess: notificationReadHostAccess{commentProjectHostAccess{deniedProjectID: f.private.ID}}})
					t.Cleanup(func() { _ = d.Close() })
					f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						r = r.WithContext(daemon.WithPrincipal(r.Context(), daemon.Principal{Kind: daemon.PrincipalHost, Subject: "reader", Actor: actor}))
						d.Handler().ServeHTTP(w, r)
					}))
					t.Cleanup(f.server.Close)
				}
				targetIssue, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: f.public.ID, Title: "Target", Author: "reader"})
				require.NoError(t, err)
				target, _, err := store.CreateComment(t.Context(), db.CreateCommentParams{IssueID: targetIssue.ID, Author: "reader", Body: "Finding"})
				require.NoError(t, err)
				_, _, err = store.CreateComment(t.Context(), db.CreateCommentParams{IssueID: f.visible.ID, Author: "reader", Body: "Participating"})
				require.NoError(t, err)
				base := fmt.Sprintf("/api/v1/projects/%d/issues/%s", f.public.ID, f.visible.ShortID)
				const message = "Restricted notification context"
				status, _, body := f.request(t, http.MethodPost, base+"/notifications", actor,
					map[string]any{"broadcast": true, "message": message, "re": target.UID}, nil)
				require.Equal(t, http.StatusOK, status, string(body))
				var notified struct {
					Event db.Event `json:"event"`
				}
				require.NoError(t, json.Unmarshal(body, &notified))
				require.Positive(t, notified.Event.ID)
				allowedProject, err := store.CreateProject(t.Context(), "another-shared-project")
				require.NoError(t, err)
				archivedProject, err := store.CreateProject(t.Context(), "archived-shared-project")
				require.NoError(t, err)

				for _, destination := range []db.Project{f.public, allowedProject, f.private, archivedProject} {
					if destination.ID != f.public.ID {
						targetIssue, err = store.IssueByID(t.Context(), targetIssue.ID)
						require.NoError(t, err)
						_, err = store.MoveIssueProject(t.Context(), db.MoveIssueProjectIn{IssueID: targetIssue.ID,
							FromProjectID: targetIssue.ProjectID, ToProjectID: destination.ID, IfMatchRev: targetIssue.Revision, Actor: "admin"})
						require.NoError(t, err)
					}
					if destination.ID == archivedProject.ID {
						_, _, err = store.RemoveProject(t.Context(), db.RemoveProjectParams{
							ProjectID: destination.ID, Actor: "admin", Force: true,
						})
						require.NoError(t, err)
					}
					hidden := destination.ID == f.private.ID || destination.ID == archivedProject.ID
					t.Run(destination.Name, func(t *testing.T) {
						for _, path := range []string{base, base + "/metadata",
							fmt.Sprintf("/api/v1/projects/%d/issues", f.public.ID),
							fmt.Sprintf("/api/v1/projects/%d/events?after_id=%d", f.public.ID, notified.Event.ID-1),
							"/api/v1/ui/snapshot?view=all-open&project_uid=" + f.public.UID + "&selected_issue_uid=" + f.visible.UID + "&include_history=true"} {
							t.Run(path, func(t *testing.T) {
								status, _, body := f.request(t, http.MethodGet, path, actor, nil, nil)
								require.Equal(t, http.StatusOK, status, string(body))
								if hidden {
									assert.NotContains(t, string(body), target.UID)
									assert.NotContains(t, string(body), message)
								} else {
									assert.Contains(t, string(body), target.UID)
									assert.Contains(t, string(body), message)
								}
							})
						}
						t.Run("rate diagnostics", func(t *testing.T) {
							status, _, body := f.request(t, http.MethodPost, base+"/notifications", actor,
								map[string]any{"broadcast": true, "message": "Another notification"}, nil)
							require.Equal(t, http.StatusTooManyRequests, status, string(body))
							if hidden {
								assert.NotContains(t, string(body), message)
								assert.Contains(t, string(body), `"last_broadcast_by":""`)
							} else {
								assert.Contains(t, string(body), message)
							}
						})
						t.Run("SSE", func(t *testing.T) {
							ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
							defer cancel()
							req, err := http.NewRequestWithContext(ctx, http.MethodGet,
								fmt.Sprintf("%s/api/v1/events/stream?project_id=%d&after_id=%d", f.server.URL, f.public.ID, notified.Event.ID-1), nil)
							require.NoError(t, err)
							req.Header.Set("Authorization", "Bearer "+actor+"-test-token")
							req.Header.Set("Accept", "text/event-stream")
							resp, err := f.server.Client().Do(req)
							require.NoError(t, err)
							defer func() { _ = resp.Body.Close() }()
							require.Equal(t, http.StatusOK, resp.StatusCode)
							frame, ok := newSSEFramer(resp.Body).Next(t, time.Second)
							require.True(t, ok, "stream must deliver the notification or a project-scope reset")
							if hidden {
								require.Contains(t, []string{"issue.metadata_updated", "sync.reset_required"}, frame.event)
								assert.NotContains(t, frame.data, target.UID)
								assert.NotContains(t, frame.data, message)
							} else {
								require.Equal(t, "issue.metadata_updated", frame.event)
								assert.Contains(t, frame.data, target.UID)
								assert.Contains(t, frame.data, message)
							}
						})
					})
				}
			})
		})
	}
}

// JSON can spell a slot key with escapes, and imported history keeps payload
// bytes as written. Projection must still find such a key when the response
// holds no literal slot prefix anywhere.
func TestNotificationProjectionSeesEscapedSlotKeys(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newProjectAccessFixture(t, store)
		ctx := t.Context()
		hidden, _, err := store.CreateComment(ctx, db.CreateCommentParams{IssueID: f.issue.ID, Author: "member", Body: "Restricted finding"})
		require.NoError(t, err)
		const message = "Restricted escaped context"
		_, err = store.PatchIssueMetadata(ctx, db.PatchIssueMetadataIn{IssueID: f.visible.ID, Actor: "member", Patch: map[string]jsontext.Value{
			"notify.cmVhZGVy": jsontext.Value(fmt.Sprintf(`{"from":"member","message":%q,"re":%q}`, message, hidden.UID)),
		}})
		require.NoError(t, err)
		_, err = store.PatchIssueMetadata(ctx, db.PatchIssueMetadataIn{IssueID: f.visible.ID, Actor: "member", Patch: map[string]jsontext.Value{
			"notify.cmVhZGVy": jsontext.Value("null"),
		}})
		require.NoError(t, err)
		writer, ok := store.(interface {
			ExecContext(context.Context, string, ...any) (sql.Result, error)
		})
		require.True(t, ok)
		_, err = writer.ExecContext(ctx, `UPDATE events SET payload=replace(payload, '"notify.', '"\u006eotify.') WHERE issue_id=$1`, f.visible.ID)
		require.NoError(t, err)

		path := "/api/v1/ui/snapshot?view=all-open&project_uid=" + f.public.UID + "&selected_issue_uid=" + f.visible.UID + "&include_history=true"
		status, _, body := f.request(t, http.MethodGet, path, "nonmember", nil, nil)
		require.Equal(t, http.StatusOK, status, string(body))
		require.Contains(t, string(body), "issue.metadata_updated", "the snapshot must include the escaped history")
		assert.NotContains(t, string(body), hidden.UID)
		assert.NotContains(t, string(body), message)
	})
}
