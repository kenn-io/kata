package dbtest

import (
	"crypto/ed25519"
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// RunRelayCheckpointCloseVerificationBoundary ensures a retained close warning
// cannot carry review details for an issue that left the enrolled project.
func RunRelayCheckpointCloseVerificationBoundary(t *testing.T, store db.Storage) {
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "hub-project")
	require.NoError(t, err)
	excluded, err := store.CreateProject(ctx, "excluded-project")
	require.NoError(t, err)
	claimIssue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Author: "worker", Title: "Completed work"})
	require.NoError(t, err)
	reviewIssue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Author: "worker", Title: "Review notes"})
	require.NoError(t, err)
	claim, _, err := store.CreateComment(ctx, db.CreateCommentParams{IssueID: claimIssue.ID, Author: "worker", Body: "Completion claim"})
	require.NoError(t, err)
	refutation, _, err := store.CreateComment(ctx, db.CreateCommentParams{
		IssueID: reviewIssue.ID, Author: "review-author", Body: "Private review excerpt",
		ReplyToUID: claim.UID, ReplyKind: "refute",
	})
	require.NoError(t, err)

	_, closeEvents, _, err := store.CloseIssueGuarded(ctx, db.CloseIssueParams{
		IssueID: claimIssue.ID, ExpectedProjectID: project.ID, Actor: "worker", Reason: "done",
		Message:               "Completed the work and retained the unresolved review warning.",
		Evidence:              []db.Evidence{{Type: "test", Command: "go test ./internal/db"}},
		VerificationIssueUIDs: []string{claimIssue.UID, reviewIssue.UID},
		VerificationWarnings: []db.VerificationWarning{{Code: "open_refutations", Refutations: []db.OpenRefutation{{
			Refutation: refutation.UID, Target: claim.UID, Author: "review-author", Summary: refutation.Body,
		}}}},
	})
	require.NoError(t, err)
	require.Len(t, closeEvents, 1)
	closeEvent := closeEvents[0]
	var closePayload struct {
		VerificationIssueUIDs []string                 `json:"verification_issue_uids"`
		VerificationWarnings  []db.VerificationWarning `json:"verification_warnings"`
	}
	require.NoError(t, json.Unmarshal([]byte(closeEvent.Payload), &closePayload))
	require.Equal(t, []string{claimIssue.UID, reviewIssue.UID}, closePayload.VerificationIssueUIDs)
	require.Equal(t, refutation.UID, closePayload.VerificationWarnings[0].Refutations[0].Refutation)
	require.Equal(t, "review-author", closePayload.VerificationWarnings[0].Refutations[0].Author)
	require.Equal(t, refutation.Body, closePayload.VerificationWarnings[0].Refutations[0].Summary)

	currentReviewIssue, err := store.IssueByID(ctx, reviewIssue.ID)
	require.NoError(t, err)
	_, err = store.MoveIssueProject(ctx, db.MoveIssueProjectIn{
		IssueID: currentReviewIssue.ID, FromProjectID: project.ID, ToProjectID: excluded.ID,
		IfMatchRev: currentReviewIssue.Revision, Actor: "worker",
	})
	require.NoError(t, err)

	public, private, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID: project.ID, Role: db.FederationRoleHub, HubProjectID: project.ID,
		HubProjectUID: project.UID, Enabled: true,
	})
	require.NoError(t, err)
	pin := db.RootKeyPin{
		ProjectUID: project.UID, AuthorityUID: store.InstanceUID(),
		KeyID: db.RootPublicKeyID(public), PublicKey: public,
	}
	require.NoError(t, store.PinRootAuthority(ctx, pin))
	parent, _, err := store.CreateAPIToken(ctx, db.CreateAPITokenParams{
		PlaintextToken: "relay-verification-parent-token", // #nosec G101 -- synthetic parent token used only in this storage test.
		Actor:          "operator", AdminActor: "admin",
	})
	require.NoError(t, err)
	grant, err := store.CreateRelayEnrollment(ctx, db.CreateRelayEnrollmentParams{
		ProjectID: project.ID, ParentTokenID: parent.ID, SpokeInstanceUID: "00000000000000000000000008",
		ProtocolVersion: db.RelayProtocolVersion, Token: "relay-verification-child-token", // #nosec G101 -- synthetic relay token used only in this storage test.
	})
	require.NoError(t, err)
	creator, ok := store.(db.RelayResetStore)
	require.True(t, ok, "native checkpoint generation is required")
	checkpoint, err := creator.CreateRelayReset(ctx, grant.Enrollment.RelayBindingUID, db.RootAttributionSigner{
		AuthorityUID: store.InstanceUID(), PrivateKey: private,
	})
	require.NoError(t, err)
	events, _, err := db.DecodeRootResetPayload(pin, checkpoint.Snapshot)
	require.NoError(t, err)

	var retainedClose bool
	for _, event := range events {
		retainedClose = retainedClose || event.EventUID == closeEvent.UID
	}
	require.False(t, retainedClose, "the checkpoint must omit the close event after its refutation issue leaves the project")
	checkpointPayload := string(checkpoint.Snapshot.Events) + string(checkpoint.Snapshot.Entities)
	for _, privateValue := range []string{"review-author", "Private review excerpt", refutation.UID} {
		require.NotContains(t, checkpointPayload, privateValue,
			"the checkpoint must not disclose a warning author, excerpt or comment UID from an excluded issue")
	}
}
