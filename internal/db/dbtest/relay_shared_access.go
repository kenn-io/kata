package dbtest

import (
	"context"
	"crypto/ed25519"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/notification"
)

// RelaySharedAccessFixture is one native root and its common-audience pair.
type RelaySharedAccessFixture struct {
	Projects [2]db.Project
	Issues   [2]db.Issue
	Grants   [2]db.CreatedFederationEnrollment
	Linked   db.Event
	Team     db.Team
	Signer   db.RootAttributionSigner
}

// NewRelaySharedAccessFixture enrolls the same native pair for storage and
// embedding-boundary tests, keeping the initial regression at native admission.
func NewRelaySharedAccessFixture(t *testing.T, store db.Storage) RelaySharedAccessFixture {
	t.Helper()
	return newRelaySharedAccessFixture(t, store, true)
}

// RunRelayCheckpointSizeWithSharedLinks exercises the read-only checkpoint
// measurement path for a project with retained references to its shared peer.
func RunRelayCheckpointSizeWithSharedLinks(t *testing.T, store db.Storage) {
	t.Helper()
	fixture := NewRelaySharedAccessFixture(t, store)
	measurer, ok := store.(interface {
		RelayCheckpointSize(context.Context, int64) (db.RelayCheckpointSize, error)
	})
	require.True(t, ok, "storage must expose relay checkpoint size measurement")
	size, err := measurer.RelayCheckpointSize(t.Context(), fixture.Projects[0].ID)
	require.NoError(t, err)
	require.Positive(t, size.Bytes)
	require.Positive(t, size.Capacity)
}

func newRelaySharedAccessFixture(t *testing.T, store db.Storage, link bool) RelaySharedAccessFixture {
	t.Helper()
	ctx := t.Context()
	team, _, err := store.CreateTeam(ctx, "shared-audience", "admin")
	require.NoError(t, err)
	pair := newRelayLinkBoundaryFixture(t, store, "shared-project", db.CreateAPITokenParams{
		Actor: "member", AdminActor: "admin", TeamUIDs: []string{team.UID},
		PlaintextToken: "shared-audience-parent-test-token",
	})
	projects := [2]db.Project{pair.project, pair.sibling}
	public := pair.signer.PrivateKey.Public().(ed25519.PublicKey)
	require.NoError(t, store.PinRootAuthority(ctx, db.RootKeyPin{ProjectUID: pair.sibling.UID,
		AuthorityUID: pair.signer.AuthorityUID, KeyID: db.RootPublicKeyID(public), PublicKey: public}))
	var issues [2]db.Issue
	for i, project := range projects {
		policy, err := store.ProjectAccessPolicy(ctx, project.UID)
		require.NoError(t, err)
		policy.Visibility, policy.TeamUIDs = "teams", []string{team.UID}
		_, _, err = store.SetProjectAccessPolicy(ctx, policy, "admin")
		require.NoError(t, err)
		issues[i], _, err = store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID,
			Author: "member", Title: "Shared endpoint"})
		require.NoError(t, err)
	}
	var linked db.Event
	if link {
		_, linked, err = store.CreateLinkAndEvent(ctx, db.CreateLinkParams{FromIssueID: issues[0].ID,
			ToIssueID: issues[1].ID, Type: "blocks", Author: "member"},
			db.LinkEventParams{EventType: "issue.linked", EventIssueID: issues[0].ID, Actor: "member",
				FromUID: issues[0].UID, ToUID: issues[1].UID, FromShortID: issues[0].ShortID, ToShortID: issues[1].ShortID})
		require.NoError(t, err)
	}
	var grants [2]db.CreatedFederationEnrollment
	for i, project := range projects {
		grants[i], err = store.CreateRelayEnrollment(ctx, db.CreateRelayEnrollmentParams{ProjectID: project.ID,
			ParentTokenID: pair.parentTokenID, SpokeInstanceUID: "00000000000000000000000006",
			ProtocolVersion: db.RelayProtocolVersion, Token: []string{"first-shared-relay-test-token", "second-shared-relay-test-token"}[i]})
		require.NoError(t, err, "root-authored crossing links with a shared audience can enroll")
	}

	return RelaySharedAccessFixture{Projects: projects, Issues: issues, Grants: grants, Linked: linked, Team: team, Signer: pair.signer}
}

// RunRelaySharedAccessEnrollment exercises native admission and exact-byte release.
func RunRelaySharedAccessEnrollment(t *testing.T, store db.Storage, compact func(context.Context, int64) error) {
	fixture := NewRelaySharedAccessFixture(t, store)
	ctx := t.Context()
	projects, issues, grants, linked, team := fixture.Projects, fixture.Issues, fixture.Grants, fixture.Linked, fixture.Team
	private := fixture.Signer.PrivateKey

	binding := grants[0].Enrollment.RelayBindingUID
	revokePeer := func(visible bool) {
		policy, err := store.ProjectAccessPolicy(ctx, projects[1].UID)
		require.NoError(t, err)
		policy.TeamUIDs = nil
		if visible {
			policy.TeamUIDs = []string{team.UID}
		}
		_, _, err = store.SetProjectAccessPolicy(ctx, policy, "admin")
		require.NoError(t, err)
	}
	revokePeer(false)
	withheld, err := store.PendingRelayDeliveries(ctx, binding, db.RelayStreamEvent, 100)
	require.Error(t, err, "first offer with inaccessible peer withholds the whole selection")
	require.Nil(t, withheld)
	revokePeer(true)
	offered, err := store.PendingRelayDeliveries(ctx, binding, db.RelayStreamEvent, 100)
	require.NoError(t, err)
	require.NotEmpty(t, offered)
	var crossing db.RelayEnvelope
	for _, envelope := range offered {
		if envelope.SourceUID == linked.UID {
			crossing = envelope
		}
	}
	require.Equal(t, linked.ContentHash, crossing.SourceHash, "original crossing history stays deliverable")
	source, err := db.DecodeRelaySourceEvent(crossing.Body)
	require.NoError(t, err)
	require.Equal(t, db.RemoteEventFromStored(linked), source)
	revokePeer(false)
	withheld, err = store.PendingRelayDeliveries(ctx, binding, db.RelayStreamEvent, 100)
	require.Error(t, err, "lost-response retry rechecks its exact retained peer")
	require.Nil(t, withheld)
	revokePeer(true)
	retry, err := store.PendingRelayDeliveries(ctx, binding, db.RelayStreamEvent, 100)
	require.NoError(t, err)
	require.Equal(t, offered, retry, "restored access retries the exact body, identities and hashes")

	// Each real native producer uses the same exact-byte release rule. The
	// existing payload parser supplies link fields; close/reply resolution is
	// exercised through retained source bodies rather than synthetic envelopes.
	targetComment, _, err := store.CreateComment(ctx, db.CreateCommentParams{IssueID: issues[1].ID, Author: "member", Body: "Peer comment"})
	require.NoError(t, err)
	for _, form := range []string{"creation", "composite", "close", "reply"} {
		drainRelayDeliveries(t, store, binding)
		switch form {
		case "creation":
			_, _, err = store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: projects[0].ID, Author: "member", Title: "Initial shared link", Links: []db.InitialLink{{Type: "related", ToNumber: issues[1].ID}}})
		case "composite":
			_, err = store.EditIssueAtomic(ctx, db.EditIssueAtomicParams{IssueID: issues[0].ID, Actor: "member", AddRelated: []int64{issues[1].ID}, AddBlockedBy: []int64{issues[1].ID}, SetParent: &issues[1].ID})
		case "close":
			_, _, _, err = store.CloseIssueWithEvents(ctx, issues[0].ID, "done", "member", "Completed the shared endpoint work and verified the affected paths.", []db.Evidence{{Type: "reviewed-paths", Paths: []string{"internal/db/dbtest/relay_shared_access.go"}}})
		case "reply":
			_, _, err = store.CreateComment(ctx, db.CreateCommentParams{IssueID: issues[0].ID, Author: "member", Body: "Shared reply", ReplyToUID: targetComment.UID, ReplyKind: "reply"})
		}
		require.NoError(t, err, form)
		exact, err := store.PendingRelayDeliveries(ctx, binding, db.RelayStreamEvent, 100)
		require.NoError(t, err, form)
		require.NotEmpty(t, exact)
		revokePeer(false)
		_, err = store.PendingRelayDeliveries(ctx, binding, db.RelayStreamEvent, 100)
		require.Error(t, err, form)
		revokePeer(true)
		retry, err := store.PendingRelayDeliveries(ctx, binding, db.RelayStreamEvent, 100)
		require.NoError(t, err, form)
		require.Equal(t, exact, retry, form)
	}
	drainRelayDeliveries(t, store, binding)
	resetter := store.(db.RelayResetStore)
	checkpoint, err := resetter.CreateRelayReset(ctx, binding, db.RootAttributionSigner{AuthorityUID: store.InstanceUID(), PrivateKey: private})
	require.NoError(t, err)
	revokePeer(false)
	_, err = resetter.CreateRelayReset(ctx, binding, db.RootAttributionSigner{AuthorityUID: store.InstanceUID(), PrivateKey: private})
	require.Error(t, err, "cached exact checkpoint rechecks peer references")
	revokePeer(true)
	resetRetry, err := resetter.CreateRelayReset(ctx, binding, db.RootAttributionSigner{AuthorityUID: store.InstanceUID(), PrivateKey: private})
	require.NoError(t, err)
	require.Equal(t, checkpoint, resetRetry)
	require.NoError(t, compact(ctx, projects[0].ID))

	fresh, err := store.CreateRelayEnrollment(ctx, db.CreateRelayEnrollmentParams{ProjectID: projects[0].ID, ParentTokenID: *grants[0].Enrollment.ParentTokenID, SpokeInstanceUID: "00000000000000000000000007", ProtocolVersion: db.RelayProtocolVersion, Token: "compacted-shared-relay-test-token"})
	require.NoError(t, err)
	binding = fresh.Enrollment.RelayBindingUID
	entityCheckpoint, err := resetter.CreateRelayReset(ctx, binding, fixture.Signer)
	require.NoError(t, err)
	var history [][]byte
	require.NoError(t, json.Unmarshal(entityCheckpoint.Snapshot.Events, &history))
	require.Empty(t, history, "compacted history leaves only exact checkpoint entities")
	require.NotEmpty(t, entityCheckpoint.Snapshot.Entities)
	revokePeer(false)
	_, err = resetter.CreateRelayReset(ctx, binding, fixture.Signer)
	require.Error(t, err, "entity-only cached checkpoint still checks its peers")
	revokePeer(true)
	entityRetry, err := resetter.CreateRelayReset(ctx, binding, fixture.Signer)
	require.NoError(t, err)
	require.Equal(t, entityCheckpoint, entityRetry)
}

// RunRelaySharedAccessNotification checks typed native metadata pointers, including the previous
// value of a cleared slot. Keep link history out of these exact selections.
func RunRelaySharedAccessNotification(t *testing.T, store db.Storage, form string, compact func(context.Context, int64) error) {
	ctx := t.Context()
	noRef := strings.HasPrefix(form, "no-ref-")
	f := newRelaySharedAccessFixture(t, store, form != "snapshot" && !noRef)
	comment, _, err := store.CreateComment(ctx, db.CreateCommentParams{IssueID: f.Issues[1].ID, Author: "member", Body: "Peer finding"})
	require.NoError(t, err)
	binding := f.Grants[0].Enrollment.RelayBindingUID
	slot, err := json.Marshal(notification.Value{From: "member", Message: "Inspect peer finding", Re: comment.UID})
	require.NoError(t, err)
	if noRef {
		var fields map[string]jsontext.Value
		require.NoError(t, json.Unmarshal(slot, &fields))
		if fields == nil {
			t.Fatal("notification fixture must decode to an object")
			return
		}
		delete(fields, "re")
		if form == "no-ref-empty" {
			fields["re"] = jsontext.Value(`""`)
		}
		if form == "no-ref-null" {
			fields["re"] = jsontext.Value(`null`)
		}
		slot, err = json.Marshal(fields)
		require.NoError(t, err)
		value, _, err := notification.DecodeValue(slot)
		require.NoError(t, err)
		require.Empty(t, value.Re, "native optional re semantics have no comment dependency")
	}
	key := notification.MetadataKey("member")
	patch := func(value jsontext.Value) db.Event {
		out, err := store.PatchIssueMetadata(ctx, db.PatchIssueMetadataIn{IssueID: f.Issues[0].ID, Actor: "member", Patch: map[string]jsontext.Value{key: value}})
		require.NoError(t, err)
		require.True(t, out.Changed)
		return out.Event
	}
	peerAccess := func(visible bool) {
		policy, err := store.ProjectAccessPolicy(ctx, f.Projects[1].UID)
		require.NoError(t, err)
		policy.TeamUIDs = nil
		if visible {
			policy.TeamUIDs = []string{f.Team.UID}
		}
		_, _, err = store.SetProjectAccessPolicy(ctx, policy, "admin")
		require.NoError(t, err)
	}
	drainRelayDeliveries(t, store, binding)
	if noRef {
		event := patch(slot)
		peerAccess(false)
		exact, err := store.PendingRelayDeliveries(ctx, binding, db.RelayStreamEvent, 100)
		require.NoError(t, err, "a notification with no reference remains deliverable")
		require.Len(t, exact, 1)
		source, err := db.DecodeRelaySourceEvent(exact[0].Body)
		require.NoError(t, err)
		require.Equal(t, db.RemoteEventFromStored(event), source, "original payload and source hash remain exact")
		retry, err := store.PendingRelayDeliveries(ctx, binding, db.RelayStreamEvent, 100)
		require.NoError(t, err)
		require.Equal(t, exact, retry)
		checkpoint, err := store.(db.RelayResetStore).CreateRelayReset(ctx, binding, f.Signer)
		require.NoError(t, err, "optional absence also allows selected reset entities")
		var bodies [][]byte
		require.NoError(t, json.Unmarshal(checkpoint.Snapshot.Entities, &bodies))
		found := false
		for _, body := range bodies {
			entity, err := db.DecodeRelaySourceEvent(body)
			require.NoError(t, err)
			if entity.IssueUID == nil || *entity.IssueUID != f.Issues[0].UID {
				continue
			}
			var payload struct {
				Metadata map[string]jsontext.Value `json:"metadata"`
			}
			require.NoError(t, json.Unmarshal(entity.Payload, &payload))
			var expected, stored map[string]jsontext.Value
			require.NoError(t, json.Unmarshal(slot, &expected))
			require.NoError(t, json.Unmarshal(payload.Metadata[key], &stored))
			require.Equal(t, expected["re"], stored["re"], "snapshot retains omitted, empty or null re without rewriting it")
			found = true
		}
		require.True(t, found, "native checkpoint includes the notification's entity")
		resetRetry, err := store.(db.RelayResetStore).CreateRelayReset(ctx, binding, f.Signer)
		require.NoError(t, err)
		require.Equal(t, checkpoint, resetRetry)
		return
	}
	if form == "from" {
		patch(slot)
		drainRelayDeliveries(t, store, binding)
	}
	value := jsontext.Value(slot)
	if form == "from" {
		value = jsontext.Value("null")
	}
	event := patch(value)
	if form == "snapshot" {
		require.NoError(t, compact(ctx, f.Projects[0].ID))
		fresh, err := store.CreateRelayEnrollment(ctx, db.CreateRelayEnrollmentParams{ProjectID: f.Projects[0].ID, ParentTokenID: *f.Grants[0].Enrollment.ParentTokenID, SpokeInstanceUID: "00000000000000000000000007", ProtocolVersion: db.RelayProtocolVersion, Token: "notification-entity-only-test-token"})
		require.NoError(t, err)
		binding = fresh.Enrollment.RelayBindingUID
		checkpoint, err := store.(db.RelayResetStore).CreateRelayReset(ctx, binding, f.Signer)
		require.NoError(t, err)
		var history [][]byte
		require.NoError(t, json.Unmarshal(checkpoint.Snapshot.Events, &history))
		require.Empty(t, history, "compacted checkpoint must authorize the entity without a retained notification diff")
		var bodies [][]byte
		require.NoError(t, json.Unmarshal(checkpoint.Snapshot.Entities, &bodies))
		found := false
		for _, body := range bodies {
			source, err := db.DecodeRelaySourceEvent(body)
			require.NoError(t, err)
			if source.IssueUID != nil && *source.IssueUID == f.Issues[0].UID && strings.Contains(string(source.Payload), key) {
				found = true
			}
		}
		require.True(t, found, "snapshot has the canonical notification metadata")
		peerAccess(false)
		_, err = store.(db.RelayResetStore).CreateRelayReset(ctx, binding, f.Signer)
		peerAccess(true)
		require.Error(t, err, "snapshot metadata must authorize its comment's project")
		retry, err := store.(db.RelayResetStore).CreateRelayReset(ctx, binding, f.Signer)
		require.NoError(t, err)
		require.Equal(t, checkpoint, retry)
		return
	}
	exact, err := store.PendingRelayDeliveries(ctx, binding, db.RelayStreamEvent, 100)
	require.NoError(t, err)
	require.Len(t, exact, 1, "older crossing history cannot mask the metadata dependency")
	require.Equal(t, event.UID, exact[0].SourceUID)
	peerAccess(false)
	withheld, err := store.PendingRelayDeliveries(ctx, binding, db.RelayStreamEvent, 100)
	peerAccess(true)
	require.Error(t, err, "metadata diff must authorize its comment's project")
	require.Nil(t, withheld)
	retry, err := store.PendingRelayDeliveries(ctx, binding, db.RelayStreamEvent, 100)
	require.NoError(t, err)
	require.Equal(t, exact, retry)
}

// RunRelaySharedAccessMovedReplyPurge refuses creation-time ownership after a
// native target move and purge. Both projects remain unfederated until then.
func RunRelaySharedAccessMovedReplyPurge(t *testing.T, store db.Storage) {
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "reply-source")
	require.NoError(t, err)
	peer, err := store.CreateProject(ctx, "moved-target")
	require.NoError(t, err)
	source, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Author: "operator", Title: "Surviving reply"})
	require.NoError(t, err)
	target, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Author: "operator", Title: "Moved finding"})
	require.NoError(t, err)
	comment, _, err := store.CreateComment(ctx, db.CreateCommentParams{IssueID: target.ID, Author: "operator", Body: "Finding follows its target issue", ValidateReply: true})
	require.NoError(t, err)
	_, event, err := store.CreateComment(ctx, db.CreateCommentParams{IssueID: source.ID, Author: "operator", Body: "Reply before the move", ReplyToUID: comment.UID, ReplyKind: "reply", ValidateReply: true})
	require.NoError(t, err)
	require.Equal(t, &target.UID, event.RelatedIssueUID)
	key := notification.MetadataKey("recipient")
	slot, err := json.Marshal(notification.Value{From: "operator", Message: "Inspect the moved finding", Re: comment.UID})
	require.NoError(t, err)
	_, err = store.PatchIssueMetadata(ctx, db.PatchIssueMetadataIn{IssueID: source.ID, Actor: "operator", Patch: map[string]jsontext.Value{key: slot}})
	require.NoError(t, err)
	target, err = store.IssueByID(ctx, target.ID)
	require.NoError(t, err)
	moved, err := store.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: target.ID, FromProjectID: project.ID, ToProjectID: peer.ID, IfMatchRev: target.Revision, Actor: "operator"})
	require.NoError(t, err)
	require.Equal(t, peer.ID, moved.Issue.ProjectID)
	_, err = store.PurgeIssue(ctx, target.ID, "operator", nil)
	require.NoError(t, err)
	policy, err := store.ProjectAccessPolicy(ctx, peer.UID)
	require.NoError(t, err)
	policy.Visibility, policy.TeamUIDs = "teams", nil
	_, _, err = store.SetProjectAccessPolicy(ctx, policy, "admin")
	require.NoError(t, err)
	accessible, err := store.AccessibleProjectUIDs(ctx, "recipient")
	require.NoError(t, err)
	require.Contains(t, accessible, project.UID)
	require.NotContains(t, accessible, peer.UID)
	public, private, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	pin := db.RootKeyPin{ProjectUID: project.UID, AuthorityUID: store.InstanceUID(), KeyID: db.RootPublicKeyID(public), PublicKey: public}
	require.NoError(t, store.PinRootAuthority(ctx, pin))
	signer := db.RootAttributionSigner{AuthorityUID: store.InstanceUID(), PrivateKey: private}
	_, err = store.EnableProjectFederation(db.WithRootAttribution(ctx, signer, "operator"), project.ID, "operator")
	require.NoError(t, err)
	parent, _, err := store.CreateAPIToken(ctx, db.CreateAPITokenParams{Actor: "recipient", AdminActor: "admin", PlaintextToken: "moved-reply-parent-test-token"})
	require.NoError(t, err)
	grant, err := store.CreateRelayEnrollment(ctx, db.CreateRelayEnrollmentParams{ProjectID: project.ID, ParentTokenID: parent.ID, SpokeInstanceUID: "00000000000000000000000008", ProtocolVersion: db.RelayProtocolVersion, Token: "moved-reply-relay-test-token"}) // #nosec G101 -- synthetic relay token used only in this test.
	require.NoError(t, err)
	before, err := store.IssueByID(ctx, source.ID)
	require.NoError(t, err)
	cursor, err := store.MaxEventID(ctx)
	require.NoError(t, err)
	for range 2 {
		checkpoint, err := store.(db.RelayResetStore).CreateRelayReset(ctx, grant.Enrollment.RelayBindingUID, signer)

		require.ErrorIs(t, err, db.ErrNotFound, "the recipient lacks the target's actual final project")
		require.Equal(t, db.RelayResetCheckpoint{}, checkpoint, "refusal releases no selected reset bytes")
	}
	after, err := store.IssueByID(ctx, source.ID)
	require.NoError(t, err)
	require.Equal(t, before, after)
	afterCursor, err := store.MaxEventID(ctx)
	require.NoError(t, err)
	require.Equal(t, cursor, afterCursor)
}

// RunRelaySharedAccessMovedBareEvidence checks that an old bare evidence ref
// cannot substitute the source namespace for a moved target's actual owner.
func RunRelaySharedAccessMovedBareEvidence(t *testing.T, store db.Storage, purge bool) {
	runRelaySharedAccessMovedBareEvidence(t, store, purge, false, false)
}

// RunRelaySharedAccessMovedBareEvidenceReusedShortID rejects historical
// evidence when a moved issue's former short ID has been reused in its source.
func RunRelaySharedAccessMovedBareEvidenceReusedShortID(t *testing.T, store db.Storage) {
	runRelaySharedAccessMovedBareEvidence(t, store, false, true, false)
}

// RunRelaySharedAccessMovedBareEvidenceReusedShortIDAfterPurge rejects a
// replacement short ID when purging its former owner removes the move event.
func RunRelaySharedAccessMovedBareEvidenceReusedShortIDAfterPurge(t *testing.T, store db.Storage) {
	runRelaySharedAccessMovedBareEvidence(t, store, true, true, false)
}

// RunRelaySharedAccessMovedBareEvidenceReturnedToSource allows historical
// evidence when the original issue, rather than a replacement, owns the ID.
func RunRelaySharedAccessMovedBareEvidenceReturnedToSource(t *testing.T, store db.Storage) {
	runRelaySharedAccessMovedBareEvidence(t, store, false, false, true)
}

// RunRelaySharedAccessQualifiedEvidenceNameReuseAfterPurge excludes history
// when a purged namespace is reused by a project with an older colliding ID.
func RunRelaySharedAccessQualifiedEvidenceNameReuseAfterPurge(t *testing.T, store db.Storage) {
	ctx := t.Context()
	fixture := NewRelaySharedAccessFixture(t, store)
	originalProject, err := store.CreateProject(ctx, "qualified-target")
	require.NoError(t, err)
	target, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: originalProject.ID, Author: "admin", Title: "Original target",
	})
	require.NoError(t, err)
	replacementUID := strings.Repeat("0", 26-len(target.ShortID)) + strings.ToUpper(target.ShortID)
	replacement, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: fixture.Projects[1].ID, Author: "member", Title: "Older colliding target",
		UID: replacementUID, ShortIDOverride: target.ShortID,
	})
	require.NoError(t, err)
	require.Equal(t, target.ShortID, replacement.ShortID)
	_, events, changed, err := store.CloseIssueWithEvents(ctx, fixture.Issues[0].ID,
		"duplicate", "member", "This work is represented by the qualified target issue.",
		[]db.Evidence{{Type: "duplicate-of", IssueRef: originalProject.Name + "#" + target.ShortID}})
	require.NoError(t, err)
	require.True(t, changed)
	var closed db.Event
	for _, event := range events {
		if event.Type == "issue.closed" {
			closed = event
		}
	}
	require.NotEmpty(t, closed.UID)
	privateProject, err := store.CreateProject(ctx, "private-target")
	require.NoError(t, err)
	moved, err := store.MoveIssueProject(ctx, db.MoveIssueProjectIn{
		IssueID: target.ID, FromProjectID: originalProject.ID, ToProjectID: privateProject.ID,
		IfMatchRev: target.Revision, Actor: "admin",
	})
	require.NoError(t, err)
	require.Equal(t, privateProject.ID, moved.Issue.ProjectID)
	privatePolicy, err := store.ProjectAccessPolicy(ctx, privateProject.UID)
	require.NoError(t, err)
	privatePolicy.Visibility, privatePolicy.TeamUIDs = "teams", nil
	_, _, err = store.SetProjectAccessPolicy(ctx, privatePolicy, "admin")
	require.NoError(t, err)
	archived, _, err := store.RemoveProject(ctx, db.RemoveProjectParams{
		ProjectID: originalProject.ID, Actor: "admin", Force: true,
	})
	require.NoError(t, err)
	_, err = store.PurgeProject(ctx, db.PurgeProjectParams{ProjectID: archived.ID, Actor: "admin"})
	require.NoError(t, err)
	_, _, changed, err = store.RenameProjectAndEvent(ctx, fixture.Projects[1].ID, originalProject.Name, "admin")
	require.NoError(t, err)
	require.True(t, changed)

	checkpoint, err := store.(db.RelayResetStore).CreateRelayReset(ctx,
		fixture.Grants[0].Enrollment.RelayBindingUID, fixture.Signer)
	require.NoError(t, err)
	var history [][]byte
	require.NoError(t, json.Unmarshal(checkpoint.Snapshot.Events, &history))
	for _, body := range history {
		event, err := db.DecodeRelaySourceEvent(body)
		require.NoError(t, err)
		require.NotEqual(t, closed.UID, event.EventUID,
			"a reused project name cannot authorize the purged namespace's historical evidence")
	}
	var entities [][]byte
	require.NoError(t, json.Unmarshal(checkpoint.Snapshot.Entities, &entities))
	closedSnapshot := false
	for _, body := range entities {
		event, err := db.DecodeRelaySourceEvent(body)
		require.NoError(t, err)
		if event.Type != "issue.snapshot" || event.IssueUID == nil || *event.IssueUID != fixture.Issues[0].UID {
			continue
		}
		var snapshot struct {
			UID    string `json:"uid"`
			Status string `json:"status"`
		}
		require.NoError(t, json.Unmarshal(event.Payload, &snapshot))
		require.Equal(t, fixture.Issues[0].UID, snapshot.UID)
		require.Equal(t, "closed", snapshot.Status)
		closedSnapshot = true
	}
	require.True(t, closedSnapshot, "checkpoint keeps current closed state in entity snapshots")
	issue, err := store.IssueByUID(ctx, fixture.Issues[0].UID, db.IncludeDeletedYes)
	require.NoError(t, err)
	require.Equal(t, "closed", issue.Status, "safe current-state snapshots retain the close")
}

func runRelaySharedAccessMovedBareEvidence(t *testing.T, store db.Storage, purge, reuseShortID, returnToSource bool) {
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "evidence-source")
	require.NoError(t, err)
	peer, err := store.CreateProject(ctx, "evidence-destination")
	require.NoError(t, err)
	source, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Author: "operator", Title: "Duplicate source"})
	require.NoError(t, err)
	target, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Author: "operator", Title: "Original target"})
	require.NoError(t, err)
	oldRef := target.ShortID
	evidence := []db.Evidence{{Type: "duplicate-of", IssueRef: oldRef}}
	_, events, changed, err := store.CloseIssueWithEvents(ctx, source.ID, "duplicate", "operator", "This work is already represented by the original target issue.", evidence)
	require.NoError(t, err)
	require.True(t, changed)
	var closed db.Event
	closedCount := 0
	for _, event := range events {
		if event.Type == "issue.closed" {
			closed, closedCount = event, closedCount+1
		}
	}
	require.Equal(t, 1, closedCount)
	var payload struct {
		Evidence []db.Evidence `json:"evidence"`
	}
	require.NoError(t, json.Unmarshal([]byte(closed.Payload), &payload))
	require.Equal(t, evidence, payload.Evidence)
	moved, err := store.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: target.ID, FromProjectID: project.ID, ToProjectID: peer.ID, IfMatchRev: target.Revision, Actor: "operator"})
	require.NoError(t, err)
	require.Equal(t, target.UID, moved.Issue.UID)
	require.Equal(t, peer.ID, moved.Issue.ProjectID)
	if returnToSource {
		moved, err = store.MoveIssueProject(ctx, db.MoveIssueProjectIn{
			IssueID: target.ID, FromProjectID: peer.ID, ToProjectID: project.ID,
			IfMatchRev: moved.Issue.Revision, Actor: "operator",
		})
		require.NoError(t, err)
		require.Equal(t, target.UID, moved.Issue.UID)
		require.Equal(t, oldRef, moved.Issue.ShortID)
	}
	if reuseShortID {
		replacementUID := strings.Repeat("0", 26-len(oldRef)) + strings.ToUpper(oldRef)
		replacement, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
			ProjectID: project.ID, Author: "operator", Title: "Replacement target",
			UID: replacementUID, ShortIDOverride: oldRef,
		})
		require.NoError(t, err)
		require.Equal(t, oldRef, replacement.ShortID, "the moved issue's source short ID can be reused")
	}
	if purge {
		purged, err := store.PurgeIssue(ctx, target.ID, "operator", nil)
		require.NoError(t, err)
		require.Equal(t, &peer.UID, purged.ProjectUID)
	}
	policy, err := store.ProjectAccessPolicy(ctx, peer.UID)
	require.NoError(t, err)
	policy.Visibility, policy.TeamUIDs = "teams", nil
	_, _, err = store.SetProjectAccessPolicy(ctx, policy, "admin")
	require.NoError(t, err)
	accessible, err := store.AccessibleProjectUIDs(ctx, "recipient")
	require.NoError(t, err)
	require.Contains(t, accessible, project.UID)
	require.NotContains(t, accessible, peer.UID)
	public, private, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	pin := db.RootKeyPin{ProjectUID: project.UID, AuthorityUID: store.InstanceUID(), KeyID: db.RootPublicKeyID(public), PublicKey: public}
	require.NoError(t, store.PinRootAuthority(ctx, pin))
	signer := db.RootAttributionSigner{AuthorityUID: store.InstanceUID(), PrivateKey: private}
	_, err = store.EnableProjectFederation(db.WithRootAttribution(ctx, signer, "operator"), project.ID, "operator")
	require.NoError(t, err)
	parent, _, err := store.CreateAPIToken(ctx, db.CreateAPITokenParams{Actor: "recipient", AdminActor: "admin", PlaintextToken: "moved-evidence-parent-test-token"})
	require.NoError(t, err)
	before, err := store.IssueByID(ctx, source.ID)
	require.NoError(t, err)
	cursor, err := store.MaxEventID(ctx)
	require.NoError(t, err)
	grant, err := store.CreateRelayEnrollment(ctx, db.CreateRelayEnrollmentParams{ProjectID: project.ID, ParentTokenID: parent.ID, SpokeInstanceUID: "00000000000000000000000009", ProtocolVersion: db.RelayProtocolVersion, Token: "moved-evidence-relay-test-token"}) // #nosec G101 -- synthetic relay token used only in this test.
	refused := err != nil
	if err != nil {
		require.ErrorIs(t, err, db.ErrNotFound)
		require.Equal(t, db.CreatedFederationEnrollment{}, grant)
	} else {
		before, err = store.IssueByID(ctx, source.ID)
		require.NoError(t, err)
		cursor, err = store.MaxEventID(ctx)
		require.NoError(t, err)
		checkpoint, err := store.(db.RelayResetStore).CreateRelayReset(ctx, grant.Enrollment.RelayBindingUID, signer)
		refused = err != nil
		if err != nil {
			require.ErrorIs(t, err, db.ErrNotFound)
			require.Equal(t, db.RelayResetCheckpoint{}, checkpoint)
		} else {
			require.NoError(t, db.VerifyRootResetManifest(pin, checkpoint.Manifest, checkpoint.Snapshot))
			selected, _, err := db.DecodeRootResetPayload(pin, checkpoint.Snapshot)
			require.NoError(t, err)
			closedRetained := false
			for _, event := range selected {
				if event.EventUID == closed.UID {
					closedRetained = true
					require.True(t, returnToSource, "old bare ref %s cannot resolve to another issue after %s moved", oldRef, target.UID)
				}
			}
			require.Equal(t, returnToSource, closedRetained)
			var entities [][]byte
			require.NoError(t, json.Unmarshal(checkpoint.Snapshot.Entities, &entities))
			closedSnapshot := false
			for _, body := range entities {
				event, err := db.DecodeRelaySourceEvent(body)
				require.NoError(t, err)
				if event.Type != "issue.snapshot" || event.IssueUID == nil || *event.IssueUID != source.UID {
					continue
				}
				var snapshot struct {
					UID    string `json:"uid"`
					Status string `json:"status"`
				}
				require.NoError(t, json.Unmarshal(event.Payload, &snapshot))
				require.Equal(t, source.UID, snapshot.UID)
				require.Equal(t, "closed", snapshot.Status)
				closedSnapshot = true
			}
			require.True(t, closedSnapshot, "safe reset must deliver the source's current closed state")
		}
	}
	after, err := store.IssueByID(ctx, source.ID)
	require.NoError(t, err)
	require.Equal(t, "closed", after.Status)
	if refused {
		require.Equal(t, before, after)
		afterCursor, err := store.MaxEventID(ctx)
		require.NoError(t, err)
		require.Equal(t, cursor, afterCursor)
	}
	if returnToSource {
		require.False(t, refused, "the original issue is still authorized after moving back to its source")
	}
}
