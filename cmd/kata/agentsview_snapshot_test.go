package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/safefileio"
	"hegel.dev/go/hegel"
)

// Contract: retain exact reviewed bytes, open once, never re-export at retry.
func TestAgentsViewFrozenSnapshotRetainsOpenedBytes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "snapshot.json")
	original := []byte(`{"v":1,"session_id":"pi:example"}`)
	require.NoError(t, os.WriteFile(path, original, 0o600))
	sum := sha256.Sum256(original)
	frozen, err := freezeAgentsViewSnapshot(t.Context(), path, hex.EncodeToString(sum[:]), int64(len(original)))
	require.NoError(t, err)
	spool := frozen.Name()
	defer frozen.Close()
	require.NoError(t, safefileio.ValidatePrivateCurrentUserFile(frozen.File))
	require.NoError(t, os.WriteFile(path, []byte("replacement"), 0o600))
	got, err := io.ReadAll(frozen)
	require.NoError(t, err)
	require.True(t, bytes.Equal(original, got))
	require.NoError(t, frozen.Close())
	_, err = os.Stat(spool)
	require.True(t, os.IsNotExist(err))
	require.FileExists(t, path)
}

func TestAgentsViewFrozenSnapshotRefusesChangedMissingAndOversized(t *testing.T) {
	path := filepath.Join(t.TempDir(), "snapshot.json")
	require.NoError(t, os.WriteFile(path, []byte("selected"), 0o600))
	sum := sha256.Sum256([]byte("selected"))
	for _, tc := range []struct {
		path, digest string
		size         int64
	}{
		{path, "", 8},
		{path, hex.EncodeToString(sum[:]), 7},
		{path, hex.EncodeToString(sum[:]), agentsViewSnapshotLimit + 1},
		{path + ".absent", hex.EncodeToString(sum[:]), 8},
		{path, string(bytes.Repeat([]byte("a"), 64)), 8},
	} {
		frozen, err := freezeAgentsViewSnapshot(t.Context(), tc.path, tc.digest, tc.size)
		require.Error(t, err)
		require.Nil(t, frozen)
	}
}

func TestAgentsViewConsentExclusivePrivateFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "consent.json")
	require.NoError(t, writeAgentsViewConsent(t.Context(), path, []byte(`{"v":1}`)))
	require.Error(t, writeAgentsViewConsent(t.Context(), path, []byte("replacement")))
	file, err := safefileio.OpenCurrentUserFile(path)
	require.NoError(t, err)
	require.NoError(t, safefileio.ValidatePrivateCurrentUserFile(file))
	require.NoError(t, file.Close())
	got, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, `{"v":1}`, string(got))
}

// Contract: every confirmed byte change must fail before a close or upload.
func TestAgentsViewFrozenSnapshotRejectsEveryChangedByte(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		text := hegel.Draw(ht, hegel.Text())
		original := []byte(text)
		if len(original) == 0 {
			original = []byte{0}
		}
		index := hegel.Draw(ht, hegel.Integers(0, len(original)-1))
		sum := sha256.Sum256(original)
		changed := bytes.Clone(original)
		changed[index] ^= 1
		path := filepath.Join(t.TempDir(), "snapshot.json")
		require.NoError(ht, os.WriteFile(path, changed, 0o600))
		frozen, err := freezeAgentsViewSnapshot(t.Context(), path, hex.EncodeToString(sum[:]), int64(len(original)))
		require.Error(ht, err)
		require.Nil(ht, frozen)
	}, hegel.WithTestCases(100))
}

func TestAgentsViewSnapshotIdentityRejectsMalformedAndSubstitutedEnvelopes(t *testing.T) {
	for _, raw := range []string{
		`{"v":1,"v":1,"session_id":"pi:selected"}`,
		`{"v":1,"session_id":"pi:other","session_id":"pi:selected","manifest":{"v":1,"native_session_id":"pi:selected","session":{"id":"pi:selected","agent":"pi"}}}`,
		`{"v":1,"session_id":"pi:selected","manifest":{"v":1,"native_session_id":"pi:selected","session":{"id":"pi:selected","agent":"other","agent":"pi"}}}`,

		`{"v":2,"session_id":"pi:selected"}`,
		`{"v":1,"session_id":"pi:other","manifest":{"v":1,"native_session_id":"pi:selected","session":{"id":"pi:selected","agent":"pi"}}}`,
		`{"v":1,"session_id":"pi:selected","manifest":{"v":1,"native_session_id":"pi:other","session":{"id":"pi:selected","agent":"pi"}}}`,
		`{"v":1,"session_id":"pi:selected","manifest":{"v":1,"native_session_id":"pi:selected","session":{"id":"pi:selected","agent":"codex"}}}`,
		`{"v":1,"session_id":"pi:selected","manifest":{"v":1,"native_session_id":"pi:selected","session":{"id":"pi:selected","agent":"pi"}}} {}`,
	} {
		path := filepath.Join(t.TempDir(), "snapshot.json")
		require.NoError(t, os.WriteFile(path, []byte(raw), 0o600))
		snapshot, err := captureAgentsViewSnapshot(t.Context(), path)
		require.NoError(t, err)
		require.Error(t, inspectAgentsViewSnapshot(t.Context(), snapshot, "pi", "pi:selected"))
		require.NoError(t, snapshot.Close())
	}
}
