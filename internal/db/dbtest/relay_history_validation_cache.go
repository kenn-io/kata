package dbtest

import (
	"crypto/ed25519"
	"encoding/json/jsontext"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
)

// RunRelayEnrollmentHistoryValidationCache verifies validation is reused when
// the source history is unchanged and invalidated by new source boundaries or
// purge markers. Enrollment authority remains live on every call.
func RunRelayEnrollmentHistoryValidationCache(t *testing.T, store db.Storage, backend Backend) {
	t.Helper()
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "relay-history-project")
	require.NoError(t, err)
	_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID: project.ID, Role: db.FederationRoleHub,
		HubProjectID: project.ID, HubProjectUID: project.UID, Enabled: true,
	})
	require.NoError(t, err)
	publicKey, _, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	pin := db.RootKeyPin{
		ProjectUID: project.UID, AuthorityUID: store.InstanceUID(),
		KeyID: db.RootPublicKeyID(publicKey), PublicKey: publicKey,
	}
	require.NoError(t, store.PinRootAuthority(ctx, pin))
	issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID, Title: "Original relay history", Author: "example-assistant",
	})
	require.NoError(t, err)
	parentToken, err := db.NewFederationToken()
	require.NoError(t, err)
	parent, _, err := store.CreateAPIToken(ctx, db.CreateAPITokenParams{
		Actor: "example-account", AdminActor: "admin", PlaintextToken: parentToken,
	})
	require.NoError(t, err)
	peerUID, err := uid.New()
	require.NoError(t, err)
	grantToken, err := db.NewFederationToken()
	require.NoError(t, err)
	grant, err := store.CreateRelayEnrollment(ctx, db.CreateRelayEnrollmentParams{
		ProjectID: project.ID, ParentTokenID: parent.ID, SpokeInstanceUID: peerUID,
		ProtocolVersion: db.RelayProtocolVersion, Token: grantToken,
	})
	require.NoError(t, err)

	// A cold handshake must release the writer before scanning history.
	cold := backend.OpenSame(t, store)
	bootstrap := cold.(db.RelayResetBootstrapStore)
	requireWriteDuringRelayScan(t, cold, backend, project.ID, func() error {
		required, err := bootstrap.RelayEnrollmentNeedsReset(ctx, grant.Enrollment.RelayBindingUID)
		require.False(t, required)
		return err
	}, nil)
	// Measure the original handle independently of the cold scan.
	fullScans := 0
	require.NotNil(t, backend.InstallRelayHistoryFullScanObserver)
	resetObserver := backend.InstallRelayHistoryFullScanObserver(store, func() { fullScans++ })
	t.Cleanup(resetObserver)
	bootstrap = store.(db.RelayResetBootstrapStore)
	for range 2 {
		required, err := bootstrap.RelayEnrollmentNeedsReset(ctx, grant.Enrollment.RelayBindingUID)
		require.NoError(t, err)
		require.False(t, required)
	}
	require.Equal(t, 1, fullScans, "unchanged history should reuse its validated cursor and result")

	_, _, err = store.CreateComment(ctx, db.CreateCommentParams{
		IssueID: issue.ID, Author: "example-assistant", Body: "New relay event",
	})
	require.NoError(t, err)
	required, err := bootstrap.RelayEnrollmentNeedsReset(ctx, grant.Enrollment.RelayBindingUID)
	require.NoError(t, err)
	require.False(t, required)
	require.Equal(t, 1, fullScans, "new events should be inspected after the cached history cursor")

	other, err := store.CreateProject(ctx, "unrelated-project")
	require.NoError(t, err)
	_, _, err = store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: other.ID, Title: "New endpoint", Author: "example-assistant",
	})
	require.NoError(t, err)
	required, err = bootstrap.RelayEnrollmentNeedsReset(ctx, grant.Enrollment.RelayBindingUID)
	require.NoError(t, err)
	require.False(t, required)
	require.Equal(t, 1, fullScans, "unrelated creations preserve validated history")
	requireReset := func(want bool, scans int, msg string) {
		t.Helper()
		required, err := bootstrap.RelayEnrollmentNeedsReset(ctx, grant.Enrollment.RelayBindingUID)
		require.NoError(t, err)
		require.Equal(t, want, required, msg)
		require.Equal(t, scans, fullScans, msg)
	}

	_, _, err = store.RemoveProject(ctx, db.RemoveProjectParams{
		ProjectID: other.ID, Actor: "example-operator", Force: true,
	})
	require.NoError(t, err)
	_, err = store.PurgeProject(ctx, db.PurgeProjectParams{
		ProjectID: other.ID, Actor: "example-operator",
	})
	require.NoError(t, err)
	required, err = bootstrap.RelayEnrollmentNeedsReset(ctx, grant.Enrollment.RelayBindingUID)
	require.NoError(t, err)
	require.False(t, required)
	require.Equal(t, 2, fullScans, "purge markers invalidate previously validated history")
	collision, err := store.CreateProject(ctx, "collision-project")
	require.NoError(t, err)
	for i, snapshotProject := range []db.Project{collision, project} {
		snapshot := newRemoteEvent(t, snapshotProject, &issue.UID, "issue.snapshot", "member", peerUID, 550, jsontext.Value(`{}`))
		_, err = store.InsertRemoteEvents(ctx, snapshotProject.ID, []db.RemoteEvent{snapshot})
		require.NoError(t, err)
		requireReset(false, 2+i, "only snapshots in the relayed project revalidate its history")
	}

	missingUID, err := uid.New()
	require.NoError(t, err)
	missingClose := newRemoteEvent(t, project, &issue.UID, "issue.closed", "member", peerUID, 575,
		jsontext.Value(fmt.Sprintf(`{"reason":"done","parent_uid":%q}`, missingUID)))
	_, err = store.InsertRemoteEvents(ctx, project.ID, []db.RemoteEvent{missingClose})
	require.NoError(t, err)
	requireReset(true, 3, "a missing parent requires reset")
	_, _, err = store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, UID: missingUID, Title: "Resolved parent", Author: "member"})
	require.NoError(t, err)
	requireReset(false, 4, "a new parent can resolve a cached reset result")

	parentIssue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Parent", Author: "member"})
	require.NoError(t, err)
	child, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Child", Author: "member"})
	require.NoError(t, err)
	legacyClose := newRemoteEvent(t, project, &child.UID, "issue.closed", "member", peerUID, 600,
		jsontext.Value(fmt.Sprintf(`{"reason":"done","closed_at":"2026-01-01T00:00:00Z","parent_short_id":%q}`, parentIssue.ShortID)))
	_, err = store.InsertRemoteEvents(ctx, project.ID, []db.RemoteEvent{legacyClose})
	require.NoError(t, err)
	requireReset(false, 4, "incremental validation tracks legacy short parents")
	_, _, err = store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: collision.ID, Title: "Different short reference", Author: "member"})
	require.NoError(t, err)
	requireReset(false, 5, "any creation revalidates a short-only parent")
	freshUID, err := uid.New()
	require.NoError(t, err)
	_, _, err = store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: collision.ID, Title: "Ambiguous parent", Author: "member", UID: freshUID[:10] + parentIssue.UID[10:], ShortIDOverride: parentIssue.ShortID})
	require.NoError(t, err)
	requireReset(true, 6, "a new short-id collision forces one full scan")

	_, _, err = store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: collision.ID, Title: "Another endpoint", Author: "member"})
	require.NoError(t, err)
	scans := requireWriteDuringRelayScan(t, store, backend, collision.ID, func() error {
		required, err := bootstrap.RelayEnrollmentNeedsReset(ctx, grant.Enrollment.RelayBindingUID)
		require.True(t, required)
		return err
	}, nil)
	require.Equal(t, int64(2), scans, "a creation during the fill causes one final fenced scan")

	reopened := backend.OpenSame(t, store)
	requireWriteDuringRelayScan(t, reopened, backend, collision.ID, func() error {
		_, err := reopened.(db.RelayResetBootstrapStore).RelayEnrollmentNeedsReset(ctx, grant.Enrollment.RelayBindingUID)
		require.ErrorIs(t, err, db.ErrNotFound, "revocation during fill is checked by the second fenced attempt")
		return nil
	}, func() {
		require.NoError(t, store.RevokeFederationEnrollment(ctx, grant.Enrollment.ID))
	})
	_, err = bootstrap.RelayEnrollmentNeedsReset(ctx, grant.Enrollment.RelayBindingUID)
	require.ErrorIs(t, err, db.ErrNotFound, "live enrollment authority is checked even when history is cached")
}

// requireWriteDuringRelayScan blocks the first full history scan started by
// scan, runs during, and requires a local issue write to commit meanwhile.
func requireWriteDuringRelayScan(t *testing.T, store db.Storage, backend Backend, projectID int64, scan func() error, during func()) int64 {
	t.Helper()
	entered, release := make(chan struct{}), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	defer unblock()
	var scans atomic.Int64
	defer backend.InstallRelayHistoryFullScanObserver(store, func() {
		if scans.Add(1) == 1 {
			close(entered)
			<-release
		}
	})()
	scanDone, writeDone := make(chan error, 1), make(chan error, 1)
	go func() { scanDone <- scan() }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("full scan did not start")
	}
	if during != nil {
		during()
	}
	go func() {
		_, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: projectID, Title: "Concurrent local issue", Author: "member"})
		writeDone <- err
	}()
	select {
	case err := <-writeDone:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Error("local write waited for the relay history scan")
		unblock()
		require.NoError(t, <-writeDone)
	}
	unblock()
	require.NoError(t, <-scanDone)
	return scans.Load()
}
