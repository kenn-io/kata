package federation

import (
	"bytes"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	clientpkg "go.kenn.io/kata/internal/client"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/jsonl"
	"go.kenn.io/kata/internal/testenv"
)

type moveDeliverySpoke struct {
	env         *testenv.Env
	projects    []db.Project
	credentials moveCredentialStore
}

func newMoveDeliveryHub(t *testing.T, names ...string) (*testenv.Env, []db.Project) {
	t.Helper()
	hub := testenv.New(t)
	projects := make([]db.Project, len(names))
	for i, name := range names {
		var err error
		projects[i], err = hub.DB.CreateProject(t.Context(), name)
		require.NoError(t, err)
		_, err = hub.DB.EnableProjectFederation(t.Context(), projects[i].ID, "worker")
		require.NoError(t, err)
	}
	return hub, projects
}

func newMoveDeliverySpoke(t *testing.T, hub *testenv.Env, projects []db.Project) moveDeliverySpoke {
	t.Helper()
	spoke := moveDeliverySpoke{env: testenv.New(t), projects: make([]db.Project, len(projects)), credentials: newMoveCredentialStore()}
	for i, project := range projects {
		var err error
		spoke.projects[i], err = spoke.env.DB.CreateProjectWithUID(t.Context(), project.Name, project.UID)
		require.NoError(t, err)
		_, err = spoke.env.DB.UpsertFederationBinding(t.Context(), db.FederationBinding{ProjectID: spoke.projects[i].ID, Role: db.FederationRoleSpoke, HubURL: hub.URL, HubProjectID: project.ID, HubProjectUID: project.UID, Enabled: true, PushEnabled: true, Actor: "worker"})
		require.NoError(t, err)
		grant, err := hub.DB.CreateFederationEnrollment(t.Context(), db.CreateFederationEnrollmentParams{SpokeInstanceUID: spoke.env.DB.InstanceUID(), ProjectID: &project.ID, Capabilities: "push,pull", Actor: "worker"})
		require.NoError(t, err)
		spoke.credentials[project.UID] = config.FederationCredential{HubURL: hub.URL, HubProjectID: project.ID, Token: grant.Token, Actor: "worker"}
	}
	return spoke
}

func (s moveDeliverySpoke) sync(t *testing.T, index int) error {
	t.Helper()
	binding, err := s.env.DB.FederationBindingByProject(t.Context(), s.projects[index].ID)
	if err != nil {
		return err
	}
	return SyncFederationOnce(t.Context(), s.env.DB, binding, s.credentials[s.projects[index].UID], s.credentials)
}

func (s moveDeliverySpoke) move(t *testing.T, issue db.Issue, from, to int) db.Issue {
	t.Helper()
	moved, err := s.env.DB.MoveIssueProject(t.Context(), db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: s.projects[from].ID, ToProjectID: s.projects[to].ID, IfMatchRev: issue.Revision, Actor: "worker"})
	require.NoError(t, err)
	return moved.Issue
}

func TestSyncFederationMoveBackKeepsCompletedLocalReplaySilent(t *testing.T) {
	ctx := t.Context()
	hub, projects := newMoveDeliveryHub(t, "source-project", "target-project")
	spoke := newMoveDeliverySpoke(t, hub, projects)
	issue, created, err := spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: spoke.projects[0].ID, Title: "Local issue", Author: "worker",
	})
	require.NoError(t, err)
	require.NoError(t, spoke.sync(t, 0))
	hubEvents, err := hub.DB.EventsByUIDs(ctx, projects[0].ID, []string{created.UID})
	require.NoError(t, err)
	require.Len(t, hubEvents, 1)
	originalHubID := hubEvents[0].ID

	pull := func(project int) []string {
		t.Helper()
		binding, err := spoke.env.DB.FederationBindingByProject(ctx, spoke.projects[project].ID)
		require.NoError(t, err)
		var delivered []string
		err = SyncFederationPullOnlyOnceWithPulledEvents(ctx, spoke.env.DB, binding, spoke.credentials[projects[project].UID], clientpkg.Opts{}, func(_ int64, events []db.Event) {
			for _, event := range events {
				delivered = append(delivered, event.UID)
			}
		}, spoke.credentials)
		require.NoError(t, err)
		return delivered
	}
	require.NoError(t, spoke.env.DB.ResetFederatedProject(ctx, spoke.projects[0].ID, originalHubID, originalHubID-1))
	require.Contains(t, pull(0), created.UID, "a fresh reset replay delivers the original event")
	completed, err := spoke.env.DB.FederationBindingByProject(ctx, spoke.projects[0].ID)
	require.NoError(t, err)
	require.GreaterOrEqual(t, completed.PullCursorEventID, originalHubID)

	current, err := hub.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	away, err := hub.DB.MoveIssueProject(ctx, db.MoveIssueProjectIn{
		IssueID: current.ID, FromProjectID: projects[0].ID, ToProjectID: projects[1].ID, IfMatchRev: current.Revision, Actor: "worker",
	})
	require.NoError(t, err)
	pull(1)
	local, err := spoke.env.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, spoke.projects[1].ID, local.ProjectID)

	_, err = hub.DB.MoveIssueProject(ctx, db.MoveIssueProjectIn{
		IssueID: away.Issue.ID, FromProjectID: projects[1].ID, ToProjectID: projects[0].ID, IfMatchRev: away.Issue.Revision, Actor: "worker",
	})
	require.NoError(t, err)
	require.NotContains(t, pull(0), created.UID, "history backfill must not deliver the completed replay again")
	local, err = spoke.env.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, spoke.projects[0].ID, local.ProjectID)
}

func TestSyncFederationMoveResetDeliversLocalReplayOnceAcrossBindings(t *testing.T) {
	for _, mode := range []string{"current", "legacy", "silent"} {
		t.Run(mode, func(t *testing.T) {
			ctx := t.Context()
			hub, projects := newMoveDeliveryHub(t, "source-project", "target-project")
			spoke := newMoveDeliverySpoke(t, hub, projects)
			issue, created, err := spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{
				ProjectID: spoke.projects[0].ID, Title: "Local issue", Author: "worker",
			})
			require.NoError(t, err)
			require.NoError(t, spoke.sync(t, 0))
			hubEvents, err := hub.DB.EventsByUIDs(ctx, projects[0].ID, []string{created.UID})
			require.NoError(t, err)
			require.Len(t, hubEvents, 1)
			originalHubID := hubEvents[0].ID
			source, err := spoke.env.DB.FederationBindingByProject(ctx, spoke.projects[0].ID)
			require.NoError(t, err)
			require.GreaterOrEqual(t, source.PullCursorEventID, originalHubID)

			current, err := hub.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
			moved, err := hub.DB.MoveIssueProject(ctx, db.MoveIssueProjectIn{
				IssueID: current.ID, FromProjectID: projects[0].ID, ToProjectID: projects[1].ID, IfMatchRev: current.Revision, Actor: "worker",
			})
			require.NoError(t, err)
			require.NoError(t, spoke.sync(t, 1))
			destination, err := spoke.env.DB.FederationBindingByProject(ctx, spoke.projects[1].ID)
			require.NoError(t, err)
			require.NoError(t, spoke.env.DB.ResetFederatedProject(ctx, destination.ProjectID, destination.ReplayHorizonEventID, moved.EventID-1))
			require.NoError(t, spoke.env.DB.RecordFederationSyncReset(ctx, destination.ProjectID, time.Now().UTC()))

			pull := func(project int) (int, error) {
				t.Helper()
				binding, err := spoke.env.DB.FederationBindingByProject(ctx, spoke.projects[project].ID)
				require.NoError(t, err)
				delivered := 0
				err = SyncFederationPullOnlyOnceWithPulledEvents(ctx, spoke.env.DB, binding, spoke.credentials[projects[project].UID], clientpkg.Opts{}, func(_ int64, events []db.Event) {
					for _, event := range events {
						if event.UID == created.UID {
							delivered++
						}
					}
				}, spoke.credentials)
				return delivered, err
			}
			t.Setenv("KATA_TEST_FEDERATION_FAILPOINTS", "during_spoke_pull_apply_before_materialize=unexpected")
			delivered, err := pull(1)
			require.ErrorContains(t, err, "during_spoke_pull_apply_before_materialize")
			require.Zero(t, delivered)
			_, err = spoke.env.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedYes)
			require.ErrorIs(t, err, db.ErrNotFound)
			retained, err := spoke.env.DB.EventsByUIDs(ctx, source.ProjectID, []string{created.UID})
			require.NoError(t, err)
			require.Len(t, retained, 1, "the destination replay restored the original envelope before failing")
			if mode == "legacy" {
				_, err = spoke.env.DB.ExecContext(ctx, `UPDATE meta SET value='50' WHERE key='schema_version'`)
				require.NoError(t, err)
				var backup bytes.Buffer
				require.NoError(t, jsonl.Export(ctx, spoke.env.DB, &backup, jsonl.ExportOptions{IncludeDeleted: true}))
				restored, err := sqlitestore.Open(ctx, filepath.Join(t.TempDir(), "restored.db"))
				require.NoError(t, err)
				t.Cleanup(func() { _ = restored.Close() })
				require.NoError(t, jsonl.Import(ctx, &backup, restored))
				spoke.env.DB = restored
			}
			if mode == "silent" {
				_, err = spoke.env.DB.ExecContext(ctx, `UPDATE federation_local_replays SET delivery_due=0 WHERE event_uid=?`, created.UID)
				require.NoError(t, err)
			}

			t.Setenv("KATA_TEST_FEDERATION_FAILPOINTS", "")
			delivered, err = pull(1)
			require.NoError(t, err)
			if mode == "silent" {
				assert.Zero(t, delivered, "an explicitly silent replay stays silent")
			} else {
				assert.Equal(t, 1, delivered, "the interrupted destination replay owes delivery despite the source cursor")
			}
			local, err := spoke.env.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedYes)
			require.NoError(t, err)
			require.Equal(t, destination.ProjectID, local.ProjectID)

			require.NoError(t, spoke.env.DB.ResetFederatedProject(ctx, source.ProjectID, originalHubID, originalHubID-1))
			delivered, err = pull(0)
			require.NoError(t, err)
			assert.Zero(t, delivered, "source catch-up must not deliver the destination replay again")
		})
	}
}

func TestSyncFederationUnsyncedCreateRoundTrip(t *testing.T) {
	hub, projects := newMoveDeliveryHub(t, "roundtrip-source", "roundtrip-target")
	spoke := newMoveDeliverySpoke(t, hub, projects)
	issue, _, err := spoke.env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: spoke.projects[0].ID, Title: "unsynced round trip", Author: "worker"})
	require.NoError(t, err)
	issue = spoke.move(t, issue, 0, 1)
	spoke.move(t, issue, 1, 0)
	require.NoError(t, spoke.sync(t, 0), "an earlier creation in this batch satisfies the return move")
	require.NoError(t, spoke.sync(t, 1))
	require.NoError(t, spoke.sync(t, 0))
	got, err := hub.DB.IssueByUID(t.Context(), issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, projects[0].ID, got.ProjectID)
	require.Equal(t, "unsynced round trip", got.Title)
}

func TestSyncFederationRetriesReverseOrderOfflineMoves(t *testing.T) {
	hub, projects := newMoveDeliveryHub(t, "chain-source", "chain-middle", "chain-target")
	spoke := newMoveDeliverySpoke(t, hub, projects)
	issue, _, err := hub.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: projects[0].ID, Title: "offline chain", Author: "worker"})
	require.NoError(t, err)
	for i := range projects {
		require.NoError(t, spoke.sync(t, i))
	}
	local, err := spoke.env.DB.IssueByUID(t.Context(), issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	local = spoke.move(t, local, 0, 1)
	spoke.move(t, local, 1, 2)
	err = spoke.sync(t, 2)
	var status *HubStatusError
	require.ErrorAs(t, err, &status)
	if status == nil {
		t.Fatal("missing hub status")
	}
	require.Equal(t, http.StatusNotFound, status.StatusCode, "an undelivered predecessor is retryable")
	_, err = spoke.env.DB.ActiveFederationQuarantine(t.Context(), spoke.projects[2].ID, db.FederationQuarantineDirectionPush)
	require.ErrorIs(t, err, db.ErrNotFound)
	binding, err := spoke.env.DB.FederationBindingByProject(t.Context(), spoke.projects[2].ID)
	require.NoError(t, err)
	require.True(t, binding.Enabled, "ordinary scheduling must retain the retrying destination")
	require.NoError(t, spoke.sync(t, 1))
	require.NoError(t, spoke.sync(t, 2), "ordinary retry should finish the chain")
	got, err := hub.DB.IssueByUID(t.Context(), issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, projects[2].ID, got.ProjectID)
}

func TestSyncFederationDelayedSourceCommentAfterLeave(t *testing.T) {
	hub, projects := newMoveDeliveryHub(t, "delayed-source", "delayed-target")
	receiver := newMoveDeliverySpoke(t, hub, projects)
	sender := newMoveDeliverySpoke(t, hub, projects)
	issue, _, err := hub.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: projects[0].ID, Title: "delayed source history", Author: "worker"})
	require.NoError(t, err)
	for i := range projects {
		require.NoError(t, receiver.sync(t, i))
		require.NoError(t, sender.sync(t, i))
	}
	old, err := sender.env.DB.IssueByUID(t.Context(), issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	_, _, err = sender.env.DB.CreateComment(t.Context(), db.CreateCommentParams{IssueID: old.ID, Author: "worker", Body: "offline source comment"})
	require.NoError(t, err)
	local, err := receiver.env.DB.IssueByUID(t.Context(), issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	receiver.move(t, local, 0, 1)
	require.NoError(t, receiver.sync(t, 1))
	_, err = receiver.env.DB.LeaveFederationReplica(t.Context(), receiver.projects[0].ID)
	require.NoError(t, err)
	require.NoError(t, receiver.credentials.DeleteFederationCredential(t.Context(), receiver.projects[0].UID))
	require.NoError(t, sender.sync(t, 0), "the delayed comment is accepted with its original project envelope")
	_, _, err = hub.DB.CreateComment(t.Context(), db.CreateCommentParams{IssueID: issue.ID, Author: "worker", Body: "destination follow-up"})
	require.NoError(t, err)
	require.NoError(t, receiver.sync(t, 1), "retained source identity must suffice after allowed source teardown")
	got, err := receiver.env.DB.IssueByUID(t.Context(), issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, receiver.projects[1].ID, got.ProjectID)
	comments, err := receiver.env.DB.CommentsByIssue(t.Context(), got.ID)
	require.NoError(t, err)
	bodies := make([]string, 0, len(comments))
	for _, comment := range comments {
		bodies = append(bodies, comment.Body)
	}
	require.ElementsMatch(t, []string{"offline source comment", "destination follow-up"}, bodies)
}
