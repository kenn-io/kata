package pgstore

import (
	"crypto/ed25519"
	"encoding/json/v2"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/embedding"
	"go.kenn.io/kata/internal/testenv"
)

// Relay sequence numbers are outbox row ids, and a receiver rejects any
// sequence at or below the prefix it already accepted. A row that takes a
// lower id but commits after a higher one was already sent is therefore lost
// for good. This observes a real PostgreSQL wait: a later artifact offer must
// not commit while an earlier outbox row is still in flight.
func TestRelayArtifactOutboxCommitsInIDOrder(t *testing.T) {
	ctx := t.Context()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)
	store, err := Open(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	project, err := store.CreateProject(ctx, "hub-project")
	require.NoError(t, err)
	_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleHub, HubProjectID: project.ID, HubProjectUID: project.UID, Enabled: true})
	require.NoError(t, err)
	public, _, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	require.NoError(t, store.PinRootAuthority(ctx, db.RootKeyPin{ProjectUID: project.UID, AuthorityUID: store.InstanceUID(), KeyID: db.RootPublicKeyID(public), PublicKey: public}))
	//nolint:gosec // This is a deterministic fixture credential, never an operational secret.
	parent, _, err := store.CreateAPIToken(ctx, db.CreateAPITokenParams{Actor: "company-member", AdminActor: "admin", PlaintextToken: "outbox-order-parent-test-token"})
	require.NoError(t, err)
	//nolint:gosec // This is a deterministic fixture credential, never an operational secret.
	grant, err := store.CreateRelayEnrollment(ctx, db.CreateRelayEnrollmentParams{ProjectID: project.ID, ParentTokenID: parent.ID, SpokeInstanceUID: "00000000000000000000000006", ProtocolVersion: db.RelayProtocolVersion, Token: "outbox-order-relay-test-token"})
	require.NoError(t, err)
	binding := grant.Enrollment.RelayBindingUID
	artifactFor := func(title string) embedding.EmbeddingArtifact {
		issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: title, Body: "Portable vectors", Author: "source-agent"})
		require.NoError(t, err)
		identity := embedding.ArtifactIdentity{ProjectUID: project.UID, IssueUID: issue.UID, ProducerInstanceUID: store.InstanceUID(), Provider: "openai-compatible", Model: "example-model", Dimensions: 2, InputType: "none", Normalization: "none", Preprocessing: "kata.issue/v2", RecipeVersion: 2, SplitMaxRunes: 2000, SplitOverlap: 200, RecipeFingerprint: strings.Repeat("a", 64)}
		artifact, err := embedding.NewArtifact(identity, embedding.EmbedText(issue.Title, issue.Body), [][]float32{{1, 0}})
		require.NoError(t, err)
		return artifact
	}
	first := artifactFor("First issue")
	second := artifactFor("Second issue")
	firstManifest, err := json.Marshal(first.Manifest())
	require.NoError(t, err)

	tx, err := store.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	require.NoError(t, queueRelayBodyTx(ctx, tx, project.ID, project.UID, db.RelayStreamArtifact, first.Digest, first.Digest, firstManifest, store.instanceUID))
	var holderPID int
	require.NoError(t, tx.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&holderPID))
	completed := make(chan error, 1)
	go func() { _, err := store.RetainEmbeddingArtifact(ctx, second); completed <- err }()
	blocked := false
	early := false
	//nolint:kennlint // sleeptest: PostgreSQL lock observation requires native server I/O outside a synctest bubble.
	require.Eventually(t, func() bool {
		select {
		case err := <-completed:
			require.NoError(t, err)
			early = true
			return true
		default:
		}
		err := store.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid)))`, holderPID).Scan(&blocked)
		require.NoError(t, err)
		return blocked
	}, 5*time.Second, 10*time.Millisecond)
	require.False(t, early, "a later artifact offer committed while an earlier outbox row was still in flight")
	require.True(t, blocked)
	require.NoError(t, tx.Commit())
	select {
	case err := <-completed:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("artifact retention did not resume after the earlier row committed")
	}
	offers, err := store.PendingRelayDeliveries(ctx, binding, db.RelayStreamArtifact, 32)
	require.NoError(t, err)
	require.Len(t, offers, 2)
	require.True(t, db.ValidRelayArtifactSourceUID(offers[0].SourceUID, first.Digest))
	require.True(t, db.ValidRelayArtifactSourceUID(offers[1].SourceUID, second.Digest))
	require.Less(t, offers[0].Sequence, offers[1].Sequence)
}
