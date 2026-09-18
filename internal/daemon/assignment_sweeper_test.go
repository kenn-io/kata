package daemon_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

func TestAssignmentSweeperExpiresLocalAndHubAssignmentsButSkipsSpokes(t *testing.T) {
	ctx := context.Background()
	env := testenv.New(t)
	now := time.Date(2026, time.September, 17, 14, 0, 0, 0, time.UTC)

	localProject, localIssue := createAssignmentSweepIssue(t, env, "local-project")
	hubProject, hubIssue := createAssignmentSweepIssue(t, env, "hub-project")
	_, err := env.DB.EnableProjectFederation(ctx, hubProject.ID, "operator")
	require.NoError(t, err)
	spokeProject, spokeIssue := createAssignmentSweepIssue(t, env, "spoke-project")

	for _, issue := range []db.Issue{localIssue, hubIssue, spokeIssue} {
		_, err := env.DB.ClaimOwner(ctx, db.ClaimOwnerParams{
			IssueID: issue.ID,
			Actor:   "worker",
			TTL:     time.Minute,
			Now:     now.Add(-2 * time.Minute),
		})
		require.NoError(t, err)
	}
	_, err = env.DB.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID:            spokeProject.ID,
		Role:                 db.FederationRoleSpoke,
		HubURL:               "http://127.0.0.1:1",
		HubProjectID:         hubProject.ID,
		HubProjectUID:        hubProject.UID,
		ReplayHorizonEventID: 1,
		Enabled:              true,
	})
	require.NoError(t, err)

	sub := env.Broadcaster.Subscribe(daemon.SubFilter{})
	defer sub.Unsub()
	sweeper := daemon.NewAssignmentSweeper(env.DB, daemon.NewEventPublisher(env.Broadcaster, nil))
	require.NoError(t, sweeper.RunOnce(ctx, now))

	projects := map[int64]bool{}
	for range 2 {
		msg := receiveMsg(t, sub.Ch, time.Second, "assignment expiry broadcast")
		require.NotNil(t, msg.Event)
		assert.Equal(t, "issue.assignment_expired", msg.Event.Type)
		projects[msg.ProjectID] = true
	}
	assert.Equal(t, map[int64]bool{localProject.ID: true, hubProject.ID: true}, projects)
	assertAssignmentSweepOwner(t, env, localIssue.ID, nil)
	assertAssignmentSweepOwner(t, env, hubIssue.ID, nil)
	worker := "worker"
	assertAssignmentSweepOwner(t, env, spokeIssue.ID, &worker)
}

func createAssignmentSweepIssue(t *testing.T, env *testenv.Env, projectName string) (db.Project, db.Issue) {
	t.Helper()
	project, err := env.DB.CreateProject(t.Context(), projectName)
	require.NoError(t, err)
	issue, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{
		ProjectID: project.ID,
		Title:     "expired assignment",
		Author:    "operator",
	})
	require.NoError(t, err)
	return project, issue
}

func assertAssignmentSweepOwner(t *testing.T, env *testenv.Env, issueID int64, want *string) {
	t.Helper()
	issue, err := env.DB.IssueByID(t.Context(), issueID)
	require.NoError(t, err)
	assert.Equal(t, want, issue.Owner)
}
