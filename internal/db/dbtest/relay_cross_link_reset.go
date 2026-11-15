package dbtest

import (
	"crypto/ed25519"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func RunRelayCrossLinkResetHistory(t *testing.T, root db.Storage, backend Backend) {
	ctx := t.Context()
	hub := backend.Open(t)
	t.Cleanup(func() { require.NoError(t, hub.Close()) })
	leaf := backend.Open(t)
	t.Cleanup(func() { require.NoError(t, leaf.Close()) })
	_, private, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	signer := db.RootAttributionSigner{AuthorityUID: root.InstanceUID(), PrivateKey: private}
	enroll := func(upstream, replica db.Storage, project db.Project, name string, downstream bool) string {
		t.Helper()
		parent, _, err := upstream.CreateAPIToken(ctx, db.CreateAPITokenParams{Actor: "member", AdminActor: "admin", PlaintextToken: name + "-parent-test-token"})
		require.NoError(t, err)
		grant, err := upstream.CreateRelayEnrollment(ctx, db.CreateRelayEnrollmentParams{ProjectID: project.ID, ParentTokenID: parent.ID, SpokeInstanceUID: replica.InstanceUID(), ProtocolVersion: db.RelayProtocolVersion, ServeDownstream: downstream, Token: name + "-relay-test-token"})
		require.NoError(t, err)
		local, err := replica.CreateProjectWithUID(ctx, name, project.UID)
		require.NoError(t, err)
		pin, err := upstream.RootAuthority(ctx, project.UID)
		require.NoError(t, err)
		require.NoError(t, replica.PinRootAuthority(ctx, pin))
		_, err = replica.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: local.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: project.ID, HubProjectUID: project.UID, Actor: "member", Enabled: true, PushEnabled: true})
		require.NoError(t, err)
		path := []string{pin.AuthorityUID, replica.InstanceUID()}
		if upstream.InstanceUID() != pin.AuthorityUID {
			path = []string{pin.AuthorityUID, upstream.InstanceUID(), replica.InstanceUID()}
		}
		_, err = replica.SetRelayBindingConfig(ctx, local.ID, db.RelayBindingConfig{ProtocolVersion: db.RelayProtocolVersion, BindingUID: grant.Enrollment.RelayBindingUID, AuthorityUID: pin.AuthorityUID, UpstreamInstanceUID: upstream.InstanceUID(), HubPath: path, LocalActor: "member", ServeDownstream: downstream, ResetEpoch: 1})
		require.NoError(t, err)
		return grant.Enrollment.RelayBindingUID
	}
	var projects [2]db.Project
	var issues [2]db.Issue
	for i, name := range []string{"reset-source", "reset-peer"} {
		project, err := root.CreateProject(ctx, name)
		require.NoError(t, err)
		projects[i] = project
		_, err = root.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleHub, HubProjectID: project.ID, HubProjectUID: project.UID, Enabled: true})
		require.NoError(t, err)
		public := signer.PrivateKey.Public().(ed25519.PublicKey)
		require.NoError(t, root.PinRootAuthority(ctx, db.RootKeyPin{ProjectUID: project.UID, AuthorityUID: root.InstanceUID(), KeyID: db.RootPublicKeyID(public), PublicKey: public}))
		issues[i], _, err = root.CreateIssue(db.WithRootAttribution(ctx, signer, "root-member"), db.CreateIssueParams{ProjectID: project.ID, Author: "root-member", Title: name})
		require.NoError(t, err)
	}
	params := db.CreateLinkParams{FromIssueID: issues[0].ID, ToIssueID: issues[1].ID, Type: "blocks", Author: "root-member"}
	link, _, err := root.CreateLinkAndEvent(db.WithRootAttribution(ctx, signer, "root-member"), params, db.LinkEventParams{EventType: "issue.linked", EventIssueID: issues[0].ID, Actor: "root-member", FromUID: issues[0].UID, ToUID: issues[1].UID, FromShortID: issues[0].ShortID, ToShortID: issues[1].ShortID})
	require.NoError(t, err)
	_, err = root.DeleteLinkAndEvent(db.WithRootAttribution(ctx, signer, "root-member"), link, db.LinkEventParams{EventType: "issue.unlinked", EventIssueID: issues[0].ID, Actor: "root-member", FromUID: issues[0].UID, ToUID: issues[1].UID, FromShortID: issues[0].ShortID, ToShortID: issues[1].ShortID})
	require.NoError(t, err)
	binding := enroll(root, hub, projects[0], "reset-hub", true)
	rootResets := root.(db.RelayResetStore)
	relayState := func(store db.Storage) []db.ImportRecord {
		var records []db.ImportRecord
		for record, err := range store.ExportRelayState(ctx) {
			require.NoError(t, err)
			records = append(records, record)
		}
		return records
	}
	before := relayState(root)
	grants, err := root.ListProjectFederationEnrollments(ctx, projects[0].ID)
	require.NoError(t, err)
	_, err = rootResets.CreateRelayReset(ctx, binding, signer)
	require.ErrorIs(t, err, db.ErrUnsupportedEventFeatures)
	require.Equal(t, before, relayState(root))
	afterGrants, err := root.ListProjectFederationEnrollments(ctx, projects[0].ID)
	require.NoError(t, err)
	require.Equal(t, grants, afterGrants)
	features := db.WithRelayPeerEventFeatures(ctx, db.SupportedEventFeatures)
	checkpoint, err := rootResets.CreateRelayReset(features, binding, signer)
	require.NoError(t, err)

	_, err = rootResets.CreateRelayReset(ctx, binding, signer)
	require.ErrorIs(t, err, db.ErrUnsupportedEventFeatures)
	retry, err := rootResets.CreateRelayReset(features, binding, signer)
	require.NoError(t, err)
	require.Equal(t, checkpoint, retry)
	hubResets := hub.(db.RelayResetStore)
	require.NoError(t, hubResets.InstallRelayReset(ctx, binding, checkpoint.Manifest, checkpoint.Snapshot, checkpoint.Translation))
	hubProject, err := hub.ProjectByUID(ctx, projects[0].UID)
	require.NoError(t, err)
	downstream := enroll(hub, leaf, hubProject, "reset-leaf", false)
	for _, stream := range []string{db.RelayStreamEvent, db.RelayStreamReceipt, db.RelayStreamArtifact} {
		for {
			items, err := hub.PendingRelayDeliveries(features, downstream, stream, 100)
			require.NoError(t, err)
			if len(items) == 0 {
				break
			}
			last := items[len(items)-1]
			require.NoError(t, hub.AckRelayDeliveries(ctx, downstream, 1, stream, last.Sequence, last.Digest))
		}
	}
	before = relayState(hub)
	grants, err = hub.ListProjectFederationEnrollments(ctx, hubProject.ID)
	require.NoError(t, err)
	_, err = hubResets.CreateRelayReset(ctx, downstream, db.RootAttributionSigner{})
	require.ErrorIs(t, err, db.ErrUnsupportedEventFeatures)
	require.Equal(t, before, relayState(hub))
	afterGrants, err = hub.ListProjectFederationEnrollments(ctx, hubProject.ID)
	require.NoError(t, err)
	require.Equal(t, grants, afterGrants)
	forwarded, err := hubResets.CreateRelayReset(features, downstream, db.RootAttributionSigner{})
	require.NoError(t, err)
	require.Equal(t, checkpoint.Manifest, forwarded.Manifest)
	require.Equal(t, checkpoint.Snapshot, forwarded.Snapshot)
	_, err = hubResets.CreateRelayReset(ctx, downstream, db.RootAttributionSigner{})
	require.ErrorIs(t, err, db.ErrUnsupportedEventFeatures)
	retry, err = hubResets.CreateRelayReset(features, downstream, db.RootAttributionSigner{})
	require.NoError(t, err)
	require.Equal(t, forwarded, retry)
}
