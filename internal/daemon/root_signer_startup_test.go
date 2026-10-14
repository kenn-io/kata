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

// R5: known root keys may never be silently replaced after loss or restore,
// and a lost key degrades only the projects pinned to it. The daemon has one
// local signing identity; foreign and retired public pins are verification
// history, not a reason to load their private material.
func TestRootAttributionSignerStartup(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		ctx := t.Context()
		path := filepath.Join(t.TempDir(), "keys", "root.key")
		foreign, err := store.CreateProject(ctx, "upstream-project")
		require.NoError(t, err)
		foreignPub, _, err := ed25519.GenerateKey(rand.Reader)
		require.NoError(t, err)
		require.NoError(t, store.PinRootAuthority(ctx, db.RootKeyPin{ProjectUID: foreign.UID, AuthorityUID: "00000000000000000000000002", KeyID: db.RootPublicKeyID(foreignPub), PublicKey: foreignPub}))
		first, err := daemon.LoadRootSigning(ctx, store, path)
		require.NoError(t, err)
		require.Equal(t, store.InstanceUID(), first.Signer().AuthorityUID)
		require.Len(t, first.Signer().PrivateKey, ed25519.PrivateKeySize, "no local pin: the first start creates the key")
		public := first.Signer().PrivateKey.Public().(ed25519.PublicKey)
		for _, name := range []string{"root-project", "other-root-project"} {
			project, err := store.CreateProject(ctx, name)
			require.NoError(t, err)
			require.NoError(t, store.PinRootAuthority(ctx, db.RootKeyPin{ProjectUID: project.UID, AuthorityUID: store.InstanceUID(), KeyID: db.RootPublicKeyID(public), PublicKey: public}))
		}
		again, err := daemon.LoadRootSigning(ctx, store, path)
		require.NoError(t, err)
		require.Equal(t, first.Signer().PrivateKey, again.Signer().PrivateKey)
		health, err := again.Health(ctx, store)
		require.NoError(t, err)
		require.Nil(t, health, "a matching key reports no problem")

		require.NoError(t, os.Remove(path))
		missing, err := daemon.LoadRootSigning(ctx, store, path)
		require.NoError(t, err, "a missing pinned key must not stop the daemon")
		require.Empty(t, missing.Signer().PrivateKey)
		_, err = os.Stat(path)
		require.True(t, os.IsNotExist(err), "known missing key must not be recreated")
		health, err = missing.Health(ctx, store)
		require.NoError(t, err)
		require.NotNil(t, health)
		require.False(t, health.KeyLoaded)
		require.Equal(t, "key file is missing", health.KeyProblem)
		require.Equal(t, path, health.KeyFile)
		require.Equal(t, []string{"other-root-project", "root-project"}, health.Projects)

		require.NoError(t, os.WriteFile(path, []byte("invalid"), 0o600))
		invalid, err := daemon.LoadRootSigning(ctx, store, path)
		require.NoError(t, err)
		health, err = invalid.Health(ctx, store)
		require.NoError(t, err)
		require.Equal(t, "invalid root signing key encoding", health.KeyProblem)
		require.Equal(t, []string{"other-root-project", "root-project"}, health.Projects)

		_, replacement, err := ed25519.GenerateKey(rand.Reader)
		require.NoError(t, err)
		require.NoError(t, os.Remove(path))
		require.NoError(t, os.WriteFile(path, []byte(encodeRootKeySeed(replacement)), 0o600))
		conflicting, err := store.CreateProject(ctx, "conflicting-root-project")
		require.NoError(t, err)
		replacementPub := replacement.Public().(ed25519.PublicKey)
		require.NoError(t, store.PinRootAuthority(ctx, db.RootKeyPin{ProjectUID: conflicting.UID, AuthorityUID: store.InstanceUID(), KeyID: db.RootPublicKeyID(replacementPub), PublicKey: replacementPub}))
		mixed, err := daemon.LoadRootSigning(ctx, store, path)
		require.NoError(t, err, "pins with different keys degrade instead of failing startup")
		require.Equal(t, replacement, mixed.Signer().PrivateKey)
		health, err = mixed.Health(ctx, store)
		require.NoError(t, err)
		require.True(t, health.KeyLoaded)
		require.Equal(t, []string{"other-root-project", "root-project"}, health.Projects, "only projects pinned to another key are unavailable")
	})
}
