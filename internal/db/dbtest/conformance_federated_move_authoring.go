package dbtest

import (
	"crypto/ed25519"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
)

// Local moves must be deliverable by the project's negotiated transport and
// actors. Preview applies the same gate without changing rows or event cursors.
func checkFederatedMoveAuthoringGates(t *testing.T, store db.Storage, backend Backend) error {
	for _, kind := range []string{"upstream-relay", "downstream-relay", "different-actors", "native"} {
		for _, endpoint := range []string{"source", "target"} {
			t.Run(kind+"/"+endpoint, func(t *testing.T) {
				ctx := t.Context()
				name := kind + "-" + endpoint
				source, err := store.CreateProject(ctx, name+"-source")
				require.NoError(t, err)
				target, err := store.CreateProject(ctx, name+"-target")
				require.NoError(t, err)
				issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: source.ID, Title: "move candidate", Author: "worker"})
				require.NoError(t, err)
				selected := source
				if endpoint == "target" {
					selected = target
				}
				for _, project := range []db.Project{source, target} {
					role := db.FederationRoleSpoke
					if kind == "downstream-relay" {
						role = db.FederationRoleHub
					}
					actor := "worker"
					if kind == "different-actors" && project.ID == selected.ID {
						actor = "other-worker"
					}
					_, err := store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: project.ID, Role: role, HubURL: "https://hub.example", HubProjectID: project.ID, HubProjectUID: project.UID, Enabled: true, PushEnabled: true, Actor: actor})
					require.NoError(t, err)
				}
				var grantID int64
				if kind == "upstream-relay" || kind == "downstream-relay" {
					public, _, err := ed25519.GenerateKey(nil)
					require.NoError(t, err)
					authority := store.InstanceUID()
					if kind == "upstream-relay" {
						authority, err = uid.New()
						require.NoError(t, err)
					}
					require.NoError(t, store.PinRootAuthority(ctx, db.RootKeyPin{ProjectUID: selected.UID, AuthorityUID: authority, KeyID: db.RootPublicKeyID(public), PublicKey: public}))
					if kind == "upstream-relay" {
						bindingUID, err := uid.New()
						require.NoError(t, err)
						_, err = store.SetRelayBindingConfig(ctx, selected.ID, db.RelayBindingConfig{ProtocolVersion: db.RelayProtocolVersion, BindingUID: bindingUID, AuthorityUID: authority, UpstreamInstanceUID: authority, HubPath: []string{authority, store.InstanceUID()}, LocalActor: "worker", ResetEpoch: 1})
						require.NoError(t, err)
					} else {
						//nolint:gosec // Deterministic test credential, never an operational secret.
						parent, _, err := store.CreateAPIToken(ctx, db.CreateAPITokenParams{Actor: "worker", AdminActor: "admin", PlaintextToken: name + "-parent-test-token"})
						require.NoError(t, err)
						peerUID, err := uid.New()
						require.NoError(t, err)
						//nolint:gosec // Deterministic test credential, never an operational secret.
						grant, err := store.CreateRelayEnrollment(ctx, db.CreateRelayEnrollmentParams{ProjectID: selected.ID, ParentTokenID: parent.ID, SpokeInstanceUID: peerUID, ProtocolVersion: db.RelayProtocolVersion, Token: name + "-relay-test-token"})
						require.NoError(t, err)
						grantID = grant.Enrollment.ID
					}
				}
				before, err := store.MaxEventID(ctx)
				require.NoError(t, err)
				for _, preview := range []bool{true, false} {
					t.Run(map[bool]string{true: "preview", false: "commit"}[preview], func(t *testing.T) {
						_, err := store.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: source.ID, ToProjectID: target.ID, IfMatchRev: issue.Revision, Actor: "worker", DryRun: preview})
						if kind == "native" {
							require.NoError(t, err)
						} else {
							require.ErrorIs(t, err, db.ErrFederatedMoveUnsupported)
						}
						if preview || kind != "native" {
							after, err := store.MaxEventID(ctx)
							require.NoError(t, err)
							require.Equal(t, before, after, "preview or refused move must not emit events")
							retained, err := store.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
							require.NoError(t, err)
							require.Equal(t, source.ID, retained.ProjectID)
							require.Equal(t, issue.Revision, retained.Revision)
						}
					})
				}
				if grantID != 0 && !t.Failed() {
					require.NoError(t, store.RevokeFederationEnrollment(ctx, grantID))
					_, err := store.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: source.ID, ToProjectID: target.ID, IfMatchRev: issue.Revision, Actor: "worker"})
					require.NoError(t, err, "a revoked downstream grant no longer constrains native moves")
				}
			})
		}
	}
	t.Run("hub history ignores replica actor differences", func(t *testing.T) {
		ctx := t.Context()
		source, err := store.CreateProject(ctx, "replay-actors-source")
		require.NoError(t, err)
		target, err := store.CreateProject(ctx, "replay-actors-target")
		require.NoError(t, err)
		issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: source.ID, Title: "hub move", Author: "hub-worker"})
		require.NoError(t, err)
		for _, project := range []db.Project{source, target} {
			_, err := store.EnableProjectFederation(ctx, project.ID, "hub-worker")
			require.NoError(t, err)
		}
		_, err = store.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: source.ID, ToProjectID: target.ID, IfMatchRev: issue.Revision, Actor: "hub-worker"})
		require.NoError(t, err)
		spoke := backend.Open(t)
		t.Cleanup(func() { require.NoError(t, spoke.Close()) })
		var destination db.Project
		for i, project := range []db.Project{source, target} {
			replica, err := spoke.CreateProjectWithUID(ctx, "replica-"+project.Name, project.UID)
			require.NoError(t, err)
			actor := []string{"source-worker", "target-worker"}[i]
			_, err = spoke.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: replica.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: project.ID, HubProjectUID: project.UID, Enabled: true, PushEnabled: true, Actor: actor})
			require.NoError(t, err)
			if project.ID == target.ID {
				destination = replica
			}
		}
		page, err := store.ReadFederation(ctx, db.FederationReadParams{ProjectID: target.ID, Limit: 100, Features: db.SupportedEventFeatures})
		require.NoError(t, err)
		remote := make([]db.RemoteEvent, len(page.Events))
		for i, event := range page.Events {
			remote[i] = db.RemoteEventFromStored(event)
		}
		_, err = spoke.InsertRemoteEvents(ctx, destination.ID, remote)
		require.NoError(t, err, "hub-authorized history is independent of local per-project accounts")
		require.NoError(t, spoke.MaterializeFederatedProject(ctx, destination.ID))
		got, err := spoke.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
		require.NoError(t, err)
		require.Equal(t, destination.ID, got.ProjectID)
	})
	return nil
}
