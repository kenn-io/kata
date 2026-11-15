package dbtest

import (
	"crypto/ed25519"
	"encoding/json/jsontext"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
)

// RunRelaySameProjectNamedEvidenceReset checks retained local close evidence.
func RunRelaySameProjectNamedEvidenceReset(t *testing.T, store db.Storage) {
	ctx := t.Context()
	fixture := newRelayLinkBoundaryFixture(t, store, "local-evidence")
	project := fixture.project
	target, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Evidence target", Author: "member"})
	require.NoError(t, err)
	closed, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Legacy close", Author: "member"})
	require.NoError(t, err)
	event := newRemoteEvent(t, project, &closed.UID, "issue.closed", "member", store.InstanceUID(), 600,
		jsontext.Value(fmt.Sprintf(`{"reason":"duplicate","evidence":[{"type":"duplicate-of","issue_ref":%q}]}`, project.Name+"#"+target.ShortID)))
	_, err = store.InsertRemoteEvents(ctx, project.ID, []db.RemoteEvent{event})
	require.NoError(t, err)
	legacyParent := newRemoteEvent(t, project, &closed.UID, "issue.closed", "member", store.InstanceUID(), 601,
		jsontext.Value(fmt.Sprintf(`{"reason":"done","parent_short_id":%q}`, target.ShortID)))
	_, err = store.InsertRemoteEvents(ctx, project.ID, []db.RemoteEvent{legacyParent})
	require.NoError(t, err)
	grant, err := store.CreateRelayEnrollment(ctx, db.CreateRelayEnrollmentParams{ProjectID: project.ID, ParentTokenID: fixture.parentTokenID,
		SpokeInstanceUID: "00000000000000000000000008", ProtocolVersion: db.RelayProtocolVersion})
	require.NoError(t, err)
	offered, err := store.PendingRelayDeliveries(ctx, grant.Enrollment.RelayBindingUID, db.RelayStreamEvent, 100)
	require.NoError(t, err)
	var found bool
	for _, envelope := range offered {
		if envelope.SourceUID == event.EventUID {
			found = true
			require.Equal(t, event.ContentHash, envelope.SourceHash)
		}
	}
	require.True(t, found)
	checkpoint, err := store.(db.RelayResetStore).CreateRelayReset(ctx, grant.Enrollment.RelayBindingUID, fixture.signer)
	require.NoError(t, err)
	public := fixture.signer.PrivateKey.Public().(ed25519.PublicKey)
	pin := db.RootKeyPin{ProjectUID: project.UID, AuthorityUID: fixture.signer.AuthorityUID, KeyID: db.RootPublicKeyID(public), PublicKey: public}
	events, _, err := db.DecodeRootResetPayload(pin, checkpoint.Snapshot)
	require.NoError(t, err)
	require.Contains(t, events, event)
	require.Contains(t, events, legacyParent)
}

// RunRelayNamedEvidenceCache verifies cached history validation follows project name resolution changes.
func RunRelayNamedEvidenceCache(t *testing.T, store db.Storage, backend Backend) {
	ctx := t.Context()
	fixture := newRelayLinkBoundaryFixture(t, store, "named-evidence")
	project := fixture.project
	target, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Evidence target", Author: "member"})
	require.NoError(t, err)
	closed, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Legacy close", Author: "member"})
	require.NoError(t, err)
	closeEvent := newRemoteEvent(t, project, &closed.UID, "issue.closed", "member", store.InstanceUID(), 600,
		jsontext.Value(fmt.Sprintf(`{"reason":"duplicate","evidence":[{"type":"duplicate-of","issue_ref":%q}]}`, project.Name+"#"+target.ShortID)))
	_, err = store.InsertRemoteEvents(ctx, project.ID, []db.RemoteEvent{closeEvent})
	require.NoError(t, err)
	grant, err := store.CreateRelayEnrollment(ctx, db.CreateRelayEnrollmentParams{
		ProjectID: project.ID, ParentTokenID: fixture.parentTokenID,
		SpokeInstanceUID: "00000000000000000000000008", ProtocolVersion: db.RelayProtocolVersion,
	})
	require.NoError(t, err)
	bootstrap := store.(db.RelayResetBootstrapStore)
	check := func(want bool) {
		t.Helper()
		cached, err := bootstrap.RelayEnrollmentNeedsReset(ctx, grant.Enrollment.RelayBindingUID)
		require.NoError(t, err)
		fresh, err := backend.OpenSame(t, store).(db.RelayResetBootstrapStore).RelayEnrollmentNeedsReset(ctx, grant.Enrollment.RelayBindingUID)
		require.NoError(t, err)
		require.Equal(t, want, fresh, "fresh validation resolves the stored project name")
		require.Equal(t, fresh, cached, "cached validation must follow name resolution changes")
	}
	check(false)
	_, err = store.RenameProject(ctx, project.ID, "renamed-evidence")
	require.NoError(t, err)
	check(true)
	other, err := store.CreateProject(ctx, project.Name)
	require.NoError(t, err)
	foreignUID, err := uid.New()
	require.NoError(t, err)
	_, _, err = store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: other.ID, Title: "Foreign evidence", Author: "member", UID: foreignUID[:10] + target.UID[10:], ShortIDOverride: target.ShortID})
	require.NoError(t, err)
	check(true)
	_, err = store.RenameProject(ctx, other.ID, "other-evidence")
	require.NoError(t, err)
	_, err = store.RenameProject(ctx, project.ID, project.Name)
	require.NoError(t, err)
	check(false)
}
