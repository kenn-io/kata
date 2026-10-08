package jsonl_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json/jsontext"
	"fmt"
	"iter"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/jsonl"
	"go.kenn.io/kata/internal/testenv"
	"go.kenn.io/kata/internal/uid"
)

type relaySnapshotter interface {
	BeginExportSnapshot(context.Context) (db.Storage, func() error, error)
}

type acceptDuringRelayExportStore struct {
	db.Storage
	snapshotter      relaySnapshotter
	beforeRelayState func(context.Context) error
}

func (s *acceptDuringRelayExportStore) BeginExportSnapshot(ctx context.Context) (db.Storage, func() error, error) {
	snapshot, release, err := s.snapshotter.BeginExportSnapshot(ctx)
	if err != nil {
		return nil, nil, err
	}
	return &acceptDuringRelayExportStore{Storage: snapshot, beforeRelayState: s.beforeRelayState}, release, nil
}

func (s *acceptDuringRelayExportStore) ExportRelayState(ctx context.Context) iter.Seq2[db.ImportRecord, error] {
	return func(yield func(db.ImportRecord, error) bool) {
		if s.beforeRelayState != nil {
			before := s.beforeRelayState
			s.beforeRelayState = nil
			if err := before(ctx); err != nil {
				yield(nil, err)
				return
			}
		}
		for record, err := range s.Storage.ExportRelayState(ctx) {
			if !yield(record, err) {
				return
			}
		}
	}
}

func (s *acceptDuringRelayExportStore) ExportEmbeddingArtifacts(
	ctx context.Context, filter db.ExportFilter, issueUIDs ...string,
) iter.Seq2[db.ImportRecord, error] {
	return s.Storage.(db.EmbeddingArtifactExporter).ExportEmbeddingArtifacts(ctx, filter, issueUIDs...)
}

// A relay acceptance committed after the export snapshot must not put its
// inbox cursor into a backup whose event and issue projections predate it.
func TestOwnerBackupKeepsConcurrentRelayIngressOutsideItsSnapshot(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			ctx := t.Context()
			var source db.Storage
			if backend == "sqlite" {
				store, err := sqlitestore.Open(ctx, filepath.Join(t.TempDir(), "source.db"))
				require.NoError(t, err)
				t.Cleanup(func() { _ = store.Close() })
				source = store
			} else {
				dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
				t.Cleanup(cleanup)
				store, err := pgstore.Open(ctx, dsn)
				require.NoError(t, err)
				t.Cleanup(func() { _ = store.Close() })
				source = store
			}

			project, err := source.CreateProject(ctx, "backup-project")
			require.NoError(t, err)
			_, err = source.UpsertFederationBinding(ctx, db.FederationBinding{
				ProjectID: project.ID, Role: db.FederationRoleHub,
				HubProjectID: project.ID, HubProjectUID: project.UID, Enabled: true,
			})
			require.NoError(t, err)
			publicKey, privateKey, err := ed25519.GenerateKey(nil)
			require.NoError(t, err)
			require.NoError(t, source.PinRootAuthority(ctx, db.RootKeyPin{
				ProjectUID: project.UID, AuthorityUID: source.InstanceUID(),
				KeyID: db.RootPublicKeyID(publicKey), PublicKey: publicKey,
			}))
			parent, _, err := source.CreateAPIToken(ctx, db.CreateAPITokenParams{
				PlaintextToken: "snapshot-parent-token", Actor: "member", AdminActor: "admin",
			})
			require.NoError(t, err)
			grant, err := source.CreateRelayEnrollment(ctx, db.CreateRelayEnrollmentParams{
				ProjectID: project.ID, ParentTokenID: parent.ID,
				SpokeInstanceUID: "00000000000000000000000006",
				ProtocolVersion: db.RelayProtocolVersion, Token: "snapshot-relay-token", ServeDownstream: true,
			})
			require.NoError(t, err)

			issueUID, err := uid.New()
			require.NoError(t, err)
			eventUID, err := uid.New()
			require.NoError(t, err)
			createdAt := time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC)
			payload := jsontext.Value(fmt.Sprintf(`{"uid":%q,"title":"Concurrent relay issue","body":"body","author":"source-assistant","status":"open","metadata":{},"created_at":"%s"}`, issueUID, createdAt.Format(db.EventTimestampFormat)))
			event := db.RemoteEvent{
				EventUID: eventUID, OriginInstanceUID: "00000000000000000000000007",
				ProjectUID: project.UID, ProjectName: project.Name, IssueUID: &issueUID,
				Type: "issue.created", Actor: "source-assistant", HLCPhysicalMS: createdAt.UnixMilli(),
				HLCCounter: 1, Payload: payload, CreatedAt: createdAt,
			}
			event.ContentHash, err = db.EventContentHash(db.EventHashInput{
				UID: event.EventUID, OriginInstanceUID: event.OriginInstanceUID,
				ProjectUID: event.ProjectUID, ProjectName: event.ProjectName, IssueUID: event.IssueUID,
				Type: event.Type, Actor: event.Actor, HLCPhysicalMS: event.HLCPhysicalMS,
				HLCCounter: event.HLCCounter, CreatedAt: event.CreatedAt.Format(db.EventTimestampFormat),
				Payload: event.Payload,
			})
			require.NoError(t, err)
			body, err := db.EncodeRelaySourceEvent(event)
			require.NoError(t, err)
			envelope, err := db.SealRelayEnvelope(db.RelayEnvelope{
				Version: db.RelayProtocolVersion, BindingUID: grant.Enrollment.RelayBindingUID,
				ProjectUID: project.UID, AuthorityUID: source.InstanceUID(),
				SenderInstanceUID: grant.Enrollment.SpokeInstanceUID, ReceiverInstanceUID: source.InstanceUID(),
				Epoch: grant.Enrollment.RelayResetEpoch, Sequence: 1, Stream: db.RelayStreamEvent,
				Path: []string{event.OriginInstanceUID, grant.Enrollment.SpokeInstanceUID},
				SourceUID: event.EventUID, SourceHash: event.ContentHash, Body: body,
			})
			require.NoError(t, err)
			batch := db.RelayBatch{Stream: db.RelayStreamEvent, Envelopes: []db.RelayEnvelope{envelope}}
			writeCtx := db.WithRootAttribution(ctx, db.RootAttributionSigner{
				AuthorityUID: source.InstanceUID(), PrivateKey: privateKey,
			}, "member")
			accept := func(ctx context.Context) error {
				accepted, err := source.AcceptRelayDeliveries(writeCtx, grant.Enrollment.RelayBindingUID, batch)
				require.NoError(t, err)
				require.Len(t, accepted.InsertedEvents, 1)
				return nil
			}
			snapshotter, ok := source.(relaySnapshotter)
			require.True(t, ok)
			wrapped := &acceptDuringRelayExportStore{Storage: source, snapshotter: snapshotter, beforeRelayState: accept}
			var backup bytes.Buffer
			require.NoError(t, jsonl.Export(ctx, wrapped, &backup, jsonl.ExportOptions{IncludeDeleted: true}))

			target, err := sqlitestore.Open(ctx, filepath.Join(t.TempDir(), "restored.db"))
			require.NoError(t, err)
			t.Cleanup(func() { _ = target.Close() })
			require.NoError(t, jsonl.Import(ctx, bytes.NewReader(backup.Bytes()), target))
			replayed, err := target.AcceptRelayDeliveries(writeCtx, grant.Enrollment.RelayBindingUID, batch)
			require.NoError(t, err)
			require.Len(t, replayed.InsertedEvents, 1,
				"restored acceptance must be retryable when its event was not part of the backup snapshot")
			restored, err := target.IssueByUID(ctx, issueUID, db.IncludeDeletedYes)
			require.NoError(t, err)
			require.Equal(t, "Concurrent relay issue", restored.Title)
		})
	}
}
