package federation_test

import (
	"context"
	"crypto/ed25519"
	"database/sql"
	"iter"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/dbtest"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/embedding"
	"go.kenn.io/kata/internal/testenv"
)

func TestRelayPreparedArtifactBackupRoundTrip(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			root := newRelayMatrixNode(t, backend, "company-member")
			personal := newRelayMatrixNode(t, backend, "personal-member")
			leaf := newRelayMatrixNode(t, backend, "leaf-member")
			project, err := root.store.CreateProject(t.Context(), "shared-project")
			require.NoError(t, err)
			root.project = project
			_, err = root.store.UpsertFederationBinding(t.Context(), db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleHub, HubProjectID: project.ID, HubProjectUID: project.UID, Enabled: true})
			require.NoError(t, err)
			public := root.signer.PrivateKey.Public().(ed25519.PublicKey)
			require.NoError(t, root.store.PinRootAuthority(t.Context(), db.RootKeyPin{ProjectUID: project.UID, AuthorityUID: root.store.InstanceUID(), KeyID: db.RootPublicKeyID(public), PublicKey: public}))
			issue, _, err := root.store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: project.ID, Author: "source-agent", Title: "Retained artifact source"})
			require.NoError(t, err)
			identity := embedding.ArtifactIdentity{ProjectUID: project.UID, IssueUID: issue.UID, ProducerInstanceUID: root.store.InstanceUID(), Provider: "openai-compatible", Model: "example-model", Dimensions: 2, InputType: "none", Normalization: "none", Preprocessing: "kata.issue/v2", RecipeVersion: 2, SplitMaxRunes: 2000, SplitOverlap: 200, RecipeFingerprint: strings.Repeat("a", 64)}
			artifact, err := embedding.NewArtifact(identity, embedding.EmbedText(issue.Title, issue.Body), [][]float32{{1, 0}})
			require.NoError(t, err)
			_, err = root.store.(db.EmbeddingArtifactStorage).RetainEmbeddingArtifact(t.Context(), artifact)
			require.NoError(t, err)
			enrollRelayMatrixReplica(t, root, personal, "personal-alias", true)
			syncRelayMatrixNode(t, personal)
			binding, err := personal.store.FederationBindingByProject(t.Context(), personal.project.ID)
			require.NoError(t, err)
			creator := root.store.(interface {
				CreateRelayReset(context.Context, string, db.RootAttributionSigner) (db.RelayResetCheckpoint, error)
			})
			_, err = creator.CreateRelayReset(t.Context(), binding.RelayConfig.BindingUID, root.signer)
			require.NoError(t, err)
			executor := root.store.(interface {
				ExecContext(context.Context, string, ...any) (sql.Result, error)
			})
			query := "DELETE FROM events WHERE project_id=?"
			if backend == "postgres" {
				query = "DELETE FROM events WHERE project_id=$1"
			}
			_, err = executor.ExecContext(t.Context(), query, project.ID)
			require.NoError(t, err)
			syncRelayMatrixNode(t, personal)
			enrollRelayMatrixReplica(t, personal, leaf, "leaf-alias", false)
			syncRelayMatrixNode(t, leaf)
			leafBinding, err := leaf.store.FederationBindingByProject(t.Context(), leaf.project.ID)
			require.NoError(t, err)
			forwarder := personal.store.(interface {
				CreateRelayReset(context.Context, string, db.RootAttributionSigner) (db.RelayResetCheckpoint, error)
			})
			forwarded, err := forwarder.CreateRelayReset(t.Context(), leafBinding.RelayConfig.BindingUID, db.RootAttributionSigner{})
			require.NoError(t, err)
			preparedEpoch := forwarded.Translation.Authority.Epoch
			personalEnrollments, err := personal.store.ListProjectFederationEnrollments(t.Context(), personal.project.ID)
			require.NoError(t, err)
			var preparedGrant *db.FederationEnrollment
			for i := range personalEnrollments {
				if personalEnrollments[i].RelayBindingUID == leafBinding.RelayConfig.BindingUID {
					preparedGrant = &personalEnrollments[i]
					break
				}
			}
			require.NotNil(t, preparedGrant)
			require.Equal(t, preparedGrant.RelayResetEpoch+1, preparedEpoch)

			records := collectPreparedRelayBackupRecords(t, personal.store)
			var preparedArtifacts []db.RelayOutboxExport
			for _, record := range records {
				outbox, ok := record.(*db.RelayOutboxExport)
				if ok && outbox.BindingUID == leafBinding.RelayConfig.BindingUID && outbox.Stream == db.RelayStreamArtifact && outbox.ResetEpoch == preparedEpoch && !outbox.Acknowledged {
					preparedArtifacts = append(preparedArtifacts, *outbox)
				}
			}
			require.Len(t, preparedArtifacts, 1, "the backup carries the prepared artifact delivery at N+1")

			restored := newRelayBackupRestoreStore(t, backend)
			require.NoError(t, restored.ImportReplay(t.Context(), records, db.ImportOptions{}), "a full backup must restore while reset activation is still deferred")
			restoredEnrollments, err := restored.ListProjectFederationEnrollments(t.Context(), personal.project.ID)
			require.NoError(t, err)
			var restoredGrant *db.FederationEnrollment
			for i := range restoredEnrollments {
				if restoredEnrollments[i].RelayBindingUID == leafBinding.RelayConfig.BindingUID {
					restoredGrant = &restoredEnrollments[i]
					break
				}
			}
			require.NotNil(t, restoredGrant)
			require.Equal(t, preparedGrant.RelayResetEpoch, restoredGrant.RelayResetEpoch,
				"restore must retain the child at epoch N until an authenticated N+1 request activates the checkpoint")
			restoredOutbox := collectRelayOutbox(t, restored)
			require.Contains(t, restoredOutbox, preparedArtifacts[0])
		})
	}
}

func collectPreparedRelayBackupRecords(t *testing.T, store db.Storage) []db.ImportRecord {
	t.Helper()
	records, err := dbtest.CollectImportRecords(t.Context(), store, db.ExportFilter{IncludeDeleted: true})
	require.NoError(t, err)
	appendRecords := func(stream iter.Seq2[db.ImportRecord, error]) {
		t.Helper()
		for record, err := range stream {
			require.NoError(t, err)
			records = append(records, record)
		}
	}
	appendRecords(store.ExportProjectAccess(t.Context()))
	appendRecords(store.ExportAttribution(t.Context(), db.ExportFilter{IncludeDeleted: true}))
	appendRecords(store.ExportRelayState(t.Context()))
	artifacts := store.(db.EmbeddingArtifactExporter)
	appendRecords(artifacts.ExportEmbeddingArtifacts(t.Context(), db.ExportFilter{IncludeDeleted: true}))
	return records
}

func collectRelayOutbox(t *testing.T, store db.Storage) []db.RelayOutboxExport {
	t.Helper()
	var outbox []db.RelayOutboxExport
	for record, err := range store.ExportRelayState(t.Context()) {
		require.NoError(t, err)
		if item, ok := record.(*db.RelayOutboxExport); ok {
			outbox = append(outbox, *item)
		}
	}
	return outbox
}

func newRelayBackupRestoreStore(t *testing.T, backend string) db.Storage {
	t.Helper()
	var store db.Storage
	if backend == "sqlite" {
		opened, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "restore.db"))
		require.NoError(t, err)
		store = opened
	} else {
		dsn, cleanup := testenv.NewPostgresContainer(t, t.Context())
		t.Cleanup(cleanup)
		opened, err := pgstore.Open(t.Context(), dsn)
		require.NoError(t, err)
		store = opened
	}
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	return store
}
