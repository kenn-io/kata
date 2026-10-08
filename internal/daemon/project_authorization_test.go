package daemon_test

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/testenv"
)

const projectAccessCanary = "relay-private-canary"

func projectAccessBackends(t *testing.T, check func(*testing.T, db.Storage)) {
	t.Helper()
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			var store db.Storage
			if backend == "sqlite" {
				s, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "kata.db"))
				require.NoError(t, err)
				store = s
			} else {
				dsn, cleanup := testenv.NewPostgresContainer(t, t.Context())
				t.Cleanup(cleanup)
				s, err := pgstore.Open(t.Context(), dsn)
				require.NoError(t, err)
				store = s
			}
			t.Cleanup(func() { require.NoError(t, store.Close()) })
			check(t, store)
		})
	}
}

type projectAccessFixture struct {
	store           db.Storage
	server          *httptest.Server
	private, public db.Project
	issue, visible  db.Issue
	team            db.Team
	tokenSuffix     string
	broadcaster     *daemon.EventBroadcaster
	api             huma.API
}

func newProjectAccessFixture(t *testing.T, store db.Storage, suffix ...string) projectAccessFixture {
	t.Helper()
	ctx := t.Context()
	privateName, publicName := "restricted-project", "shared-project"
	teamName := "engineering"
	tokenSuffix := ""
	if len(suffix) > 0 && suffix[0] != "" {
		privateName += "-" + suffix[0]
		publicName += "-" + suffix[0]
		teamName += "-" + suffix[0]
		tokenSuffix = suffix[0]
	}
	private, err := store.CreateProject(ctx, privateName)
	require.NoError(t, err)
	public, err := store.CreateProject(ctx, publicName)
	require.NoError(t, err)
	visible, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: public.ID, Title: "Visible task", Author: "member"})
	require.NoError(t, err)
	issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: private.ID, Title: projectAccessCanary, Body: projectAccessCanary, Author: "member"})
	require.NoError(t, err)
	team, _, err := store.CreateTeam(ctx, teamName, "admin")
	require.NoError(t, err)
	_, err = store.SetTeamMembership(ctx, team.UID, "member", true, "admin")
	require.NoError(t, err)
	_, _, err = store.SetProjectAccessPolicy(ctx, db.ProjectAccessPolicy{ProjectUID: private.UID, Visibility: "teams", TeamUIDs: []string{team.UID}}, "admin")
	require.NoError(t, err)
	for _, actor := range []string{"member", "nonmember"} {
		plaintext := actor + "-test-token"
		if tokenSuffix != "" {
			plaintext = actor + "-" + tokenSuffix + "-test-token"
		}
		_, _, err = store.CreateAPIToken(ctx, db.CreateAPITokenParams{PlaintextToken: plaintext, Actor: actor, AdminActor: "admin"})
		require.NoError(t, err)
	}
	broadcaster := daemon.NewEventBroadcaster()
	server := daemon.NewServer(daemon.ServerConfig{DB: store, Broadcaster: broadcaster, Auth: config.AuthConfig{Token: "bootstrap-test-token", RequireTokenIdentity: true}})
	t.Cleanup(func() { require.NoError(t, server.Close()) })
	httpServer := httptest.NewServer(server.Handler())
	t.Cleanup(httpServer.Close)
	return projectAccessFixture{store: store, server: httpServer, private: private, public: public, issue: issue, visible: visible, team: team, tokenSuffix: tokenSuffix, broadcaster: broadcaster, api: server.API()}
}

func (f projectAccessFixture) request(t *testing.T, method, path, actor string, body any, headers map[string]string) (int, http.Header, []byte) {
	t.Helper()
	var data []byte
	if body != nil {
		var err error
		data, err = json.Marshal(body)
		require.NoError(t, err)
	}
	req, err := http.NewRequestWithContext(t.Context(), method, f.server.URL+path, bytes.NewReader(data))
	require.NoError(t, err)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if actor != "" {
		token := actor + "-test-token"
		if f.tokenSuffix != "" && actor != "admin" {
			token = actor + "-" + f.tokenSuffix + "-test-token"
		}
		if actor == "admin" {
			token = "bootstrap-test-token"
		}
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	resp, err := f.server.Client().Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, resp.Header, raw
}

func openProjectAccessSSE(t *testing.T, f projectAccessFixture, cursor int64, actor string) (*http.Response, *sseFramer) {
	t.Helper()
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet,
		fmt.Sprintf("%s/api/v1/events/stream?after_id=%d", f.server.URL, cursor), nil)
	require.NoError(t, err)
	token := actor + "-test-token"
	if actor == "admin" {
		token = "bootstrap-test-token"
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Accept", "text/event-stream")
	response, err := f.server.Client().Do(request)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	t.Cleanup(func() { _ = response.Body.Close() })
	return response, newSSEFramer(response.Body)
}

func createProjectAccessIssue(t *testing.T, f projectAccessFixture, projectID int64, actor, title string) {
	t.Helper()
	status, _, body := f.request(t, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%d/issues", projectID), actor,
		map[string]string{"title": title, "actor": actor}, nil)
	require.True(t, status >= http.StatusOK && status < http.StatusMultipleChoices,
		"create issue: status=%d body=%s", status, body)
}

type projectAccessBeforeEditStore struct {
	db.Storage
	before func(context.Context, db.EditIssueAtomicParams) error
}

type projectAccessBeforeIssueMutationStore struct {
	db.Storage
	before func(context.Context, int64) error
}

type projectAccessBeforeCreateIssueStore struct {
	db.Storage
	before func(context.Context, db.CreateIssueParams) error
}

func (s *projectAccessBeforeCreateIssueStore) CreateIssue(
	ctx context.Context, params db.CreateIssueParams,
) (db.Issue, db.Event, error) {
	if s.before != nil {
		before := s.before
		s.before = nil
		if err := before(ctx, params); err != nil {
			return db.Issue{}, db.Event{}, err
		}
	}
	return s.Storage.CreateIssue(ctx, params)
}

func (s *projectAccessBeforeIssueMutationStore) runBefore(ctx context.Context, issueID int64) error {
	if s.before == nil {
		return nil
	}
	before := s.before
	s.before = nil
	return before(ctx, issueID)
}

func (s *projectAccessBeforeIssueMutationStore) PatchIssueMetadata(
	ctx context.Context, input db.PatchIssueMetadataIn,
) (db.PatchIssueMetadataOut, error) {
	if err := s.runBefore(ctx, input.IssueID); err != nil {
		return db.PatchIssueMetadataOut{}, err
	}
	return s.Storage.PatchIssueMetadata(ctx, input)
}

func (s *projectAccessBeforeIssueMutationStore) SoftDeleteIssue(
	ctx context.Context, issueID int64, actor string,
) (db.Issue, *db.Event, bool, error) {
	if err := s.runBefore(ctx, issueID); err != nil {
		return db.Issue{}, nil, false, err
	}
	return s.Storage.SoftDeleteIssue(ctx, issueID, actor)
}

func (s *projectAccessBeforeIssueMutationStore) RestoreIssue(
	ctx context.Context, issueID int64, actor string,
) (db.Issue, *db.Event, bool, error) {
	if err := s.runBefore(ctx, issueID); err != nil {
		return db.Issue{}, nil, false, err
	}
	return s.Storage.RestoreIssue(ctx, issueID, actor)
}

type projectAccessBeforeLinkStore struct {
	db.Storage
	before func(context.Context) error
}

func (s *projectAccessBeforeLinkStore) runBefore(ctx context.Context) error {
	if s.before == nil {
		return nil
	}
	before := s.before
	s.before = nil
	return before(ctx)
}

func (s *projectAccessBeforeLinkStore) EditIssueAtomic(
	ctx context.Context, params db.EditIssueAtomicParams,
) (db.EditIssueAtomicResult, error) {
	if err := s.runBefore(ctx); err != nil {
		return db.EditIssueAtomicResult{}, err
	}
	return s.Storage.EditIssueAtomic(ctx, params)
}

func (s *projectAccessBeforeLinkStore) CreateLinkAndEvent(
	ctx context.Context, params db.CreateLinkParams, event db.LinkEventParams,
) (db.Link, db.Event, error) {
	if err := s.runBefore(ctx); err != nil {
		return db.Link{}, db.Event{}, err
	}
	return s.Storage.CreateLinkAndEvent(ctx, params, event)
}

func (s *projectAccessBeforeEditStore) EditIssueAtomic(
	ctx context.Context, params db.EditIssueAtomicParams,
) (db.EditIssueAtomicResult, error) {
	if s.before != nil {
		before := s.before
		s.before = nil
		if err := before(ctx, params); err != nil {
			return db.EditIssueAtomicResult{}, err
		}
	}
	return s.Storage.EditIssueAtomic(ctx, params)
}

func projectAccessFixtureWithStorage(
	t *testing.T, fixture projectAccessFixture, store db.Storage,
) projectAccessFixture {
	t.Helper()
	server := daemon.NewServer(daemon.ServerConfig{
		DB: store, Broadcaster: fixture.broadcaster,
		Auth: config.AuthConfig{Token: "bootstrap-test-token", RequireTokenIdentity: true},
	})
	t.Cleanup(func() { require.NoError(t, server.Close()) })
	httpServer := httptest.NewServer(server.Handler())
	t.Cleanup(httpServer.Close)
	fixture.server = httpServer
	return fixture
}

func projectAccessHiddenProject(t *testing.T, store db.Storage, name string) db.Project {
	t.Helper()
	project, err := store.CreateProject(t.Context(), name)
	require.NoError(t, err)
	team, _, err := store.CreateTeam(t.Context(), name+"-team", "admin")
	require.NoError(t, err)
	_, err = store.SetTeamMembership(t.Context(), team.UID, "other-member", true, "admin")
	require.NoError(t, err)
	_, _, err = store.SetProjectAccessPolicy(t.Context(), db.ProjectAccessPolicy{
		ProjectUID: project.UID, Visibility: "teams", TeamUIDs: []string{team.UID},
	}, "admin")
	require.NoError(t, err)
	return project
}

func TestProjectAccessRechecksMetadataDeleteAndRestoreAfterIssueMove(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		for _, mutation := range []string{"metadata", "delete", "restore"} {
			t.Run(mutation, func(t *testing.T) {
				f := newProjectAccessFixture(t, store, mutation)
				hidden := projectAccessHiddenProject(t, store, "hidden-"+mutation)
				wrapped := &projectAccessBeforeIssueMutationStore{Storage: store}
				wrapped.before = func(_ context.Context, issueID int64) error {
					if issueID != f.visible.ID {
						return nil
					}
					_, err := store.MoveIssueProject(t.Context(), db.MoveIssueProjectIn{
						IssueID: f.visible.ID, FromProjectID: f.public.ID, ToProjectID: hidden.ID,
						IfMatchRev: f.visible.Revision, Actor: "admin",
					})
					return err
				}
				f = projectAccessFixtureWithStorage(t, f, wrapped)

				path := fmt.Sprintf("/api/v1/projects/%d/issues/%s", f.public.ID, f.visible.ShortID)
				var status int
				var body []byte
				switch mutation {
				case "metadata":
					status, _, body = f.request(t, http.MethodPost, path+"/metadata", "member",
						map[string]any{"actor": "member", "patch": map[string]any{"race_secret": "should-not-commit"}}, nil)
				case "delete":
					status, _, body = f.request(t, http.MethodPost, path+"/actions/delete", "member",
						map[string]any{"actor": "member"}, map[string]string{
							"X-Kata-Confirm": "DELETE " + f.public.Name + "#" + f.visible.ShortID,
						})
				case "restore":
					status, _, body = f.request(t, http.MethodPost, path+"/actions/restore", "member",
						map[string]any{"actor": "member"}, nil)
				}
				assert.Equal(t, http.StatusNotFound, status, string(body))
				assert.NotContains(t, string(body), projectAccessCanary)
				assert.NotContains(t, string(body), hidden.UID)
				current, err := store.IssueByID(t.Context(), f.visible.ID)
				require.NoError(t, err)
				assert.Equal(t, hidden.UID, current.ProjectUID, "the owner-side move should remain committed")
				switch mutation {
				case "metadata":
					assert.NotContains(t, string(current.Metadata), "should-not-commit")
				case "delete":
					assert.Nil(t, current.DeletedAt, "the denied delete must not change issue visibility")
				}
			})
		}
	})
}

func TestMoveIssueProjectAdvancesProjectAccessRevision(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newProjectAccessFixture(t, store)
		target, err := store.CreateProject(t.Context(), "move-target-project")
		require.NoError(t, err)
		before, err := store.ProjectAccessRevision(t.Context())
		require.NoError(t, err)
		_, err = store.MoveIssueProject(t.Context(), db.MoveIssueProjectIn{
			IssueID: f.visible.ID, FromProjectID: f.public.ID, ToProjectID: target.ID,
			IfMatchRev: f.visible.Revision, Actor: "admin",
		})
		require.NoError(t, err)
		after, err := store.ProjectAccessRevision(t.Context())
		require.NoError(t, err)
		assert.Greater(t, after, before)
	})
}

func TestProjectAccessAuthorizedMemberMoveReturnsCommittedResponse(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newProjectAccessFixture(t, store, "authorized-move")
		target, err := store.CreateProject(t.Context(), "authorized-move-target")
		require.NoError(t, err)
		status, headers, body := f.request(t, http.MethodPost,
			fmt.Sprintf("/api/v1/projects/%d/issues/%s/actions/move", f.public.ID, f.visible.ShortID),
			"member", map[string]any{
				"actor": "member", "to_project_uid": target.UID,
			}, map[string]string{"If-Match": fmt.Sprintf(`"rev-%d"`, f.visible.Revision)})
		require.Equal(t, http.StatusOK, status, string(body))
		assert.Equal(t, `"rev-2"`, headers.Get("ETag"))
		assert.Contains(t, string(body), target.UID)
		current, err := store.IssueByID(t.Context(), f.visible.ID)
		require.NoError(t, err)
		assert.Equal(t, target.UID, current.ProjectUID)
	})
}

func TestProjectAccessRechecksInitialLinkPeerAfterMove(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newProjectAccessFixture(t, store, "initial-link")
		peer, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{
			ProjectID: f.public.ID, Title: "Initial link peer", Author: "member",
		})
		require.NoError(t, err)
		hidden := projectAccessHiddenProject(t, store, "initial-link-hidden")
		wrapped := &projectAccessBeforeCreateIssueStore{Storage: store}
		wrapped.before = func(_ context.Context, params db.CreateIssueParams) error {
			if params.Title != "Create with moved peer" {
				return nil
			}
			_, err := store.MoveIssueProject(t.Context(), db.MoveIssueProjectIn{
				IssueID: peer.ID, FromProjectID: f.public.ID, ToProjectID: hidden.ID,
				IfMatchRev: peer.Revision, Actor: "admin",
			})
			return err
		}
		f = projectAccessFixtureWithStorage(t, f, wrapped)
		status, _, body := f.request(t, http.MethodPost,
			fmt.Sprintf("/api/v1/projects/%d/issues", f.public.ID), "member", map[string]any{
				"actor": "member", "title": "Create with moved peer",
				"links": []map[string]any{{"type": "related", "to_ref": peer.ShortID}},
			}, nil)
		assert.Equal(t, http.StatusNotFound, status, string(body))
		issues, err := store.ListIssues(t.Context(), db.ListIssuesParams{ProjectID: f.public.ID})
		require.NoError(t, err)
		require.Len(t, issues, 1, "the moved peer leaves the public project and failed creation leaves no new issue")
		assert.Equal(t, f.visible.UID, issues[0].UID)
		links, err := store.LinksByIssue(t.Context(), peer.ID)
		require.NoError(t, err)
		assert.Empty(t, links, "a hidden peer cannot be linked by initial creation")
	})
}

func TestProjectAccessRechecksCurrentPeerProjectBeforeLinkWrites(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		for _, mutation := range []string{"atomic", "dedicated"} {
			t.Run(mutation, func(t *testing.T) {
				f := newProjectAccessFixture(t, store, "link-"+mutation)
				peer, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{
					ProjectID: f.public.ID, Title: "Peer task", Author: "member",
				})
				require.NoError(t, err)
				hidden := projectAccessHiddenProject(t, store, "link-hidden-"+mutation)
				wrapped := &projectAccessBeforeLinkStore{Storage: store}
				wrapped.before = func(context.Context) error {
					_, err := store.MoveIssueProject(t.Context(), db.MoveIssueProjectIn{
						IssueID: peer.ID, FromProjectID: f.public.ID, ToProjectID: hidden.ID,
						IfMatchRev: peer.Revision, Actor: "admin",
					})
					return err
				}
				f = projectAccessFixtureWithStorage(t, f, wrapped)
				path := fmt.Sprintf("/api/v1/projects/%d/issues/%s", f.public.ID, f.visible.ShortID)
				var status int
				var body []byte
				if mutation == "atomic" {
					status, _, body = f.request(t, http.MethodPatch, path, "member", map[string]any{
						"actor": "member", "links_delta": map[string]any{"add_related": []string{peer.ShortID}},
					}, nil)
				} else {
					status, _, body = f.request(t, http.MethodPost, path+"/links", "member", map[string]any{
						"actor": "member", "type": "related", "to_ref": peer.ShortID,
					}, nil)
				}
				assert.Equal(t, http.StatusNotFound, status, string(body))
				assert.NotContains(t, string(body), hidden.UID)
				links, err := store.LinksByIssue(t.Context(), f.visible.ID)
				require.NoError(t, err)
				assert.Empty(t, links, "a peer moved into a hidden project cannot be linked")
			})
		}
	})
}

func TestProjectAccessRechecksMovedIssueInsideEditTransaction(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newProjectAccessFixture(t, store)
		hidden, err := store.CreateProject(t.Context(), "hidden-project")
		require.NoError(t, err)
		otherTeam, _, err := store.CreateTeam(t.Context(), "other-team", "admin")
		require.NoError(t, err)
		_, err = store.SetTeamMembership(t.Context(), otherTeam.UID, "other-member", true, "admin")
		require.NoError(t, err)
		_, _, err = store.SetProjectAccessPolicy(t.Context(), db.ProjectAccessPolicy{
			ProjectUID: hidden.UID, Visibility: "teams", TeamUIDs: []string{otherTeam.UID},
		}, "admin")
		require.NoError(t, err)

		wrapped := &projectAccessBeforeEditStore{Storage: store}
		wrapped.before = func(_ context.Context, params db.EditIssueAtomicParams) error {
			if params.IssueID != f.visible.ID {
				return nil
			}
			_, err := store.MoveIssueProject(t.Context(), db.MoveIssueProjectIn{
				IssueID: f.visible.ID, FromProjectID: f.public.ID, ToProjectID: hidden.ID,
				IfMatchRev: f.visible.Revision, Actor: "admin",
			})
			return err
		}
		f = projectAccessFixtureWithStorage(t, f, wrapped)

		status, _, body := f.request(t, http.MethodPatch,
			fmt.Sprintf("/api/v1/projects/%d/issues/%s", f.public.ID, f.visible.ShortID),
			"member", map[string]any{"actor": "member", "title": "Unauthorized edit"}, nil)
		assert.Equal(t, http.StatusNotFound, status, string(body))
		assert.NotContains(t, string(body), hidden.UID)
		assert.NotContains(t, string(body), hidden.Name)
		assert.NotContains(t, string(body), "Unauthorized edit")

		current, err := store.IssueByID(t.Context(), f.visible.ID)
		require.NoError(t, err)
		project, err := store.ProjectByID(t.Context(), current.ProjectID)
		require.NoError(t, err)
		assert.Equal(t, hidden.UID, project.UID, "the owner-side move should remain committed")
		assert.Equal(t, f.visible.Title, current.Title, "the denied member edit must not change issue content")
	})
}

func TestProjectAccessRechecksMovedIssueInsideCommentTransaction(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newProjectAccessFixture(t, store)
		hidden, err := store.CreateProject(t.Context(), "comment-hidden-project")
		require.NoError(t, err)
		otherTeam, _, err := store.CreateTeam(t.Context(), "comment-owner-team", "admin")
		require.NoError(t, err)
		_, err = store.SetTeamMembership(t.Context(), otherTeam.UID, "other-member", true, "admin")
		require.NoError(t, err)
		_, _, err = store.SetProjectAccessPolicy(t.Context(), db.ProjectAccessPolicy{
			ProjectUID: hidden.UID, Visibility: "teams", TeamUIDs: []string{otherTeam.UID},
		}, "admin")
		require.NoError(t, err)

		wrapped := projectAccessBeforeComment{Storage: store, before: func() {
			_, err := store.MoveIssueProject(t.Context(), db.MoveIssueProjectIn{
				IssueID: f.visible.ID, FromProjectID: f.public.ID, ToProjectID: hidden.ID,
				IfMatchRev: f.visible.Revision, Actor: "admin",
			})
			require.NoError(t, err)
		}}
		f = projectAccessFixtureWithStorage(t, f, wrapped)

		status, _, body := f.request(t, http.MethodPost,
			fmt.Sprintf("/api/v1/projects/%d/issues/%s/comments", f.public.ID, f.visible.ShortID),
			"member", map[string]string{"actor": "member", "body": "Unauthorized comment"}, nil)
		assert.Equal(t, http.StatusNotFound, status, string(body))
		assert.NotContains(t, string(body), hidden.UID)
		assert.NotContains(t, string(body), hidden.Name)

		comments, err := store.CommentsByIssue(t.Context(), f.visible.ID)
		require.NoError(t, err)
		assert.Empty(t, comments, "the member comment must not commit after the issue moves")
	})
}

func TestProjectAccessRechecksMovedIssueInsideReopenTransaction(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newProjectAccessFixture(t, store)
		hidden, err := store.CreateProject(t.Context(), "reopen-hidden-project")
		require.NoError(t, err)
		otherTeam, _, err := store.CreateTeam(t.Context(), "reopen-owner-team", "admin")
		require.NoError(t, err)
		_, err = store.SetTeamMembership(t.Context(), otherTeam.UID, "other-member", true, "admin")
		require.NoError(t, err)
		_, _, err = store.SetProjectAccessPolicy(t.Context(), db.ProjectAccessPolicy{
			ProjectUID: hidden.UID, Visibility: "teams", TeamUIDs: []string{otherTeam.UID},
		}, "admin")
		require.NoError(t, err)
		_, _, _, err = store.CloseIssue(t.Context(), f.visible.ID, "done", "member", "Closed issue", nil)
		require.NoError(t, err)

		wrapped := &projectAccessBeforeReopenStore{Storage: store, before: func(issueID int64) {
			require.Equal(t, f.visible.ID, issueID)
			_, err := store.MoveIssueProject(t.Context(), db.MoveIssueProjectIn{
				IssueID: issueID, FromProjectID: f.public.ID, ToProjectID: hidden.ID,
				IfMatchRev: f.visible.Revision + 1, Actor: "admin",
			})
			require.NoError(t, err)
		}}
		f = projectAccessFixtureWithStorage(t, f, wrapped)

		status, _, body := f.request(t, http.MethodPost,
			fmt.Sprintf("/api/v1/projects/%d/issues/%s/actions/reopen", f.public.ID, f.visible.ShortID),
			"member", map[string]string{"actor": "member"}, nil)
		assert.Equal(t, http.StatusNotFound, status, string(body))
		assert.NotContains(t, string(body), hidden.UID)
		assert.NotContains(t, string(body), hidden.Name)

		current, err := store.IssueByID(t.Context(), f.visible.ID)
		require.NoError(t, err)
		project, err := store.ProjectByID(t.Context(), current.ProjectID)
		require.NoError(t, err)
		assert.Equal(t, hidden.UID, project.UID, "the owner-side move should remain committed")
		assert.Equal(t, "closed", current.Status, "the member must not reopen the moved issue")
	})
}

func TestProjectAccessDoesNotHydrateCommentsAfterIssueMoves(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newProjectAccessFixture(t, store)
		hidden, err := store.CreateProject(t.Context(), "comment-read-hidden-project")
		require.NoError(t, err)
		otherTeam, _, err := store.CreateTeam(t.Context(), "comment-read-owner-team", "admin")
		require.NoError(t, err)
		_, err = store.SetTeamMembership(t.Context(), otherTeam.UID, "other-member", true, "admin")
		require.NoError(t, err)
		_, _, err = store.SetProjectAccessPolicy(t.Context(), db.ProjectAccessPolicy{
			ProjectUID: hidden.UID, Visibility: "teams", TeamUIDs: []string{otherTeam.UID},
		}, "admin")
		require.NoError(t, err)

		wrapped := &projectAccessBeforeCommentsReadStore{Storage: store, before: func(issueID int64) {
			_, err := store.MoveIssueProject(t.Context(), db.MoveIssueProjectIn{
				IssueID: issueID, FromProjectID: f.public.ID, ToProjectID: hidden.ID,
				IfMatchRev: f.visible.Revision, Actor: "admin",
			})
			require.NoError(t, err)
			_, _, err = store.CreateComment(t.Context(), db.CreateCommentParams{
				IssueID: issueID, Author: "other-member", Body: projectAccessCanary,
			})
			require.NoError(t, err)
		}}
		f = projectAccessFixtureWithStorage(t, f, wrapped)

		status, _, body := f.request(t, http.MethodGet,
			fmt.Sprintf("/api/v1/projects/%d/issues/%s", f.public.ID, f.visible.ShortID),
			"member", nil, nil)
		assert.Equal(t, http.StatusNotFound, status, string(body))
		assert.NotContains(t, string(body), projectAccessCanary,
			"the response must be discarded when the hydrated issue has moved outside the caller's projects")
	})
}

func TestProjectAccessPreservesSoftDeletedShowReads(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newProjectAccessFixture(t, store)
		deleted, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{
			ProjectID: f.public.ID, Title: "Readable deleted issue", Author: "member",
		})
		require.NoError(t, err)
		_, _, _, err = store.SoftDeleteIssue(t.Context(), deleted.ID, "member")
		require.NoError(t, err)
		status, _, body := f.request(t, http.MethodGet,
			fmt.Sprintf("/api/v1/projects/%d/issues/%s?include_deleted=true", f.public.ID, deleted.ShortID),
			"member", nil, nil)
		assert.Equal(t, http.StatusOK, status, string(body))
		assert.Contains(t, string(body), "Readable deleted issue")

		live, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{
			ProjectID: f.public.ID, Title: "Live issue with deleted peer", Author: "member",
		})
		require.NoError(t, err)
		deletedPeer, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{
			ProjectID: f.public.ID, Title: "Deleted linked peer", Author: "member",
		})
		require.NoError(t, err)
		_, err = store.CreateLink(t.Context(), db.CreateLinkParams{
			FromIssueID: live.ID, ToIssueID: deletedPeer.ID, Type: "related", Author: "member",
		})
		require.NoError(t, err)
		_, _, _, err = store.SoftDeleteIssue(t.Context(), deletedPeer.ID, "member")
		require.NoError(t, err)
		status, _, body = f.request(t, http.MethodGet,
			fmt.Sprintf("/api/v1/projects/%d/issues/%s", f.public.ID, live.ShortID),
			"member", nil, nil)
		assert.Equal(t, http.StatusOK, status, string(body))
		assert.Contains(t, string(body), "Live issue with deleted peer")
	})
}

func TestProjectAccessEventReferenceResetsPollAndSSE(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newProjectAccessFixture(t, store)
		hidden, err := store.CreateProject(t.Context(), "event-reference-hidden-project")
		require.NoError(t, err)
		otherTeam, _, err := store.CreateTeam(t.Context(), "event-reference-owner-team", "admin")
		require.NoError(t, err)
		_, err = store.SetTeamMembership(t.Context(), otherTeam.UID, "other-member", true, "admin")
		require.NoError(t, err)
		_, _, err = store.SetProjectAccessPolicy(t.Context(), db.ProjectAccessPolicy{
			ProjectUID: hidden.UID, Visibility: "teams", TeamUIDs: []string{otherTeam.UID},
		}, "admin")
		require.NoError(t, err)
		hiddenParent, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{
			ProjectID: hidden.ID, Title: "Hidden parent", Author: "other-member",
		})
		require.NoError(t, err)
		_, err = store.CreateLink(t.Context(), db.CreateLinkParams{
			FromIssueID: f.visible.ID, ToIssueID: hiddenParent.ID, Type: "parent", Author: "admin",
		})
		require.NoError(t, err)
		afterID, err := store.MaxEventID(t.Context())
		require.NoError(t, err)
		_, closeEvents, _, err := store.CloseIssueWithEvents(
			t.Context(), f.visible.ID, "done", "member", "Visible closure", nil,
		)
		require.NoError(t, err)
		require.Len(t, closeEvents, 1)
		require.Contains(t, closeEvents[0].Payload, hiddenParent.UID,
			"the close event must exercise a hidden parent reference")

		status, _, body := f.request(t, http.MethodGet,
			fmt.Sprintf("/api/v1/projects/%d/events?after_id=%d", f.public.ID, afterID),
			"member", nil, nil)
		require.Equal(t, http.StatusOK, status, string(body))
		var polled api.PollEventsResponse
		require.NoError(t, json.Unmarshal(body, &polled.Body))
		assert.True(t, polled.Body.ResetRequired,
			"polling must invalidate visible state when a visible close is filtered for a private parent")
		assert.Equal(t, closeEvents[0].ID, polled.Body.ResetAfterID)
		assert.Empty(t, polled.Body.Events)
		assert.NotContains(t, string(body), hiddenParent.UID)
		assert.NotContains(t, string(body), hiddenParent.ShortID)

		streamCtx, cancel := context.WithTimeout(t.Context(), 4*time.Second)
		defer cancel()
		request, err := http.NewRequestWithContext(streamCtx, http.MethodGet,
			fmt.Sprintf("%s/api/v1/events/stream?project_id=%d&after_id=%d", f.server.URL, f.public.ID, afterID), nil)
		require.NoError(t, err)
		request.Header.Set("Authorization", "Bearer member-test-token")
		request.Header.Set("Accept", "text/event-stream")
		response, err := f.server.Client().Do(request)
		require.NoError(t, err)
		defer func() { _ = response.Body.Close() }()
		require.Equal(t, http.StatusOK, response.StatusCode)
		frame, ok := newSSEFramer(response.Body).Next(t, 2*time.Second)
		require.True(t, ok, "the open stream must receive an invalidation frame")
		assert.Equal(t, "sync.reset_required", frame.event)
		assert.Equal(t, fmt.Sprint(closeEvents[0].ID), frame.id)
		assert.NotContains(t, frame.data, hiddenParent.UID)
	})
}

func TestProjectAccessNewUnrestrictedProjectRefreshesExistingSSEAdmission(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newProjectAccessFixture(t, store)
		cursor, err := store.MaxEventID(t.Context())
		require.NoError(t, err)
		response, stream := openProjectAccessSSE(t, f, cursor, "member")

		createProjectAccessIssue(t, f, f.public.ID, "member", "Before catalog change")
		first, ok := stream.Next(t, 2*time.Second)
		require.True(t, ok, "the authenticated stream should be subscribed before the catalog change")
		assert.Equal(t, "issue.created", first.event)
		cursor, err = store.MaxEventID(t.Context())
		require.NoError(t, err)

		status, _, body := f.request(t, http.MethodPost, "/api/v1/projects", "member",
			map[string]string{"name": "new-shared-project", "actor": "member"}, nil)
		require.True(t, status >= http.StatusOK && status < http.StatusMultipleChoices,
			"create project: status=%d body=%s", status, body)
		project, err := store.ProjectByName(t.Context(), "new-shared-project")
		require.NoError(t, err)
		createProjectAccessIssue(t, f, project.ID, "member", "New project task")
		createProjectAccessIssue(t, f, f.public.ID, "member", "After catalog change")

		leaked, ok := stream.Next(t, 2*time.Second)
		require.False(t, ok,
			"the old stream must close when an unrestricted project changes the admitted project set; got %s", leaked.event)

		_, refreshed := openProjectAccessSSE(t, f, cursor, "member")
		created, ok := refreshed.Next(t, 2*time.Second)
		require.True(t, ok, "a newly admitted stream should receive the new project catalog event")
		assert.Equal(t, "project.created", created.event)
		assert.Contains(t, created.data, project.UID)
		issue, ok := refreshed.Next(t, 2*time.Second)
		require.True(t, ok, "a newly admitted stream should receive the new project's issue event")
		assert.Equal(t, "issue.created", issue.event)
		assert.Contains(t, issue.data, project.UID)
		_ = response.Body.Close()
	})
}

func TestProjectAccessRestoredUnrestrictedProjectRefreshesExistingSSEAdmission(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newProjectAccessFixture(t, store)
		project, err := store.CreateProject(t.Context(), "restored-shared-project")
		require.NoError(t, err)
		_, _, err = store.RemoveProject(t.Context(), db.RemoveProjectParams{
			ProjectID: project.ID, Actor: "admin",
		})
		require.NoError(t, err)

		cursor, err := store.MaxEventID(t.Context())
		require.NoError(t, err)
		response, stream := openProjectAccessSSE(t, f, cursor, "member")
		createProjectAccessIssue(t, f, f.public.ID, "member", "Before restore")
		first, ok := stream.Next(t, 2*time.Second)
		require.True(t, ok, "the authenticated stream should be subscribed before the restore")
		assert.Equal(t, "issue.created", first.event)
		cursor, err = store.MaxEventID(t.Context())
		require.NoError(t, err)

		restoredProject, event, changed, err := store.RestoreProject(t.Context(), project.ID, "admin")
		require.NoError(t, err)
		require.True(t, changed)
		require.NotNil(t, event)
		f.broadcaster.Broadcast(daemon.NewEventMsg(restoredProject.ID, *event))
		_, issueEvent, err := store.CreateIssue(t.Context(), db.CreateIssueParams{
			ProjectID: restoredProject.ID, Title: "Restored project task", Author: "member",
		})
		require.NoError(t, err)
		f.broadcaster.Broadcast(daemon.NewEventMsg(restoredProject.ID, issueEvent))
		createProjectAccessIssue(t, f, f.public.ID, "member", "After restore")

		leaked, ok := stream.Next(t, 2*time.Second)
		require.False(t, ok,
			"the old stream must close when restoring an unrestricted project changes its admitted set; got %s", leaked.event)

		_, refreshed := openProjectAccessSSE(t, f, cursor, "member")
		restored, ok := refreshed.Next(t, 2*time.Second)
		require.True(t, ok, "a newly admitted stream should receive the restore event")
		assert.Equal(t, "project.restored", restored.event)
		assert.Contains(t, restored.data, project.UID)
		issue, ok := refreshed.Next(t, 2*time.Second)
		require.True(t, ok, "a newly admitted stream should receive events from the restored project")
		assert.Equal(t, "issue.created", issue.event)
		assert.Contains(t, issue.data, project.UID)
		_ = response.Body.Close()
	})
}

func TestProjectAccessMoveResetsFormerProjectPollAndSSE(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newProjectAccessFixture(t, store)
		hidden, err := store.CreateProject(t.Context(), "move-hidden-project")
		require.NoError(t, err)
		otherTeam, _, err := store.CreateTeam(t.Context(), "move-owner-team", "admin")
		require.NoError(t, err)
		_, err = store.SetTeamMembership(t.Context(), otherTeam.UID, "other-member", true, "admin")
		require.NoError(t, err)
		_, _, err = store.SetProjectAccessPolicy(t.Context(), db.ProjectAccessPolicy{
			ProjectUID: hidden.UID, Visibility: "teams", TeamUIDs: []string{otherTeam.UID},
		}, "admin")
		require.NoError(t, err)
		afterID, err := store.MaxEventID(t.Context())
		require.NoError(t, err)
		moved, err := store.MoveIssueProject(t.Context(), db.MoveIssueProjectIn{
			IssueID: f.visible.ID, FromProjectID: f.public.ID, ToProjectID: hidden.ID,
			IfMatchRev: f.visible.Revision, Actor: "admin",
		})
		require.NoError(t, err)

		status, _, body := f.request(t, http.MethodGet,
			fmt.Sprintf("/api/v1/projects/%d/events?after_id=%d", f.public.ID, afterID),
			"member", nil, nil)
		require.Equal(t, http.StatusOK, status, string(body))
		var polled api.PollEventsResponse
		require.NoError(t, json.Unmarshal(body, &polled.Body))
		assert.True(t, polled.Body.ResetRequired)
		assert.Equal(t, moved.EventID, polled.Body.ResetAfterID)
		assert.Empty(t, polled.Body.Events)
		assert.NotContains(t, string(body), hidden.UID)
		assert.NotContains(t, string(body), hidden.Name)

		streamCtx, cancel := context.WithTimeout(t.Context(), 4*time.Second)
		defer cancel()
		request, err := http.NewRequestWithContext(streamCtx, http.MethodGet,
			fmt.Sprintf("%s/api/v1/events/stream?project_id=%d&after_id=%d", f.server.URL, f.public.ID, afterID), nil)
		require.NoError(t, err)
		request.Header.Set("Authorization", "Bearer member-test-token")
		request.Header.Set("Accept", "text/event-stream")
		response, err := f.server.Client().Do(request)
		require.NoError(t, err)
		defer func() { _ = response.Body.Close() }()
		require.Equal(t, http.StatusOK, response.StatusCode)
		frame, ok := newSSEFramer(response.Body).Next(t, 2*time.Second)
		require.True(t, ok)
		assert.Equal(t, db.ProjectScopeResetEventType, frame.event)
		assert.Equal(t, fmt.Sprint(moved.EventID), frame.id)
		assert.NotContains(t, frame.data, hidden.UID)
		assert.NotContains(t, frame.data, hidden.Name)
	})
}

func TestProjectAccessRechecksExistingParentInsideReplacementTransaction(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newProjectAccessFixture(t, store)
		hiddenProject, err := store.CreateProject(t.Context(), "hidden-parent-project")
		require.NoError(t, err)
		otherTeam, _, err := store.CreateTeam(t.Context(), "parent-owner-team", "admin")
		require.NoError(t, err)
		_, err = store.SetTeamMembership(t.Context(), otherTeam.UID, "other-member", true, "admin")
		require.NoError(t, err)
		_, _, err = store.SetProjectAccessPolicy(t.Context(), db.ProjectAccessPolicy{
			ProjectUID: hiddenProject.UID, Visibility: "teams", TeamUIDs: []string{otherTeam.UID},
		}, "admin")
		require.NoError(t, err)
		oldParent, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{
			ProjectID: f.public.ID, Title: "Visible old parent", Author: "member",
		})
		require.NoError(t, err)
		newParent, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{
			ProjectID: f.public.ID, Title: "Visible replacement parent", Author: "member",
		})
		require.NoError(t, err)
		hiddenParent, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{
			ProjectID: hiddenProject.ID, Title: "Hidden parent endpoint", Author: "admin",
		})
		require.NoError(t, err)
		initialParentID := oldParent.ID
		_, err = store.EditIssueAtomic(t.Context(), db.EditIssueAtomicParams{
			IssueID: f.visible.ID, Actor: "member", SetParent: &initialParentID,
		})
		require.NoError(t, err)
		beforeID, err := store.MaxEventID(t.Context())
		require.NoError(t, err)

		wrapped := &projectAccessBeforeEditStore{Storage: store}
		wrapped.before = func(_ context.Context, params db.EditIssueAtomicParams) error {
			if params.IssueID != f.visible.ID {
				return nil
			}
			ownerParentID := hiddenParent.ID
			_, err := store.EditIssueAtomic(t.Context(), db.EditIssueAtomicParams{
				IssueID: f.visible.ID, Actor: "admin", SetParent: &ownerParentID,
			})
			return err
		}
		f = projectAccessFixtureWithStorage(t, f, wrapped)

		status, _, body := f.request(t, http.MethodPatch,
			fmt.Sprintf("/api/v1/projects/%d/issues/%s", f.public.ID, f.visible.ShortID),
			"member", map[string]any{
				"actor": "member", "links_delta": map[string]any{"set_parent": newParent.ShortID},
			}, nil)
		assert.Equal(t, http.StatusNotFound, status, string(body))
		for _, hidden := range []string{hiddenProject.Name, hiddenProject.UID,
			hiddenParent.Title, hiddenParent.ShortID, hiddenParent.UID} {
			assert.NotContains(t, string(body), hidden, "denied replacement must not expose the actual old endpoint")
		}

		parent, err := store.ParentOf(t.Context(), f.visible.ID)
		require.NoError(t, err)
		assert.Equal(t, hiddenParent.ID, parent.ToIssueID, "the denied replacement must leave the owner-side parent intact")
		events, err := store.EventsAfter(t.Context(), db.EventsAfterParams{AfterID: beforeID, Limit: 100})
		require.NoError(t, err)
		for _, event := range events {
			assert.NotEqual(t, "member", event.Actor, "a denied replacement must emit no member-authored event")
		}
		require.Len(t, events, 1, "only the owner-side interleaving event should be committed")
		assert.Equal(t, "admin", events[0].Actor)
	})
}

// R2 requires project policy across selectors, collections and mutation errors.
// The route inventory expands this table as each registered operation is fenced.
func TestProjectAccessRouteCoverage(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newProjectAccessFixture(t, store)
		root := fmt.Sprintf("/api/v1/projects/%d", f.private.ID)
		issue := root + "/issues/" + f.issue.ShortID
		for _, route := range []struct{ name, path string }{
			{"project", root}, {"issue list", root + "/issues"}, {"issue short selector", issue},
			{"issue UID selector", "/api/v1/issues/" + f.issue.UID}, {"graph", issue + "/graph"},
			{"search", root + "/search?q=canary&mode=lexical"}, {"labels", root + "/labels"}, {"ready", root + "/ready"},
			{"events", root + "/events"}, {"digest", root + "/digest?since=2026-01-01T00:00:00Z"},
			{"audit", fmt.Sprintf("/api/v1/audit/closes?project_id=%d", f.private.ID)},
			{"UI reference", fmt.Sprintf("/api/v1/ui/issue-reference?project_id=%d&ref=%s", f.private.ID, f.issue.ShortID)},
			{"UI snapshot", "/api/v1/ui/snapshot?project_uid=" + f.private.UID},
		} {
			t.Run(route.name, func(t *testing.T) {
				for _, actor := range []string{"nonmember", "member", "admin", ""} {
					t.Run(actor, func(t *testing.T) {
						status, _, body := f.request(t, http.MethodGet, route.path, actor, nil, nil)
						want := http.StatusOK
						if actor == "nonmember" {
							want = http.StatusNotFound
						}
						if actor == "" {
							want = http.StatusUnauthorized
						}
						assert.Equal(t, want, status, string(body))
						if actor == "nonmember" || actor == "" {
							assert.NotContains(t, string(body), projectAccessCanary)
							assert.NotContains(t, string(body), f.private.UID)
						}
					})
				}
			})
		}
		for _, path := range []string{"/api/v1/projects?include=stats", "/api/v1/issues?limit=1", "/api/v1/ready?limit=1", "/api/v1/events", "/api/v1/digest?since=2026-01-01T00:00:00Z", "/api/v1/ui/snapshot?limit=1", "/api/v1/ui/references?q=canary"} {
			t.Run("collection "+path, func(t *testing.T) {
				status, _, body := f.request(t, http.MethodGet, path, "nonmember", nil, nil)
				assert.Equal(t, http.StatusOK, status, string(body))
				assert.NotContains(t, string(body), projectAccessCanary)
				assert.NotContains(t, string(body), f.private.UID)
				if path == "/api/v1/issues?limit=1" || path == "/api/v1/ready?limit=1" {
					assert.Contains(t, string(body), "Visible task", "authorized candidate must survive the limit")
				}
			})
		}
		for _, mutation := range []struct {
			path string
			body any
		}{
			{root + "/issues", map[string]string{"title": "Blocked mutation", "actor": "member"}},
			{issue + "/comments", map[string]string{"body": "Blocked mutation", "actor": "member"}},
		} {
			t.Run("mutation "+mutation.path, func(t *testing.T) {
				status, _, body := f.request(t, http.MethodPost, mutation.path, "nonmember", mutation.body, nil)
				assert.Equal(t, http.StatusNotFound, status, string(body))
				assert.NotContains(t, string(body), projectAccessCanary)
			})
		}
		comments, err := store.CommentsByIssue(t.Context(), f.issue.ID)
		require.NoError(t, err)
		assert.Empty(t, comments)
	})
}

// ETags and both shared caches must separate principals and current policy.
func TestProjectAccessCacheAndETagRevocation(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newProjectAccessFixture(t, store)
		path := "/api/v1/ui/snapshot?view=all-open"
		status, headers, body := f.request(t, http.MethodGet, path, "member", nil, nil)
		require.Equal(t, http.StatusOK, status, string(body))
		require.Contains(t, string(body), projectAccessCanary)
		etag := headers.Get("ETag")
		require.NotEmpty(t, etag)
		status, _, body = f.request(t, http.MethodGet, path, "nonmember", nil, map[string]string{"If-None-Match": etag})
		assert.Equal(t, http.StatusOK, status, string(body))
		assert.NotContains(t, string(body), projectAccessCanary)
		assert.NotContains(t, string(body), f.private.UID)
		_, err := store.SetTeamMembership(t.Context(), f.team.UID, "member", false, "admin")
		require.NoError(t, err)
		status, _, body = f.request(t, http.MethodGet, path, "member", nil, map[string]string{"If-None-Match": etag})
		assert.Equal(t, http.StatusOK, status, string(body))
		assert.NotContains(t, string(body), projectAccessCanary)
		assert.NotContains(t, string(body), f.private.UID)
	})
}

func TestProjectAccessDiagnosticsRequireOwner(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newProjectAccessFixture(t, store)
		for _, path := range []string{"/debug/pprof/", "/debug/pprof/cmdline"} {
			for _, actor := range []string{"", "nonmember", "member", "admin"} {
				status, _, body := f.request(t, http.MethodGet, path, actor, nil, nil)
				if actor == "admin" {
					assert.Equal(t, http.StatusOK, status)
				} else {
					assert.NotEqual(t, http.StatusOK, status, "diagnostics require owner authority")
					assert.NotContains(t, string(body), "goroutine profile")
				}
			}
		}
	})
}

func TestProjectAccessDiagnosticsAllowRemoteStaticOwnerToken(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		server := daemon.NewServer(daemon.ServerConfig{
			DB:   store,
			Auth: config.AuthConfig{Token: "remote-owner-test-token", TrustPrivateNetwork: true}, // #nosec G101 -- synthetic test credential, not a real secret.
		})
		t.Cleanup(func() { require.NoError(t, server.Close()) })

		request := httptest.NewRequest(http.MethodGet, "/debug/pprof/", nil)
		request.RemoteAddr = "192.0.2.10:43210"
		request.Header.Set("Authorization", "Bearer remote-owner-test-token")
		request = request.WithContext(context.WithValue(request.Context(), http.LocalAddrContextKey, &net.TCPAddr{
			IP: net.ParseIP("192.0.2.20"), Port: 7777,
		}))
		recorder := httptest.NewRecorder()
		server.Handler().ServeHTTP(recorder, request)
		assert.Equal(t, http.StatusOK, recorder.Code, "a valid daemon-owner token grants diagnostics on a remote listener")
	})
}

func TestProjectAccessHealthDiagnosticsRequireOwner(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newProjectAccessFixture(t, store)
		for _, actor := range []string{"member", "nonmember", "admin"} {
			status, _, body := f.request(t, http.MethodGet, "/api/v1/health", actor, nil, nil)
			assert.Equal(t, http.StatusOK, status, string(body))
			if actor == "admin" {
				assert.Contains(t, string(body), "db_path")
			} else {
				assert.NotContains(t, string(body), "db_path")
				assert.NotContains(t, string(body), "federation_config")
				assert.NotContains(t, string(body), "embeddings")
			}
		}
	})
}

func TestProjectAccessUnclassifiedOperationFailsClosed(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newProjectAccessFixture(t, store)
		server := daemon.NewServer(daemon.ServerConfig{DB: store, Auth: config.AuthConfig{Token: "bootstrap-test-token", RequireTokenIdentity: true}})
		t.Cleanup(func() { _ = server.Close() })
		huma.Register(server.API(), huma.Operation{OperationID: "futureUnclassifiedResource", Method: http.MethodGet, Path: "/api/v1/unclassified-resource"}, func(context.Context, *struct{}) (*struct{ Body string }, error) {
			return &struct{ Body string }{Body: projectAccessCanary}, nil
		})
		target := httptest.NewServer(server.Handler())
		t.Cleanup(target.Close)
		f.server = target
		status, _, body := f.request(t, http.MethodGet, "/api/v1/unclassified-resource", "member", nil, nil)
		assert.NotEqual(t, http.StatusOK, status, "new operations need an explicit policy classification")
		assert.NotContains(t, string(body), projectAccessCanary)
	})
}

// Revocation after dispatch must be checked in the native mutation transaction,
// including when a host adapter installs its own fence.
type revokeBeforeCreateStore struct {
	db.Storage
	before func()
}

func (s *revokeBeforeCreateStore) CreateIssue(ctx context.Context, p db.CreateIssueParams) (db.Issue, db.Event, error) {
	s.before()
	return s.Storage.CreateIssue(ctx, p)
}

type projectAccessHostController struct{ fence *bool }

type projectAccessFinalHostFence struct {
	called *bool
	deny   bool
}

func (h projectAccessFinalHostFence) Authorize(context.Context, daemon.HostAccessRequest) (daemon.HostAccessDecision, error) {
	return daemon.HostAccessDecision{TransactionFence: func(context.Context, db.Transaction) error {
		*h.called = true
		if h.deny {
			return daemon.ErrHostAccessDenied
		}
		return nil
	}}, nil
}

type projectAccessBeforeComment struct {
	db.Storage
	before func()
}

func (s projectAccessBeforeComment) CreateComment(ctx context.Context, p db.CreateCommentParams) (db.Comment, db.Event, error) {
	s.before()
	return s.Storage.CreateComment(ctx, p)
}

type projectAccessBeforeCommentsReadStore struct {
	db.Storage
	before func(int64)
}

type projectAccessBeforeReopenStore struct {
	db.Storage
	before func(int64)
}

func (s *projectAccessBeforeReopenStore) ReopenIssue(
	ctx context.Context, issueID int64, actor string,
) (db.Issue, *db.Event, bool, error) {
	if s.before != nil {
		before := s.before
		s.before = nil
		before(issueID)
	}
	return s.Storage.ReopenIssue(ctx, issueID, actor)
}

func (s *projectAccessBeforeCommentsReadStore) CommentsByIssue(ctx context.Context, issueID int64) ([]db.Comment, error) {
	if s.before != nil {
		before := s.before
		s.before = nil
		before(issueID)
	}
	return s.Storage.CommentsByIssue(ctx, issueID)
}

type projectAccessDenyHost struct{}

func (projectAccessDenyHost) Authorize(context.Context, daemon.HostAccessRequest) (daemon.HostAccessDecision, error) {
	return daemon.HostAccessDecision{}, daemon.ErrHostAccessDenied
}

func (h projectAccessHostController) Authorize(context.Context, daemon.HostAccessRequest) (daemon.HostAccessDecision, error) {
	return daemon.HostAccessDecision{Revalidate: func(context.Context) error { return nil }, TransactionFence: func(context.Context, db.Transaction) error { *h.fence = true; return nil }}, nil
}
func TestProjectAccessRevocationFence(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newProjectAccessFixture(t, store)
		hostFence := false
		wrapped := &revokeBeforeCreateStore{Storage: store, before: func() {
			_, err := store.SetTeamMembership(t.Context(), f.team.UID, "member", false, "admin")
			require.NoError(t, err)
		}}
		server := daemon.NewServer(daemon.ServerConfig{DB: wrapped, HostAccess: projectAccessHostController{&hostFence}})
		t.Cleanup(func() { _ = server.Close() })
		request := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/v1/projects/%d/issues", f.private.ID), strings.NewReader(`{"title":"Revoked mutation","actor":"member","force_new":true}`))
		request.Header.Set("Content-Type", "application/json")
		request = request.WithContext(daemon.WithPrincipal(request.Context(), daemon.Principal{Kind: daemon.PrincipalHost, Actor: "member", Subject: "host-member"}))
		recorder := httptest.NewRecorder()
		server.Handler().ServeHTTP(recorder, request)
		assert.Equal(t, http.StatusNotFound, recorder.Code, recorder.Body.String())
		issues, err := store.ListIssues(t.Context(), db.ListIssuesParams{ProjectID: f.private.ID})
		require.NoError(t, err)
		assert.Len(t, issues, 1, "revoked write cannot commit")
	})
}

func TestProjectAccessHostSubtreeProjectFenceIntersection(t *testing.T) {
	for _, boundary := range []string{"allowed", "host", "subtree", "project"} {
		t.Run(boundary, func(t *testing.T) {
			projectAccessBackends(t, func(t *testing.T, store db.Storage) {
				f := newProjectAccessFixture(t, store)
				expires := time.Now().Add(time.Hour)
				scope := &db.APITokenScope{Kind: db.APITokenScopeIssueSubtree, ProjectUID: f.private.UID, RootIssueUID: f.issue.UID}
				//nolint:gosec // This is a deterministic fixture credential, never an operational secret.
				token, _, err := store.CreateAPIToken(t.Context(), db.CreateAPITokenParams{PlaintextToken: "intersected-token", Actor: "member", AdminActor: "admin", Scope: scope, ExpiresAt: &expires})
				require.NoError(t, err)
				wrapped := projectAccessBeforeComment{Storage: store, before: func() {
					switch boundary {
					case "subtree":
						_, _, err := store.RevokeAPIToken(t.Context(), token.ID, "admin")
						require.NoError(t, err)
					case "project":
						_, err := store.SetTeamMembership(t.Context(), f.team.UID, "member", false, "admin")
						require.NoError(t, err)
					}
				}}
				called := false
				server := daemon.NewServer(daemon.ServerConfig{DB: wrapped, HostAccess: projectAccessFinalHostFence{called: &called, deny: boundary == "host"}})
				t.Cleanup(func() { require.NoError(t, server.Close()) })
				request := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/v1/projects/%d/issues/%s/comments", f.private.ID, f.issue.ShortID), strings.NewReader(`{"body":"Intersected write","actor":"member"}`))
				request.Header.Set("Content-Type", "application/json")
				// Host principals do not carry native DB-token scopes. Install the
				// admitted native subtree fence on the incoming context, as a host
				// integration does when composing its additional domain grant.
				ctx := db.WithIssueScopeTargets(request.Context())
				db.RecordIssueScopeTarget(ctx, f.issue.ID)
				ctx = db.WithAdditionalTransactionFence(ctx, func(ctx context.Context, tx db.Transaction) error {
					err := store.IssueScopedTokenTransactionFence(token)(ctx, tx)
					if errors.Is(err, db.ErrNotFound) {
						return api.NewError(http.StatusUnauthorized, "unauthorized", "authentication required", "", nil)
					}
					return err
				})
				request = request.WithContext(daemon.WithPrincipal(ctx, daemon.Principal{Kind: daemon.PrincipalHost, Actor: "member", Subject: "host-member"}))
				recorder := httptest.NewRecorder()
				server.Handler().ServeHTTP(recorder, request)
				comments, err := store.CommentsByIssue(t.Context(), f.issue.ID)
				require.NoError(t, err)
				if boundary == "allowed" {
					assert.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
					assert.Len(t, comments, 1)
					assert.True(t, called, "host fence remains in the final transaction")
				} else {
					assert.Contains(t, []int{http.StatusNotFound, http.StatusUnauthorized}, recorder.Code, recorder.Body.String())
					assert.Empty(t, comments, "every rejected grant must prevent the commit")
				}
			})
		})
	}
}
func TestProjectAccessStreamRevocation(t *testing.T) {
	for _, wakeup := range []string{"event", "reset", "heartbeat"} {
		t.Run(wakeup, func(t *testing.T) {
			projectAccessBackends(t, func(t *testing.T, store db.Storage) {
				f := newProjectAccessFixture(t, store)
				ctx, cancel := context.WithTimeout(t.Context(), 35*time.Second)
				defer cancel()
				req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s/api/v1/events/stream?project_id=%d", f.server.URL, f.private.ID), nil)
				require.NoError(t, err)
				req.Header.Set("Authorization", "Bearer member-test-token")
				req.Header.Set("Accept", "text/event-stream")
				resp, err := f.server.Client().Do(req)
				require.NoError(t, err)
				defer func() { _ = resp.Body.Close() }()
				require.Equal(t, http.StatusOK, resp.StatusCode)
				reader := bufio.NewReader(resp.Body)
				for {
					line, err := reader.ReadString('\n')
					require.NoError(t, err)
					if strings.Contains(line, projectAccessCanary) {
						break
					}
				}
				// Finish the admitted frame before establishing the revocation boundary.
				for {
					line, err := reader.ReadString('\n')
					require.NoError(t, err)
					if line == "\n" {
						break
					}
				}
				_, err = store.SetTeamMembership(t.Context(), f.team.UID, "member", false, "admin")
				require.NoError(t, err)
				publisher := daemon.NewEventPublisher(f.broadcaster, nil)
				switch wakeup {
				case "event":
					_, event, err := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: f.private.ID, Title: "post-revocation-canary", Author: "member"})
					require.NoError(t, err)
					publisher.Event(f.private.ID, event)
				case "reset":
					publisher.Reset(f.private.ID, 999999)
				}
				raw, err := io.ReadAll(reader)
				assert.Empty(t, string(raw), "no event, reset or heartbeat follows revocation")
				assert.NoError(t, err, "revocation closes stream before the request deadline")
			})
		})
	}
}

// A configured target token cannot widen the source user's project grant.
func TestProjectAccessGatewayPreservesSourceAuthority(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newProjectAccessFixture(t, store)
		calls := 0
		remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { calls++; _, _ = w.Write([]byte(projectAccessCanary)) }))
		t.Cleanup(remote.Close)
		server := daemon.NewServer(daemon.ServerConfig{DB: store, Auth: config.AuthConfig{Token: "bootstrap-test-token", RequireTokenIdentity: true}, WebDaemons: []config.CatalogDaemonConfig{{Name: "remote-example", URL: remote.URL, Token: "broader-test-token"}}})
		t.Cleanup(func() { _ = server.Close() })
		gateway := httptest.NewServer(server.Handler())
		t.Cleanup(gateway.Close)
		f.server = gateway
		status, _, body := f.request(t, http.MethodGet, "/api/v1/ui/daemons", "nonmember", nil, nil)
		assert.Equal(t, http.StatusOK, status, string(body))
		assert.NotContains(t, string(body), remote.URL, "a restricted source cannot enumerate credentialed target authority")
		assert.Equal(t, 0, calls, "roster probes cannot borrow a configured target credential")
		status, _, body = f.request(t, http.MethodGet, "/api/v1/ui/proxy/api/v1/ui/snapshot", "nonmember", nil, map[string]string{"X-Kata-Web-Daemon": "remote-example"})
		assert.Equal(t, http.StatusForbidden, status, string(body))
		assert.NotContains(t, string(body), projectAccessCanary)
		assert.Zero(t, calls, "source policy must be checked before any remote read")
	})
}

func TestProjectAccessEnrollmentAdministrationRequiresOwner(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newProjectAccessFixture(t, store)
		created, err := store.CreateFederationEnrollment(t.Context(), db.CreateFederationEnrollmentParams{SpokeInstanceUID: f.issue.UID, ProjectID: &f.private.ID, Capabilities: "pull,push", Actor: "member"})
		require.NoError(t, err)
		for _, actor := range []string{"member", "nonmember"} {
			status, _, body := f.request(t, http.MethodGet, "/api/v1/federation/enrollments", actor, nil, nil)
			assert.Equal(t, http.StatusNotFound, status, string(body))
			assert.NotContains(t, string(body), f.issue.UID)
			status, _, body = f.request(t, http.MethodPost, fmt.Sprintf("/api/v1/federation/enrollments/%d/revoke", created.Enrollment.ID), actor, map[string]any{}, nil)
			assert.Equal(t, http.StatusNotFound, status, string(body))
			status, _, body = f.request(t, http.MethodPost, "/api/v1/federation/enrollments", actor, map[string]any{"project_id": nil, "spoke_instance_uid": f.issue.UID, "capabilities": "pull,push", "actor": "member"}, nil)
			assert.Equal(t, http.StatusNotFound, status, "ordinary credentials cannot enroll an all-project transport: %s", body)
		}
	})
}

func TestProjectAccessNameAndAliasSelectors(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newProjectAccessFixture(t, store)
		_, err := store.AttachAlias(t.Context(), f.private.ID, "git:example.com/private-project", "git")
		require.NoError(t, err)
		for _, input := range []map[string]any{
			{"name": f.private.Name},
			{"alias": map[string]string{"identity": "git:example.com/private-project", "kind": "git"}},
			{"name": f.private.Name, "alias": map[string]string{"identity": "git:example.com/new-private-alias", "kind": "git"}},
		} {
			status, _, body := f.request(t, http.MethodPost, "/api/v1/projects/resolve", "nonmember", input, nil)
			assert.Equal(t, http.StatusNotFound, status, string(body))
			assert.NotContains(t, string(body), f.private.UID)
		}
		_, err = store.AliasByIdentity(t.Context(), "git:example.com/new-private-alias")
		assert.ErrorIs(t, err, db.ErrNotFound, "denied resolve cannot persist a new alias")
		status, _, body := f.request(t, http.MethodPost, "/api/v1/projects", "nonmember", map[string]string{"name": f.private.Name, "actor": "member"}, nil)
		assert.Equal(t, http.StatusNotFound, status, string(body))
		assert.NotContains(t, string(body), f.private.UID)
		status, _, body = f.request(t, http.MethodPost, "/api/v1/projects/name:"+f.private.Name+"/issues", "nonmember", map[string]any{"title": "Blocked name mutation", "actor": "member", "force_new": true}, nil)
		assert.Equal(t, http.StatusNotFound, status, string(body))
		assert.NotContains(t, string(body), projectAccessCanary)
	})
}

func TestProjectAccessAliasConflictDoesNotExposePrivateProject(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newProjectAccessFixture(t, store)
		_, err := store.AttachAlias(t.Context(), f.private.ID, "git:example.com/private-project", "git")
		require.NoError(t, err)
		status, _, body := f.request(t, http.MethodPost, "/api/v1/projects", "nonmember", map[string]any{"name": "new-project", "actor": "member", "alias": map[string]string{"identity": "git:example.com/private-project", "kind": "git"}}, nil)
		assert.Equal(t, http.StatusNotFound, status, string(body))
		assert.NotContains(t, string(body), "existing_project_id")
		assert.NotContains(t, string(body), f.private.UID)
		_, err = store.ProjectByName(t.Context(), "new-project")
		assert.ErrorIs(t, err, db.ErrNotFound, "denied alias conflict must precede project creation")
	})
}

func TestProjectAccessArchivedProjectNameDoesNotExposeExistence(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newProjectAccessFixture(t, store)
		_, _, err := store.RemoveProject(t.Context(), db.RemoveProjectParams{ProjectID: f.private.ID, Actor: "admin", Force: true})
		require.NoError(t, err)
		status, _, body := f.request(t, http.MethodPost, "/api/v1/projects", "nonmember", map[string]any{"name": f.private.Name, "actor": "member"}, nil)
		assert.Equal(t, http.StatusNotFound, status, string(body))
		assert.NotContains(t, string(body), "deleted_at")
		assert.NotContains(t, string(body), "project_archived")
	})
}

func TestProjectAccessCrossProjectLinksAndEvents(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newProjectAccessFixture(t, store)
		_, _, err := store.CreateLinkAndEvent(t.Context(), db.CreateLinkParams{FromIssueID: f.issue.ID, ToIssueID: f.visible.ID, Type: "blocks", Author: "member"}, db.LinkEventParams{EventType: "issue.linked", EventIssueID: f.visible.ID, FromUID: f.visible.UID, FromShortID: f.visible.ShortID, ToUID: f.issue.UID, ToShortID: f.issue.ShortID, Actor: "member"})
		require.NoError(t, err)
		root := fmt.Sprintf("/api/v1/projects/%d/issues/%s", f.public.ID, f.visible.ShortID)
		for _, path := range []string{root, root + "/graph", fmt.Sprintf("/api/v1/projects/%d/events", f.public.ID), "/api/v1/ui/snapshot?view=all-open&include_graph=true&include_history=true&selected_issue_uid=" + f.visible.UID} {
			status, _, body := f.request(t, http.MethodGet, path, "nonmember", nil, nil)
			assert.Equal(t, http.StatusOK, status, string(body))
			assert.NotContains(t, string(body), projectAccessCanary)
			assert.NotContains(t, string(body), f.private.UID)
			assert.NotContains(t, string(body), f.issue.UID)
		}
	})
}

func TestProjectAccessRelationshipFacetsUseAllowedEndpoints(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newProjectAccessFixture(t, store)
		_, _, err := store.CreateLinkAndEvent(t.Context(), db.CreateLinkParams{FromIssueID: f.issue.ID, ToIssueID: f.visible.ID, Type: "blocks", Author: "member"}, db.LinkEventParams{EventType: "issue.linked", EventIssueID: f.visible.ID, FromUID: f.visible.UID, ToUID: f.issue.UID, Actor: "member"})
		require.NoError(t, err)
		for _, path := range []string{fmt.Sprintf("/api/v1/projects/%d/issues/%s", f.public.ID, f.visible.ShortID), "/api/v1/issues", "/api/v1/ui/snapshot?view=all-open"} {
			status, _, body := f.request(t, http.MethodGet, path, "nonmember", nil, nil)
			assert.Equal(t, http.StatusOK, status, string(body))
			assert.NotContains(t, string(body), `"blocked":true`, "hidden endpoints cannot contribute relationship facets")
		}
		status, _, body := f.request(t, http.MethodGet, fmt.Sprintf("/api/v1/projects/%d/ready", f.public.ID), "nonmember", nil, nil)
		assert.Equal(t, http.StatusOK, status, string(body))
		assert.Contains(t, string(body), f.visible.UID, "private blockers cannot change the authorized candidate collection")
	})
}

// Event payloads retain historical project and peer identities. All referenced
// identities must be authorized even when the event's own project is public.
func TestProjectAccessCompoundAndMovedEvents(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newProjectAccessFixture(t, store)
		peer, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: f.public.ID, Title: "Allowed peer", Author: "member"})
		require.NoError(t, err)
		_, err = store.EditIssueAtomic(t.Context(), db.EditIssueAtomicParams{IssueID: f.visible.ID, Actor: "member", AddRelated: []int64{f.issue.ID, peer.ID}})
		require.NoError(t, err)
		status, _, body := f.request(t, http.MethodGet, fmt.Sprintf("/api/v1/projects/%d/events", f.public.ID), "nonmember", nil, nil)
		assert.Equal(t, http.StatusOK, status, string(body))
		assert.NotContains(t, string(body), f.issue.UID, "every compound peer must be authorized")
		_, err = store.MoveIssueProject(t.Context(), db.MoveIssueProjectIn{IssueID: f.issue.ID, FromProjectID: f.private.ID, ToProjectID: f.public.ID, IfMatchRev: f.issue.Revision, Actor: "member"})
		require.NoError(t, err)
		for _, path := range []string{fmt.Sprintf("/api/v1/projects/%d/events", f.public.ID), "/api/v1/ui/snapshot?view=all-open&include_history=true&selected_issue_uid=" + f.visible.UID} {
			status, _, body := f.request(t, http.MethodGet, path, "nonmember", nil, nil)
			assert.Equal(t, http.StatusOK, status, string(body))
			assert.NotContains(t, string(body), f.private.UID, "historical project identities remain private")
		}
		// Moving a public issue into a private project also hides its earlier
		// public events, which carry the original title and issue UID.
		_, err = store.MoveIssueProject(t.Context(), db.MoveIssueProjectIn{IssueID: peer.ID, FromProjectID: f.public.ID, ToProjectID: f.private.ID, IfMatchRev: peer.Revision, Actor: "member"})
		require.NoError(t, err)
		status, _, body = f.request(t, http.MethodGet, fmt.Sprintf("/api/v1/projects/%d/events", f.public.ID), "nonmember", nil, nil)
		assert.Equal(t, http.StatusOK, status, string(body))
		assert.NotContains(t, string(body), peer.UID)
	})
}

// Every registered project path denies a nonmember before input decoding or
// dispatch, including administration, claims, recurrences and integrations.
func TestProjectAccessRegisteredProjectOperations(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newProjectAccessFixture(t, store)
		checked := 0
		for path, item := range f.api.OpenAPI().Paths {
			if !strings.Contains(path, "{project_id}") {
				continue
			}
			for _, op := range []*huma.Operation{item.Get, item.Post, item.Put, item.Patch, item.Delete} {
				if op == nil {
					continue
				}
				checked++
				t.Run(op.OperationID, func(t *testing.T) {
					route := strings.NewReplacer("{project_id}", fmt.Sprint(f.private.ID), "{ref}", f.issue.ShortID, "{uid}", f.issue.UID, "{id}", "1", "{comment_ref}", "1", "{label}", "example-label", "{link_id}", "1", "{alias_id}", "1", "{quarantine_id}", "1", "{recurrence_uid}", f.issue.UID, "{provider}", "example-provider").Replace(path)
					var requestBody any
					if op.OperationID == "disconnectRelayEnrollment" {
						requestBody = map[string]string{"spoke_instance_uid": "01J00000000000000000000001"}
					}
					status, _, body := f.request(t, op.Method, route, "nonmember", requestBody, nil)
					want := http.StatusNotFound
					// Body-bearing federation routes authenticate enrollment before
					// Huma dispatch or reading input. Other project routes retain 404.
					switch op.OperationID {
					case "ingestFederationProjectEvents", "acceptRelayDeliveries", "ackRelayDeliveries":
						want = http.StatusForbidden
					}
					assert.Equal(t, want, status, string(body))
					assert.NotContains(t, string(body), projectAccessCanary)
					assert.NotContains(t, string(body), f.private.UID)
				})
			}
		}
		require.GreaterOrEqual(t, checked, 70, "inventory must include every registered base project operation")
	})
}

func TestProjectAccessReferenceFacets(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newProjectAccessFixture(t, store)
		_, err := store.AddLabel(t.Context(), f.issue.ID, projectAccessCanary, "member")
		require.NoError(t, err)
		owner := projectAccessCanary
		_, _, _, err = store.UpdateOwner(t.Context(), f.issue.ID, &owner, "member")
		require.NoError(t, err)
		status, _, body := f.request(t, http.MethodGet, "/api/v1/ui/references", "nonmember", nil, nil)
		require.Equal(t, http.StatusOK, status, string(body))
		assert.NotContains(t, string(body), projectAccessCanary)
		assert.NotContains(t, string(body), f.private.UID)
	})
}

func TestProjectAccessSubtreeIntersection(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newProjectAccessFixture(t, store)
		expires := time.Now().Add(time.Hour)
		_, _, err := store.CreateAPIToken(t.Context(), db.CreateAPITokenParams{PlaintextToken: "subtree-test-token", Actor: "member", AdminActor: "admin", Scope: &db.APITokenScope{Kind: db.APITokenScopeIssueSubtree, ProjectUID: f.private.UID, RootIssueUID: f.issue.UID}, ExpiresAt: &expires})
		require.NoError(t, err)
		headers := map[string]string{"Authorization": "Bearer subtree-test-token"}
		status, _, body := f.request(t, http.MethodGet, "/api/v1/projects", "", nil, headers)
		require.Equal(t, http.StatusOK, status, string(body))
		require.Contains(t, string(body), f.private.UID)
		require.NotContains(t, string(body), f.public.UID)
		_, err = store.SetTeamMembership(t.Context(), f.team.UID, "member", false, "admin")
		require.NoError(t, err)
		for _, path := range []string{"/api/v1/projects", "/api/v1/issues", "/api/v1/ui/snapshot?view=all-open", "/api/v1/ui/references"} {
			status, _, body = f.request(t, http.MethodGet, path, "", nil, headers)
			assert.NotContains(t, string(body), f.private.UID)
			assert.NotContains(t, string(body), projectAccessCanary)
			assert.Contains(t, []int{http.StatusNotFound, http.StatusUnauthorized}, status, "revoked project grant bounds the subtree token")
		}
	})
}

func TestProjectAccessHostedDiagnosticsRequireIdentity(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		hostFence := false
		// Even a conflicting internal trusted-caller flag cannot override the
		// configured host's requirement for an authenticated identity.
		hosted := daemon.NewServer(daemon.ServerConfig{DB: store, HostAccess: projectAccessHostController{&hostFence}, TrustCallerAuthentication: true})
		t.Cleanup(func() { require.NoError(t, hosted.Close()) })
		for _, path := range []string{"/debug/pprof/", "/debug/pprof/cmdline"} {
			request := httptest.NewRequest(http.MethodGet, path, nil)
			request.RemoteAddr = "192.0.2.10:43210"
			request = request.WithContext(context.WithValue(request.Context(), http.LocalAddrContextKey, &net.TCPAddr{IP: net.ParseIP("192.0.2.20"), Port: 7777}))
			recorder := httptest.NewRecorder()
			hosted.Handler().ServeHTTP(recorder, request)
			assert.NotEqual(t, http.StatusOK, recorder.Code, "hosted missing identity cannot borrow owner authority")
		}
		// A standalone listener on the owner-local host keeps its existing access.
		local := daemon.NewServer(daemon.ServerConfig{DB: store})
		t.Cleanup(func() { require.NoError(t, local.Close()) })
		request := httptest.NewRequest(http.MethodGet, "/debug/pprof/", nil)
		request.RemoteAddr = "127.0.0.1:43210"
		request = request.WithContext(context.WithValue(request.Context(), http.LocalAddrContextKey, &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 7777}))
		recorder := httptest.NewRecorder()
		local.Handler().ServeHTTP(recorder, request)
		assert.Equal(t, http.StatusOK, recorder.Code, "genuine owner-local diagnostics remain available")
		request = httptest.NewRequest(http.MethodGet, "/debug/pprof/", nil)
		recorder = httptest.NewRecorder()
		local.Handler().ServeHTTP(recorder, request)
		assert.NotEqual(t, http.StatusOK, recorder.Code, "missing transport facts establish no owner authority")
	})
}

func TestProjectAccessAnonymousPrivateNetworkWrites(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newProjectAccessFixture(t, store)
		server := daemon.NewServer(daemon.ServerConfig{DB: store, Auth: config.AuthConfig{AllowUnauthenticatedPrivateNetworkWrites: true}})
		t.Cleanup(func() { require.NoError(t, server.Close()) })
		for _, project := range []db.Project{f.public, f.private} {
			request := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/v1/projects/%d/issues", project.ID), strings.NewReader(`{"title":"Network task","actor":"member","force_new":true}`))
			request.Header.Set("Content-Type", "application/json")
			request.RemoteAddr = "192.168.1.10:43210"
			request = request.WithContext(context.WithValue(request.Context(), http.LocalAddrContextKey, &net.TCPAddr{IP: net.ParseIP("192.168.1.20"), Port: 7777}))
			recorder := httptest.NewRecorder()
			server.Handler().ServeHTTP(recorder, request)
			want := http.StatusOK
			if project.ID == f.private.ID {
				want = http.StatusNotFound
			}
			assert.Equal(t, want, recorder.Code, recorder.Body.String())
		}
	})
}

func TestProjectAccessArchivedSelectionIsIndistinguishable(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newProjectAccessFixture(t, store)
		_, _, _, err := store.SoftDeleteIssue(t.Context(), f.issue.ID, "member")
		require.NoError(t, err)
		ctx := db.WithAuthorizedProjects(t.Context(), []string{f.public.UID})
		data, err := store.(db.UIStore).ReadUISnapshot(ctx, db.UISnapshotQuery{View: "all-open", SelectedIssueUID: f.issue.UID})
		require.NoError(t, err)
		assert.Equal(t, "missing", data.SelectedState, "unauthorized archival state must not disclose existence")
	})
}

func TestProjectAccessBodyProjectSelectors(t *testing.T) {
	for _, operation := range []string{"move", "merge", "enrollment"} {
		t.Run(operation, func(t *testing.T) {
			projectAccessBackends(t, func(t *testing.T, store db.Storage) {
				f := newProjectAccessFixture(t, store)
				var route string
				var body map[string]any
				switch operation {
				case "move":
					route = fmt.Sprintf("/api/v1/projects/%d/issues/%s/actions/move", f.public.ID, f.visible.ShortID)
					body = map[string]any{"to_project_uid": f.private.UID, "dry_run": true, "actor": "member"}
				case "merge":
					route = fmt.Sprintf("/api/v1/projects/%d/merge", f.public.ID)
					body = map[string]any{"source_project_id": f.private.ID, "actor": "member"}
				case "enrollment":
					route = "/api/v1/federation/enrollments"
					body = map[string]any{"project_id": f.private.ID, "actor": "member", "spoke_instance_uid": f.issue.UID, "capabilities": "pull,push"}
				}
				status, _, response := f.request(t, http.MethodPost, route, "nonmember", body, nil)
				assert.Equal(t, http.StatusNotFound, status, string(response))
				assert.NotContains(t, string(response), f.private.UID)
				project, err := store.ProjectByID(t.Context(), f.private.ID)
				require.NoError(t, err)
				assert.Nil(t, project.DeletedAt, "a denied selector cannot merge private data")
			})
		})
	}
}

func TestProjectAccessProxyAndHostBoundaries(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newProjectAccessFixture(t, store)
		cfg := daemon.ServerConfig{DB: store}
		cfg.Auth.Proxy.TrustedActorHeader = "X-Authenticated-Actor"
		cfg.Auth.Proxy.TrustedProxyListeners = []string{"127.0.0.1:7777"}
		server := daemon.NewServer(cfg)
		t.Cleanup(func() { require.NoError(t, server.Close()) })
		for _, actor := range []string{"", "nonmember", "member"} {
			request := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/v1/projects/%d", f.private.ID), nil)
			request.RemoteAddr = "127.0.0.1:43210"
			request.Header.Set("X-Authenticated-Actor", actor)
			request = request.WithContext(context.WithValue(request.Context(), http.LocalAddrContextKey, &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 7777}))
			recorder := httptest.NewRecorder()
			server.Handler().ServeHTTP(recorder, request)
			want := http.StatusNotFound
			if actor == "member" {
				want = http.StatusOK
			}
			assert.Equal(t, want, recorder.Code, recorder.Body.String())
		}
		host := daemon.NewServer(daemon.ServerConfig{DB: store, HostAccess: projectAccessDenyHost{}})
		t.Cleanup(func() { require.NoError(t, host.Close()) })
		request := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/v1/projects/%d", f.private.ID), nil)
		request = request.WithContext(daemon.WithPrincipal(request.Context(), daemon.Principal{Kind: daemon.PrincipalHost, Actor: "member", Subject: "host-member"}))
		recorder := httptest.NewRecorder()
		host.Handler().ServeHTTP(recorder, request)
		assert.Equal(t, http.StatusNotFound, recorder.Code, "project membership never replaces a host denial")
	})
}

func TestProjectAccessDistinctTeamCollections(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newProjectAccessFixture(t, store)
		otherProject, err := store.CreateProject(t.Context(), "other-restricted-project")
		require.NoError(t, err)
		otherIssue, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: otherProject.ID, Title: "second-private-canary", Author: "other-member"})
		require.NoError(t, err)
		otherTeam, _, err := store.CreateTeam(t.Context(), "other-team", "admin")
		require.NoError(t, err)
		_, err = store.SetTeamMembership(t.Context(), otherTeam.UID, "other-member", true, "admin")
		require.NoError(t, err)
		_, _, err = store.SetProjectAccessPolicy(t.Context(), db.ProjectAccessPolicy{ProjectUID: otherProject.UID, Visibility: "teams", TeamUIDs: []string{otherTeam.UID}}, "admin")
		require.NoError(t, err)
		_, _, err = store.CreateAPIToken(t.Context(), db.CreateAPITokenParams{PlaintextToken: "other-member-test-token", Actor: "other-member", AdminActor: "admin"})
		require.NoError(t, err)
		for _, actor := range []string{"member", "other-member", "nonmember", "admin"} {
			for _, path := range []string{"/api/v1/projects?include=stats", "/api/v1/issues", "/api/v1/ui/snapshot?view=all-open&include_graph=true", "/api/v1/events"} {
				status, _, body := f.request(t, http.MethodGet, path, actor, nil, nil)
				assert.Equal(t, http.StatusOK, status, string(body))
				assert.Contains(t, string(body), f.public.UID)
				if actor == "member" || actor == "admin" {
					assert.Contains(t, string(body), f.private.UID)
				} else {
					assert.NotContains(t, string(body), f.private.UID)
					assert.NotContains(t, string(body), f.issue.UID)
				}
				if actor == "other-member" || actor == "admin" {
					assert.Contains(t, string(body), otherProject.UID)
				} else {
					assert.NotContains(t, string(body), otherProject.UID)
					assert.NotContains(t, string(body), otherIssue.UID)
				}
			}
		}
	})
}

func TestProjectAccessEnrollmentTransportUsesBoundActor(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newProjectAccessFixture(t, store)
		_, err := store.EnableProjectFederation(t.Context(), f.private.ID, "admin")
		require.NoError(t, err)
		for _, actor := range []string{"member", "nonmember"} {
			created, err := store.CreateFederationEnrollment(t.Context(), db.CreateFederationEnrollmentParams{SpokeInstanceUID: f.issue.UID, ProjectID: &f.private.ID, Capabilities: "pull,push", Actor: actor})
			require.NoError(t, err)
			path := fmt.Sprintf("/api/v1/projects/%d/federation/metadata", f.private.ID)
			status, _, body := f.request(t, http.MethodGet, path, "", nil, map[string]string{"Authorization": "Bearer " + created.Token})
			want := http.StatusNotFound
			if actor == "member" {
				want = http.StatusOK
			}
			assert.Equal(t, want, status, string(body))
			if actor == "member" {
				_, err = store.SetTeamMembership(t.Context(), f.team.UID, "member", false, "admin")
				require.NoError(t, err)
				status, _, body = f.request(t, http.MethodGet, path, "", nil, map[string]string{"Authorization": "Bearer " + created.Token})
				assert.Equal(t, http.StatusNotFound, status, string(body))
			}
		}
	})
}

// Late claim credentials must replace anonymous admission with their validated
// authority. Caller-supplied holder labels never supply team membership.
func TestProjectAccessLateClaimIdentity(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newProjectAccessFixture(t, store)
		_, err := store.EnableProjectFederation(t.Context(), f.private.ID, "admin")
		require.NoError(t, err)
		path := fmt.Sprintf("/api/v1/projects/%d/issues/%s/lease/actions/acquire", f.private.ID, f.issue.ShortID)
		for _, actor := range []string{"nonmember", "member"} {
			status, _, body := f.request(t, http.MethodPost, path, actor, map[string]any{"holder": "member", "client_kind": "cli", "claim_kind": "hard"}, nil)
			want := http.StatusOK
			if actor == "nonmember" {
				want = http.StatusNotFound
			}
			require.Equal(t, want, status, string(body))
		}
		_, err = store.SetTeamMembership(t.Context(), f.team.UID, "member", false, "admin")
		require.NoError(t, err)
		status, _, body := f.request(t, http.MethodGet, fmt.Sprintf("/api/v1/projects/%d/issues/%s/lease", f.private.ID, f.issue.ShortID), "member", nil, nil)
		require.Equal(t, http.StatusNotFound, status, string(body))
	})
}

// Close completeness retains all children, but restricted errors cannot reveal
// children in another project, their titles, qualified refs, or counts.
func TestProjectAccessCloseGuardConcealsPrivateChildren(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newProjectAccessFixture(t, store)
		_, _, err := store.CreateLinkAndEvent(t.Context(), db.CreateLinkParams{FromIssueID: f.issue.ID, ToIssueID: f.visible.ID, Type: "parent", Author: "member"}, db.LinkEventParams{EventType: "issue.linked", EventIssueID: f.visible.ID, FromUID: f.issue.UID, ToUID: f.visible.UID, Actor: "member"})
		require.NoError(t, err)
		path := fmt.Sprintf("/api/v1/projects/%d/issues/%s/actions/close", f.public.ID, f.visible.ShortID)
		status, _, body := f.request(t, http.MethodPost, path, "nonmember", map[string]any{"reason": "done", "message": "Completed the requested behavior with concrete review evidence.", "evidence": []map[string]string{{"type": "commit", "sha": "abcdef0123456789"}}}, nil)
		require.Equal(t, http.StatusConflict, status, string(body))
		assert.NotContains(t, string(body), projectAccessCanary)
		assert.NotContains(t, string(body), f.private.Name)
		assert.NotContains(t, string(body), "1 open children")
		current, err := store.IssueByID(t.Context(), f.visible.ID)
		require.NoError(t, err)
		assert.Equal(t, "open", current.Status)
	})
}

// R6 forbids related UID disclosure when a dangling endpoint has no admissible
// project. Legacy owner graph diagnostics remain available.
func TestProjectAccessUnresolvedGraphEndpointsRequireProject(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newProjectAccessFixture(t, store)
		var raw *sql.DB
		switch native := store.(type) {
		case *sqlitestore.Store:
			raw = native.DB
		case *pgstore.Store:
			raw = native.DB
		}
		conn, err := raw.Conn(t.Context())
		require.NoError(t, err)
		t.Cleanup(func() { _ = conn.Close() })
		if _, ok := store.(*sqlitestore.Store); ok {
			_, err = conn.ExecContext(t.Context(), "PRAGMA foreign_keys=OFF")
		} else {
			_, err = conn.ExecContext(t.Context(), "ALTER TABLE links DROP CONSTRAINT links_to_issue_id_fkey; ALTER TABLE links DISABLE TRIGGER USER")
		}
		require.NoError(t, err)
		missingUID := "00000000000000000000000009"
		_, err = conn.ExecContext(t.Context(), `INSERT INTO links(from_issue_id,to_issue_id,from_issue_uid,to_issue_uid,type,author) VALUES($1,$2,$3,$4,'blocks','member')`, f.visible.ID, 9001, f.visible.UID, missingUID)
		require.NoError(t, err)
		if _, ok := store.(*sqlitestore.Store); ok {
			_, err = conn.ExecContext(t.Context(), "PRAGMA foreign_keys=ON")
			require.NoError(t, err)
		}
		if _, ok := store.(*pgstore.Store); ok {
			_, err = conn.ExecContext(t.Context(), "ALTER TABLE links ENABLE TRIGGER USER")
			require.NoError(t, err)
		}
		require.NoError(t, conn.Close())
		ui := store.(db.UIStore)
		query := db.UISnapshotQuery{View: "all-open", SelectedIssueUID: f.visible.UID, IncludeGraph: true}
		owner, err := ui.ReadUISnapshot(t.Context(), query)
		require.NoError(t, err)
		require.NotEmpty(t, owner.GraphUnresolvedRefs)
		restricted, err := ui.ReadUISnapshot(db.WithAuthorizedProjects(t.Context(), []string{f.public.UID}), query)
		require.NoError(t, err)
		encoded, err := json.Marshal(restricted)
		require.NoError(t, err)
		assert.NotContains(t, string(encoded), missingUID)
	})
}
