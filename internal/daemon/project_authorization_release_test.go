package daemon_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// projectAccessReleaseStore runs a concurrent change at the precise point a
// request has finished its own storage work but has not released its result:
// after the project lookup of a POST read, after a metadata patch committed,
// between a purge's issue resolution and its transaction, or while an event
// stream reads its high-water mark after admission.
type projectAccessReleaseStore struct {
	db.Storage
	afterProjectByName func()
	afterMove          func()
	afterList          func()
	afterPatch         func(db.PatchIssueMetadataOut)
	afterCreateComment func()
	afterClose         func()
	afterRename        func()
	afterEventsAfter   func([]db.Event)
	beforePurge        func(issueID int64)
	beforeMaxEventID   func(call int)
	maxEventIDCalls    *int
}

func (s projectAccessReleaseStore) RenameProjectAndEvent(ctx context.Context, id int64, name, actor string) (db.Project, *db.Event, bool, error) {
	project, event, changed, err := s.Storage.RenameProjectAndEvent(ctx, id, name, actor)
	if err == nil && s.afterRename != nil {
		s.afterRename()
	}
	return project, event, changed, err
}

func (s projectAccessReleaseStore) EventsAfter(ctx context.Context, params db.EventsAfterParams) ([]db.Event, error) {
	rows, err := s.Storage.EventsAfter(ctx, params)
	if err == nil && s.afterEventsAfter != nil {
		s.afterEventsAfter(rows)
	}
	return rows, err
}

func (s projectAccessReleaseStore) MaxEventID(ctx context.Context) (int64, error) {
	if s.beforeMaxEventID != nil {
		*s.maxEventIDCalls++
		s.beforeMaxEventID(*s.maxEventIDCalls)
	}
	return s.Storage.MaxEventID(ctx)
}

func (s projectAccessReleaseStore) ProjectByName(ctx context.Context, name string) (db.Project, error) {
	project, err := s.Storage.ProjectByName(ctx, name)
	if err == nil && s.afterProjectByName != nil {
		s.afterProjectByName()
	}
	return project, err
}

func (s projectAccessReleaseStore) PatchIssueMetadata(ctx context.Context, in db.PatchIssueMetadataIn) (db.PatchIssueMetadataOut, error) {
	out, err := s.Storage.PatchIssueMetadata(ctx, in)
	if err == nil && s.afterPatch != nil {
		s.afterPatch(out)
	}
	return out, err
}

func (s projectAccessReleaseStore) CreateComment(ctx context.Context, params db.CreateCommentParams) (db.Comment, db.Event, error) {
	comment, event, err := s.Storage.CreateComment(ctx, params)
	if err == nil && s.afterCreateComment != nil {
		s.afterCreateComment()
	}
	return comment, event, err
}

func (s projectAccessReleaseStore) CloseIssueGuarded(ctx context.Context, params db.CloseIssueParams) (db.Issue, []db.Event, bool, error) {
	issue, events, changed, err := s.Storage.CloseIssueGuarded(ctx, params)
	if err == nil && s.afterClose != nil {
		s.afterClose()
		// The close committed, but the caller never learned it: the keyed
		// retry inside the same request answers from the idempotency record.
		return issue, events, changed, errors.New("connection lost after commit")
	}
	return issue, events, changed, err
}

func (s projectAccessReleaseStore) MoveIssueProject(ctx context.Context, in db.MoveIssueProjectIn) (db.MoveIssueProjectOut, error) {
	out, err := s.Storage.MoveIssueProject(ctx, in)
	if err == nil && s.afterMove != nil {
		s.afterMove()
	}
	return out, err
}

func (s projectAccessReleaseStore) ListIssues(ctx context.Context, params db.ListIssuesParams) ([]db.Issue, error) {
	issues, err := s.Storage.ListIssues(ctx, params)
	if err == nil && s.afterList != nil {
		s.afterList()
	}
	return issues, err
}

func (s projectAccessReleaseStore) PurgeIssue(ctx context.Context, issueID int64, actor string, reason *string) (db.PurgeLog, error) {
	if s.beforePurge != nil {
		s.beforePurge(issueID)
	}
	return s.Storage.PurgeIssue(ctx, issueID, actor, reason)
}

// A POST that only reads, such as resolving a project by name, is released
// under the same final access check as a GET: membership revoked after
// admission withholds the result.
func TestProjectAccessPostReadWithheldAfterMembershipRevoked(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newProjectAccessFixture(t, store)
		f = projectAccessFixtureWithStorage(t, f, projectAccessReleaseStore{
			Storage: store,
			afterProjectByName: func() {
				_, err := store.SetTeamMembership(t.Context(), f.team.UID, "member", false, "admin")
				require.NoError(t, err)
			},
		})
		status, _, body := f.request(t, http.MethodPost, "/api/v1/projects/resolve", "member",
			map[string]any{"name": f.private.Name}, nil)
		assert.Equal(t, http.StatusNotFound, status, string(body))
		assert.NotContains(t, string(body), f.private.UID)
	})
}

// A metadata patch authorized inside its own transaction is released even
// when the issue is soft-deleted by someone else before the response leaves,
// so the caller learns that its write committed.
func TestProjectAccessCommittedPatchReleasedAfterConcurrentDelete(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newProjectAccessFixture(t, store)
		f = projectAccessFixtureWithStorage(t, f, projectAccessReleaseStore{
			Storage: store,
			afterPatch: func(db.PatchIssueMetadataOut) {
				_, _, _, err := store.SoftDeleteIssue(t.Context(), f.issue.ID, "admin")
				require.NoError(t, err)
			},
		})
		status, _, body := f.request(t, http.MethodPost,
			fmt.Sprintf("/api/v1/projects/%d/issues/%s/metadata", f.private.ID, f.issue.ShortID), "member",
			map[string]any{"actor": "member", "patch": map[string]any{"work.note": "committed"}}, nil)
		assert.Equal(t, http.StatusOK, status, string(body))
		assert.Contains(t, string(body), "committed")
	})
}

// Purge checks the issue's current project inside its transaction. An issue
// moved into a project the caller cannot see after the request resolved it is
// not deleted.
func TestProjectAccessPurgeRechecksCurrentProjectInTransaction(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newProjectAccessFixture(t, store)
		hidden := projectAccessHiddenProject(t, store, "hidden-destination")
		f = projectAccessFixtureWithStorage(t, f, projectAccessReleaseStore{
			Storage: store,
			beforePurge: func(issueID int64) {
				_, err := store.MoveIssueProject(t.Context(), db.MoveIssueProjectIn{
					IssueID: issueID, FromProjectID: f.private.ID, ToProjectID: hidden.ID,
					IfMatchRev: f.issue.Revision, Actor: "admin",
				})
				require.NoError(t, err)
			},
		})
		status, _, body := f.request(t, http.MethodPost,
			fmt.Sprintf("/api/v1/projects/%d/issues/%s/actions/purge", f.private.ID, f.issue.ShortID), "member",
			map[string]any{"actor": "member"}, map[string]string{"X-Kata-Confirm": fmt.Sprintf("PURGE %s#%s", f.private.Name, f.issue.ShortID)})
		assert.Equal(t, http.StatusNotFound, status, string(body))
		kept, err := store.IssueByID(t.Context(), f.issue.ID)
		require.NoError(t, err, "the moved issue survives the refused purge")
		assert.Equal(t, hidden.ID, kept.ProjectID)
	})
}

// An active issue stays readable when a linked peer's project is archived:
// access holders keep archived projects, so the archived endpoint is still an
// admitted one.
func TestProjectAccessShowIssueKeepsArchivedLinkPeer(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newProjectAccessFixture(t, store)
		peerProject, err := store.CreateProject(t.Context(), "archived-peer-project")
		require.NoError(t, err)
		peer, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: peerProject.ID, Title: "Peer in archived project", Author: "member"})
		require.NoError(t, err)
		_, err = store.CreateLink(t.Context(), db.CreateLinkParams{FromIssueID: f.issue.ID, ToIssueID: peer.ID, Type: "related", Author: "member"})
		require.NoError(t, err)
		_, _, err = store.RemoveProject(t.Context(), db.RemoveProjectParams{ProjectID: peerProject.ID, Actor: "admin", Force: true})
		require.NoError(t, err)

		status, _, body := f.request(t, http.MethodGet,
			fmt.Sprintf("/api/v1/projects/%d/issues/%s", f.private.ID, f.issue.ShortID), "member", nil, nil)
		assert.Equal(t, http.StatusOK, status, string(body))
		assert.Contains(t, string(body), projectAccessCanary)
	})
}

// A policy grant for a project whose lifecycle event the stream has already
// passed closes the stream: the client has neither that event nor the
// project's history, so it must reload. Only a project that appears in the
// stream's unprocessed window joins it in place.
func TestProjectAccessStreamClosesOnGrantBehindProcessedCursor(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newProjectAccessFixture(t, store)
		hidden := projectAccessHiddenProject(t, store, "granted-later")
		policy, err := store.ProjectAccessPolicy(t.Context(), hidden.UID)
		require.NoError(t, err)
		_, _, err = store.RemoveProject(t.Context(), db.RemoveProjectParams{ProjectID: hidden.ID, Actor: "admin"})
		require.NoError(t, err)
		cursor, err := store.MaxEventID(t.Context())
		require.NoError(t, err)
		response, stream := openProjectAccessSSE(t, f, cursor, "member")
		defer func() { _ = response.Body.Close() }()

		createProjectAccessIssue(t, f, f.public.ID, "member", "Before restore")
		first, ok := stream.Next(t, 2*time.Second)
		require.True(t, ok, "the stream delivers before the restore")
		assert.Equal(t, "issue.created", first.event)

		_, _, _, err = store.RestoreProject(t.Context(), hidden.ID, "admin")
		require.NoError(t, err)
		createProjectAccessIssue(t, f, f.public.ID, "member", "After restore")
		next, ok := stream.Next(t, 2*time.Second)
		require.True(t, ok, "a restore the member cannot see leaves the stream open")
		assert.Contains(t, next.data, "After restore")

		_, err = store.SetTeamMembership(t.Context(), policy.TeamUIDs[0], "member", true, "admin")
		require.NoError(t, err)
		createProjectAccessIssue(t, f, f.public.ID, "member", "After grant")
		late, ok := stream.Next(t, 2*time.Second)
		assert.False(t, ok, "the stream must close instead of continuing without the project's history: got %+v", late)
	})
}

// A project created after the stream was admitted but before it subscribed
// has its creation event inside the first drained window. The drain must
// carry that event, because no later broadcast will repeat it.
func TestProjectAccessStreamDrainCarriesProjectCreatedDuringAdmission(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newProjectAccessFixture(t, store)
		var created db.Project
		calls := 0
		f = projectAccessFixtureWithStorage(t, f, projectAccessReleaseStore{
			Storage:         store,
			maxEventIDCalls: &calls,
			beforeMaxEventID: func(call int) {
				// The first read is the admission cursor; the second is the
				// high-water mark read after the stream subscribed.
				if call != 2 {
					return
				}
				var err error
				created, err = store.CreateProject(t.Context(), "created-during-admission")
				require.NoError(t, err)
			},
		})
		cursor, err := store.MaxEventID(t.Context())
		require.NoError(t, err)
		response, stream := openProjectAccessSSE(t, f, cursor, "member")
		defer func() { _ = response.Body.Close() }()

		frame, ok := stream.Next(t, 2*time.Second)
		require.True(t, ok, "the drain delivers the project created during admission")
		assert.Equal(t, "project.created", frame.event)
		assert.Contains(t, frame.data, created.UID)
	})
}

// A rename that committed persists, but membership revoked before the
// response leaves withholds the response like any other: a revoked member
// learns nothing about the project, not even its new name.
func TestProjectAccessCommittedRenameWithheldAfterRevocation(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newProjectAccessFixture(t, store)
		f = projectAccessFixtureWithStorage(t, f, projectAccessReleaseStore{
			Storage: store,
			afterRename: func() {
				_, err := store.SetTeamMembership(t.Context(), f.team.UID, "member", false, "admin")
				require.NoError(t, err)
			},
		})
		status, _, body := f.request(t, http.MethodPatch,
			fmt.Sprintf("/api/v1/projects/%d", f.private.ID), "member",
			map[string]any{"name": "restricted-project-renamed", "actor": "member"}, nil)
		assert.Equal(t, http.StatusNotFound, status, string(body))
		assert.NotContains(t, string(body), "restricted-project-renamed")
		renamed, err := store.ProjectByID(t.Context(), f.private.ID)
		require.NoError(t, err)
		assert.Equal(t, "restricted-project-renamed", renamed.Name, "the committed rename persists")
	})
}

// A grant that lands between reading a batch and writing its frames may cover
// a lifecycle event the narrower read already excluded, so the stream closes
// for a reload instead of adopting the project and skipping its history.
func TestProjectAccessStreamClosesOnGrantDuringBatch(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newProjectAccessFixture(t, store)
		hidden := projectAccessHiddenProject(t, store, "granted-mid-batch")
		policy, err := store.ProjectAccessPolicy(t.Context(), hidden.UID)
		require.NoError(t, err)
		_, _, err = store.RemoveProject(t.Context(), db.RemoveProjectParams{ProjectID: hidden.ID, Actor: "admin"})
		require.NoError(t, err)
		granted := false
		f = projectAccessFixtureWithStorage(t, f, projectAccessReleaseStore{
			Storage: store,
			afterEventsAfter: func(rows []db.Event) {
				for _, row := range rows {
					if granted || !strings.Contains(row.Payload, "After restore") {
						continue
					}
					granted = true
					_, err := store.SetTeamMembership(t.Context(), policy.TeamUIDs[0], "member", true, "admin")
					require.NoError(t, err)
				}
			},
		})
		cursor, err := store.MaxEventID(t.Context())
		require.NoError(t, err)
		response, stream := openProjectAccessSSE(t, f, cursor, "member")
		defer func() { _ = response.Body.Close() }()

		createProjectAccessIssue(t, f, f.public.ID, "member", "Before restore")
		first, ok := stream.Next(t, 2*time.Second)
		require.True(t, ok)
		assert.Contains(t, first.data, "Before restore")

		_, _, _, err = store.RestoreProject(t.Context(), hidden.ID, "admin")
		require.NoError(t, err)
		createProjectAccessIssue(t, f, f.public.ID, "member", "After restore")
		late, ok := stream.Next(t, 2*time.Second)
		assert.False(t, ok, "the batch excluded the restore, so the grant must close the stream: got %+v", late)
		assert.True(t, granted, "the grant ran while the batch was in flight")
	})
}

// A handler that reloads the issue after its mutation committed reads outside
// the fence. Membership revoked after the commit, followed by a confidential
// edit, must not reach the revoked caller through that reload.
func TestProjectAccessPostCommitReloadWithheldAfterRevocation(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newProjectAccessFixture(t, store)
		f = projectAccessFixtureWithStorage(t, f, projectAccessReleaseStore{
			Storage: store,
			afterCreateComment: func() {
				_, err := store.SetTeamMembership(t.Context(), f.team.UID, "member", false, "admin")
				require.NoError(t, err)
				body := "confidential after revocation"
				_, _, _, err = store.EditIssue(t.Context(), db.EditIssueParams{IssueID: f.issue.ID, Body: &body, Actor: "admin"})
				require.NoError(t, err)
			},
		})
		status, _, body := f.request(t, http.MethodPost,
			fmt.Sprintf("/api/v1/projects/%d/issues/%s/comments", f.private.ID, f.issue.ShortID), "member",
			map[string]any{"actor": "member", "body": "my comment"}, nil)
		assert.Equal(t, http.StatusNotFound, status, string(body))
		assert.NotContains(t, string(body), "confidential after revocation")
		comments, err := store.CommentsByIssue(t.Context(), f.issue.ID)
		require.NoError(t, err)
		assert.Len(t, comments, 1, "the comment itself committed before the revocation")
	})
}

// A keyed close whose acknowledgement is lost answers from the idempotency
// record by reloading the issue. That reload runs after the fenced commit, so
// a revoked member must not receive a confidential edit through it.
func TestProjectAccessCloseRecoveryWithheldAfterRevocation(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newProjectAccessFixture(t, store)
		f = projectAccessFixtureWithStorage(t, f, projectAccessReleaseStore{
			Storage: store,
			afterClose: func() {
				_, err := store.SetTeamMembership(t.Context(), f.team.UID, "member", false, "admin")
				require.NoError(t, err)
				body := "confidential after revocation"
				_, _, _, err = store.EditIssue(t.Context(), db.EditIssueParams{IssueID: f.issue.ID, Body: &body, Actor: "admin"})
				require.NoError(t, err)
			},
		})
		status, _, body := f.request(t, http.MethodPost,
			fmt.Sprintf("/api/v1/projects/%d/issues/%s/actions/close", f.private.ID, f.issue.ShortID), "member",
			map[string]any{"actor": "member", "reason": "done", "message": "Finished the work and verified the fix with the regression suite on both backends.", "retry_protocol": "close-v1", "evidence": []map[string]any{{"type": "commit", "sha": "abc1234"}}},
			map[string]string{"Idempotency-Key": "close-once"})
		assert.Equal(t, http.StatusNotFound, status, string(body))
		assert.NotContains(t, string(body), "confidential after revocation")
		closed, err := store.IssueByID(t.Context(), f.issue.ID)
		require.NoError(t, err)
		assert.Equal(t, "closed", closed.Status, "the close itself committed before the revocation")
	})
}

// A move preview authorizes inside a transaction it rolls back, so it is a
// read: membership revoked before the response leaves withholds the issue.
func TestProjectAccessMovePreviewWithheldAfterRevocation(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newProjectAccessFixture(t, store)
		f = projectAccessFixtureWithStorage(t, f, projectAccessReleaseStore{
			Storage: store,
			afterMove: func() {
				_, err := store.SetTeamMembership(t.Context(), f.team.UID, "member", false, "admin")
				require.NoError(t, err)
			},
		})
		status, _, body := f.request(t, http.MethodPost,
			fmt.Sprintf("/api/v1/projects/%d/issues/%s/actions/move", f.private.ID, f.issue.ShortID), "member",
			map[string]any{"actor": "member", "to_project_uid": f.public.UID, "dry_run": true}, nil)
		assert.Equal(t, http.StatusNotFound, status, string(body))
		assert.NotContains(t, string(body), projectAccessCanary)
	})
}

// A collection read that loaded an issue which then moved into a project the
// caller cannot see is withheld, the same as a single-issue read.
func TestProjectAccessListWithheldAfterIssueMovedToHiddenProject(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newProjectAccessFixture(t, store)
		hidden := projectAccessHiddenProject(t, store, "hidden-list-destination")
		moved := false
		f = projectAccessFixtureWithStorage(t, f, projectAccessReleaseStore{
			Storage: store,
			afterList: func() {
				if moved {
					return
				}
				moved = true
				_, err := store.MoveIssueProject(t.Context(), db.MoveIssueProjectIn{
					IssueID: f.issue.ID, FromProjectID: f.private.ID, ToProjectID: hidden.ID,
					IfMatchRev: f.issue.Revision, Actor: "admin",
				})
				require.NoError(t, err)
			},
		})
		status, _, body := f.request(t, http.MethodGet,
			fmt.Sprintf("/api/v1/projects/%d/issues", f.private.ID), "member", nil, nil)
		assert.Equal(t, http.StatusNotFound, status, string(body))
		assert.NotContains(t, string(body), projectAccessCanary)
	})
}

// An event page read before its issue moved into a project the caller cannot
// see is withheld: the issue's creation event carries its title and body.
func TestProjectAccessEventPollWithheldAfterIssueMovedToHiddenProject(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		for _, feed := range []struct{ name, path string }{
			{name: "global", path: "/api/v1/events?after_id=0"},
			{name: "project", path: "/api/v1/projects/%d/events?after_id=0"},
		} {
			t.Run(feed.name, func(t *testing.T) {
				f := newProjectAccessFixture(t, store, feed.name+"-poll")
				hidden := projectAccessHiddenProject(t, store, "hidden-poll-"+feed.name)
				moved := false
				f = projectAccessFixtureWithStorage(t, f, projectAccessReleaseStore{
					Storage: store,
					afterEventsAfter: func([]db.Event) {
						if moved {
							return
						}
						moved = true
						_, err := store.MoveIssueProject(t.Context(), db.MoveIssueProjectIn{
							IssueID: f.issue.ID, FromProjectID: f.private.ID, ToProjectID: hidden.ID,
							IfMatchRev: f.issue.Revision, Actor: "admin",
						})
						require.NoError(t, err)
					},
				})
				target := feed.path
				if strings.Contains(target, "%d") {
					target = fmt.Sprintf(target, f.private.ID)
				}
				status, _, body := f.request(t, http.MethodGet, target, "member", nil, nil)
				assert.True(t, moved, "the move ran between the read and the release")
				assert.Equal(t, http.StatusNotFound, status, string(body))
				assert.NotContains(t, string(body), projectAccessCanary)
			})
		}
	})
}

// A stream frame whose issue moved into a project the caller cannot see after
// its batch was read is replaced by a reset, and the stream closes for a
// reload. A plain team member has no issue-scoped token, so this is the
// project boundary alone.
func TestProjectAccessStreamWithholdsFrameAfterIssueMovedToHiddenProject(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newProjectAccessFixture(t, store)
		hidden := projectAccessHiddenProject(t, store, "hidden-stream-destination")
		const secret = "moving-secret-title"
		moved := false
		f = projectAccessFixtureWithStorage(t, f, projectAccessReleaseStore{
			Storage: store,
			afterEventsAfter: func(rows []db.Event) {
				for _, row := range rows {
					if moved || row.IssueID == nil || !strings.Contains(row.Payload, secret) {
						continue
					}
					moved = true
					issue, err := store.IssueByID(t.Context(), *row.IssueID)
					require.NoError(t, err)
					_, err = store.MoveIssueProject(t.Context(), db.MoveIssueProjectIn{
						IssueID: issue.ID, FromProjectID: f.private.ID, ToProjectID: hidden.ID,
						IfMatchRev: issue.Revision, Actor: "admin",
					})
					require.NoError(t, err)
				}
			},
		})
		cursor, err := store.MaxEventID(t.Context())
		require.NoError(t, err)
		response, stream := openProjectAccessSSE(t, f, cursor, "member")
		defer func() { _ = response.Body.Close() }()

		// The stream may move the issue before the create response leaves; the
		// release rule then withholds that response, but the write commits.
		status, _, body := f.request(t, http.MethodPost,
			fmt.Sprintf("/api/v1/projects/%d/issues", f.private.ID), "member",
			map[string]string{"title": secret, "actor": "member"}, nil)
		require.True(t, status == http.StatusOK || status == http.StatusCreated || status == http.StatusNotFound,
			"create issue: status=%d body=%s", status, body)
		frame, ok := stream.Next(t, 2*time.Second)
		require.True(t, ok, "the stream signals the withheld frame")
		assert.True(t, moved, "the move ran between the read and the frame")
		assert.Equal(t, "sync.reset_required", frame.event)
		assert.NotContains(t, frame.data, secret)
		late, ok := stream.Next(t, 2*time.Second)
		assert.False(t, ok, "the stream closes after the reset: got %+v", late)
	})
}

// projectAccessUIReleaseStore runs afterRead once a browser projection read
// returns, before the daemon releases the response built from it. A warmed
// snapshot cache answers after its cursor read, so that read fires it too.
type projectAccessUIReleaseStore struct {
	db.Storage
	ui        db.UIStore
	afterRead func()
}

func (s projectAccessUIReleaseStore) fire(err error) {
	if err == nil && s.afterRead != nil {
		s.afterRead()
	}
}

func (s projectAccessUIReleaseStore) UIEventCursor(ctx context.Context) (int64, error) {
	cursor, err := s.ui.UIEventCursor(ctx)
	s.fire(err)
	return cursor, err
}

func (s projectAccessUIReleaseStore) ReadUISnapshot(ctx context.Context, query db.UISnapshotQuery) (db.UISnapshotData, error) {
	data, err := s.ui.ReadUISnapshot(ctx, query)
	s.fire(err)
	return data, err
}

func (s projectAccessUIReleaseStore) ReadUIReferenceHydration(ctx context.Context, query db.UIReferencesQuery) (db.UIReferenceHydration, error) {
	data, err := s.ui.ReadUIReferenceHydration(ctx, query)
	s.fire(err)
	return data, err
}

func (s projectAccessUIReleaseStore) ReadUIReferences(ctx context.Context, query db.UIReferencesQuery) (db.UIReferencesData, error) {
	data, err := s.ui.ReadUIReferences(ctx, query)
	s.fire(err)
	return data, err
}

// A browser projection read before its issue moved into a project the caller
// cannot see is withheld, whether the daemon read it fresh, served it from a
// warmed snapshot cache, or listed the issue as a reference choice.
func TestProjectAccessBrowserReadsWithheldAfterIssueMovedToHiddenProject(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		for _, tc := range []struct {
			name string
			path func(projectAccessFixture) string
			warm bool
		}{
			{name: "cold-snapshot", path: func(f projectAccessFixture) string {
				return "/api/v1/ui/snapshot?view=all-open&project_uid=" + f.private.UID
			}},
			{name: "warm-snapshot", warm: true, path: func(f projectAccessFixture) string {
				return "/api/v1/ui/snapshot?view=all-open&project_uid=" + f.private.UID
			}},
			{name: "references", path: func(projectAccessFixture) string {
				return "/api/v1/ui/references?q=" + projectAccessCanary
			}},
			{name: "reference-hydration", path: func(f projectAccessFixture) string {
				return "/api/v1/ui/references?issue_uid=" + f.issue.UID
			}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				f := newProjectAccessFixture(t, store, tc.name)
				hidden := projectAccessHiddenProject(t, store, "hidden-ui-"+tc.name)
				ui, ok := store.(db.UIStore)
				require.True(t, ok)
				armed, moved := false, false
				f = projectAccessFixtureWithStorage(t, f, projectAccessUIReleaseStore{
					Storage: store, ui: ui,
					afterRead: func() {
						if !armed || moved {
							return
						}
						moved = true
						_, err := store.MoveIssueProject(t.Context(), db.MoveIssueProjectIn{
							IssueID: f.issue.ID, FromProjectID: f.private.ID, ToProjectID: hidden.ID,
							IfMatchRev: f.issue.Revision, Actor: "admin",
						})
						require.NoError(t, err)
					},
				})
				path := tc.path(f)
				if tc.warm {
					status, _, body := f.request(t, http.MethodGet, path, "member", nil, nil)
					require.Equal(t, http.StatusOK, status, string(body))
					require.Contains(t, string(body), projectAccessCanary, "the first read warms the cache")
				}
				armed = true
				status, _, body := f.request(t, http.MethodGet, path, "member", nil, nil)
				assert.True(t, moved, "the move ran between the read and the release")
				assert.Equal(t, http.StatusNotFound, status, string(body))
				assert.NotContains(t, string(body), projectAccessCanary)
			})
		}
	})
}

// A cached history response depends on every issue its events reference, not
// only on their subjects. When a referenced issue moves into a project the
// caller cannot see while a warmed snapshot is released, the response is
// withheld exactly as the uncached read is.
func TestProjectAccessCachedHistoryWithheldAfterReferencedIssueMoved(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		for _, tc := range []struct {
			name    string
			subject func(*testing.T, projectAccessFixture) db.Issue
		}{
			{name: "duplicate-evidence", subject: func(t *testing.T, f projectAccessFixture) db.Issue {
				issue, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{
					ProjectID: f.public.ID, Title: "Closed as a duplicate", Author: "member",
				})
				require.NoError(t, err)
				_, _, _, err = store.CloseIssueWithEvents(t.Context(), issue.ID, "done", "member",
					"Duplicate of the restricted task.", []db.Evidence{{
						Type: "duplicate-of", IssueRef: f.private.Name + "#" + f.issue.ShortID,
					}})
				require.NoError(t, err)
				return issue
			}},
			{name: "removed-initial-link", subject: func(t *testing.T, f projectAccessFixture) db.Issue {
				issue, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{
					ProjectID: f.public.ID, Title: "Created with a link", Author: "member",
					Links: []db.InitialLink{{Type: "related", ToNumber: f.issue.ID, ExpectedProjectUID: f.private.UID}},
				})
				require.NoError(t, err)
				links, err := store.LinksByIssue(t.Context(), issue.ID)
				require.NoError(t, err)
				require.Len(t, links, 1)
				require.NoError(t, store.DeleteLinkByID(t.Context(), links[0].ID))
				return issue
			}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				f := newProjectAccessFixture(t, store, "history-"+tc.name)
				hidden := projectAccessHiddenProject(t, store, "hidden-history-"+tc.name)
				subject := tc.subject(t, f)
				ui, ok := store.(db.UIStore)
				require.True(t, ok)
				armed, moved := false, false
				f = projectAccessFixtureWithStorage(t, f, projectAccessUIReleaseStore{
					Storage: store, ui: ui,
					afterRead: func() {
						if !armed || moved {
							return
						}
						moved = true
						_, err := store.MoveIssueProject(t.Context(), db.MoveIssueProjectIn{
							IssueID: f.issue.ID, FromProjectID: f.private.ID, ToProjectID: hidden.ID,
							IfMatchRev: f.issue.Revision, Actor: "admin",
						})
						require.NoError(t, err)
					},
				})
				path := "/api/v1/ui/snapshot?view=all-open&project_uid=" + f.public.UID +
					"&include_history=true&selected_issue_uid=" + subject.UID
				status, _, body := f.request(t, http.MethodGet, path, "member", nil, nil)
				require.Equal(t, http.StatusOK, status, string(body))
				require.Contains(t, string(body), f.issue.ShortID, "the warmed history references the issue")

				armed = true
				status, _, body = f.request(t, http.MethodGet, path, "member", nil, nil)
				assert.True(t, moved, "the move ran between the cached read and the release")
				assert.Equal(t, http.StatusNotFound, status, string(body))
				assert.NotContains(t, string(body), f.issue.ShortID)
			})
		}
	})
}

// A snapshot exposes a link's far endpoint, its UID, qualified ID and status,
// even when that issue is outside the snapshot's collection. When the endpoint
// moves into a project the caller cannot see before the snapshot is released,
// the response is withheld, whether it was read fresh or served from cache.
func TestProjectAccessSnapshotWithheldAfterLinkEndpointMoved(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		for _, tc := range []struct {
			name  string
			query string
			warm  bool
		}{
			{name: "fresh-collection", query: ""},
			{name: "cached-collection", query: "", warm: true},
			{name: "fresh-selected", query: "&selected_issue_uid="},
			{name: "cached-selected", query: "&selected_issue_uid=", warm: true},
		} {
			t.Run(tc.name, func(t *testing.T) {
				f := newProjectAccessFixture(t, store, "link-"+tc.name)
				hidden := projectAccessHiddenProject(t, store, "hidden-link-"+tc.name)
				_, err := store.CreateLink(t.Context(), db.CreateLinkParams{
					FromIssueID: f.visible.ID, ToIssueID: f.issue.ID, Type: "related", Author: "member",
				})
				require.NoError(t, err)
				ui, ok := store.(db.UIStore)
				require.True(t, ok)
				armed, moved := false, false
				f = projectAccessFixtureWithStorage(t, f, projectAccessUIReleaseStore{
					Storage: store, ui: ui,
					afterRead: func() {
						if !armed || moved {
							return
						}
						moved = true
						_, err := store.MoveIssueProject(t.Context(), db.MoveIssueProjectIn{
							IssueID: f.issue.ID, FromProjectID: f.private.ID, ToProjectID: hidden.ID,
							IfMatchRev: f.issue.Revision, Actor: "admin",
						})
						require.NoError(t, err)
					},
				})
				path := "/api/v1/ui/snapshot?view=all-open&project_uid=" + f.public.UID
				if tc.query != "" {
					// An empty collection leaves the selected issue's links as
					// the only place the endpoint appears.
					empty, err := store.CreateProject(t.Context(), "empty-link-"+tc.name)
					require.NoError(t, err)
					path = "/api/v1/ui/snapshot?view=all-open&project_uid=" + empty.UID + tc.query + f.visible.UID
				}
				if tc.warm {
					status, _, body := f.request(t, http.MethodGet, path, "member", nil, nil)
					require.Equal(t, http.StatusOK, status, string(body))
					require.Contains(t, string(body), f.issue.UID, "the warmed snapshot carries the link endpoint")
				}
				armed = true
				status, _, body := f.request(t, http.MethodGet, path, "member", nil, nil)
				assert.True(t, moved, "the move ran between the read and the release")
				assert.Equal(t, http.StatusNotFound, status, string(body))
				assert.NotContains(t, string(body), f.issue.UID)
			})
		}
	})
}

// Owner and label suggestions come from issues the reference search may not
// list. When the only issue behind a suggestion moves into a project the
// caller cannot see before the references are released, the response is
// withheld rather than carrying that issue's owner or label.
func TestProjectAccessReferenceSuggestionsWithheldAfterSourceIssueMoved(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newProjectAccessFixture(t, store)
		hidden := projectAccessHiddenProject(t, store, "hidden-suggestion-source")
		const owner, label = "suggestion-only-owner", "suggestion-only-label"
		ownerName := owner
		_, _, _, err := store.UpdateOwner(t.Context(), f.issue.ID, &ownerName, "admin")
		require.NoError(t, err)
		_, err = store.AddLabel(t.Context(), f.issue.ID, label, "admin")
		require.NoError(t, err)
		ui, ok := store.(db.UIStore)
		require.True(t, ok)
		armed, moved := false, false
		f = projectAccessFixtureWithStorage(t, f, projectAccessUIReleaseStore{
			Storage: store, ui: ui,
			afterRead: func() {
				if !armed || moved {
					return
				}
				moved = true
				issue, err := store.IssueByID(t.Context(), f.issue.ID)
				require.NoError(t, err)
				_, err = store.MoveIssueProject(t.Context(), db.MoveIssueProjectIn{
					IssueID: issue.ID, FromProjectID: f.private.ID, ToProjectID: hidden.ID,
					IfMatchRev: issue.Revision, Actor: "admin",
				})
				require.NoError(t, err)
			},
		})
		path := "/api/v1/ui/references?q=matches-no-issue"
		status, _, body := f.request(t, http.MethodGet, path, "member", nil, nil)
		require.Equal(t, http.StatusOK, status, string(body))
		require.NotContains(t, string(body), f.issue.UID, "the search does not list the source issue")
		require.Contains(t, string(body), owner)
		require.Contains(t, string(body), label)

		armed = true
		status, _, body = f.request(t, http.MethodGet, path, "member", nil, nil)
		assert.True(t, moved, "the move ran between the read and the release")
		assert.Equal(t, http.StatusNotFound, status, string(body))
		assert.NotContains(t, string(body), owner)
		assert.NotContains(t, string(body), label)
	})
}
