package dbtest

import (
	"crypto/ed25519"
	"crypto/rand"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// RunRootNativeAttribution exercises root native attribution on the supplied native store.
// R5/Astra: direct root writes and signed acceptance commit together. The
// account comes from trusted request context, never from declared source labels.
func RunRootNativeAttribution(t *testing.T, store db.Storage) {
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "root-project")
	require.NoError(t, err)
	_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleHub, HubProjectID: project.ID, HubProjectUID: project.UID, Enabled: true})
	require.NoError(t, err)
	public, private, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	pin := db.RootKeyPin{ProjectUID: project.UID, AuthorityUID: store.InstanceUID(), KeyID: db.RootPublicKeyID(public), PublicKey: public}
	require.NoError(t, store.PinRootAuthority(ctx, pin))
	legacy, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Existing legacy task", Author: "legacy-agent"})
	require.NoError(t, err)
	signer := db.RootAttributionSigner{AuthorityUID: pin.AuthorityUID, PrivateKey: private}
	writeCtx := db.WithRootAttribution(ctx, signer, "company-member")
	issue, event, err := store.CreateIssue(writeCtx, db.CreateIssueParams{ProjectID: project.ID, Title: "New root task", Author: "assistant"})
	require.NoError(t, err)
	receipt, err := store.EntityAttribution(ctx, project.UID, "issue", issue.UID)
	require.NoError(t, err)
	require.NoError(t, db.VerifyRootReceipt(pin, receipt))
	require.Equal(t, "company-member", receipt.AccountableActor)
	require.Equal(t, "assistant", receipt.SourceActor)
	require.Equal(t, event.ContentHash, receipt.ContentHash)
	require.Equal(t, store.InstanceUID(), receipt.IngressInstanceUID)
	comment, _, err := store.CreateComment(writeCtx, db.CreateCommentParams{IssueID: issue.ID, Author: "comment-assistant", Teammate: "reviewer", Body: "Root comment"})
	require.NoError(t, err)
	commentReceipt, err := store.EntityAttribution(ctx, project.UID, "comment", comment.UID)
	require.NoError(t, err)
	require.Equal(t, "company-member", commentReceipt.AccountableActor)
	require.Equal(t, "reviewer", commentReceipt.Teammate)
	title := "Edited root task"
	_, _, _, err = store.EditIssue(db.WithRootAttribution(ctx, signer, "other-member"), db.EditIssueParams{IssueID: issue.ID, Actor: "editor", Title: &title})
	require.NoError(t, err)
	same, err := store.EntityAttribution(ctx, project.UID, "issue", issue.UID)
	require.NoError(t, err)
	require.Equal(t, receipt, same)
	page, err := store.AttributionReceiptsAfter(ctx, project.UID, 1, 0, 10)
	require.NoError(t, err)
	require.Len(t, page, 3)
	require.Equal(t, "other-member", page[2].AccountableActor)
	_, err = store.EntityAttribution(ctx, project.UID, "issue", legacy.UID)
	require.ErrorIs(t, err, db.ErrNotFound, "new signing authority never promotes legacy creators")
	_, wrongPrivate, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	before, err := store.MaxEventID(ctx)
	require.NoError(t, err)
	_, _, err = store.CreateIssue(db.WithRootAttribution(ctx, db.RootAttributionSigner{AuthorityUID: pin.AuthorityUID, PrivateKey: wrongPrivate}, "company-member"), db.CreateIssueParams{ProjectID: project.ID, Title: "Must roll back", Author: "assistant"})
	require.Error(t, err)
	after, err := store.MaxEventID(ctx)
	require.NoError(t, err)
	require.Equal(t, before, after, "a missing receipt may never leave its source event committed")
	issues, err := store.ListIssues(ctx, db.ListIssuesParams{ProjectID: project.ID})
	require.NoError(t, err)
	require.Len(t, issues, 2, "proof failure rolls back the projection too")
	team, _, err := store.CreateTeam(ctx, "project-team", "admin")
	require.NoError(t, err)
	_, err = store.SetTeamMembership(ctx, team.UID, "company-member", true, "admin")
	require.NoError(t, err)
	policy, err := store.ProjectAccessPolicy(ctx, project.UID)
	require.NoError(t, err)
	policy.Visibility = "teams"
	policy.TeamUIDs = []string{team.UID}
	_, _, err = store.SetProjectAccessPolicy(ctx, policy, "admin")
	require.NoError(t, err)
	_, _, err = store.CreateIssue(writeCtx, db.CreateIssueParams{ProjectID: project.ID, Title: "Account authorized, source not a member", Author: "assistant"})
	require.NoError(t, err)
	_, err = store.SetTeamMembership(ctx, team.UID, "company-member", false, "admin")
	require.NoError(t, err)
	_, _, err = store.CreateIssue(writeCtx, db.CreateIssueParams{ProjectID: project.ID, Title: "Revoked account", Author: "assistant"})
	require.Error(t, err, "previously admitted account loses authority despite unchanged source label")
	privateProject, err := store.CreateProject(ctx, "private-project")
	require.NoError(t, err)
	privateIssue, _, err := store.CreateIssue(writeCtx, db.CreateIssueParams{ProjectID: privateProject.ID, Title: "Local private task", Author: "assistant"})
	require.NoError(t, err)
	_, err = store.EntityAttribution(ctx, privateProject.UID, "issue", privateIssue.UID)
	require.ErrorIs(t, err, db.ErrNotFound, "private/nonfederated work stays local")
	// First root write pins this daemon's known local signing identity atomically.
	freshRoot, err := store.CreateProject(ctx, "fresh-root-project")
	require.NoError(t, err)
	_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: freshRoot.ID, Role: db.FederationRoleHub, HubProjectID: freshRoot.ID, HubProjectUID: freshRoot.UID, Enabled: true})
	require.NoError(t, err)
	first, _, err := store.CreateIssue(writeCtx, db.CreateIssueParams{ProjectID: freshRoot.ID, Title: "First signed write", Author: "assistant"})
	require.NoError(t, err)
	freshPin, err := store.RootAuthority(ctx, freshRoot.UID)
	require.NoError(t, err)
	require.Equal(t, pin.KeyID, freshPin.KeyID)
	_, err = store.EntityAttribution(ctx, freshRoot.UID, "issue", first.UID)
	require.NoError(t, err)
	var wait sync.WaitGroup
	failures := make([]error, 2)
	for index := range failures {
		wait.Go(func() {
			_, _, failures[index] = store.CreateIssue(writeCtx, db.CreateIssueParams{ProjectID: freshRoot.ID, Title: "Concurrent signed write", Author: "assistant"})
		})
	}
	wait.Wait()
	for _, failure := range failures {
		require.NoError(t, failure)
	}
	page, err = store.AttributionReceiptsAfter(ctx, freshRoot.UID, 1, 0, 10)
	require.NoError(t, err)
	require.Len(t, page, 3)
	for index, proof := range page {
		require.Equal(t, int64(index+1), proof.Sequence)
		require.NoError(t, db.VerifyRootReceipt(freshPin, proof))
	}
	// A physical downstream hub with a foreign root pin remains a descendant.
	foreign, err := store.CreateProject(ctx, "relay-project")
	require.NoError(t, err)
	_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: foreign.ID, Role: db.FederationRoleHub, HubProjectID: foreign.ID, HubProjectUID: foreign.UID, Enabled: true})
	require.NoError(t, err)
	foreignPublic, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	require.NoError(t, store.PinRootAuthority(ctx, db.RootKeyPin{ProjectUID: foreign.UID, AuthorityUID: "00000000000000000000000002", KeyID: db.RootPublicKeyID(foreignPublic), PublicKey: foreignPublic}))
	pending, _, err := store.CreateIssue(writeCtx, db.CreateIssueParams{ProjectID: foreign.ID, Title: "Pending upstream receipt", Author: "assistant"})
	require.NoError(t, err)
	_, err = store.EntityAttribution(ctx, foreign.UID, "issue", pending.UID)
	require.ErrorIs(t, err, db.ErrNotFound, "descendant cannot mint its root's verified attribution")

}
