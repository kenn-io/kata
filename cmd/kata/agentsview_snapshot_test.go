package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/safefileio"
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

func TestAgentsViewSnapshotIdentityRejectsMalformedAndSubstitutedEnvelopes(t *testing.T) {
	for _, raw := range []string{
		`{"v":1,"v":1,"session_id":"pi:selected","manifest":{"v":5,"native_session_id":"pi:selected","session":{"id":"pi:selected","agent":"pi"}}}`,
		`{"v":1,"session_id":"pi:other","session_id":"pi:selected","manifest":{"v":5,"native_session_id":"pi:selected","session":{"id":"pi:selected","agent":"pi"}}}`,
		`{"v":1,"session_id":"pi:selected","manifest":{"v":5,"native_session_id":"pi:selected","session":{"id":"pi:selected","agent":"other","agent":"pi"}}}`,

		`{"v":2,"session_id":"pi:selected","manifest":{"v":5,"native_session_id":"pi:selected","session":{"id":"pi:selected","agent":"pi"}}}`,
		`{"v":1,"session_id":"pi:other","manifest":{"v":5,"native_session_id":"pi:selected","session":{"id":"pi:selected","agent":"pi"}}}`,
		`{"v":1,"session_id":"pi:selected","manifest":{"v":5,"native_session_id":"pi:other","session":{"id":"pi:selected","agent":"pi"}}}`,
		`{"v":1,"session_id":"pi:selected","manifest":{"v":5,"native_session_id":"pi:selected","session":{"id":"pi:selected","agent":"codex"}}}`,
		`{"v":1,"session_id":"pi:selected","manifest":{"v":5,"native_session_id":"pi:selected","session":{"id":"pi:selected","agent":"pi"}}} {}`,
	} {
		path := filepath.Join(t.TempDir(), "snapshot.json")
		require.NoError(t, os.WriteFile(path, []byte(raw), 0o600))
		snapshot, err := captureAgentsViewSnapshot(t.Context(), path)
		require.NoError(t, err)
		require.Error(t, inspectAgentsViewSnapshot(t.Context(), snapshot, "pi", "pi:selected"))
		require.NoError(t, snapshot.Close())
	}
}

// Part2's Astra source finding: literal slash-containing JSON names must not
// alias structural identity fields, even when every member name is unique.
func TestAgentsViewSnapshotRejectsSlashKeyIdentityAliases(t *testing.T) {
	for _, raw := range []string{
		`{"v":1,"session_id":"pi:selected","manifest":{"v":5,"native_session_id":"pi:other","session":{"id":"pi:other","agent":"pi"}},"manifest/native_session_id":"pi:selected","manifest/session/id":"pi:selected"}`,
		`{"v":1,"session_id":"pi:selected","manifest":{"v":5,"native_session_id":"pi:selected","session":{"id":"pi:other","agent":"other"},"session/id":"pi:selected","session/agent":"pi"}}`,
		`{"v":1,"session_id":"pi:selected","manifest":{"v":5,"native_session_id":"pi:selected"},"manifest/session":{"id":"pi:selected","agent":"pi"}}`,
	} {
		path := filepath.Join(t.TempDir(), "snapshot.json")
		require.NoError(t, os.WriteFile(path, []byte(raw), 0o600))
		snapshot, err := captureAgentsViewSnapshot(t.Context(), path)
		require.NoError(t, err)
		require.Error(t, inspectAgentsViewSnapshot(t.Context(), snapshot, "pi", "pi:selected"))
		require.NoError(t, snapshot.Close())
	}
}

// The canonical ID includes a separator in addition to both native limits.
func TestAgentsViewSnapshotAcceptsMaximumCanonicalIdentity(t *testing.T) {
	agent := strings.Repeat("a", 32)
	native := strings.Repeat("b", 128)
	canonical, err := (agentsViewNativeIdentity{Agent: agent, NativeSessionID: native}).canonical()
	require.NoError(t, err)
	raw := fmt.Sprintf(`{"v":1,"session_id":%q,"manifest":{"v":5,"native_session_id":%q,"session":{"id":%q,"agent":%q}}}`, canonical, canonical, canonical, agent)
	path := filepath.Join(t.TempDir(), "snapshot.json")
	require.NoError(t, os.WriteFile(path, []byte(raw), 0o600))
	snapshot, err := captureAgentsViewSnapshot(t.Context(), path)
	require.NoError(t, err)
	defer snapshot.Close()
	require.NoError(t, inspectAgentsViewSnapshot(t.Context(), snapshot, agent, canonical))
}

// Outer push v1 and artifact manifest v2..7 are independent contracts from
// the published session-push codec; the normal exporter emits v5,6 or7.
func TestAgentsViewSnapshotManifestVersions(t *testing.T) {
	for version := 0; version <= 8; version++ {
		raw := fmt.Sprintf(`{"v":1,"session_id":"pi:selected","manifest":{"v":%d,"native_session_id":"pi:selected","session":{"id":"pi:selected","agent":"pi"}}}`, version)
		path := filepath.Join(t.TempDir(), "snapshot.json")
		require.NoError(t, os.WriteFile(path, []byte(raw), 0o600))
		snapshot, err := captureAgentsViewSnapshot(t.Context(), path)
		require.NoError(t, err)
		err = inspectAgentsViewSnapshot(t.Context(), snapshot, "pi", "pi:selected")
		if version >= 2 && version <= 7 {
			require.NoError(t, err)
		} else {
			require.Error(t, err)
		}
		require.NoError(t, snapshot.Close())
	}
}

type agentsViewFailingOutput struct {
	stage string
	cause error
}

func (f agentsViewFailingOutput) Write(p []byte) (int, error) {
	if f.stage == "write" {
		return 0, f.cause
	}
	return len(p), nil
}

func (f agentsViewFailingOutput) Sync() error {
	if f.stage == "sync" {
		return f.cause
	}
	return nil
}

func (f agentsViewFailingOutput) Close() error {
	if f.stage == "close" {
		return f.cause
	}
	return nil
}

func TestAgentsViewPublicationFailuresNeverExposeSelectedPath(t *testing.T) {
	for _, stage := range []string{"write", "sync", "close"} {
		cause := &os.PathError{Op: stage, Path: "private-selected-path", Err: io.ErrClosedPipe}
		err := writeAgentsViewPrivateContent(t.Context(), agentsViewFailingOutput{stage, cause}, []byte("consent"))
		require.ErrorIs(t, err, io.ErrClosedPipe)
		var human, machine bytes.Buffer
		emitHumanError(&human, err, true)
		emitJSONError(&machine, err, true)
		require.NotContains(t, human.String(), "private-selected-path")
		require.NotContains(t, machine.String(), "private-selected-path")
		require.Contains(t, human.String(), "publication failed")
	}
}
