package daemon_test

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// projectAccessCatalogChurnStore runs a concurrent change at a precise point
// of a request: while show-issue hydrates comments, or right after an issue
// create commits.
type projectAccessCatalogChurnStore struct {
	db.Storage
	beforeComments   func(issueID int64)
	afterCreateIssue func()
}

func (s projectAccessCatalogChurnStore) ReadCommentGraph(ctx context.Context, query db.CommentGraphQuery) (db.CommentGraphData, error) {
	if s.beforeComments != nil {
		s.beforeComments(query.SelectedIssueID)
	}
	return s.Storage.ReadCommentGraph(ctx, query)
}

func (s projectAccessCatalogChurnStore) CreateIssue(
	ctx context.Context, params db.CreateIssueParams,
) (db.Issue, db.Event, error) {
	issue, event, err := s.Storage.CreateIssue(ctx, params)
	if err == nil && s.afterCreateIssue != nil {
		s.afterCreateIssue()
	}
	return issue, event, err
}

// projectAccessCatalogChurn moves an issue between two projects the member
// cannot see and creates a new unrestricted project. Neither change touches
// the member's existing projects.
type projectAccessCatalogChurn struct {
	t      *testing.T
	store  db.Storage
	a, b   db.Project
	issue  db.Issue
	rounds int
}

func newProjectAccessCatalogChurn(t *testing.T, store db.Storage) *projectAccessCatalogChurn {
	t.Helper()
	a := projectAccessHiddenProject(t, store, "churn-hidden-a")
	b := projectAccessHiddenProject(t, store, "churn-hidden-b")
	issue, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{
		ProjectID: a.ID, Title: "Hidden mover", Author: "other-member",
	})
	require.NoError(t, err)
	return &projectAccessCatalogChurn{t: t, store: store, a: a, b: b, issue: issue}
}

func (c *projectAccessCatalogChurn) run() {
	c.t.Helper()
	from, to := c.a, c.b
	if c.rounds%2 == 1 {
		from, to = c.b, c.a
	}
	moved, err := c.store.MoveIssueProject(c.t.Context(), db.MoveIssueProjectIn{
		IssueID: c.issue.ID, FromProjectID: from.ID, ToProjectID: to.ID,
		IfMatchRev: c.issue.Revision, Actor: "admin",
	})
	require.NoError(c.t, err)
	c.issue.Revision = moved.NewRevision
	_, err = c.store.CreateProject(c.t.Context(), fmt.Sprintf("churn-created-%d", c.rounds))
	require.NoError(c.t, err)
	c.rounds++
}

func TestProjectAccessRequestsSurviveUnrelatedCatalogChanges(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newProjectAccessFixture(t, store)
		churn := newProjectAccessCatalogChurn(t, store)
		f = projectAccessFixtureWithStorage(t, f, projectAccessCatalogChurnStore{
			Storage:          store,
			beforeComments:   func(int64) { churn.run() },
			afterCreateIssue: churn.run,
		})

		status, _, body := f.request(t, http.MethodPost,
			fmt.Sprintf("/api/v1/projects/%d/issues", f.public.ID), "member",
			map[string]any{"actor": "member", "title": "Committed during catalog change", "force_new": true}, nil)
		assert.Equal(t, http.StatusOK, status, string(body))
		assert.Contains(t, string(body), "Committed during catalog change")
		issues, err := store.ListIssues(t.Context(), db.ListIssuesParams{ProjectID: f.public.ID})
		require.NoError(t, err)
		assert.Len(t, issues, 2, "the create committed exactly once")

		status, _, body = f.request(t, http.MethodGet,
			fmt.Sprintf("/api/v1/projects/%d/issues/%s", f.public.ID, f.visible.ShortID), "member", nil, nil)
		assert.Equal(t, http.StatusOK, status, string(body))
		assert.Contains(t, string(body), "Visible task")
		assert.Equal(t, 2, churn.rounds, "both requests ran their concurrent catalog change")
	})
}

func TestProjectAccessShowIssueWithheldAfterMembershipRevokedMidRead(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newProjectAccessFixture(t, store)
		f = projectAccessFixtureWithStorage(t, f, projectAccessCatalogChurnStore{
			Storage: store,
			beforeComments: func(int64) {
				_, err := store.SetTeamMembership(t.Context(), f.team.UID, "member", false, "admin")
				require.NoError(t, err)
			},
		})

		status, _, body := f.request(t, http.MethodGet,
			fmt.Sprintf("/api/v1/projects/%d/issues/%s", f.private.ID, f.issue.ShortID), "member", nil, nil)
		assert.Equal(t, http.StatusNotFound, status, string(body))
		assert.NotContains(t, string(body), projectAccessCanary)
	})
}

func TestProjectAccessStreamSurvivesUnrelatedIssueMove(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newProjectAccessFixture(t, store)
		churn := newProjectAccessCatalogChurn(t, store)
		cursor, err := store.MaxEventID(t.Context())
		require.NoError(t, err)
		response, stream := openProjectAccessSSE(t, f, cursor, "member")
		defer func() { _ = response.Body.Close() }()

		createProjectAccessIssue(t, f, f.public.ID, "member", "Before move")
		first, ok := stream.Next(t, 2*time.Second)
		require.True(t, ok, "the authenticated stream should be subscribed before the move")
		assert.Equal(t, "issue.created", first.event)

		moved, err := store.MoveIssueProject(t.Context(), db.MoveIssueProjectIn{
			IssueID: churn.issue.ID, FromProjectID: churn.a.ID, ToProjectID: churn.b.ID,
			IfMatchRev: churn.issue.Revision, Actor: "admin",
		})
		require.NoError(t, err)
		createProjectAccessIssue(t, f, f.public.ID, "member", "After move")

		next, ok := stream.Next(t, 2*time.Second)
		require.True(t, ok, "the stream must stay open across a move between projects it cannot see")
		assert.Equal(t, "issue.created", next.event)
		assert.Contains(t, next.data, "After move")
		assert.NotContains(t, next.data, churn.b.UID)
		assert.NotEqual(t, fmt.Sprint(moved.EventID), next.id)
	})
}
