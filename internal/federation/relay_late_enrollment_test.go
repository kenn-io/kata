package federation_test

import (
	"context"
	"crypto/ed25519"
	"database/sql"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/federation"
)

// R1/R4: enrolling an explicitly shared populated project must include its
// existing content and root creation proof, including a leaf joining later.
func TestRelayLateEnrollmentPopulatedProject(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) { runRelayLateEnrollment(t, backend, false, false) })
	}
}

// R4/R5: late enrollment must bootstrap signed current state even after source
// history compaction; receipt-only transport cannot restore an absent entity.
func TestRelayLateEnrollmentCompactedProject(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) { runRelayLateEnrollment(t, backend, true, false) })
	}
}

// R3/R5: a newly enrolled replica must verify retained creation receipts issued
// before a signed root-key rotation, through both the root and relay hops.
func TestRelayLateEnrollmentAfterKeyRotation(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) { runRelayLateEnrollment(t, backend, false, true) })
	}
}

// A fresh relay must install a signed baseline before pulling mutations for
// entities that only exist in that baseline after the root has purged history.
func TestRelayLateEnrollmentAfterPurgeBeforeInitialSync(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			root := newRelayMatrixNode(t, backend, "company-member")
			replica := newRelayMatrixNode(t, backend, "personal-member")
			project, err := root.store.CreateProject(t.Context(), "shared-project")
			require.NoError(t, err)
			root.project = project
			_, err = root.store.UpsertFederationBinding(t.Context(), db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleHub, HubProjectID: project.ID, HubProjectUID: project.UID, Enabled: true})
			require.NoError(t, err)
			public := root.signer.PrivateKey.Public().(ed25519.PublicKey)
			pin := db.RootKeyPin{ProjectUID: project.UID, AuthorityUID: root.store.InstanceUID(), KeyID: db.RootPublicKeyID(public), PublicKey: public}
			require.NoError(t, root.store.PinRootAuthority(t.Context(), pin))
			writeContext := db.WithRootAttribution(t.Context(), root.signer, root.account)
			purged, _, err := root.store.CreateIssue(writeContext, db.CreateIssueParams{ProjectID: project.ID, Author: "source-agent", Title: "Purged history"})
			require.NoError(t, err)
			retained, _, err := root.store.CreateIssue(writeContext, db.CreateIssueParams{ProjectID: project.ID, Author: "source-agent", Title: "Current shared issue"})
			require.NoError(t, err)
			_, err = root.store.PurgeIssue(t.Context(), purged.ID, "admin", nil)
			require.NoError(t, err)
			enrollRelayMatrixReplica(t, root, replica, "replica-project", false)
			title := "Updated before initial sync"
			_, _, _, err = root.store.EditIssue(writeContext, db.EditIssueParams{IssueID: retained.ID, Actor: "source-agent", Title: &title})
			require.NoError(t, err)
			binding, err := replica.store.FederationBindingByProject(t.Context(), replica.project.ID)
			require.NoError(t, err)
			bootstrap := root.store.(db.RelayResetBootstrapStore)
			required, err := bootstrap.RelayEnrollmentNeedsReset(t.Context(), binding.RelayConfig.BindingUID)
			require.NoError(t, err)
			require.True(t, required, "the purged history requires a signed baseline")
			require.NoError(t, federation.SyncFederationOnce(t.Context(), replica.store, binding, replica.credential))
			got, err := replica.store.IssueByUID(t.Context(), retained.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
			require.Equal(t, title, got.Title)
			require.Equal(t, "company-member", got.AccountableActor)
			quarantines, err := replica.store.ActiveFederationQuarantinesByProject(t.Context(), replica.project.ID)
			require.NoError(t, err)
			require.Empty(t, quarantines, "a baseline-first bootstrap must not quarantine the pending root mutation")
		})
	}
}

func runRelayLateEnrollment(t *testing.T, backend string, compact, rotate bool) {
	t.Helper()
	root := newRelayMatrixNode(t, backend, "company-member")
	personal := newRelayMatrixNode(t, backend, "personal-member")
	leaf := newRelayMatrixNode(t, backend, "leaf-member")
	project, err := root.store.CreateProject(t.Context(), "shared-project")
	require.NoError(t, err)
	root.project = project
	_, err = root.store.UpsertFederationBinding(t.Context(), db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleHub, HubProjectID: project.ID, HubProjectUID: project.UID, Enabled: true})
	require.NoError(t, err)
	public := root.signer.PrivateKey.Public().(ed25519.PublicKey)
	pin := db.RootKeyPin{ProjectUID: project.UID, AuthorityUID: root.store.InstanceUID(), KeyID: db.RootPublicKeyID(public), PublicKey: public}
	require.NoError(t, root.store.PinRootAuthority(t.Context(), pin))
	issue, _, err := root.store.CreateIssue(db.WithRootAttribution(t.Context(), root.signer, root.account), db.CreateIssueParams{ProjectID: project.ID, Author: "source-agent", Title: "Content before enrollment"})
	require.NoError(t, err)
	var comment db.Comment
	var legacy db.Issue
	if compact {
		signed := db.WithRootAttribution(t.Context(), root.signer, root.account)
		comment, _, err = root.store.CreateComment(signed, db.CreateCommentParams{IssueID: issue.ID, Author: "source-agent", Teammate: "writer", Body: "Original reply"})
		require.NoError(t, err)
		title := "Current state after history compaction"
		_, _, _, err = root.store.EditIssue(signed, db.EditIssueParams{IssueID: issue.ID, Actor: "maintenance-agent", Title: &title})
		require.NoError(t, err)
		_, _, _, err = root.store.EditComment(signed, db.EditCommentParams{IssueID: issue.ID, CommentUID: comment.UID, Actor: "maintenance-agent", Body: "Current reply"})
		require.NoError(t, err)
		legacy, _, err = root.store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: project.ID, Author: "historical-agent", Title: "Legacy source without receipt"})
		require.NoError(t, err)
		executor := root.store.(interface {
			ExecContext(context.Context, string, ...any) (sql.Result, error)
		})
		query := "DELETE FROM events WHERE project_id=?"
		if backend == "postgres" {
			query = "DELETE FROM events WHERE project_id=$1"
		}
		_, err := executor.ExecContext(t.Context(), query, project.ID)
		require.NoError(t, err)
	}
	if rotate {
		nextPublic, nextPrivate, err := ed25519.GenerateKey(nil)
		require.NoError(t, err)
		next := db.RootKeyPin{ProjectUID: project.UID, AuthorityUID: pin.AuthorityUID, KeyID: db.RootPublicKeyID(nextPublic), PublicKey: nextPublic}
		transition, err := db.SignRootKeyTransition(pin, next, root.signer.PrivateKey)
		require.NoError(t, err)
		require.NoError(t, root.store.RotateRootAuthority(t.Context(), transition))
		root.signer.PrivateKey = nextPrivate
	}
	enrollRelayMatrixReplica(t, root, personal, "personal-alias", true)
	syncRelayMatrixNode(t, personal)
	personalProject, err := personal.store.ProjectByID(t.Context(), personal.project.ID)
	require.NoError(t, err)
	require.Equal(t, "personal-alias", personalProject.Name)
	_, err = personal.store.IssueByUID(t.Context(), issue.UID, db.IncludeDeletedYes)
	require.NoError(t, err, "enrollment must bootstrap existing shared content")
	enrollRelayMatrixReplica(t, personal, leaf, "leaf-alias", false)
	var preBootstrap db.Issue
	if compact {
		leafBinding, err := leaf.store.FederationBindingByProject(t.Context(), leaf.project.ID)
		require.NoError(t, err)
		bootstrap := personal.store.(db.RelayResetBootstrapStore)
		resetRequired, err := bootstrap.RelayEnrollmentNeedsReset(t.Context(), leafBinding.RelayConfig.BindingUID)
		require.NoError(t, err)
		require.True(t, resetRequired, "the intermediate relay advertises its forwarded compacted checkpoint")
		preBootstrap, _, err = leaf.store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: leaf.project.ID, Author: "leaf-agent", Title: "Created before first checkpoint"})
		require.NoError(t, err)
	}
	if compact {
		leafBinding, err := leaf.store.FederationBindingByProject(t.Context(), leaf.project.ID)
		require.NoError(t, err)
		err = federation.SyncFederationOnce(t.Context(), leaf.store, leafBinding, leaf.credential)
		require.ErrorIs(t, err, db.ErrFederationResetBlockedByPendingPush, "the intermediate must wait for root acceptance before forwarding the checkpoint")
		syncRelayMatrixNode(t, personal)
		err = federation.SyncFederationOnce(t.Context(), leaf.store, leafBinding, leaf.credential)
		require.NoError(t, err)
	} else {
		syncRelayMatrixNode(t, leaf)
	}
	if compact {
		retained, err := leaf.store.IssueByUID(t.Context(), preBootstrap.UID, db.IncludeDeletedYes)
		require.NoError(t, err, "installing the older forwarded checkpoint must preserve the child's accepted local event")
		require.Equal(t, preBootstrap.Title, retained.Title)
		require.Equal(t, "leaf-agent", retained.Author)
		require.Equal(t, "company-member", retained.AccountableActor)
		proof, err := leaf.store.EntityAttribution(t.Context(), project.UID, "issue", preBootstrap.UID)
		require.NoError(t, err)
		require.NoError(t, db.VerifyRootReceipt(pin, proof))
	}
	leafProject, err := leaf.store.ProjectByID(t.Context(), leaf.project.ID)
	require.NoError(t, err)
	require.Equal(t, "leaf-alias", leafProject.Name)
	_, err = leaf.store.IssueByUID(t.Context(), issue.UID, db.IncludeDeletedYes)
	require.NoError(t, err, "late leaf must receive existing relayed content")
	proof, err := leaf.store.EntityAttribution(t.Context(), project.UID, "issue", issue.UID)
	require.NoError(t, err)
	require.NoError(t, db.VerifyRootReceipt(pin, proof))
	if compact {
		for _, node := range []*relayMatrixNode{personal, leaf} {
			current, err := node.store.IssueByUID(t.Context(), issue.UID, db.IncludeDeletedYes)
			require.NoError(t, err)
			require.Equal(t, "Current state after history compaction", current.Title)
			require.Equal(t, "source-agent", current.Author)
			require.Equal(t, "company-member", current.AccountableActor)
			replies, err := node.store.CommentsByIssue(t.Context(), current.ID)
			require.NoError(t, err)
			require.Len(t, replies, 1)
			require.Equal(t, "Current reply", replies[0].Body)
			require.Equal(t, comment.UID, replies[0].UID)
			require.Equal(t, "source-agent", replies[0].Author)
			require.Equal(t, "writer", replies[0].Teammate)
			require.Equal(t, "company-member", replies[0].AccountableActor)
			historical, err := node.store.IssueByUID(t.Context(), legacy.UID, db.IncludeDeletedYes)
			require.NoError(t, err)
			require.Equal(t, "legacy", historical.Verification)
			require.Empty(t, historical.AccountableActor)
		}
	}
}
