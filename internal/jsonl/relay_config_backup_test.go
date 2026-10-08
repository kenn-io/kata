package jsonl

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json/v2"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/testenv"
)

// R3/R4/A9: owner backups preserve negotiated upstream configuration and the
// same retained retry bytes. Project-scoped exports omit hub-local relay state.
func TestRelayConfigurationOwnerBackup(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(map[bool]string{false: "native", true: "forward_cutover"}[legacy], func(t *testing.T) {
			ctx := t.Context()
			source, err := sqlitestore.Open(ctx, filepath.Join(t.TempDir(), "source.db"))
			require.NoError(t, err)
			t.Cleanup(func() { _ = source.Close() })
			project, err := source.CreateProject(ctx, "shared-project")
			require.NoError(t, err)
			_, err = source.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: 42, HubProjectUID: project.UID, Actor: "company-member", PushEnabled: true, Enabled: true})
			require.NoError(t, err)
			rootUID := "00000000000000000000000002"
			public, _, err := ed25519.GenerateKey(nil)
			require.NoError(t, err)
			require.NoError(t, source.PinRootAuthority(ctx, db.RootKeyPin{ProjectUID: project.UID, AuthorityUID: rootUID, KeyID: db.RootPublicKeyID(public), PublicKey: public}))
			config := db.RelayBindingConfig{ProtocolVersion: db.RelayProtocolVersion, BindingUID: "00000000000000000000000005", UpstreamInstanceUID: rootUID, AuthorityUID: rootUID, HubPath: []string{rootUID, source.InstanceUID()}, LocalActor: "personal-member", ServeDownstream: true, ResetEpoch: 1}
			_, err = source.SetRelayBindingConfig(ctx, project.ID, config)
			require.NoError(t, err)
			_, _, err = source.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Retained upstream delivery", Author: "assistant"})
			require.NoError(t, err)
			pending, err := source.PendingRelayDeliveries(ctx, config.BindingUID, db.RelayStreamEvent, 10)
			require.NoError(t, err)
			require.Len(t, pending, 1)
			var backup bytes.Buffer
			if legacy {
				err = exportSnapshot(ctx, source, &backup, ExportOptions{})
			} else {
				err = Export(ctx, source, &backup, ExportOptions{})
			}
			require.NoError(t, err)
			dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
			t.Cleanup(cleanup)
			postgres, err := pgstore.Open(ctx, dsn)
			require.NoError(t, err)
			t.Cleanup(func() { _ = postgres.Close() })
			require.NoError(t, Import(ctx, bytes.NewReader(backup.Bytes()), postgres))
			check := func(store db.Storage) {
				binding, err := store.FederationBindingByProject(ctx, project.ID)
				require.NoError(t, err)
				require.Equal(t, &config, binding.RelayConfig)
				retry, err := store.PendingRelayDeliveries(ctx, config.BindingUID, db.RelayStreamEvent, 10)
				require.NoError(t, err)
				require.Equal(t, pending, retry)
			}
			check(postgres)
			var forwarded bytes.Buffer
			require.NoError(t, Export(ctx, postgres, &forwarded, ExportOptions{}))
			restored, err := sqlitestore.Open(ctx, filepath.Join(t.TempDir(), "restored.db"))
			require.NoError(t, err)
			t.Cleanup(func() { _ = restored.Close() })
			require.NoError(t, Import(ctx, bytes.NewReader(forwarded.Bytes()), restored))
			check(restored)
			for changeIndex, change := range []func(*db.RelayBindingConfig){
				func(c *db.RelayBindingConfig) { c.HubPath = []string{rootUID, rootUID, source.InstanceUID()} },
				func(c *db.RelayBindingConfig) {
					c.AuthorityUID = "00000000000000000000000008"
					c.HubPath[0] = c.AuthorityUID
				},
				func(c *db.RelayBindingConfig) { c.BindingUID = "00000000000000000000000009" },
			} {
				records, err := NewDecoder(bytes.NewReader(backup.Bytes())).ReadAll(ctx)
				require.NoError(t, err)
				var corrupt bytes.Buffer
				encoder := NewEncoder(&corrupt)
				for _, record := range records {
					if record.Kind == KindFederationBinding {
						var b db.FederationBindingExport
						require.NoError(t, json.Unmarshal(record.Data, &b))
						change(b.RelayConfig)
						record.Data, err = json.Marshal(b)
						require.NoError(t, err)
					}
					require.NoError(t, encoder.Write(record))
				}
				if changeIndex == 2 {
					// A reconnect can retain the old relay namespace as detached
					// history while installing a new active binding identity.
					var detachedEnvelopeBefore string
					require.NoError(t, restored.QueryRowContext(ctx, `SELECT envelope FROM federation_relay_outbox WHERE binding_uid=? AND stream=? AND reset_epoch=?`, config.BindingUID, db.RelayStreamEvent, config.ResetEpoch).Scan(&detachedEnvelopeBefore))
					require.NoError(t, Import(ctx, bytes.NewReader(corrupt.Bytes()), restored))
					binding, err := restored.FederationBindingByProject(ctx, project.ID)
					require.NoError(t, err)
					detached := config
					detached.BindingUID = "00000000000000000000000009"
					require.Equal(t, &detached, binding.RelayConfig)
					var detachedEnvelopeAfter string
					require.NoError(t, restored.QueryRowContext(ctx, `SELECT envelope FROM federation_relay_outbox WHERE binding_uid=? AND stream=? AND reset_epoch=?`, config.BindingUID, db.RelayStreamEvent, config.ResetEpoch).Scan(&detachedEnvelopeAfter))
					require.Equal(t, detachedEnvelopeBefore, detachedEnvelopeAfter, "detached retry bytes remain under their original identity")
					continue
				}
				require.Error(t, Import(ctx, bytes.NewReader(corrupt.Bytes()), restored), "invalid relay restore %d must fail before clearing retained state", changeIndex)
				check(restored)
			}
			var scoped bytes.Buffer
			if legacy {
				err = exportSnapshot(ctx, source, &scoped, ExportOptions{ProjectID: project.ID})
			} else {
				err = Export(ctx, source, &scoped, ExportOptions{ProjectID: project.ID})
			}
			require.NoError(t, err)
			records, err := NewDecoder(bytes.NewReader(scoped.Bytes())).ReadAll(ctx)
			require.NoError(t, err)
			for _, record := range records {
				if record.Kind == KindFederationBinding {
					var b db.FederationBindingExport
					require.NoError(t, json.Unmarshal(record.Data, &b))
					require.Nil(t, b.RelayConfig)
				}
			}
		})
	}
}
