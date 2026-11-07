package daemon_test

import (
	"encoding/json"
	"encoding/json/jsontext"
	"fmt"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

// The issue history contract filters before limit and acknowledges the resolved
// subject even on the final empty page, so clients can reject older daemons.
func TestPollProjectEventsIssuePagination(t *testing.T) {
	env := testenv.New(t)
	project, err := env.DB.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	mkIssue(t, env, project.ID, "Unrelated before")
	issue, created := mkIssueWithEvent(t, env, project.ID, "Subject")
	mkIssue(t, env, project.ID, "Unrelated between")
	_, commented, err := env.DB.CreateComment(t.Context(), db.CreateCommentParams{IssueID: issue.ID, Author: "tester", Body: "Progress"})
	require.NoError(t, err)
	for _, ref := range []string{issue.ShortID, issue.UID, project.Name + "#" + issue.ShortID} {
		after := int64(0)
		for _, want := range []int64{created.ID, commented.ID, 0} {
			resp, body := envGetRaw(t, env, fmt.Sprintf("%s?issue=%s&after_id=%d&limit=1", scopedProjectPath(project.ID, "events"), url.QueryEscape(ref), after))
			require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
			require.Equal(t, issue.UID, resp.Header.Get("X-Kata-Events-Issue-UID"))
			var page api.PollEventsBody
			require.NoError(t, json.Unmarshal(body, &page))
			require.False(t, page.ResetRequired)
			if want == 0 {
				require.Empty(t, page.Events)
				require.Equal(t, after, page.NextAfterID)
				continue
			}
			require.Len(t, page.Events, 1)
			require.Equal(t, want, page.Events[0].EventID)
			require.Equal(t, issue.UID, *page.Events[0].IssueUID)
			require.Equal(t, want, page.NextAfterID)
			after = page.NextAfterID
		}
	}
	other, err := env.DB.CreateProject(t.Context(), "other-project")
	require.NoError(t, err)
	foreign := mkIssue(t, env, other.ID, "Foreign")
	for _, ref := range []string{"invalid!", foreign.UID, other.Name + "#" + foreign.ShortID} {
		resp, body := envGetRaw(t, env, scopedProjectPath(project.ID, "events")+"?issue="+url.QueryEscape(ref))
		require.Equal(t, http.StatusNotFound, resp.StatusCode, string(body))
	}
	for _, suffix := range []string{"&limit=0", "&after_id=-1"} {
		resp, _ := envGetRaw(t, env, scopedProjectPath(project.ID, "events")+"?issue="+issue.UID+suffix)
		require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	}
	_, err = env.DB.PurgeIssue(t.Context(), foreign.ID, "tester", nil)
	require.NoError(t, err)
	// Purging another issue in this project leaves the subject's history
	// readable, both fresh and from a cursor held across the purge.
	doomed := mkIssue(t, env, project.ID, "Doomed")
	_, err = env.DB.PurgeIssue(t.Context(), doomed.ID, "tester", nil)
	require.NoError(t, err)
	for after, want := range map[int64][]int64{0: {created.ID, commented.ID}, created.ID: {commented.ID}} {
		resp, body := envGetRaw(t, env, fmt.Sprintf("%s?issue=%s&after_id=%d", scopedProjectPath(project.ID, "events"), issue.UID, after))
		require.Equal(t, http.StatusOK, resp.StatusCode)
		var page api.PollEventsBody
		require.NoError(t, json.Unmarshal(body, &page))
		require.False(t, page.ResetRequired)
		got := make([]int64, 0, len(page.Events))
		for _, event := range page.Events {
			got = append(got, event.EventID)
		}
		require.Equal(t, want, got)
	}
}

func TestPollProjectEventsIssueScopedHistory(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("bootstrap-token"), testenv.WithRequireTokenIdentity())
	project, err := env.DB.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	root := createScopedHTTPTestIssue(t, env, project.ID, "Root", nil)
	hidden := createScopedHTTPTestIssue(t, env, project.ID, "Hidden", nil)
	other := createScopedHTTPTestIssue(t, env, project.ID, "Other hidden peer", nil)
	newScopedTokens(t, env, project, root)
	headers := map[string]string{"Authorization": "Bearer worker-token"}
	resp, body := envDoRaw(t, env, http.MethodGet, scopedProjectPath(project.ID, "events")+"?issue="+hidden.UID, nil, headers)
	require.Equal(t, http.StatusNotFound, resp.StatusCode, string(body))
	after, err := env.DB.MaxEventID(t.Context())
	require.NoError(t, err)
	// Each hidden-peer link is omitted from history. A visible comment after
	// multiple omitted pages must still fill this limit=1 response.
	for range 2 {
		peer := createScopedHTTPTestIssue(t, env, project.ID, "Hidden peer", nil)
		resp, body = envDoRaw(t, env, http.MethodPost, scopedProjectPath(project.ID, "issues/"+root.ShortID+"/links"),
			map[string]any{"actor": "coordinator", "type": "blocks", "to_ref": peer.UID}, map[string]string{"Authorization": "Bearer coordinator-token"})
		require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	}
	_, comment, err := env.DB.CreateComment(t.Context(), db.CreateCommentParams{IssueID: root.ID, Author: "tester", Body: "Visible progress"})
	require.NoError(t, err)
	resp, body = envDoRaw(t, env, http.MethodGet, fmt.Sprintf("%s?issue=%s&after_id=%d&limit=1", scopedProjectPath(project.ID, "events"), root.UID, after), nil, headers)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	require.Equal(t, root.UID, resp.Header.Get("X-Kata-Events-Issue-UID"))
	var page api.PollEventsBody
	require.NoError(t, json.Unmarshal(body, &page))
	require.False(t, page.ResetRequired)
	require.Len(t, page.Events, 1)
	require.Equal(t, comment.ID, page.Events[0].EventID)
	require.Empty(t, page.Events[0].OriginInstanceUID)
	require.Empty(t, page.Events[0].ContentHash)
	// Compound link history survives with its peer-bearing payload redacted.
	resp, body = envDoRaw(t, env, http.MethodPatch, scopedProjectPath(project.ID, "issues/"+root.ShortID),
		map[string]any{"actor": "coordinator", "links_delta": map[string]any{"add_related": []string{hidden.UID, other.UID}}},
		map[string]string{"Authorization": "Bearer coordinator-token"})
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	require.Contains(t, string(body), "issue.links_changed")
	resp, body = envDoRaw(t, env, http.MethodGet, fmt.Sprintf("%s?issue=%s&after_id=%d&limit=1", scopedProjectPath(project.ID, "events"), root.UID, comment.ID), nil, headers)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	page = api.PollEventsBody{}
	require.NoError(t, json.Unmarshal(body, &page))
	require.False(t, page.ResetRequired)
	require.Len(t, page.Events, 1, string(body))
	require.Equal(t, "issue.links_changed", page.Events[0].Type)
	require.Empty(t, page.Events[0].Payload)
	require.NotContains(t, string(body), hidden.UID)
}

func TestPollProjectEventsIssuePayloadOnlyClose(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		for index, identityField := range []string{"issue_uid", "uid"} {
			t.Run(identityField, func(t *testing.T) {
				f := newProjectAccessFixture(t, store, identityField)
				issue := f.visible
				expiresAt := time.Now().UTC().Add(time.Hour)
				_, _, err := store.CreateAPIToken(t.Context(), db.CreateAPITokenParams{
					PlaintextToken: "history-token-" + identityField, Actor: "member", AdminActor: db.BootstrapActor,
					Scope: &db.APITokenScope{
						Kind: db.APITokenScopeIssueSubtree, ProjectUID: f.public.UID, RootIssueUID: issue.UID,
					}, ExpiresAt: &expiresAt,
				})
				require.NoError(t, err)
				_, closed, changed, err := store.CloseIssueWithEvents(t.Context(), issue.ID, "done", "member", "Closed before newer federated evidence.", nil)
				require.NoError(t, err)
				require.True(t, changed)
				require.Len(t, closed, 1)
				remote := db.RemoteEvent{
					EventUID: fmt.Sprintf("%026d", index+1), OriginInstanceUID: "7ZZZZZZZZZZZZZZZZZZZZZZZZZ",
					ProjectUID: f.public.UID, ProjectName: f.public.Name,
					Type: "issue.closed", Actor: "member", HLCPhysicalMS: closed[0].HLCPhysicalMS + 1,
					Payload:   jsontext.Value(fmt.Sprintf(`{"%s":%q,"reason":"done","evidence":[{"type":"commit","sha":"0123456789abcdef0123456789abcdef01234567"}]}`, identityField, issue.UID)),
					CreatedAt: closed[0].CreatedAt,
				}
				remote.ContentHash, err = db.RemoteEventContentHash(remote)
				require.NoError(t, err)
				inserted, err := store.InsertRemoteEvent(t.Context(), f.public.ID, remote)
				require.NoError(t, err)
				require.True(t, inserted)

				path := fmt.Sprintf("%s?issue=%s&include_effective_close_event_uid=true", scopedProjectPath(f.public.ID, "events"), issue.UID)
				for _, token := range []string{"member-" + identityField + "-test-token", "history-token-" + identityField} {
					status, _, body := f.request(t, http.MethodGet, path, "", nil, map[string]string{"Authorization": "Bearer " + token})
					require.Equal(t, http.StatusOK, status, string(body))
					var page api.PollEventsBody
					require.NoError(t, json.Unmarshal(body, &page))
					require.Equal(t, remote.EventUID, page.EffectiveCloseEventUID, string(body))
					var selected *api.EventEnvelope
					for i := range page.Events {
						if page.Events[i].EventUID == remote.EventUID {
							selected = &page.Events[i]
						}
					}
					require.NotNil(t, selected, "the selected close must be returned in issue history")
					require.NotNil(t, selected.IssueUID)
					require.Equal(t, issue.UID, *selected.IssueUID)
					require.Contains(t, string(selected.Payload), "0123456789abcdef0123456789abcdef01234567")
				}
			})
		}
	})
}

func TestPollProjectEventsIssueReturnsEffectiveCloseEvent(t *testing.T) {
	t.Run("federated tie keeps origin identity private", func(t *testing.T) {
		env := testenv.New(t, testenv.WithAuthToken("bootstrap-token"), testenv.WithRequireTokenIdentity())
		project, err := env.DB.CreateProject(t.Context(), "example-project")
		require.NoError(t, err)
		issue := createScopedHTTPTestIssue(t, env, project.ID, "Close evidence", nil)
		newScopedTokens(t, env, project, issue)
		_, closedEvents, changed, err := env.DB.CloseIssueWithEvents(t.Context(), issue.ID, "done", "tester", "Completed with a source close event.", nil)
		require.NoError(t, err)
		require.True(t, changed)
		require.Len(t, closedEvents, 1)

		remoteUID := "00000000000000000000000001"
		remoteOrigin := "7ZZZZZZZZZZZZZZZZZZZZZZZZZ"
		insertRemoteIssueHistoryEvent(t, env, project, issue, remoteUID, remoteOrigin, "issue.closed",
			closedEvents[0].HLCPhysicalMS, closedEvents[0].HLCCounter,
			`{"reason":"done","evidence":[{"type":"commit","sha":"0123456789abcdef0123456789abcdef01234567"}]}`,
			closedEvents[0].CreatedAt)

		path := fmt.Sprintf("%s?issue=%s&include_effective_close_event_uid=true", scopedProjectPath(project.ID, "events"), url.QueryEscape(issue.UID))
		resp, body := envDoRaw(t, env, http.MethodGet, path, nil, map[string]string{"Authorization": "Bearer worker-token"})
		require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
		require.Equal(t, issue.UID, resp.Header.Get("X-Kata-Events-Issue-UID"))
		var raw map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(body, &raw))
		var effectiveUID string
		require.NoError(t, json.Unmarshal(raw["effective_close_event_uid"], &effectiveUID), string(body))
		require.Equal(t, remoteUID, effectiveUID, string(body))
		var page api.PollEventsBody
		require.NoError(t, json.Unmarshal(body, &page))
		require.NotEmpty(t, page.Events)
		foundEffectiveClose := false
		for _, event := range page.Events {
			require.Empty(t, event.OriginInstanceUID, "scoped history keeps origin identity redacted")
			foundEffectiveClose = foundEffectiveClose || event.EventUID == remoteUID
		}
		require.True(t, foundEffectiveClose, "the selected close must remain available in scoped history")
		require.NotContains(t, string(body), remoteOrigin)
	})

	t.Run("provider transitions clear prior evidence", func(t *testing.T) {
		env := testenv.New(t, testenv.WithAuthToken("bootstrap-token"), testenv.WithRequireTokenIdentity())
		project, err := env.DB.CreateProject(t.Context(), "example-project")
		require.NoError(t, err)
		issue := createScopedHTTPTestIssue(t, env, project.ID, "Provider status", nil)
		newScopedTokens(t, env, project, issue)
		_, closedEvents, changed, err := env.DB.CloseIssueWithEvents(t.Context(), issue.ID, "done", "tester", "Completed with evidence.", nil)
		require.NoError(t, err)
		require.True(t, changed)
		require.Len(t, closedEvents, 1)
		baseHLC := closedEvents[0].HLCPhysicalMS
		origin := "7ZZZZZZZZZZZZZZZZZZZZZZZZZ"
		createdAt := closedEvents[0].CreatedAt
		insertRemoteIssueHistoryEvent(t, env, project, issue, "00000000000000000000000002", origin, "issue.updated",
			baseHLC+1, 0, `{"status":"open"}`, createdAt)
		insertRemoteIssueHistoryEvent(t, env, project, issue, "00000000000000000000000003", origin, "issue.snapshot",
			baseHLC+2, 0, `{"uid":"`+issue.UID+`","short_id":"`+issue.ShortID+`","title":"Provider status","status":"closed"}`, createdAt)

		path := fmt.Sprintf("%s?issue=%s&include_effective_close_event_uid=true", scopedProjectPath(project.ID, "events"), url.QueryEscape(issue.UID))
		resp, body := envDoRaw(t, env, http.MethodGet, path, nil, map[string]string{"Authorization": "Bearer worker-token"})
		require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
		var raw map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(body, &raw))
		_, hasEffectiveClose := raw["effective_close_event_uid"]
		require.False(t, hasEffectiveClose, string(body))
		require.NotContains(t, string(body), `"status":"open"`)
	})

	t.Run("hidden project transitions cannot leave an older visible close selected", func(t *testing.T) {
		env := testenv.New(t, testenv.WithAuthToken("bootstrap-token"), testenv.WithRequireTokenIdentity())
		visible, err := env.DB.CreateProject(t.Context(), "example-project")
		require.NoError(t, err)
		hidden, err := env.DB.CreateProject(t.Context(), "restricted-project")
		require.NoError(t, err)
		hiddenTeam, _, err := env.DB.CreateTeam(t.Context(), "history-restricted-team", db.BootstrapActor)
		require.NoError(t, err)
		_, _, err = env.DB.SetProjectAccessPolicy(t.Context(), db.ProjectAccessPolicy{
			ProjectUID: hidden.UID, Visibility: "teams", TeamUIDs: []string{hiddenTeam.UID},
		}, db.BootstrapActor)
		require.NoError(t, err)
		issue := createScopedHTTPTestIssue(t, env, visible.ID, "Moved close evidence", nil)
		newScopedTokens(t, env, visible, issue)
		_, closedEvents, changed, err := env.DB.CloseIssueWithEvents(t.Context(), issue.ID, "done", "tester", "Closed before moving.", nil)
		require.NoError(t, err)
		require.True(t, changed)
		require.Len(t, closedEvents, 1)

		current, err := env.DB.IssueByUID(t.Context(), issue.UID, db.IncludeDeletedNo)
		require.NoError(t, err)
		_, err = env.DB.MoveIssueProject(t.Context(), db.MoveIssueProjectIn{
			IssueID: current.ID, FromProjectID: visible.ID, ToProjectID: hidden.ID,
			IfMatchRev: current.Revision, Actor: "tester", Single: true,
		})
		require.NoError(t, err)

		origin := "7ZZZZZZZZZZZZZZZZZZZZZZZZZ"
		createdAt := closedEvents[0].CreatedAt
		insertRemoteIssueHistoryEvent(t, env, hidden, issue, "00000000000000000000000004", origin, "issue.updated",
			closedEvents[0].HLCPhysicalMS+100, 0, `{"status":"open"}`, createdAt)
		const hiddenCloseUID = "00000000000000000000000005"
		insertRemoteIssueHistoryEvent(t, env, hidden, issue, hiddenCloseUID, origin, "issue.closed",
			closedEvents[0].HLCPhysicalMS+200, 0,
			`{"reason":"done","evidence":[{"type":"commit","sha":"0123456789abcdef0123456789abcdef01234567"}]}`,
			createdAt)

		current, err = env.DB.IssueByUID(t.Context(), issue.UID, db.IncludeDeletedNo)
		require.NoError(t, err)
		_, err = env.DB.MoveIssueProject(t.Context(), db.MoveIssueProjectIn{
			IssueID: current.ID, FromProjectID: hidden.ID, ToProjectID: visible.ID,
			IfMatchRev: current.Revision, Actor: "tester", Single: true,
		})
		require.NoError(t, err)

		path := fmt.Sprintf("%s?issue=%s&include_effective_close_event_uid=true", scopedProjectPath(visible.ID, "events"), url.QueryEscape(issue.UID))
		resp, body := envDoRaw(t, env, http.MethodGet, path, nil, map[string]string{"Authorization": "Bearer worker-token"})
		require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
		var raw map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(body, &raw))
		_, hasEffectiveClose := raw["effective_close_event_uid"]
		require.False(t, hasEffectiveClose, string(body))
		require.NotContains(t, string(body), hiddenCloseUID)
	})
}

func TestPollProjectEventsIssueHidesEffectiveCloseWithPrivateReference(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newProjectAccessFixture(t, store)
		for referenceForm := range 2 {
			for evidenceForm := range 2 {
				t.Run(fmt.Sprintf("reference-%d/evidence-%d", referenceForm, evidenceForm), func(t *testing.T) {
					visible, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{
						ProjectID: f.public.ID, Title: "Visible close evidence source", Author: "member",
					})
					require.NoError(t, err)

					evidenceType, reason := "duplicate-of", "duplicate"
					if evidenceForm == 1 {
						evidenceType, reason = "superseded-by", "superseded"
					}
					issueRef := f.issue.UID
					if referenceForm == 1 {
						issueRef = f.private.Name + "#" + f.issue.ShortID
					}
					_, closeEvents, changed, err := store.CloseIssueWithEvents(t.Context(), visible.ID, reason, "member",
						"Closed with a reference to a restricted issue.", []db.Evidence{{
							Type: evidenceType, IssueRef: issueRef,
						}})
					require.NoError(t, err)
					require.True(t, changed)
					require.Len(t, closeEvents, 1)

					path := fmt.Sprintf("%s?issue=%s&include_effective_close_event_uid=true", scopedProjectPath(f.public.ID, "events"), url.QueryEscape(visible.UID))
					status, _, body := f.request(t, http.MethodGet, path, "nonmember", nil, nil)
					require.Equal(t, http.StatusOK, status, string(body))
					var page api.PollEventsBody
					require.NoError(t, json.Unmarshal(body, &page))
					require.Empty(t, page.EffectiveCloseEventUID, string(body))
					for _, event := range page.Events {
						require.NotEqual(t, "issue.closed", event.Type, "a close with a hidden evidence target must remain omitted")
					}
				})
			}
		}
	})
}

func TestPollProjectEventsIssueOmitsCloseWhenEvidenceReferenceMovesBetweenAuthorizedProjects(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newProjectAccessFixture(t, store)
		target, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{
			ProjectID: f.public.ID, Title: "Evidence target", Author: "member",
		})
		require.NoError(t, err)
		_, closeEvents, changed, err := store.CloseIssueWithEvents(t.Context(), f.visible.ID, "duplicate", "member",
			"Closed with a historical short reference to its duplicate.", []db.Evidence{{
				Type: "duplicate-of", IssueRef: target.ShortID,
			}})
		require.NoError(t, err)
		require.True(t, changed)
		require.Len(t, closeEvents, 1)

		_, err = store.MoveIssueProject(t.Context(), db.MoveIssueProjectIn{
			IssueID: target.ID, FromProjectID: f.public.ID, ToProjectID: f.private.ID,
			IfMatchRev: target.Revision, Actor: "member", Single: true,
		})
		require.NoError(t, err)

		path := fmt.Sprintf("%s?issue=%s&include_effective_close_event_uid=true",
			scopedProjectPath(f.public.ID, "events"), url.QueryEscape(f.visible.UID))
		status, _, body := f.request(t, http.MethodGet, path, "member", nil, nil)
		require.Equal(t, http.StatusOK, status, string(body))
		var page api.PollEventsBody
		require.NoError(t, json.Unmarshal(body, &page))
		require.Empty(t, page.EffectiveCloseEventUID, string(body))
		for _, event := range page.Events {
			require.NotEqual(t, "issue.closed", event.Type,
				"a close replaced by a reference reset must not be advertised as the effective close")
		}
	})
}

func TestPollProjectEventsIssueSurvivesUnrelatedPolicyChangeWithHiddenEndpoint(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newProjectAccessFixture(t, store)
		_, _, err := store.CreateLinkAndEvent(t.Context(), db.CreateLinkParams{
			FromIssueID: f.visible.ID, ToIssueID: f.issue.ID, Type: "blocks", Author: "member",
		}, db.LinkEventParams{
			EventType: "issue.linked", EventIssueID: f.visible.ID,
			FromShortID: f.visible.ShortID, FromUID: f.visible.UID,
			ToShortID: f.issue.ShortID, ToUID: f.issue.UID, Actor: "member",
		})
		require.NoError(t, err)
		_, closed, _, err := store.CloseIssueWithEvents(t.Context(), f.visible.ID, "done", "member",
			"Completed the visible task.", []db.Evidence{{Type: "commit", SHA: "0123456789abcdef0123456789abcdef01234567"}})
		require.NoError(t, err)
		require.Len(t, closed, 1)

		f = projectAccessFixtureWithStorage(t, f, projectAccessReleaseStore{
			Storage: store,
			afterEventsAfter: func([]db.Event) {
				_, err := store.SetTeamMembership(t.Context(), f.team.UID, "unrelated-actor", true, "admin")
				require.NoError(t, err)
			},
		})
		path := fmt.Sprintf("%s?issue=%s&include_effective_close_event_uid=true",
			scopedProjectPath(f.public.ID, "events"), url.QueryEscape(f.visible.UID))
		status, _, body := f.request(t, http.MethodGet, path, "nonmember", nil, nil)
		require.Equal(t, http.StatusOK, status, string(body))
		var page api.PollEventsBody
		require.NoError(t, json.Unmarshal(body, &page))
		require.Equal(t, closed[0].UID, page.EffectiveCloseEventUID)
	})
}

func insertRemoteIssueHistoryEvent(
	t *testing.T,
	env *testenv.Env,
	project db.Project,
	issue db.Issue,
	eventUID, originUID, eventType string,
	hlcPhysicalMS, hlcCounter int64,
	payload string,
	createdAt time.Time,
) {
	t.Helper()
	issueUID := issue.UID
	event := db.RemoteEvent{
		EventUID: eventUID, OriginInstanceUID: originUID,
		ProjectUID: project.UID, ProjectName: project.Name, IssueUID: &issueUID,
		Type: eventType, Actor: "tester", HLCPhysicalMS: hlcPhysicalMS, HLCCounter: hlcCounter,
		Payload: jsontext.Value(payload), CreatedAt: createdAt,
	}
	hash, err := db.RemoteEventContentHash(event)
	require.NoError(t, err)
	event.ContentHash = hash
	inserted, err := env.DB.InsertRemoteEvent(t.Context(), project.ID, event)
	require.NoError(t, err)
	require.True(t, inserted)
}

func TestPollProjectEventsIssueOmitsPrivatePeerEvents(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newProjectAccessFixture(t, store)
		_, err := store.CreateLink(t.Context(), db.CreateLinkParams{
			FromIssueID: f.visible.ID, ToIssueID: f.issue.ID, Type: "parent", Author: "member",
		})
		require.NoError(t, err)
		after, err := store.MaxEventID(t.Context())
		require.NoError(t, err)
		_, events, _, err := store.CloseIssueWithEvents(t.Context(), f.visible.ID, "done", "member", "Completed visible task", nil)
		require.NoError(t, err)
		require.Len(t, events, 1)
		_, comment, err := store.CreateComment(t.Context(), db.CreateCommentParams{IssueID: f.visible.ID, Author: "member", Body: "Visible follow-up"})
		require.NoError(t, err)
		// History omits the event that names a private peer instead of
		// truncating the read at it.
		status, headers, body := f.request(t, http.MethodGet, fmt.Sprintf("%s?issue=%s&after_id=%d", scopedProjectPath(f.public.ID, "events"), f.visible.UID, after), "nonmember", nil, nil)
		require.Equal(t, http.StatusOK, status, string(body))
		require.Equal(t, f.visible.UID, headers.Get("X-Kata-Events-Issue-UID"))
		var page api.PollEventsBody
		require.NoError(t, json.Unmarshal(body, &page))
		require.False(t, page.ResetRequired)
		require.Len(t, page.Events, 1, string(body))
		require.Equal(t, comment.ID, page.Events[0].EventID)
		require.Equal(t, comment.ID, page.NextAfterID)
		require.NotContains(t, string(body), f.issue.UID)
	})
}

func TestPollProjectEventsMovedIssueRespectsHistoricalProjectAccess(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newProjectAccessFixture(t, store)
		_, err := store.MoveIssueProject(t.Context(), db.MoveIssueProjectIn{IssueID: f.issue.ID, FromProjectID: f.private.ID, ToProjectID: f.public.ID, IfMatchRev: f.issue.Revision, Actor: "member"})
		require.NoError(t, err)
		path := fmt.Sprintf("%s?issue=%s", scopedProjectPath(f.public.ID, "events"), f.issue.UID)
		for _, tc := range []struct {
			actor       string
			wantCreated bool
		}{{"member", true}, {"nonmember", false}} {
			status, _, body := f.request(t, http.MethodGet, path, tc.actor, nil, nil)
			require.Equal(t, http.StatusOK, status, string(body))
			var page api.PollEventsBody
			require.NoError(t, json.Unmarshal(body, &page))
			found := false
			for _, event := range page.Events {
				if event.Type == "issue.created" {
					found = true
				}
			}
			require.Equal(t, tc.wantCreated, found, string(body))
		}
	})
}
