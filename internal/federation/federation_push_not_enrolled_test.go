package federation

import (
	"bytes"
	"encoding/json/v2"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	clientpkg "go.kenn.io/kata/internal/client"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/db/pgstore/pgstoretest"
	"go.kenn.io/kata/internal/db/sqlitestore"
)

// A spoke comments on its copy of an issue that the hub has meanwhile moved
// to a project the spoke has not joined. The hub refuses the push. The pull
// must still run, so hub work keeps arriving and the move parks the issue
// instead of the spoke stalling on the refusal (#1049).
func TestSyncFederationPullsAfterPushRefusedForUnjoinedProject(t *testing.T) {
	ctx := t.Context()
	hub, projects := newMoveDeliveryHub(t, "joined-project", "absent-target")
	spoke := newMoveDeliverySpoke(t, hub, projects[:1])
	issue, _, err := hub.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: projects[0].ID, Title: "moves away", Author: "worker"})
	require.NoError(t, err)
	require.NoError(t, spoke.sync(t, 0))
	local, err := spoke.env.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	_, _, err = spoke.env.DB.CreateComment(ctx, db.CreateCommentParams{IssueID: local.ID, Author: "worker", Body: "offline comment"})
	require.NoError(t, err)
	_, err = hub.DB.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: projects[0].ID, ToProjectID: projects[1].ID, IfMatchRev: issue.Revision, Actor: "worker"})
	require.NoError(t, err)
	later, laterEvent, err := hub.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: projects[0].ID, Title: "later hub work", Author: "worker"})
	require.NoError(t, err)

	bindingBefore, err := spoke.env.DB.FederationBindingByProject(ctx, spoke.projects[0].ID)
	require.NoError(t, err)
	credential := spoke.credentials[spoke.projects[0].UID]
	client, err := NewClient(ctx, credential.HubURL, credential.Token, clientOptsWithDefault(clientpkg.Opts{}))
	require.NoError(t, err)
	polls := heldPollFixture(bindingBefore.ProjectID, bindingBefore.PullCursorEventID, client.heldPollTarget(credential.HubProjectID))
	err = SyncFederationOnce(withHubLongPolls(ctx, polls), spoke.env.DB, bindingBefore, credential, spoke.credentials)
	require.ErrorContains(t, err, "federation_project_not_enrolled", "the refusal stays visible")
	require.ErrorContains(t, err, issue.UID)

	binding, err := spoke.env.DB.FederationBindingByProject(ctx, spoke.projects[0].ID)
	require.NoError(t, err)
	require.GreaterOrEqual(t, binding.PullCursorEventID, laterEvent.ID, "the pull runs after the refused push")
	_, err = spoke.env.DB.IssueByUID(ctx, later.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	parked, err := spoke.env.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.NotEqual(t, spoke.projects[0].ID, parked.ProjectID, "the pulled move parks the issue outside the joined project")
}

// A stale local edit on an issue moved to an unenrolled project must not block
// unrelated queued edits in the same project from pushing, while the stale edit
// stays surfaced and delivers once the spoke enrolls in the destination project
// or the issue returns (#1049).
func TestSyncFederationUnrelatedEditsPushPastUnenrolledIssue(t *testing.T) {
	for _, recovery := range []string{"enroll-destination", "issue-returns-after-reset"} {
		t.Run(recovery, func(t *testing.T) {
			for _, backend := range []string{"sqlite", "postgres"} {
				t.Run(backend, func(t *testing.T) {
					ctx := t.Context()
					hub, projects := newMoveDeliveryHub(t, "joined-project", "absent-target")
					var spoke db.Storage
					var err error
					if backend == "postgres" {
						dsn, cleanup := pgstoretest.NewDatabase(t, ctx)
						t.Cleanup(cleanup)
						spoke, err = pgstore.Open(ctx, dsn)
					} else {
						spoke, err = sqlitestore.Open(ctx, filepath.Join(t.TempDir(), "spoke.db"))
					}
					require.NoError(t, err)
					t.Cleanup(func() { _ = spoke.Close() })
					credentials := newMoveCredentialStore()
					localJoined, err := spoke.CreateProjectWithUID(ctx, projects[0].Name, projects[0].UID)
					require.NoError(t, err)
					_, err = spoke.UpsertFederationBinding(ctx, db.FederationBinding{
						ProjectID: localJoined.ID, Role: db.FederationRoleSpoke, HubURL: hub.URL,
						HubProjectID: projects[0].ID, HubProjectUID: projects[0].UID,
						Enabled: true, PushEnabled: true, Actor: "worker",
					})
					require.NoError(t, err)
					joinGrant, err := hub.DB.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{
						SpokeInstanceUID: spoke.InstanceUID(), ProjectID: &projects[0].ID,
						Capabilities: "push,pull", Actor: "worker",
					})
					require.NoError(t, err)
					credentials[projects[0].UID] = config.FederationCredential{
						HubURL: hub.URL, HubProjectID: projects[0].ID, Token: joinGrant.Token, Actor: "worker",
					}
					localProjects := []db.Project{localJoined}
					syncProject := func(index int, onPulled func(int64, []db.Event)) error {
						binding, err := spoke.FederationBindingByProject(ctx, localProjects[index].ID)
						if err != nil {
							return err
						}
						return SyncFederationOnceWithPulledEvents(ctx, spoke, binding, credentials[localProjects[index].UID], clientOptsWithDefault(clientpkg.Opts{}), onPulled, credentials)
					}

					movedIssue, _, err := hub.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: projects[0].ID, Title: "moves away", Author: "worker"})
					require.NoError(t, err)
					stayingIssue, _, err := hub.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: projects[0].ID, Title: "stays here", Author: "worker"})
					require.NoError(t, err)
					require.NoError(t, syncProject(0, nil))

					localMoved, err := spoke.IssueByUID(ctx, movedIssue.UID, db.IncludeDeletedNo)
					require.NoError(t, err)
					localStaying, err := spoke.IssueByUID(ctx, stayingIssue.UID, db.IncludeDeletedNo)
					require.NoError(t, err)

					// Queue unrelated edit before, stale edit on moved issue in the middle,
					// and unrelated edit after.
					_, beforeEvent, err := spoke.CreateComment(ctx, db.CreateCommentParams{IssueID: localStaying.ID, Author: "worker", Body: "before stuck"})
					require.NoError(t, err)
					_, stuckEvent, err := spoke.CreateComment(ctx, db.CreateCommentParams{IssueID: localMoved.ID, Author: "worker", Body: "offline on moved issue"})
					require.NoError(t, err)
					_, afterEvent, err := spoke.CreateComment(ctx, db.CreateCommentParams{IssueID: localStaying.ID, Author: "worker", Body: "after stuck"})
					require.NoError(t, err)

					awayMove, err := hub.DB.MoveIssueProject(ctx, db.MoveIssueProjectIn{
						IssueID:       movedIssue.ID,
						FromProjectID: projects[0].ID,
						ToProjectID:   projects[1].ID,
						IfMatchRev:    movedIssue.Revision,
						Actor:         "worker",
					})
					require.NoError(t, err)

					var pulledEchoUIDs []string
					err = syncProject(0, func(_ int64, events []db.Event) {
						for _, ev := range events {
							pulledEchoUIDs = append(pulledEchoUIDs, ev.UID)
						}
					})
					require.ErrorContains(t, err, db.FederationProjectNotEnrolledCode)
					require.ErrorContains(t, err, movedIssue.UID)
					require.NotContains(t, err.Error(), projects[1].Name)
					require.NotContains(t, err.Error(), projects[1].UID)

					// Both unrelated edits (before and after the stuck edit) must reach the hub.
					hubComments, err := hub.DB.CommentsByIssue(ctx, stayingIssue.ID)
					require.NoError(t, err)
					require.Len(t, hubComments, 2)
					require.Equal(t, "before stuck", hubComments[0].Body)
					require.Equal(t, "after stuck", hubComments[1].Body)

					// The stuck edit must not be delivered yet, and must remain pending on the spoke.
					movedComments, err := hub.DB.CommentsByIssue(ctx, movedIssue.ID)
					require.NoError(t, err)
					require.Empty(t, movedComments)

					// Out-of-order accepted local edits must not re-fire as duplicate local echoes on pull.
					require.NotContains(t, pulledEchoUIDs, beforeEvent.UID)
					require.NotContains(t, pulledEchoUIDs, afterEvent.UID)

					binding, err := spoke.FederationBindingByProject(ctx, localProjects[0].ID)
					require.NoError(t, err)
					pending, err := spoke.PendingFederationPushEvents(ctx, localProjects[0].ID, spoke.InstanceUID(), binding.PushCursorEventID, 100)
					require.NoError(t, err)
					require.Len(t, pending, 1)
					require.Equal(t, stuckEvent.UID, pending[0].UID)

					// Another unrelated edit queued later must also push past the stuck edit.
					_, laterEvent, err := spoke.CreateComment(ctx, db.CreateCommentParams{IssueID: localStaying.ID, Author: "worker", Body: "later unrelated"})
					require.NoError(t, err)
					err = syncProject(0, nil)
					require.ErrorContains(t, err, db.FederationProjectNotEnrolledCode)
					require.ErrorContains(t, err, movedIssue.UID)

					hubComments, err = hub.DB.CommentsByIssue(ctx, stayingIssue.ID)
					require.NoError(t, err)
					require.Len(t, hubComments, 3)
					require.Equal(t, "later unrelated", hubComments[2].Body)

					if recovery == "enroll-destination" {
						destLocal, err := spoke.CreateProjectWithUID(ctx, projects[1].Name, projects[1].UID)
						if err != nil {
							// Already parked locally by the pulled move; restore and bind it.
							destLocal, err = spoke.ProjectByUID(ctx, projects[1].UID)
							require.NoError(t, err)
							_, _, _, err = spoke.RestoreProject(ctx, destLocal.ID, "worker")
							require.NoError(t, err)
						}
						_, err = spoke.UpsertFederationBinding(ctx, db.FederationBinding{
							ProjectID: destLocal.ID, Role: db.FederationRoleSpoke, HubURL: hub.URL,
							HubProjectID: projects[1].ID, HubProjectUID: projects[1].UID,
							Enabled: true, PushEnabled: true, Actor: "worker",
						})
						require.NoError(t, err)
						grant, err := hub.DB.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{
							SpokeInstanceUID: spoke.InstanceUID(), ProjectID: &projects[1].ID,
							Capabilities: "push,pull", Actor: "worker",
						})
						require.NoError(t, err)
						credentials[projects[1].UID] = config.FederationCredential{
							HubURL: hub.URL, HubProjectID: projects[1].ID, Token: grant.Token, Actor: "worker",
						}
					} else {
						// Verify #1289 reset safety: project A can reset while stuckEvent is held
						// and laterEvent is acknowledged out of order, without losing stuckEvent.
						binding, err = spoke.FederationBindingByProject(ctx, localProjects[0].ID)
						require.NoError(t, err)
						require.NoError(t, spoke.ResetFederatedProjectIfNoPendingPush(ctx, localProjects[0].ID, 0, 0, spoke.InstanceUID(), binding.PushCursorEventID))
						_, err = hub.DB.MoveIssueProject(ctx, db.MoveIssueProjectIn{
							IssueID:       movedIssue.ID,
							FromProjectID: projects[1].ID,
							ToProjectID:   projects[0].ID,
							IfMatchRev:    awayMove.Issue.Revision,
							Actor:         "worker",
						})
						require.NoError(t, err)
					}

					require.NoError(t, syncProject(0, nil))
					delivered, err := hub.DB.EventsByUIDs(ctx, projects[0].ID, []string{stuckEvent.UID})
					require.NoError(t, err)
					require.Len(t, delivered, 1)
					require.Equal(t, stuckEvent.ContentHash, delivered[0].ContentHash)
					movedComments, err = hub.DB.CommentsByIssue(ctx, movedIssue.ID)
					require.NoError(t, err)
					require.Len(t, movedComments, 1)
					require.Equal(t, "offline on moved issue", movedComments[0].Body)

					binding, err = spoke.FederationBindingByProject(ctx, localProjects[0].ID)
					require.NoError(t, err)
					pending, err = spoke.PendingFederationPushEvents(ctx, localProjects[0].ID, spoke.InstanceUID(), binding.PushCursorEventID, 100)
					require.NoError(t, err)
					require.Empty(t, pending)
					if recovery == "enroll-destination" {
						require.GreaterOrEqual(t, binding.PushCursorEventID, laterEvent.ID)
					}
				})
			}
		})
	}
}

// When multiple distinct issues in one batch moved to unenrolled projects, the
// refusal surfaces all of them while still delivering unrelated edits, and
// older hubs that ignore partial_not_enrolled and return 403 keep pulling.
func TestSyncFederationMultipleUnenrolledIssuesAndLegacyHub403(t *testing.T) {
	t.Run("multiple stuck issues surfaced", func(t *testing.T) {
		ctx := t.Context()
		hub, projects := newMoveDeliveryHub(t, "joined-multi", "absent-one", "absent-two")
		spoke := newMoveDeliverySpoke(t, hub, projects[:1])
		issue1, _, err := hub.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: projects[0].ID, Title: "moves to one", Author: "worker"})
		require.NoError(t, err)
		issue2, _, err := hub.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: projects[0].ID, Title: "moves to two", Author: "worker"})
		require.NoError(t, err)
		staying, _, err := hub.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: projects[0].ID, Title: "stays", Author: "worker"})
		require.NoError(t, err)
		require.NoError(t, spoke.sync(t, 0))

		local1, err := spoke.env.DB.IssueByUID(ctx, issue1.UID, db.IncludeDeletedNo)
		require.NoError(t, err)
		local2, err := spoke.env.DB.IssueByUID(ctx, issue2.UID, db.IncludeDeletedNo)
		require.NoError(t, err)
		localStay, err := spoke.env.DB.IssueByUID(ctx, staying.UID, db.IncludeDeletedNo)
		require.NoError(t, err)

		_, _, err = spoke.env.DB.CreateComment(ctx, db.CreateCommentParams{IssueID: local1.ID, Author: "worker", Body: "stuck 1"})
		require.NoError(t, err)
		_, _, err = spoke.env.DB.CreateComment(ctx, db.CreateCommentParams{IssueID: local2.ID, Author: "worker", Body: "stuck 2"})
		require.NoError(t, err)
		_, _, err = spoke.env.DB.CreateComment(ctx, db.CreateCommentParams{IssueID: localStay.ID, Author: "worker", Body: "unrelated"})
		require.NoError(t, err)

		_, err = hub.DB.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: issue1.ID, FromProjectID: projects[0].ID, ToProjectID: projects[1].ID, IfMatchRev: issue1.Revision, Actor: "worker"})
		require.NoError(t, err)
		_, err = hub.DB.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: issue2.ID, FromProjectID: projects[0].ID, ToProjectID: projects[2].ID, IfMatchRev: issue2.Revision, Actor: "worker"})
		require.NoError(t, err)

		err = spoke.sync(t, 0)
		require.ErrorContains(t, err, db.FederationProjectNotEnrolledCode)
		require.ErrorContains(t, err, issue1.UID)
		require.ErrorContains(t, err, issue2.UID)
		require.NotContains(t, err.Error(), projects[1].Name)
		require.NotContains(t, err.Error(), projects[2].Name)

		status, err := spoke.env.DB.FederationSyncStatusByProject(ctx, spoke.projects[0].ID)
		require.NoError(t, err)
		require.NotNil(t, status.LastError)
		require.Contains(t, *status.LastError, issue1.UID)
		require.Contains(t, *status.LastError, issue2.UID)

		comments, err := hub.DB.CommentsByIssue(ctx, staying.ID)
		require.NoError(t, err)
		require.Len(t, comments, 1)
		require.Equal(t, "unrelated", comments[0].Body)
	})

	t.Run("all-refused first batch still pushes subsequent refusal-free batch", func(t *testing.T) {
		ctx := t.Context()
		hub, projects := newMoveDeliveryHub(t, "joined-split", "absent-split")

		type ingestBatchResult struct {
			accepted int
			refused  int
		}
		var mu sync.Mutex
		var batches []ingestBatchResult
		targetURL, err := url.Parse(hub.URL)
		require.NoError(t, err)
		proxy := httputil.NewSingleHostReverseProxy(targetURL)
		proxy.ModifyResponse = func(resp *http.Response) error {
			if strings.HasSuffix(resp.Request.URL.Path, "/federation/events:ingest") && resp.StatusCode == http.StatusOK {
				raw, readErr := io.ReadAll(resp.Body)
				if readErr != nil {
					return readErr
				}
				resp.Body = io.NopCloser(bytes.NewReader(raw))
				var body api.FederationIngestEventsBody
				if err := json.Unmarshal(raw, &body); err == nil {
					mu.Lock()
					batches = append(batches, ingestBatchResult{accepted: body.Accepted, refused: len(body.RefusedEvents)})
					mu.Unlock()
				}
			}
			return nil
		}
		observer := httptest.NewServer(proxy)
		t.Cleanup(observer.Close)

		spoke := newMoveDeliverySpoke(t, hub, projects[:1])
		binding, err := spoke.env.DB.FederationBindingByProject(ctx, spoke.projects[0].ID)
		require.NoError(t, err)
		binding.HubURL = observer.URL
		_, err = spoke.env.DB.UpsertFederationBinding(ctx, binding)
		require.NoError(t, err)
		cred := spoke.credentials[spoke.projects[0].UID]
		cred.HubURL = observer.URL
		spoke.credentials[spoke.projects[0].UID] = cred

		moved, _, err := hub.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: projects[0].ID, Title: "moved big", Author: "worker"})
		require.NoError(t, err)
		staying, _, err := hub.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: projects[0].ID, Title: "staying after split", Author: "worker"})
		require.NoError(t, err)
		require.NoError(t, spoke.sync(t, 0))

		localMoved, err := spoke.env.DB.IssueByUID(ctx, moved.UID, db.IncludeDeletedNo)
		require.NoError(t, err)
		localStay, err := spoke.env.DB.IssueByUID(ctx, staying.UID, db.IncludeDeletedNo)
		require.NoError(t, err)

		// Give all three comments large bodies (> half of maxFederationPushIngestBodyBytes)
		// so batch 1 = [stuck1] (all-refused), batch 2 = [stuck2] (all-refused), and
		// batch 3 = [okAfter] (refusal-free) exercises the post-refusal acceptance path.
		bigBody := strings.Repeat("x", (maxFederationPushIngestBodyBytes/2)+64<<10)
		okAfterBody := bigBody + "-after-all-refused"
		_, stuck1, err := spoke.env.DB.CreateComment(ctx, db.CreateCommentParams{IssueID: localMoved.ID, Author: "worker", Body: bigBody})
		require.NoError(t, err)
		_, stuck2, err := spoke.env.DB.CreateComment(ctx, db.CreateCommentParams{IssueID: localMoved.ID, Author: "worker", Body: bigBody})
		require.NoError(t, err)
		_, okAfter, err := spoke.env.DB.CreateComment(ctx, db.CreateCommentParams{IssueID: localStay.ID, Author: "worker", Body: okAfterBody})
		require.NoError(t, err)

		_, err = hub.DB.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: moved.ID, FromProjectID: projects[0].ID, ToProjectID: projects[1].ID, IfMatchRev: moved.Revision, Actor: "worker"})
		require.NoError(t, err)

		err = spoke.sync(t, 0)
		require.ErrorContains(t, err, db.FederationProjectNotEnrolledCode)
		require.ErrorContains(t, err, moved.UID)

		mu.Lock()
		gotBatches := append([]ingestBatchResult(nil), batches...)
		mu.Unlock()
		require.Equal(t, []ingestBatchResult{
			{accepted: 0, refused: 1},
			{accepted: 0, refused: 1},
			{accepted: 1, refused: 0},
		}, gotBatches, "must exercise a refusal-free batch after all-refused batches")

		comments, err := hub.DB.CommentsByIssue(ctx, staying.ID)
		require.NoError(t, err)
		require.Len(t, comments, 1)
		require.Equal(t, okAfterBody, comments[0].Body)

		// Cursor must not have jumped past stuck1/stuck2 even though batch 3 had 0 refusals.
		binding, err = spoke.env.DB.FederationBindingByProject(ctx, spoke.projects[0].ID)
		require.NoError(t, err)
		require.Less(t, binding.PushCursorEventID, stuck1.ID)
		pending, err := spoke.env.DB.PendingFederationPushEvents(ctx, spoke.projects[0].ID, spoke.env.DB.InstanceUID(), binding.PushCursorEventID, 100)
		require.NoError(t, err)
		require.Len(t, pending, 2)
		require.Equal(t, stuck1.UID, pending[0].UID)
		require.Equal(t, stuck2.UID, pending[1].UID)
		ack, err := spoke.env.DB.IsFederationEventAcknowledged(ctx, spoke.projects[0].ID, okAfter.UID)
		require.NoError(t, err)
		require.True(t, ack)
	})

	t.Run("refused creation dependents deferred across request-size boundary", func(t *testing.T) {
		ctx := t.Context()
		hub, projects := newMoveDeliveryHub(t, "joined-cross-batch", "absent-cross-batch")
		spoke := newMoveDeliverySpoke(t, hub, projects[:1])
		moved, _, err := hub.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: projects[0].ID, Title: "moved peer", Author: "worker"})
		require.NoError(t, err)
		staying, _, err := hub.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: projects[0].ID, Title: "staying sibling", Author: "worker"})
		require.NoError(t, err)
		require.NoError(t, spoke.sync(t, 0))

		localMoved, err := spoke.env.DB.IssueByUID(ctx, moved.UID, db.IncludeDeletedNo)
		require.NoError(t, err)
		localStay, err := spoke.env.DB.IssueByUID(ctx, staying.UID, db.IncludeDeletedNo)
		require.NoError(t, err)

		// Batch 1 contains only the large issue.created linking to moved; batch 2
		// receives the follow-up update, a comment on the refused creation, a
		// cross-issue reply to that comment, and an unrelated comment on staying.
		bigBody := strings.Repeat("a", (maxFederationPushIngestBodyBytes/2)+64<<10)
		bigBodyUpdated := strings.Repeat("b", (maxFederationPushIngestBodyBytes/2)+64<<10)
		createdLocal, createEv, err := spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{
			ProjectID: spoke.projects[0].ID,
			Title:     "Created linking moved",
			Body:      bigBody,
			Author:    "worker",
			Links:     []db.InitialLink{{Type: "blocks", ToNumber: localMoved.ID}},
		})
		require.NoError(t, err)
		updatedTitle := "Updated across batch boundary"
		_, updateEv, _, err := spoke.env.DB.EditIssue(ctx, db.EditIssueParams{
			IssueID: createdLocal.ID,
			Title:   &updatedTitle,
			Body:    &bigBodyUpdated,
			Actor:   "worker",
		})
		require.NoError(t, err)
		require.NotNil(t, updateEv)
		depComment, depCommentEv, err := spoke.env.DB.CreateComment(ctx, db.CreateCommentParams{
			IssueID: createdLocal.ID, Author: "worker", Body: "comment on refused creation",
		})
		require.NoError(t, err)
		_, crossReplyEv, err := spoke.env.DB.CreateComment(ctx, db.CreateCommentParams{
			IssueID: localStay.ID, Author: "worker", Body: "reply to refused creation comment",
			ReplyToUID: depComment.UID, ReplyKind: "reply",
		})
		require.NoError(t, err)
		_, okAfterDep, err := spoke.env.DB.CreateComment(ctx, db.CreateCommentParams{
			IssueID: localStay.ID, Author: "worker", Body: "unrelated after cross-batch dep",
		})
		require.NoError(t, err)

		_, err = hub.DB.MoveIssueProject(ctx, db.MoveIssueProjectIn{
			IssueID: moved.ID, FromProjectID: projects[0].ID, ToProjectID: projects[1].ID,
			IfMatchRev: moved.Revision, Actor: "worker",
		})
		require.NoError(t, err)

		err = spoke.sync(t, 0)
		require.ErrorContains(t, err, db.FederationProjectNotEnrolledCode)
		require.ErrorContains(t, err, moved.UID)

		// Must not quarantine the project when dependent events cross a batch boundary.
		_, qErr := spoke.env.DB.ActiveFederationQuarantine(ctx, spoke.projects[0].ID, db.FederationQuarantineDirectionPush)
		require.ErrorIs(t, qErr, db.ErrNotFound)

		// Unrelated edit after the deferred dependents must reach the hub.
		comments, err := hub.DB.CommentsByIssue(ctx, staying.ID)
		require.NoError(t, err)
		require.Len(t, comments, 1)
		require.Equal(t, "unrelated after cross-batch dep", comments[0].Body)
		_, err = hub.DB.IssueByUID(ctx, createdLocal.UID, db.IncludeDeletedYes)
		require.ErrorIs(t, err, db.ErrNotFound)

		// All four dependent events remain pending; okAfterDep is acknowledged.
		binding, err := spoke.env.DB.FederationBindingByProject(ctx, spoke.projects[0].ID)
		require.NoError(t, err)
		pending, err := spoke.env.DB.PendingFederationPushEvents(ctx, spoke.projects[0].ID, spoke.env.DB.InstanceUID(), binding.PushCursorEventID, 100)
		require.NoError(t, err)
		require.Equal(t, []string{createEv.UID, updateEv.UID, depCommentEv.UID, crossReplyEv.UID}, []string{
			pending[0].UID, pending[1].UID, pending[2].UID, pending[3].UID,
		})
		ack, err := spoke.env.DB.IsFederationEventAcknowledged(ctx, spoke.projects[0].ID, okAfterDep.UID)
		require.NoError(t, err)
		require.True(t, ack)

		// Enrolling in the destination project delivers the creation and all deferred dependents.
		destLocal, err := spoke.env.DB.ProjectByUID(ctx, projects[1].UID)
		if err != nil {
			destLocal, err = spoke.env.DB.CreateProjectWithUID(ctx, projects[1].Name, projects[1].UID)
			require.NoError(t, err)
		} else {
			_, _, _, err = spoke.env.DB.RestoreProject(ctx, destLocal.ID, "worker")
			require.NoError(t, err)
		}
		_, err = spoke.env.DB.UpsertFederationBinding(ctx, db.FederationBinding{
			ProjectID: destLocal.ID, Role: db.FederationRoleSpoke, HubURL: hub.URL,
			HubProjectID: projects[1].ID, HubProjectUID: projects[1].UID,
			Enabled: true, PushEnabled: true, Actor: "worker",
		})
		require.NoError(t, err)
		grant, err := hub.DB.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{
			SpokeInstanceUID: spoke.env.DB.InstanceUID(), ProjectID: &projects[1].ID,
			Capabilities: "push,pull", Actor: "worker",
		})
		require.NoError(t, err)
		spoke.credentials[projects[1].UID] = config.FederationCredential{
			HubURL: hub.URL, HubProjectID: projects[1].ID, Token: grant.Token, Actor: "worker",
		}

		require.NoError(t, spoke.sync(t, 0))
		delivered, err := hub.DB.IssueByUID(ctx, createdLocal.UID, db.IncludeDeletedNo)
		require.NoError(t, err)
		require.Equal(t, updatedTitle, delivered.Title)
		require.Equal(t, bigBodyUpdated, delivered.Body)
		deliveredComments, err := hub.DB.CommentsByIssue(ctx, delivered.ID)
		require.NoError(t, err)
		require.Len(t, deliveredComments, 1)
		require.Equal(t, "comment on refused creation", deliveredComments[0].Body)
		comments, err = hub.DB.CommentsByIssue(ctx, staying.ID)
		require.NoError(t, err)
		require.Len(t, comments, 2)
		binding, err = spoke.env.DB.FederationBindingByProject(ctx, spoke.projects[0].ID)
		require.NoError(t, err)
		pending, err = spoke.env.DB.PendingFederationPushEvents(ctx, spoke.projects[0].ID, spoke.env.DB.InstanceUID(), binding.PushCursorEventID, 100)
		require.NoError(t, err)
		require.Empty(t, pending)
	})

	t.Run("refused move across request boundary still allows replies to existing comments", func(t *testing.T) {
		ctx := t.Context()
		hub, projects := newMoveDeliveryHub(t, "joined-move-a", "joined-move-b", "absent-move-c")
		spoke := newMoveDeliverySpoke(t, hub, projects[:2])

		bigBody := strings.Repeat("m", (maxFederationPushIngestBodyBytes/2)+64<<10)
		moved, _, err := hub.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: projects[0].ID, Title: "moved peer", Author: "worker"})
		require.NoError(t, err)
		staying, _, err := hub.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: projects[0].ID, Title: "staying sibling", Author: "worker"})
		require.NoError(t, err)
		mover, _, err := hub.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: projects[0].ID, Title: "mover issue", Body: bigBody, Author: "worker"})
		require.NoError(t, err)
		hubExistingComment, _, err := hub.DB.CreateComment(ctx, db.CreateCommentParams{IssueID: mover.ID, Author: "worker", Body: "comment already on hub"})
		require.NoError(t, err)
		require.NoError(t, spoke.sync(t, 0))
		require.NoError(t, spoke.sync(t, 1))

		localMoved, err := spoke.env.DB.IssueByUID(ctx, moved.UID, db.IncludeDeletedNo)
		require.NoError(t, err)
		localStay, err := spoke.env.DB.IssueByUID(ctx, staying.UID, db.IncludeDeletedNo)
		require.NoError(t, err)
		localMover, err := spoke.env.DB.IssueByUID(ctx, mover.UID, db.IncludeDeletedNo)
		require.NoError(t, err)

		// Move `moved` on the hub into the unjoined third project.
		_, err = hub.DB.MoveIssueProject(ctx, db.MoveIssueProjectIn{
			IssueID: moved.ID, FromProjectID: projects[0].ID, ToProjectID: projects[2].ID,
			IfMatchRev: moved.Revision, Actor: "worker",
		})
		require.NoError(t, err)

		// Offline on spoke:
		// 1. Add a comment on localMover in project A (will be accepted in Batch 1 before the move-back).
		acceptedBeforeMove, _, err := spoke.env.DB.CreateComment(ctx, db.CreateCommentParams{
			IssueID: localMover.ID, Author: "worker", Body: "accepted before move-back",
		})
		require.NoError(t, err)
		// 2. Round-trip localMover A -> B -> A, adding an unpushed comment in B and linking to moved
		//    so the move-back snapshot (> 448 KiB) carries all three comments and is refused for moved.
		movedToB, err := spoke.env.DB.MoveIssueProject(ctx, db.MoveIssueProjectIn{
			IssueID: localMover.ID, FromProjectID: spoke.projects[0].ID, ToProjectID: spoke.projects[1].ID,
			IfMatchRev: localMover.Revision, Actor: "worker",
		})
		require.NoError(t, err)
		unpushedInB, _, err := spoke.env.DB.CreateComment(ctx, db.CreateCommentParams{
			IssueID: movedToB.Issue.ID, Author: "worker", Body: "unpushed comment added in B",
		})
		require.NoError(t, err)
		_, _, err = spoke.env.DB.CreateLinkAndEvent(ctx,
			db.CreateLinkParams{FromIssueID: movedToB.Issue.ID, ToIssueID: localMoved.ID, Type: "blocks", Author: "worker"},
			db.LinkEventParams{EventType: "issue.linked", EventIssueID: movedToB.Issue.ID, FromUID: movedToB.Issue.UID, FromShortID: movedToB.Issue.ShortID, ToUID: localMoved.UID, ToShortID: localMoved.ShortID, Actor: "worker"},
		)
		require.NoError(t, err)
		refreshedMover, err := spoke.env.DB.IssueByUID(ctx, mover.UID, db.IncludeDeletedNo)
		require.NoError(t, err)
		_, err = spoke.env.DB.MoveIssueProject(ctx, db.MoveIssueProjectIn{
			IssueID: refreshedMover.ID, FromProjectID: spoke.projects[1].ID, ToProjectID: spoke.projects[0].ID,
			IfMatchRev: refreshedMover.Revision, Actor: "worker",
		})
		require.NoError(t, err)

		// 3. Replies on staying forced into Batch 2 by bigBody:
		//    - reply to hubExistingComment (already on hub -> MUST push)
		//    - reply to acceptedBeforeMove (accepted earlier in Batch 1 -> MUST push)
		//    - reply to unpushedInB (only inside refused move snapshot -> MUST stay deferred)
		replyHubExisting, _, err := spoke.env.DB.CreateComment(ctx, db.CreateCommentParams{
			IssueID: localStay.ID, Author: "worker", Body: bigBody + "-reply-hub-existing",
			ReplyToUID: hubExistingComment.UID, ReplyKind: "reply",
		})
		require.NoError(t, err)
		replyAcceptedBefore, _, err := spoke.env.DB.CreateComment(ctx, db.CreateCommentParams{
			IssueID: localStay.ID, Author: "worker", Body: "reply-accepted-before-move",
			ReplyToUID: acceptedBeforeMove.UID, ReplyKind: "reply",
		})
		require.NoError(t, err)
		replyUnpushedInB, _, err := spoke.env.DB.CreateComment(ctx, db.CreateCommentParams{
			IssueID: localStay.ID, Author: "worker", Body: "reply-unpushed-in-b",
			ReplyToUID: unpushedInB.UID, ReplyKind: "reply",
		})
		require.NoError(t, err)

		err = spoke.sync(t, 0)
		require.ErrorContains(t, err, db.FederationProjectNotEnrolledCode)
		require.ErrorContains(t, err, moved.UID)

		_, qErr := spoke.env.DB.ActiveFederationQuarantine(ctx, spoke.projects[0].ID, db.FederationQuarantineDirectionPush)
		require.ErrorIs(t, qErr, db.ErrNotFound)

		stayingComments, err := hub.DB.CommentsByIssue(ctx, staying.ID)
		require.NoError(t, err)
		var stayingCommentUIDs []string
		for _, c := range stayingComments {
			stayingCommentUIDs = append(stayingCommentUIDs, c.UID)
		}
		require.Contains(t, stayingCommentUIDs, replyHubExisting.UID, "reply to comment already on hub must publish across request boundary")
		require.Contains(t, stayingCommentUIDs, replyAcceptedBefore.UID, "reply to comment accepted earlier in the pass must publish across request boundary")
		require.NotContains(t, stayingCommentUIDs, replyUnpushedInB.UID, "reply to comment only introduced by the refused move must remain deferred")
	})

	t.Run("legacy hub ignoring partial_not_enrolled still pulls after 403", func(t *testing.T) {
		ctx := t.Context()
		hub, projects := newMoveDeliveryHub(t, "joined-legacy", "absent-legacy")
		// Proxy strips partial_not_enrolled to simulate an older hub that rejects the whole batch with 403.
		legacyProxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			cloned := r.Clone(r.Context())
			q := cloned.URL.Query()
			q.Del("partial_not_enrolled")
			cloned.URL.RawQuery = q.Encode()
			targetURL, err := url.Parse(hub.URL)
			require.NoError(t, err)
			httputil.NewSingleHostReverseProxy(targetURL).ServeHTTP(w, cloned)
		}))
		t.Cleanup(legacyProxy.Close)

		spoke := newMoveDeliverySpoke(t, hub, projects[:1])
		binding, err := spoke.env.DB.FederationBindingByProject(ctx, spoke.projects[0].ID)
		require.NoError(t, err)
		binding.HubURL = legacyProxy.URL
		_, err = spoke.env.DB.UpsertFederationBinding(ctx, binding)
		require.NoError(t, err)
		cred := spoke.credentials[spoke.projects[0].UID]
		cred.HubURL = legacyProxy.URL
		spoke.credentials[spoke.projects[0].UID] = cred

		moved, _, err := hub.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: projects[0].ID, Title: "legacy moved", Author: "worker"})
		require.NoError(t, err)
		require.NoError(t, spoke.sync(t, 0))
		localMoved, err := spoke.env.DB.IssueByUID(ctx, moved.UID, db.IncludeDeletedNo)
		require.NoError(t, err)
		_, _, err = spoke.env.DB.CreateComment(ctx, db.CreateCommentParams{IssueID: localMoved.ID, Author: "worker", Body: "legacy stuck"})
		require.NoError(t, err)
		_, err = hub.DB.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: moved.ID, FromProjectID: projects[0].ID, ToProjectID: projects[1].ID, IfMatchRev: moved.Revision, Actor: "worker"})
		require.NoError(t, err)
		later, _, err := hub.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: projects[0].ID, Title: "pulled despite 403", Author: "worker"})
		require.NoError(t, err)

		err = spoke.sync(t, 0)
		require.ErrorContains(t, err, db.FederationProjectNotEnrolledCode)
		require.ErrorContains(t, err, moved.UID)
		_, err = spoke.env.DB.IssueByUID(ctx, later.UID, db.IncludeDeletedNo)
		require.NoError(t, err, "spoke still pulls from legacy hub after 403")
	})
}

func heldPollFixture(projectID, cursor int64, target string) *hubLongPolls {
	poll := &hubLongPoll{cancel: func() {}, done: make(chan struct{})}
	polls := &hubLongPolls{polls: map[int64]*hubLongPoll{projectID: poll}}
	polls.setHeld(poll, cursor, target, func() {})
	return polls
}
