package jsonl_test

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json/v2"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/jsonl"
	"go.kenn.io/kata/internal/testenv"
)

func TestStandaloneCopyCanPinItsNewRootAuthority(t *testing.T) {
	ctx := t.Context()
	source := openExportTestDB(t)
	project, err := source.CreateProject(ctx, "pinned-source-project")
	require.NoError(t, err)
	rootAuthorityUID := source.InstanceUID()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	pin := db.RootKeyPin{ProjectUID: project.UID, AuthorityUID: rootAuthorityUID, KeyID: db.RootPublicKeyID(public), PublicKey: public}
	require.NoError(t, source.PinRootAuthority(ctx, pin))
	nextPublic, nextPrivate, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	nextPin := pin
	nextPin.KeyID, nextPin.PublicKey = db.RootPublicKeyID(nextPublic), nextPublic
	transition, err := db.SignRootKeyTransition(pin, nextPin, private)
	require.NoError(t, err)
	require.NoError(t, source.RotateRootAuthority(ctx, transition))

	_, err = source.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID: project.ID, Role: db.FederationRoleHub,
		HubProjectID: project.ID, HubProjectUID: project.UID, Enabled: true,
	})
	require.NoError(t, err)
	parent, _, err := source.CreateAPIToken(ctx, db.CreateAPITokenParams{
		PlaintextToken: "standalone-relay-parent-token", Actor: "project-member", AdminActor: "admin",
	})
	require.NoError(t, err)
	_, err = source.CreateRelayEnrollment(ctx, db.CreateRelayEnrollmentParams{
		ProjectID: project.ID, ParentTokenID: parent.ID, SpokeInstanceUID: "00000000000000000000000006",
		ProtocolVersion: db.RelayProtocolVersion, Token: "standalone-relay-token",
	})
	require.NoError(t, err)
	writeCtx := db.WithRootAttribution(ctx, db.RootAttributionSigner{AuthorityUID: rootAuthorityUID, PrivateKey: nextPrivate}, "project-member")
	issue, _, err := source.CreateIssue(writeCtx, db.CreateIssueParams{ProjectID: project.ID, Title: "Pinned source issue", Author: "assistant"})
	require.NoError(t, err)
	for _, key := range []string{
		db.RelayResetMetadataPrefix + project.UID,
		db.AttributionUIResetMetadataPrefix + project.UID,
		db.PendingCreationMetadataPrefix + project.UID + ".issue." + issue.UID,
	} {
		_, err = source.ExecContext(ctx, `INSERT INTO meta(key,value) VALUES(?,?)`, key, "source-federation-state")
		require.NoError(t, err)
	}
	var input bytes.Buffer
	require.NoError(t, jsonl.Export(ctx, source, &input, jsonl.ExportOptions{IncludeDeleted: true}))
	inputRecords, err := jsonl.NewDecoder(bytes.NewReader(input.Bytes())).ReadAll(ctx)
	require.NoError(t, err)
	inputKinds := map[jsonl.Kind]bool{}
	inputMetaKeys := map[string]bool{}
	rootTransitionFound := false
	for _, record := range inputRecords {
		inputKinds[record.Kind] = true
		if record.Kind == jsonl.KindMeta {
			var metadata db.MetaKV
			require.NoError(t, json.Unmarshal(record.Data, &metadata))
			inputMetaKeys[metadata.Key] = true
			rootTransitionFound = rootTransitionFound || strings.HasPrefix(metadata.Key, db.RootKeyTransitionMetadataPrefix)
		}
	}
	for _, kind := range []jsonl.Kind{jsonl.KindRootKey, jsonl.KindEventProvenance, jsonl.KindEntityProvenance, jsonl.KindRelayOutbox} {
		require.True(t, inputKinds[kind], "source export should contain %s", kind)
	}
	require.True(t, rootTransitionFound, "source export should contain a root-key transition")
	for _, prefix := range []string{db.RelayResetMetadataPrefix, db.AttributionUIResetMetadataPrefix, db.PendingCreationMetadataPrefix} {
		found := false
		for key := range inputMetaKeys {
			found = found || strings.HasPrefix(key, prefix)
		}
		require.True(t, found, "source export should contain %s metadata", prefix)
	}

	copied := openImportTargetDB(t)
	require.NoError(t, jsonl.ImportWithOptions(ctx, bytes.NewReader(input.Bytes()), copied, jsonl.ImportOptions{AsStandalone: true}))
	_, err = copied.RootAuthority(ctx, project.UID)
	require.ErrorIs(t, err, db.ErrNotFound, "a standalone copy must not retain the source's active root authority")
	var output bytes.Buffer
	require.NoError(t, jsonl.Export(ctx, copied, &output, jsonl.ExportOptions{IncludeDeleted: true}))
	outputRecords, err := jsonl.NewDecoder(bytes.NewReader(output.Bytes())).ReadAll(ctx)
	require.NoError(t, err)
	for _, record := range outputRecords {
		assert.NotContains(t, []jsonl.Kind{
			jsonl.KindRootKey, jsonl.KindEventProvenance, jsonl.KindEntityProvenance,
			jsonl.KindRelayOutbox, jsonl.KindRelayInbox, jsonl.KindRelayCursors,
		}, record.Kind)
		if record.Kind == jsonl.KindMeta {
			var metadata db.MetaKV
			require.NoError(t, json.Unmarshal(record.Data, &metadata))
			for _, prefix := range []string{
				db.RootKeyTransitionMetadataPrefix, db.RelayResetMetadataPrefix,
				db.AttributionUIResetMetadataPrefix, db.PendingCreationMetadataPrefix,
			} {
				assert.False(t, strings.HasPrefix(metadata.Key, prefix), "standalone copy retained source metadata %q", metadata.Key)
			}
		}
	}

	newPublic, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	require.NoError(t, copied.PinRootAuthority(ctx, db.RootKeyPin{
		ProjectUID: project.UID, AuthorityUID: copied.InstanceUID(),
		KeyID: db.RootPublicKeyID(newPublic), PublicKey: newPublic,
	}), "the new owner must be able to establish its own root authority")
}

func TestStandaloneCopyPreservesContentWithoutRestoringAuthority(t *testing.T) {
	ctx := t.Context()
	source := openExportTestDB(t)
	first, err := source.CreateProject(ctx, "first-project")
	require.NoError(t, err)
	second, err := source.CreateProject(ctx, "second-project")
	require.NoError(t, err)
	archive, err := source.CreateProject(ctx, "archived-project")
	require.NoError(t, err)
	for _, p := range []db.Project{first, second} {
		_, err = source.EnableProjectFederation(ctx, p.ID, "operator")
		require.NoError(t, err)
	}
	one := createTesterIssue(ctx, t, source, first.ID, "First task", "Historical body", "label-a")
	two := createTesterIssue(ctx, t, source, second.ID, "Second task", "Another body", "label-b")
	deleted := createTesterIssue(ctx, t, source, first.ID, "Deleted task", "Retained content")
	_, _, _, err = source.SoftDeleteIssue(ctx, deleted.ID, "user-a")
	require.NoError(t, err)
	createTesterIssue(ctx, t, source, archive.ID, "Archived task", "Still archived")
	_, _, err = source.RemoveProject(ctx, db.RemoveProjectParams{ProjectID: archive.ID, Actor: "user-a", Force: true})
	require.NoError(t, err)
	addTesterComment(ctx, t, source, one.ID, "Historical comment")
	_, _, err = source.CreateLinkAndEvent(ctx, db.CreateLinkParams{
		FromIssueID: one.ID, ToIssueID: two.ID, Type: "related", Author: "user-b",
	}, db.LinkEventParams{
		EventType: "issue.linked", EventIssueID: one.ID,
		FromShortID: one.ShortID, FromUID: one.UID, ToShortID: two.ShortID, ToUID: two.UID, Actor: "user-b",
	})
	require.NoError(t, err)
	principal := db.ClaimPrincipal{HolderInstanceUID: source.InstanceUID(), Holder: "user-a", ClientKind: "cli"}
	_, err = source.AcquireClaim(ctx, db.AcquireClaimParams{
		ProjectID: first.ID, IssueRef: one.UID, Principal: principal, ClaimKind: "hard", Now: time.Now(),
	})
	require.NoError(t, err)
	_, err = source.EnqueuePendingClaim(ctx, db.PendingClaimParams{
		ProjectID: second.ID, IssueRef: two.UID, Principal: principal, ClaimKind: "hard", Now: time.Now(),
	})
	require.NoError(t, err)
	const token = "synthetic-original-api-token"
	_, _, err = source.CreateAPIToken(ctx, db.CreateAPITokenParams{
		PlaintextToken: token, Actor: "user-a", AdminActor: db.BootstrapActor,
	})
	require.NoError(t, err)
	revoked, _, err := source.CreateAPIToken(ctx, db.CreateAPITokenParams{
		PlaintextToken: "synthetic-revoked-api-token", Actor: "user-b", AdminActor: db.BootstrapActor,
	})
	require.NoError(t, err)
	_, _, err = source.RevokeAPIToken(ctx, revoked.ID, db.BootstrapActor)
	require.NoError(t, err)
	require.NoError(t, source.RecordFederationSyncPullStarted(ctx, first.ID, time.Now()))
	_, err = source.RecordFederationQuarantine(ctx, db.RecordFederationQuarantineParams{
		ProjectID: first.ID, Direction: db.FederationQuarantineDirectionPush,
		FirstEventID: 1, LastEventID: 2, EventUIDs: []string{"source-event"},
		Error: "synthetic pending batch", CreatedAt: time.Now(),
	})
	require.NoError(t, err)
	input := exportToBuffer(ctx, t, source).Bytes()

	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			var copied db.Storage
			if backend == "sqlite" {
				copied = openImportTargetDB(t)
			} else {
				if testing.Short() {
					t.Skip("requires postgres testcontainer")
				}
				dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
				t.Cleanup(cleanup)
				pg, err := pgstore.Open(ctx, dsn)
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, pg.Close()) })
				copied = pg
			}
			identity := copied.InstanceUID()
			require.NoError(t, jsonl.ImportWithOptions(ctx, bytes.NewReader(input), copied, jsonl.ImportOptions{AsStandalone: true}))
			assert.Equal(t, identity, copied.InstanceUID())
			assert.NotEqual(t, source.InstanceUID(), copied.InstanceUID())
			bindings, err := copied.ListFederationBindings(ctx)
			require.NoError(t, err)
			assert.Empty(t, bindings)
			count, err := copied.CountLiveClaims(ctx, first.ID)
			require.NoError(t, err)
			assert.Zero(t, count)
			count, err = copied.CountPendingClaims(ctx, second.ID)
			require.NoError(t, err)
			assert.Zero(t, count)
			_, err = copied.ResolveAPIToken(ctx, token)
			assert.ErrorIs(t, err, db.ErrNotFound)

			var snapshot bytes.Buffer
			require.NoError(t, jsonl.Export(ctx, copied, &snapshot, jsonl.ExportOptions{IncludeDeleted: true}))
			records, err := jsonl.NewDecoder(bytes.NewReader(snapshot.Bytes())).ReadAll(ctx)
			require.NoError(t, err)
			for _, record := range records {
				assert.NotContains(t, []jsonl.Kind{jsonl.KindFederationBinding, jsonl.KindFederationSyncStatus,
					jsonl.KindFederationQuarantine, jsonl.KindFederationEnrollment,
					jsonl.KindIssueClaim, jsonl.KindPendingClaimRequest}, record.Kind)
			}
			assert.Equal(t, standaloneContent(t, input), standaloneContent(t, snapshot.Bytes()),
				"content, cross-project links, archived/deleted rows and historical events retain their identities")
			// Removing only the token projection would let a later ordinary
			// restore silently recreate the source's token authority.
			restored := openImportTargetDB(t)
			require.NoError(t, jsonl.Import(ctx, bytes.NewReader(snapshot.Bytes()), restored))
			_, err = restored.ResolveAPIToken(ctx, token)
			assert.ErrorIs(t, err, db.ErrNotFound)
			assert.Equal(t, standaloneContent(t, input), standaloneContent(t, exportToBuffer(ctx, t, restored).Bytes()))
			// Refusing a second replay also proves the copy cannot overwrite
			// an already initialized destination through the library path.
			err = jsonl.ImportWithOptions(ctx, bytes.NewReader(input), copied, jsonl.ImportOptions{AsStandalone: true})
			require.Error(t, err)
		})
	}
	assert.Equal(t, input, exportToBuffer(ctx, t, source).Bytes(), "copying never mutates the source")
}

func standaloneContent(t *testing.T, data []byte) []string {
	t.Helper()
	records, err := jsonl.NewDecoder(bytes.NewReader(data)).ReadAll(t.Context())
	require.NoError(t, err)
	var content []string
	for _, record := range records {
		switch record.Kind {
		case jsonl.KindProject, jsonl.KindIssue, jsonl.KindComment, jsonl.KindIssueLabel,
			jsonl.KindLink, jsonl.KindRecurrence, jsonl.KindPurgeLog, jsonl.KindProjectPurgeLog:
		case jsonl.KindEvent:
			var event db.EventExport
			require.NoError(t, json.Unmarshal(record.Data, &event))
			if event.Type == "token.created" || event.Type == "token.revoked" {
				continue
			}
		default:
			continue
		}
		content = append(content, string(record.Kind)+":"+string(record.Data))
	}
	return content
}
