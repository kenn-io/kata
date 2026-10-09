package dbtest

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json/jsontext"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// RunRelayAttribution exercises relay attribution on the supplied native store.
// R5: root acceptance binds the stored authenticated enrollment, never a
// declared source label. It is immutable, independently delivered and durable.
func RunRelayAttribution(t *testing.T, store db.Storage) {
	t.Helper()
	ctx := t.Context()
	provenance, ok := store.(db.AttributionStorage)
	require.True(t, ok, "native storage must persist root provenance")
	project, err := store.CreateProject(ctx, "shared-project")
	require.NoError(t, err)
	_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleHub, HubProjectID: project.ID, HubProjectUID: project.UID, Enabled: true})
	require.NoError(t, err)
	public, private, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	pin := db.RootKeyPin{ProjectUID: project.UID, AuthorityUID: "00000000000000000000000001", KeyID: db.RootPublicKeyID(public), PublicKey: public}
	require.NoError(t, provenance.PinRootAuthority(ctx, pin))
	trusted, err := provenance.RootAuthority(ctx, project.UID)
	require.NoError(t, err)
	assert.Equal(t, pin, trusted)
	require.NoError(t, provenance.PinRootAuthority(ctx, pin), "exact enrollment retry is idempotent")
	otherPublic, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	replacement := pin
	replacement.KeyID, replacement.PublicKey = db.RootPublicKeyID(otherPublic), otherPublic
	require.Error(t, provenance.PinRootAuthority(ctx, replacement), "known authority cannot silently replace key")
	grant, err := store.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{ProjectID: &project.ID, SpokeInstanceUID: "00000000000000000000000002", Actor: "member", Capabilities: "pull,push", Token: "receipt-grant"})
	require.NoError(t, err)
	issue, event, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Source task", Author: "assistant", Metadata: map[string]jsontext.Value{"teammate": jsontext.Value(`"researcher"`)}})
	require.NoError(t, err)
	source := remoteEventFromStored(event)
	signer := db.RootAttributionSigner{AuthorityUID: pin.AuthorityUID, PrivateKey: private}
	rootUI := store.(db.UIStore)
	rootBefore, err := rootUI.UIEventCursor(ctx)
	require.NoError(t, err)
	receipt, err := provenance.RecordRootAttribution(ctx, grant.Enrollment.ID, source, signer)
	require.NoError(t, err)
	require.NoError(t, db.VerifyRootReceipt(pin, receipt))
	rootAfter, err := rootUI.UIEventCursor(ctx)
	require.NoError(t, err)
	require.Greater(t, rootAfter, rootBefore, "late root issuance invalidates previously published creator projections")
	rootReset, err := store.PurgeResetCheck(ctx, rootBefore, project.ID)
	require.NoError(t, err)
	require.Equal(t, rootAfter, rootReset)

	assert.Equal(t, "member", receipt.AccountableActor)
	assert.Equal(t, "assistant", receipt.SourceActor)
	assert.Equal(t, "researcher", receipt.Teammate)
	assert.Equal(t, source.ContentHash, receipt.ContentHash)
	assert.Equal(t, grant.Enrollment.SpokeInstanceUID, receipt.IngressInstanceUID)
	assert.Equal(t, int64(1), receipt.Sequence)
	assert.Equal(t, int64(1), receipt.ResetEpoch)
	stored, err := store.EventsByUIDs(ctx, project.ID, []string{event.UID})
	require.NoError(t, err)
	require.Len(t, stored, 1)
	assert.Equal(t, event, stored[0], "receipt never rewrites the source event")
	creator, err := provenance.EntityAttribution(ctx, project.UID, "issue", issue.UID)
	require.NoError(t, err)
	assert.Equal(t, receipt, creator)
	var portable []db.ImportRecord
	for record, e := range provenance.ExportAttribution(ctx, db.ExportFilter{ProjectID: &project.ID}) {
		require.NoError(t, e)
		portable = append(portable, record)
	}
	require.Len(t, portable, 3)
	assert.Equal(t, &pin, portable[0])
	assert.Equal(t, &receipt, portable[1])
	assert.Equal(t, &db.EntityProvenance{ProjectUID: project.UID, Kind: "issue", EntityUID: issue.UID, EventUID: event.UID}, portable[2])
	title := "Edited by another member"
	_, _, _, err = store.EditIssue(ctx, db.EditIssueParams{IssueID: issue.ID, Title: &title, Actor: "other-member"})
	require.NoError(t, err)
	creator, err = provenance.EntityAttribution(ctx, project.UID, "issue", issue.UID)
	require.NoError(t, err)
	assert.Equal(t, receipt, creator, "last editor never replaces creation proof")
	duplicate, err := provenance.RecordRootAttribution(ctx, grant.Enrollment.ID, source, signer)
	require.NoError(t, err)
	assert.Equal(t, receipt, duplicate)
	second, err := store.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{ProjectID: &project.ID, SpokeInstanceUID: "00000000000000000000000003", Actor: "other-member", Capabilities: "pull,push", Token: "second-grant"})
	require.NoError(t, err)
	duplicate, err = provenance.RecordRootAttribution(ctx, second.Enrollment.ID, source, signer)
	require.NoError(t, err)
	assert.Equal(t, receipt, duplicate, "another account cannot take over a previously accepted creation")
	tampered := source
	tampered.ContentHash = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	_, err = provenance.RecordRootAttribution(ctx, grant.Enrollment.ID, tampered, signer)
	require.Error(t, err, "same event UID with changed content rejects")
	page, err := provenance.AttributionReceiptsAfter(ctx, project.UID, 1, 0, 10)
	require.NoError(t, err)
	assert.Equal(t, []db.AttributionReceipt{receipt}, page)
	page, err = provenance.AttributionReceiptsAfter(ctx, project.UID, 1, receipt.Sequence, 10)
	require.NoError(t, err)
	assert.Empty(t, page)
	hidden := db.WithAuthorizedProjects(ctx, []string{})
	_, err = provenance.EntityAttribution(hidden, project.UID, "issue", issue.UID)
	assert.ErrorIs(t, err, db.ErrNotFound)
	page, err = provenance.AttributionReceiptsAfter(hidden, project.UID, 1, 0, 10)
	require.NoError(t, err)
	assert.Empty(t, page)
	// Source labels cannot grant access after enrollment actor loses team membership.
	team, _, err := store.CreateTeam(ctx, "members", "admin")
	require.NoError(t, err)
	_, err = store.SetTeamMembership(ctx, team.UID, "assistant", true, "admin")
	require.NoError(t, err)
	policy, err := store.ProjectAccessPolicy(ctx, project.UID)
	require.NoError(t, err)
	policy.Visibility, policy.TeamUIDs = "teams", []string{team.UID}
	_, _, err = store.SetProjectAccessPolicy(ctx, policy, "admin")
	require.NoError(t, err)
	_, secondEvent, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Second source task", Author: "assistant"})
	require.NoError(t, err)
	_, err = provenance.RecordRootAttribution(ctx, grant.Enrollment.ID, remoteEventFromStored(secondEvent), signer)
	require.Error(t, err, "source actor membership does not authorize accountable actor")
	_, err = store.SetTeamMembership(ctx, team.UID, "member", true, "admin")
	require.NoError(t, err)
	secondReceipt, err := provenance.RecordRootAttribution(ctx, grant.Enrollment.ID, remoteEventFromStored(secondEvent), signer)
	require.NoError(t, err)
	assert.Equal(t, int64(2), secondReceipt.Sequence)
	require.NoError(t, store.RevokeFederationEnrollment(ctx, grant.Enrollment.ID))
	_, thirdEvent, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Third source task", Author: "assistant"})
	require.NoError(t, err)
	_, err = provenance.RecordRootAttribution(ctx, grant.Enrollment.ID, remoteEventFromStored(thirdEvent), signer)
	require.Error(t, err, "revoked enrollment cannot issue new root acceptance")
}

// RunUpstreamAttribution exercises upstream attribution on the supplied native store.
// R5: replicas verify the enrolled root and preserve the company's account,
// even when the replica's own credential and source actor use different names.
func RunUpstreamAttribution(t *testing.T, store db.Storage) {
	t.Helper()
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "replica-project")
	require.NoError(t, err)
	issue, event, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Imported source task", Author: "assistant"})
	require.NoError(t, err)
	_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: 42, HubProjectUID: project.UID, Actor: "personal-member", Enabled: true})
	require.NoError(t, err)
	public, private, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	pin := db.RootKeyPin{ProjectUID: project.UID, AuthorityUID: "00000000000000000000000001", KeyID: db.RootPublicKeyID(public), PublicKey: public}
	require.NoError(t, store.PinRootAuthority(ctx, pin))
	proof, err := db.SignRootReceipt(db.AttributionReceipt{Version: 1, ProjectUID: project.UID, AuthorityUID: pin.AuthorityUID, KeyID: pin.KeyID, EventUID: event.UID, ContentHash: event.ContentHash, AccountableActor: "company-member", SourceActor: event.Actor, IngressInstanceUID: "00000000000000000000000002", AcceptedAt: time.Now().UTC(), ResetEpoch: 1, Sequence: 1}, private)
	require.NoError(t, err)
	ui, ok := store.(db.UIStore)
	require.True(t, ok)
	before, err := ui.UIEventCursor(ctx)
	require.NoError(t, err)
	require.NoError(t, store.ApplyUpstreamAttribution(ctx, pin, proof))
	after, err := ui.UIEventCursor(ctx)
	require.NoError(t, err)
	require.Greater(t, after, before, "receipt-only creation proof invalidates prior browser snapshots")
	reset, err := store.PurgeResetCheck(ctx, before, project.ID)
	require.NoError(t, err)
	require.Equal(t, after, reset, "reconnected streams must refresh the changed creator projection")
	snapshot, err := ui.ReadUISnapshot(ctx, db.UISnapshotQuery{ProjectUID: project.UID, Limit: 10})
	require.NoError(t, err)
	require.Equal(t, after, snapshot.Cursor)
	require.Len(t, snapshot.Issues, 1)
	require.Equal(t, "verified", snapshot.Issues[0].Verification)
	require.NoError(t, store.ApplyUpstreamAttribution(ctx, pin, proof), "delivery retry is idempotent")
	retryCursor, err := ui.UIEventCursor(ctx)
	require.NoError(t, err)
	require.Equal(t, after, retryCursor, "a duplicate receipt cannot generate another reset")
	reset, err = store.PurgeResetCheck(ctx, after, project.ID)
	require.NoError(t, err)
	require.Zero(t, reset)
	unchanged, err := store.EventsByUIDs(ctx, project.ID, []string{event.UID})
	require.NoError(t, err)
	require.Equal(t, []db.Event{event}, unchanged, "receipt delivery cannot rewrite immutable source history")
	creator, err := store.EntityAttribution(ctx, project.UID, "issue", issue.UID)
	require.NoError(t, err)
	assert.Equal(t, proof, creator)
	altered := proof
	altered.AccountableActor = "personal-member"
	altered, err = db.SignRootReceipt(altered, private)
	require.NoError(t, err)
	require.Error(t, store.ApplyUpstreamAttribution(ctx, pin, altered), "even another valid proof cannot overwrite accepted attribution")
	forgedPin := pin
	forgedPin.AuthorityUID = "00000000000000000000000003"
	require.Error(t, store.ApplyUpstreamAttribution(ctx, forgedPin, proof))
	rejectedCursor, err := ui.UIEventCursor(ctx)
	require.NoError(t, err)
	require.Equal(t, after, rejectedCursor, "rejected proofs leave snapshot authority unchanged")
	other, err := store.CreateProject(ctx, "unrelated-project")
	require.NoError(t, err)
	reset, err = store.PurgeResetCheck(ctx, before, other.ID)
	require.NoError(t, err)
	require.Zero(t, reset, "project streams do not receive another project's proof reset")
	_, later, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: other.ID, Title: "Later local write", Author: "member"})
	require.NoError(t, err)
	require.Greater(t, later.ID, after, "future events stay above the reserved reset cursor")
	hidden := db.WithAuthorizedProjects(ctx, []string{})
	var leaked []db.ImportRecord
	for record, e := range store.ExportAttribution(hidden, db.ExportFilter{}) {
		require.NoError(t, e)
		leaked = append(leaked, record)
	}
	require.Empty(t, leaked)
	require.ErrorIs(t, store.ApplyUpstreamAttribution(hidden, pin, proof), db.ErrNotFound)
	for _, projectID := range []int64{0, project.ID} {
		reset, err := store.PurgeResetCheck(hidden, before, projectID)
		require.NoError(t, err)
		require.Zero(t, reset, "hidden project proofs cannot expose reset authority")
	}
}
