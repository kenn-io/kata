package dbtest

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// Preparation must authorize the actual current parent, independently of the
// caller's proposed peers. A later adjudication refusal cannot repair issuance.
func RunScopedExpectationCurrentParent(t *testing.T, s db.Storage) {
	t.Helper()
	f := NewScopedCaptureFixture(t, s)
	stable, ok := s.(scopedStablePreparationStore)
	require.True(t, ok)
	normal, ok := s.(scopedPreparationStore)
	require.True(t, ok)
	writeP1 := db.WithRootAttribution(t.Context(), ScopedCaptureSigner(t, f, f.Projects[0].UID), "member")
	writeP2 := db.WithRootAttribution(t.Context(), ScopedCaptureSigner(t, f, f.Projects[1].UID), "member")
	current, _, err := s.CreateIssue(writeP2, db.CreateIssueParams{ProjectID: f.Projects[1].ID, Author: "member", Title: "Current cross-project parent"})
	require.NoError(t, err)
	proposed, _, err := s.CreateIssue(writeP1, db.CreateIssueParams{ProjectID: f.Projects[0].ID, Author: "member", Title: "Proposed same-project parent"})
	require.NoError(t, err)
	_, err = s.CreateLink(t.Context(), db.CreateLinkParams{FromIssueID: f.Issue.ID, ToIssueID: current.ID, Type: "parent", Author: "member"})
	require.NoError(t, err, "native project-independent parent edge must be established before preparation")
	link, err := s.ParentOf(t.Context(), f.Issue.ID)
	require.NoError(t, err)
	require.Equal(t, current.ID, link.ToIssueID)
	require.Equal(t, current.UID, link.ToIssueUID)

	view := f.Params.View
	ctx := db.WithScopedSourceAccess(t.Context(), db.ScopedSourceAccess{View: view.Key, Principal: db.CustodyPrincipal{AuthorityUID: view.AuthorityUID, BindingUID: view.Route[0].BindingUID, Epoch: view.Route[0].Epoch, AccountableActor: f.Parent.Actor, RouteDigest: view.RouteDigest}})
	wide := db.WithAuthorizedProjects(ctx, []string{f.Projects[0].UID, f.Projects[1].UID})
	readParent, err := s.IssueByID(wide, link.ToIssueID)
	require.NoError(t, err, "readable current-parent counterpart must precede refusal controls")
	require.Equal(t, current.ProjectUID, readParent.ProjectUID)
	complete := db.ScopedMutationPreparation{
		Kind:      "editIssue",
		Subject:   db.ScopedMutationSubject{Kind: "issue", ProjectUID: f.Issue.ProjectUID, IssueUID: f.Issue.UID},
		Predicate: "parent",
		Peers: []db.ScopedEndpoint{
			{ProjectUID: current.ProjectUID, IssueUID: current.UID},
			{ProjectUID: proposed.ProjectUID, IssueUID: proposed.UID},
		},
	}
	token, err := stable.PrepareScopedMutation(wide, complete)
	require.NoError(t, err, "complete readable current and proposed parents enter the real native core")
	require.Equal(t, complete, scopedPreparedPredicate(t, s, token).Preparation)
	command := db.ScopedCommand{
		Kind:    "editIssue",
		Subject: db.ScopedEndpoint{ProjectUID: f.Issue.ProjectUID, IssueUID: f.Issue.UID},
		Actor:   "source-label",
		Edit:    &db.EditIssueAtomicParams{IssueID: f.Issue.ID, Actor: "source-label", SetParent: &proposed.ID},
	}
	commandToken, err := normal.PrepareMutation(wide, command)
	require.NoError(t, err, "the normal adapter has the same readable native counterpart")
	require.NotEmpty(t, commandToken.Token)

	narrow := db.WithAuthorizedProjects(ctx, []string{f.Projects[0].UID})
	_, err = s.IssueByID(narrow, f.Issue.ID)
	require.NoError(t, err)
	_, err = s.IssueByID(narrow, proposed.ID)
	require.NoError(t, err)
	_, err = s.IssueByID(narrow, current.ID)
	require.ErrorIs(t, err, db.ErrNotFound, "the real P2 parent is outside this caller's readable projects")
	for _, name := range []string{"stable_omitted_current", "stable_misclaimed_current", "command_proposed_only"} {
		t.Run(name, func(t *testing.T) {
			before := scopedAuthoritySnapshot(t, s)
			request := complete
			request.Peers = nil
			if name == "stable_misclaimed_current" {
				request.Peers = []db.ScopedEndpoint{{ProjectUID: proposed.ProjectUID, IssueUID: proposed.UID}}
			}
			var refused db.MutationExpectation
			var err error
			if name == "command_proposed_only" {
				refused, err = normal.PrepareMutation(narrow, command)
			} else {
				refused, err = stable.PrepareScopedMutation(narrow, request)
			}
			require.ErrorIs(t, err, db.ErrNotFound, "caller peers cannot hide the canonical unreadable current parent")
			require.NotContains(t, err.Error(), current.UID)
			require.Empty(t, refused.Token)
			require.Equal(t, before, scopedAuthoritySnapshot(t, s), "refusal must not create a token or mutate private predicate state")
		})
	}
}
