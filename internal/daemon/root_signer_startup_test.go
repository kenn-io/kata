package daemon_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
)

// R5: known root keys may never be silently replaced after loss or restore.
// The daemon has one local signing identity; foreign and retired public pins
// are verification history, not a reason to load their private material.
func TestRootAttributionSignerStartup(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		ctx := t.Context()
		path := filepath.Join(t.TempDir(), "keys", "root.key")
		foreign, err := store.CreateProject(ctx, "upstream-project")
		require.NoError(t, err)
		foreignPub, _, err := ed25519.GenerateKey(rand.Reader)
		require.NoError(t, err)
		require.NoError(t, store.PinRootAuthority(ctx, db.RootKeyPin{ProjectUID: foreign.UID, AuthorityUID: "00000000000000000000000002", KeyID: db.RootPublicKeyID(foreignPub), PublicKey: foreignPub}))
		first, err := daemon.LoadRootAttributionSigner(ctx, store, path)
		require.NoError(t, err)
		require.Equal(t, store.InstanceUID(), first.AuthorityUID)
		require.Len(t, first.PrivateKey, ed25519.PrivateKeySize)
		public := first.PrivateKey.Public().(ed25519.PublicKey)
		for _, name := range []string{"root-project", "other-root-project"} {
			project, err := store.CreateProject(ctx, name)
			require.NoError(t, err)
			require.NoError(t, store.PinRootAuthority(ctx, db.RootKeyPin{ProjectUID: project.UID, AuthorityUID: store.InstanceUID(), KeyID: db.RootPublicKeyID(public), PublicKey: public}))
		}
		again, err := daemon.LoadRootAttributionSigner(ctx, store, path)
		require.NoError(t, err)
		require.Equal(t, first.PrivateKey, again.PrivateKey)
		require.NoError(t, os.Remove(path))
		_, err = daemon.LoadRootAttributionSigner(ctx, store, path)
		require.Error(t, err)
		_, err = os.Stat(path)
		require.True(t, os.IsNotExist(err), "known missing key must not be recreated")
		recoveryPath := filepath.Join(t.TempDir(), "root.key")
		_, err = daemon.LoadRootAttributionSigner(ctx, store, recoveryPath)
		require.Error(t, err, "public database backup does not restore the signing secret")
		require.NoError(t, os.WriteFile(path, []byte("invalid"), 0600))
		_, err = daemon.LoadRootAttributionSigner(ctx, store, path)
		require.Error(t, err)
		conflicting, err := store.CreateProject(ctx, "conflicting-root-project")
		require.NoError(t, err)
		conflictingPub, _, err := ed25519.GenerateKey(rand.Reader)
		require.NoError(t, err)
		require.NoError(t, store.PinRootAuthority(ctx, db.RootKeyPin{ProjectUID: conflicting.UID, AuthorityUID: store.InstanceUID(), KeyID: db.RootPublicKeyID(conflictingPub), PublicKey: conflictingPub}))
		_, err = daemon.LoadRootAttributionSigner(ctx, store, path)
		require.ErrorContains(t, err, "different active signing keys")
	})
}
