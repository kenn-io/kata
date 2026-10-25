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
	// Root-only parent changes include old and new peers and remain atomic.
	parentLink, err := store.CreateLink(ctx, db.CreateLinkParams{FromIssueID: issues[0].ID, ToIssueID: issues[1].ID, Type: "parent", Author: "member"})
	require.NoError(t, err)
	localParent, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: projects[0].ID, Author: "member", Title: "Replacement parent"})
	require.NoError(t, err)
	replaced, err := store.(db.ParentLinkReplacementStorage).ReplaceParentAndEvents(ctx, db.ReplaceParentAndEventsParams{
		ExpectedParentLinkID: parentLink.ID, ExpectedParentIssueID: issues[1].ID,
		Link:        db.CreateLinkParams{FromIssueID: issues[0].ID, ToIssueID: localParent.ID, Type: "parent", Author: "member"},
		UnlinkEvent: db.LinkEventParams{EventType: "issue.unlinked", EventIssueID: issues[0].ID, Actor: "member", FromUID: issues[0].UID, ToUID: issues[1].UID, FromShortID: issues[0].ShortID, ToShortID: issues[1].ShortID},
		LinkEvent:   db.LinkEventParams{EventType: "issue.linked", EventIssueID: issues[0].ID, Actor: "member", FromUID: issues[0].UID, ToUID: localParent.UID, FromShortID: issues[0].ShortID, ToShortID: localParent.ShortID},
	})
	require.NoError(t, err)
	require.NoError(t, store.DeleteLinkByID(ctx, replaced.Link.ID))
	_, err = store.EditIssueAtomic(ctx, db.EditIssueAtomicParams{IssueID: issues[0].ID, Actor: "member", SetParent: &issues[1].ID})
	require.NoError(t, err)
	_, err = store.EditIssueAtomic(ctx, db.EditIssueAtomicParams{IssueID: issues[0].ID, Actor: "member", RemoveParent: &issues[1].ID})
	require.NoError(t, err)

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
			_, _, _, err = store.CloseIssueWithEvents(ctx, issues[0].ID, "done", "member", "Shared evidence", []db.Evidence{{Type: "duplicate-of", IssueRef: issues[1].UID}, {Type: "superseded-by", IssueRef: projects[1].Name + "#" + issues[1].ShortID}})
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

// Notifications are typed native metadata pointers, including the previous
// value of a cleared slot. Keep link history out of these exact selections.
func RunRelaySharedAccessNotification(t *testing.T, store db.Storage, form string) {
	ctx := t.Context()
	f := newRelaySharedAccessFixture(t, store, form != "snapshot")
	comment, _, err := store.CreateComment(ctx, db.CreateCommentParams{IssueID: f.Issues[1].ID, Author: "member", Body: "Peer finding"})
	require.NoError(t, err)
	binding := f.Grants[0].Enrollment.RelayBindingUID
	slot, err := json.Marshal(notification.Value{From: "member", Message: "Inspect peer finding", Re: comment.UID})
	require.NoError(t, err)
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
		checkpoint, err := store.(db.RelayResetStore).CreateRelayReset(ctx, binding, f.Signer)
		require.NoError(t, err)
		var bodies [][]byte
		require.NoError(t, json.Unmarshal(checkpoint.Snapshot.Entities, &bodies))
		found := false
		for _, body := range bodies {
			source, err := db.DecodeRelaySourceEvent(body)
			require.NoError(t, err)
			if source.IssueUID != nil && *source.IssueUID == f.Issues[0].UID && strings.Contains(string(source.Payload), key) {
				found = true
				lookup := db.NewEventIssueLookup(func(uid string) (db.Issue, error) { return store.IssueByUID(ctx, uid, db.IncludeDeletedYes) }, nil)
				projects, err := db.RelayEventReferencedProjects(source, nil, lookup, func(_ string, uid string) (db.Issue, error) {
					require.Equal(t, comment.UID, uid)
					return store.IssueByID(ctx, comment.IssueID)
				}, nil)
				require.NoError(t, err)
				require.Contains(t, projects, f.Projects[1].ID, "native snapshot metadata carries the peer dependency independently of retained events")
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
