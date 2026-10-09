package dbtest

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json/jsontext"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
)

// RunAttributionReceiptBeforeEvent exercises attribution receipt before event on the supplied native store.
// R5: receipt delivery is independent of source delivery; a durable proof may
// arrive first, but conflicting source content must never commit underneath it.
func RunAttributionReceiptBeforeEvent(t *testing.T, store db.Storage) {
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "receipt-first-project")
	require.NoError(t, err)
	issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Replica projection", Author: "assistant"})
	require.NoError(t, err)
	// Seed imported projections before activating the read-only upstream replica.
	projections := make([]db.Issue, 11)
	for index := range projections {
		projections[index], _, err = store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Replica projection", Author: "assistant"})
		require.NoError(t, err)
	}
	_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: 42, HubProjectUID: project.UID, Actor: "personal-member", Enabled: true})
	require.NoError(t, err)
	public, private, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	pin := db.RootKeyPin{ProjectUID: project.UID, AuthorityUID: "00000000000000000000000001", KeyID: db.RootPublicKeyID(public), PublicKey: public}
	require.NoError(t, store.PinRootAuthority(ctx, pin))
	now := time.Now().UTC()
	source := db.RemoteEvent{EventUID: "00000000000000000000000003", OriginInstanceUID: pin.AuthorityUID, ProjectUID: project.UID, ProjectName: project.Name, IssueUID: &issue.UID, Type: "issue.created", Actor: "assistant", HLCPhysicalMS: now.UnixMilli(), CreatedAt: now, Payload: jsontext.Value(`{"title":"Original source","metadata":{}}`)}
	hash := func(event db.RemoteEvent) string {
		h, e := db.EventContentHash(db.EventHashInput{UID: event.EventUID, OriginInstanceUID: event.OriginInstanceUID, ProjectUID: event.ProjectUID, ProjectName: event.ProjectName, IssueUID: event.IssueUID, RelatedIssueUID: event.RelatedIssueUID, Type: event.Type, Actor: event.Actor, HLCPhysicalMS: event.HLCPhysicalMS, HLCCounter: event.HLCCounter, CreatedAt: event.CreatedAt.Format(db.EventTimestampFormat), Payload: event.Payload})
		require.NoError(t, e)
		return h
	}
	source.ContentHash = hash(source)
	_, _, err = db.ValidateRemoteEventContentHash(source)
	require.NoError(t, err, "source fixture must satisfy the released millisecond-hash contract")
	receipt, err := db.SignRootReceipt(db.AttributionReceipt{Version: 1, ProjectUID: project.UID, EventUID: source.EventUID, ContentHash: source.ContentHash, AuthorityUID: pin.AuthorityUID, KeyID: pin.KeyID, AccountableActor: "company-member", SourceActor: source.Actor, IngressInstanceUID: "00000000000000000000000002", AcceptedAt: now, ResetEpoch: 1, Sequence: 1}, private)
	require.NoError(t, err)
	require.NoError(t, store.ApplyUpstreamAttribution(ctx, pin, receipt))
	page, err := store.AttributionReceiptsAfter(ctx, project.UID, 1, 0, 10)
	require.NoError(t, err)
	require.Equal(t, []db.AttributionReceipt{receipt}, page)
	_, err = store.EntityAttribution(ctx, project.UID, "issue", issue.UID)
	require.ErrorIs(t, err, db.ErrNotFound, "proof waits for immutable source creation")
	binding, err := store.FederationBindingByProject(ctx, project.ID)
	require.NoError(t, err)
	binding.HubProjectUID = "00000000000000000000000007"
	_, err = store.UpsertFederationBinding(ctx, binding)
	require.NoError(t, err)
	_, err = store.InsertRemoteEvent(ctx, project.ID, source)
	require.Error(t, err, "pending source must recheck the current upstream project identity")
	binding.HubProjectUID = project.UID
	_, err = store.UpsertFederationBinding(ctx, binding)
	require.NoError(t, err)
	conflicting := source
	conflicting.Payload = jsontext.Value(`{"title":"Conflicting source","metadata":{}}`)
	conflicting.ContentHash = hash(conflicting)
	_, _, err = db.ValidateRemoteEventContentHash(conflicting)
	require.NoError(t, err, "conflicting source is independently self-consistent")
	_, err = store.InsertRemoteEvent(ctx, project.ID, conflicting)
	require.Error(t, err, "a different valid source hash cannot commit below the already accepted root receipt")
	inserted, err := store.InsertRemoteEvent(ctx, project.ID, source)
	require.NoError(t, err)
	require.True(t, inserted)
	got, err := store.EntityAttribution(ctx, project.UID, "issue", issue.UID)
	require.NoError(t, err)
	require.Equal(t, receipt, got, "source arrival attaches creation without needing a receipt retry")
	inserted, err = store.InsertRemoteEvent(ctx, project.ID, source)
	require.NoError(t, err)
	require.False(t, inserted)
	require.NoError(t, store.ApplyUpstreamAttribution(ctx, pin, receipt))
	got, err = store.EntityAttribution(ctx, project.UID, "issue", issue.UID)
	require.NoError(t, err)
	require.Equal(t, receipt, got)
	// Delivery streams race independently. Whichever commits first, the source
	// creator must have exactly the same durable proof after both complete.
	for index := range 10 {
		raceIssue := projections[index]
		next := source
		next.EventUID, err = uid.New()
		require.NoError(t, err)
		next.IssueUID = &raceIssue.UID
		next.ContentHash = hash(next)
		nextReceipt := receipt
		nextReceipt.EventUID, nextReceipt.ContentHash, nextReceipt.Sequence = next.EventUID, next.ContentHash, int64(index+2)
		nextReceipt, err = db.SignRootReceipt(nextReceipt, private)
		require.NoError(t, err)
		start := make(chan struct{})
		var wait sync.WaitGroup
		failures := make([]error, 2)
		wait.Go(func() { <-start; failures[0] = store.ApplyUpstreamAttribution(ctx, pin, nextReceipt) })
		wait.Go(func() { <-start; _, failures[1] = store.InsertRemoteEvent(ctx, project.ID, next) })
		close(start)
		wait.Wait()
		for _, failure := range failures {
			require.NoError(t, failure)
		}
		creator, e := store.EntityAttribution(ctx, project.UID, "issue", raceIssue.UID)
		require.NoError(t, e)
		require.Equal(t, nextReceipt, creator)
	}
	// A proof accepted before local revocation does not authorize a later source
	// commit. Re-admission may use the same immutable source and proof.
	heldIssue := projections[10]
	held := source
	held.EventUID, err = uid.New()
	require.NoError(t, err)
	held.IssueUID = &heldIssue.UID
	held.ContentHash = hash(held)
	heldReceipt := receipt
	heldReceipt.EventUID, heldReceipt.ContentHash, heldReceipt.Sequence = held.EventUID, held.ContentHash, 12
	heldReceipt, err = db.SignRootReceipt(heldReceipt, private)
	require.NoError(t, err)
	require.NoError(t, store.ApplyUpstreamAttribution(ctx, pin, heldReceipt))
	team, _, err := store.CreateTeam(ctx, "replica-team", "admin")
	require.NoError(t, err)
	_, err = store.SetTeamMembership(ctx, team.UID, "personal-member", true, "admin")
	require.NoError(t, err)
	policy, err := store.ProjectAccessPolicy(ctx, project.UID)
	require.NoError(t, err)
	policy.Visibility = "teams"
	policy.TeamUIDs = []string{team.UID}
	_, _, err = store.SetProjectAccessPolicy(ctx, policy, "admin")
	require.NoError(t, err)
	_, err = store.SetTeamMembership(ctx, team.UID, "personal-member", false, "admin")
	require.NoError(t, err)
	_, err = store.InsertRemoteEvent(db.WithAuthorizedProjects(ctx, []string{project.UID}), project.ID, held)
	require.Error(t, err, "captured project grant cannot survive membership revocation")
	_, err = store.EntityAttribution(ctx, project.UID, "issue", heldIssue.UID)
	require.ErrorIs(t, err, db.ErrNotFound)
	_, err = store.SetTeamMembership(ctx, team.UID, "personal-member", true, "admin")
	require.NoError(t, err)
	_, err = store.InsertRemoteEvent(ctx, project.ID, held)
	require.NoError(t, err)
	got, err = store.EntityAttribution(ctx, project.UID, "issue", heldIssue.UID)
	require.NoError(t, err)
	require.Equal(t, heldReceipt, got)

}
