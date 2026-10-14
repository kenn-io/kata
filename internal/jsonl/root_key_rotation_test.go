package jsonl

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json/v2"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/testenv"
)

// R3/A5: both owner export paths retain signed rotation history across native
// backends. Invalid transition bytes are rejected before an existing target clears.
func TestRootKeyRotationBackupAndValidation(t *testing.T) {
	for _, cutover := range []bool{false, true} {
		name := "current_storage"
		if cutover {
			name = "forward_cutover"
		}
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			source, err := sqlitestore.Open(ctx, filepath.Join(t.TempDir(), "source.db"))
			require.NoError(t, err)
			t.Cleanup(func() { _ = source.Close() })
			project := seedRootKeyRotation(t, source)
			expected, err := source.RootKeyTransitions(ctx, project.UID)
			require.NoError(t, err)
			var backup bytes.Buffer
			if cutover {
				err = exportForCutover(ctx, source, &backup, ExportOptions{IncludeDeleted: true})
			} else {
				err = Export(ctx, source, &backup, ExportOptions{IncludeDeleted: true})
			}
			require.NoError(t, err)
			envelopes, err := NewDecoder(bytes.NewReader(backup.Bytes())).ReadAll(ctx)
			require.NoError(t, err)
			var corrupt bytes.Buffer
			encoder := NewEncoder(&corrupt)
			changed := false
			for _, envelope := range envelopes {
				if envelope.Kind == KindMeta {
					var metadata db.MetaKV
					require.NoError(t, json.Unmarshal(envelope.Data, &metadata))
					if strings.HasPrefix(metadata.Key, db.RootKeyTransitionMetadataPrefix) {
						var transition db.RootKeyTransition
						require.NoError(t, json.Unmarshal([]byte(metadata.Value), &transition))
						transition.Signature[0] ^= 1
						raw, err := json.Marshal(transition)
						require.NoError(t, err)
						metadata.Value = string(raw)
						envelope.Data, err = json.Marshal(metadata)
						require.NoError(t, err)
						changed = true
					}
				}
				require.NoError(t, encoder.Write(envelope))
			}
			require.True(t, changed)
			dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
			t.Cleanup(cleanup)
			postgres, err := pgstore.Open(ctx, dsn)
			require.NoError(t, err)
			t.Cleanup(func() { _ = postgres.Close() })
			sqlite, err := sqlitestore.Open(ctx, filepath.Join(t.TempDir(), "target.db"))
			require.NoError(t, err)
			t.Cleanup(func() { _ = sqlite.Close() })
			for index, target := range []db.Storage{postgres, sqlite} {
				t.Run([]string{"postgres", "sqlite"}[index], func(t *testing.T) {
					require.NoError(t, Import(ctx, bytes.NewReader(backup.Bytes()), target))
					got, err := target.RootKeyTransitions(ctx, project.UID)
					require.NoError(t, err)
					require.Equal(t, expected, got)
					sentinel, err := target.CreateProject(ctx, "keep-project")
					require.NoError(t, err)
					assert.Error(t, Import(ctx, bytes.NewReader(corrupt.Bytes()), target), "invalid signed transition must be rejected before clearing")
					retained, err := target.ProjectByName(ctx, sentinel.Name)
					require.NoError(t, err)
					require.Equal(t, sentinel, retained)
				})
			}
		})
	}
}

// seedRootKeyRotation builds a pinned root project with one issue attributed
// by the original key and one signed transition to a replacement key.
func seedRootKeyRotation(t *testing.T, store *sqlitestore.Store) db.Project {
	t.Helper()
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "rotation-project")
	require.NoError(t, err)
	_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleHub, HubProjectID: project.ID, HubProjectUID: project.UID, Enabled: true})
	require.NoError(t, err)
	public, private, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	pin := db.RootKeyPin{ProjectUID: project.UID, AuthorityUID: store.InstanceUID(), KeyID: db.RootPublicKeyID(public), PublicKey: public}
	require.NoError(t, store.PinRootAuthority(ctx, pin))
	signer := db.RootAttributionSigner{AuthorityUID: pin.AuthorityUID, PrivateKey: private}
	_, _, err = store.CreateIssue(db.WithRootAttribution(ctx, signer, "root-member"), db.CreateIssueParams{ProjectID: project.ID, Author: "source-agent", Title: "Keep historical proof"})
	require.NoError(t, err)
	nextPublic, _, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	next := db.RootKeyPin{ProjectUID: project.UID, AuthorityUID: pin.AuthorityUID, KeyID: db.RootPublicKeyID(nextPublic), PublicKey: nextPublic}
	transition, err := db.SignRootKeyTransition(pin, next, private)
	require.NoError(t, err)
	require.NoError(t, store.RotateRootAuthority(ctx, transition))
	return project
}
