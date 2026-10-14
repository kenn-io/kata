package dbtest

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
)

// RunPendingCreationLifecycle checks that a relay spoke's local creation
// intent rows never outlive their purpose: a signed root receipt replaces the
// row for its entity, an issue purge drops the rows for the issue and its
// comments, and a project purge drops every remaining row for the project
// together with its attribution stream cursor.
func RunPendingCreationLifecycle(t *testing.T, store db.Storage) {
	t.Helper()
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "spoke-project")
	require.NoError(t, err)
	rootUID := "00000000000000000000000002"
	public, private, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	pin := db.RootKeyPin{
		ProjectUID: project.UID, AuthorityUID: rootUID,
		KeyID: db.RootPublicKeyID(public), PublicKey: public,
	}
	require.NoError(t, store.PinRootAuthority(ctx, pin))
	_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID: project.ID, Role: db.FederationRoleSpoke,
		HubURL: "https://hub.example", HubProjectID: 42, HubProjectUID: project.UID,
		Actor: "example-actor", Enabled: true, PushEnabled: true,
	})
	require.NoError(t, err)
	bindingUID, err := uid.New()
	require.NoError(t, err)
	config := db.RelayBindingConfig{
		ProtocolVersion: db.RelayProtocolVersion, BindingUID: bindingUID,
		UpstreamInstanceUID: rootUID, AuthorityUID: rootUID,
		HubPath: []string{rootUID, store.InstanceUID()}, LocalActor: "example-actor",
		ResetEpoch: 1,
	}
	_, err = store.SetRelayBindingConfig(ctx, project.ID, config)
	require.NoError(t, err)

	metaKeys := func() map[string]string {
		t.Helper()
		keys := map[string]string{}
		for record, err := range store.ExportMeta(ctx) {
			require.NoError(t, err)
			keys[record.Key] = record.Value
		}
		return keys
	}
	pendingKey := func(kind, entityUID string) string {
		return db.PendingCreationMetadataPrefix + project.UID + "." + kind + "." + entityUID
	}
	var sequence int64
	accept := func(event db.Event, teammate string) {
		t.Helper()
		sequence++
		receipt, err := db.SignRootReceipt(db.AttributionReceipt{
			Version: 1, ProjectUID: project.UID, AuthorityUID: rootUID, KeyID: pin.KeyID,
			EventUID: event.UID, ContentHash: event.ContentHash,
			AccountableActor: "example-account", SourceActor: event.Actor, Teammate: teammate,
			IngressInstanceUID: store.InstanceUID(), AcceptedAt: time.Now().UTC(),
			ResetEpoch: 1, Sequence: sequence,
		}, private)
		require.NoError(t, err)
		require.NoError(t, store.ApplyUpstreamAttribution(ctx, pin, receipt))
	}

	issue, issueEvent, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID, Title: "Accepted later", Author: "example-actor",
	})
	require.NoError(t, err)
	comment, commentEvent, err := store.CreateComment(ctx, db.CreateCommentParams{
		IssueID: issue.ID, Author: "example-actor", Body: "Accepted later too",
	})
	require.NoError(t, err)
	keys := metaKeys()
	require.Contains(t, keys, pendingKey("issue", issue.UID), "local creation on a relay spoke is pending until the root accepts it")
	require.Contains(t, keys, pendingKey("comment", comment.UID))

	accept(issueEvent, "")
	keys = metaKeys()
	require.NotContains(t, keys, pendingKey("issue", issue.UID), "the signed receipt replaces the issue's pending intent")
	require.Contains(t, keys, pendingKey("comment", comment.UID), "another entity's receipt leaves the comment pending")
	shown, err := store.IssueByID(ctx, issue.ID)
	require.NoError(t, err)
	require.Equal(t, "verified", shown.Verification)
	require.Equal(t, "example-account", shown.AccountableActor)

	accept(commentEvent, "")
	keys = metaKeys()
	require.NotContains(t, keys, pendingKey("comment", comment.UID), "the signed receipt replaces the comment's pending intent")
	require.Contains(t, keys, db.AttributionUIResetMetadataPrefix+project.UID, "accepted receipts reserve the project's attribution stream cursor")

	// A push-enabled spoke cannot leave while local work is unaccepted, and a
	// bound spoke cannot purge, so a purge only ever meets pending rows that
	// outlived their entity, such as rows restored from an older owner backup.
	// Accept everything, then put such stale rows back for the purge checks.
	purged, purgedEvent, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID, Title: "Purged after acceptance", Author: "example-actor",
	})
	require.NoError(t, err)
	purgedComment, purgedCommentEvent, err := store.CreateComment(ctx, db.CreateCommentParams{
		IssueID: purged.ID, Author: "example-actor", Body: "Purged with its issue",
	})
	require.NoError(t, err)
	retained, retainedEvent, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID, Title: "Outlives the issue purge", Author: "example-actor",
	})
	require.NoError(t, err)
	accept(purgedEvent, "")
	accept(purgedCommentEvent, "")
	accept(retainedEvent, "")
	metadata, ok := store.(interface {
		ExecContext(context.Context, string, ...any) (sql.Result, error)
	})
	require.True(t, ok)
	for _, key := range []string{
		pendingKey("issue", purged.UID), pendingKey("comment", purgedComment.UID), pendingKey("issue", retained.UID),
	} {
		quotedKey := "'" + strings.ReplaceAll(key, "'", "''") + "'"
		_, err := metadata.ExecContext(ctx, `INSERT INTO meta(key,value) VALUES(`+quotedKey+`,'{}')`)
		require.NoError(t, err)
	}
	keys = metaKeys()
	require.Contains(t, keys, pendingKey("issue", purged.UID))
	require.Contains(t, keys, pendingKey("comment", purgedComment.UID))
	require.Contains(t, keys, pendingKey("issue", retained.UID))

	for _, stream := range []string{db.RelayStreamEvent, db.RelayStreamReceipt, db.RelayStreamArtifact} {
		queued, err := store.PendingRelayDeliveries(ctx, bindingUID, stream, 100)
		require.NoError(t, err)
		if len(queued) == 0 {
			continue
		}
		last := queued[len(queued)-1]
		require.NoError(t, store.AckRelayDeliveries(ctx, bindingUID, config.ResetEpoch, stream, last.Sequence, last.Digest))
	}
	_, err = store.LeaveFederationReplica(ctx, project.ID)
	require.NoError(t, err)

	_, err = store.PurgeIssue(ctx, purged.ID, "example-operator", nil)
	require.NoError(t, err)
	keys = metaKeys()
	require.NotContains(t, keys, pendingKey("issue", purged.UID), "issue purge drops the issue's pending intent")
	require.NotContains(t, keys, pendingKey("comment", purgedComment.UID), "issue purge drops its comments' pending intent")
	require.Contains(t, keys, pendingKey("issue", retained.UID), "issue purge leaves other issues' pending intent alone")

	_, _, err = store.RemoveProject(ctx, db.RemoveProjectParams{ProjectID: project.ID, Actor: "example-operator", Force: true})
	require.NoError(t, err)
	_, err = store.PurgeProject(ctx, db.PurgeProjectParams{ProjectID: project.ID, Actor: "example-operator"})
	require.NoError(t, err)
	keys = metaKeys()
	require.NotContains(t, keys, pendingKey("issue", retained.UID), "project purge drops the project's remaining pending intent")
	require.NotContains(t, keys, db.AttributionUIResetMetadataPrefix+project.UID, "project purge drops the project's attribution stream cursor")
}

// RunRelayResetDropsOrphanPendingCreations installs a root reset over a spoke
// whose metadata still carries pending creation rows for entities the
// authoritative snapshot no longer contains, such as rows restored from an
// older owner backup for issues the root has since purged. The rebuilt
// projection keeps no pending row without an entity.
func RunRelayResetDropsOrphanPendingCreations(t *testing.T, store db.Storage) {
	t.Helper()
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "spoke-project")
	require.NoError(t, err)
	// A local author rewrite from before the project joined feeds the
	// projection fold; the authoritative snapshot supersedes it.
	_, _, err = store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Local work before joining", Author: "example-actor"})
	require.NoError(t, err)
	_, err = store.RewriteAuthorIdentity(ctx, db.RewriteAuthorIdentityParams{ProjectID: project.ID, Actor: "example-actor", From: "example-actor", To: "rewritten-actor"})
	require.NoError(t, err)
	rewrites, err := store.EventsAfter(ctx, db.EventsAfterParams{ProjectID: project.ID, Types: []string{"project.author_rewritten"}, Limit: 1})
	require.NoError(t, err)
	require.Len(t, rewrites, 1)
	rootUID := "00000000000000000000000002"
	public, private, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	pin := db.RootKeyPin{ProjectUID: project.UID, AuthorityUID: rootUID, KeyID: db.RootPublicKeyID(public), PublicKey: public}
	require.NoError(t, store.PinRootAuthority(ctx, pin))
	// The pre-join work is already pushed, like the artifact reset scenario,
	// so the reset is not blocked by work awaiting its root receipt.
	pushCursor, err := store.MaxLocalOriginEventID(ctx, project.ID)
	require.NoError(t, err)
	_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example",
		HubProjectID: 42, HubProjectUID: project.UID, Actor: "example-actor", Enabled: true, PushEnabled: true,
		PushCursorEventID: pushCursor,
	})
	require.NoError(t, err)
	bindingUID, err := uid.New()
	require.NoError(t, err)
	_, err = store.SetRelayBindingConfig(ctx, project.ID, db.RelayBindingConfig{
		ProtocolVersion: db.RelayProtocolVersion, BindingUID: bindingUID, UpstreamInstanceUID: rootUID,
		AuthorityUID: rootUID, HubPath: []string{rootUID, store.InstanceUID()}, LocalActor: "example-actor", ResetEpoch: 1,
	})
	require.NoError(t, err)

	pendingKey := func(kind, entityUID string) string {
		return db.PendingCreationMetadataPrefix + project.UID + "." + kind + "." + entityUID
	}
	purgedIssueUID, err := uid.New()
	require.NoError(t, err)
	purgedCommentUID, err := uid.New()
	require.NoError(t, err)
	metadata, ok := store.(interface {
		ExecContext(context.Context, string, ...any) (sql.Result, error)
	})
	require.True(t, ok)
	for _, key := range []string{pendingKey("issue", purgedIssueUID), pendingKey("comment", purgedCommentUID)} {
		quotedKey := "'" + strings.ReplaceAll(key, "'", "''") + "'"
		_, err := metadata.ExecContext(ctx, `INSERT INTO meta(key,value) VALUES(`+quotedKey+`,'{}')`)
		require.NoError(t, err)
	}

	// Daemon-local catalog history has no root receipt and is not part of the
	// snapshot; the projection rebuild must leave it in place.
	project, _, _, err = store.RenameProjectAndEvent(ctx, project.ID, "spoke-project-renamed", "example-actor")
	require.NoError(t, err)

	keptUID, err := uid.New()
	require.NoError(t, err)
	kept := newRemoteEvent(t, project, &keptUID, "issue.snapshot", "example-actor", rootUID, 400,
		jsontext.Value(`{"uid":"`+keptUID+`","title":"Kept by the root","author":"example-actor","metadata":{},"comments":[],"labels":[],"links":[]}`))
	raw, err := db.EncodeRelaySourceEvent(kept)
	require.NoError(t, err)
	entities, err := json.Marshal([][]byte{raw})
	require.NoError(t, err)
	provenance, err := json.Marshal(db.RootResetProvenance{Keys: []db.RootKeyPin{pin}, Receipts: []db.AttributionReceipt{}, Entities: []db.EntityProvenance{}})
	require.NoError(t, err)
	snapshot := db.RootResetSnapshot{Events: []byte(`[]`), Entities: entities, Provenance: provenance, Artifacts: []byte(`[]`)}
	snapshotUID, err := uid.New()
	require.NoError(t, err)
	manifest, err := db.SignRootResetManifest(db.RootResetManifest{Version: 1, ProjectUID: project.UID, AuthorityUID: rootUID, KeyID: pin.KeyID, ResetEpoch: 2, SnapshotUID: snapshotUID}, snapshot, private)
	require.NoError(t, err)
	translation := db.RelayResetTranslation{
		Authority:   db.RelayHopAuthority{BindingUID: bindingUID, ProjectUID: project.UID, AuthorityUID: rootUID, SenderInstanceUID: rootUID, ReceiverInstanceUID: store.InstanceUID(), Epoch: 2},
		SnapshotUID: snapshotUID, SnapshotDigest: manifest.SnapshotDigest,
	}
	installer, ok := store.(db.RelayResetStore)
	require.True(t, ok)
	require.NoError(t, installer.InstallRelayReset(ctx, bindingUID, manifest, snapshot, translation))

	_, err = store.IssueByUID(ctx, keptUID, db.IncludeDeletedNo)
	require.NoError(t, err, "the snapshot entity is installed")
	keys := map[string]struct{}{}
	for record, err := range store.ExportMeta(ctx) {
		require.NoError(t, err)
		keys[record.Key] = struct{}{}
	}
	require.NotContains(t, keys, pendingKey("issue", purgedIssueUID), "an issue absent from the rebuilt projection keeps no pending row")
	require.NotContains(t, keys, pendingKey("comment", purgedCommentUID), "a comment absent from the rebuilt projection keeps no pending row")
	renamed, err := store.EventsAfter(ctx, db.EventsAfterParams{ProjectID: project.ID, Types: []string{"project.renamed"}, Limit: 1})
	require.NoError(t, err)
	require.Len(t, renamed, 1, "the reset keeps daemon-local catalog history that the snapshot never carried")
	rewrites, err = store.EventsAfter(ctx, db.EventsAfterParams{ProjectID: project.ID, Types: []string{"project.author_rewritten"}, Limit: 1})
	require.NoError(t, err)
	require.Empty(t, rewrites, "a local author rewrite does not outlive the authoritative snapshot")
}
