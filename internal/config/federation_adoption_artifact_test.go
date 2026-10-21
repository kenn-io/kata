package config

import (
	"encoding/json/v2"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/uid"
)

const adoptionArtifactUID = "01HZNQ7VFPK1XGD8R5MABCD4EX"
const adoptionArtifactDigest = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func adoptionArtifactHome(t testing.TB) string {
	t.Helper()
	home, err := os.MkdirTemp("", "kra-adoption-*")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(home)) })
	return home
}

// The config layer preserves opaque envelope bytes; the daemon validates the
// full publication codec and its cryptographic digest before transmission.
func adoptionArtifactBytes(t testing.TB, payload []byte) []byte {
	t.Helper()
	raw, err := json.Marshal(struct {
		Digest string `json:"digest"`
		Data   []byte `json:"data"`
	}{adoptionArtifactDigest, payload})
	require.NoError(t, err)
	return raw
}

func TestFederationAdoptionEnvelopeLifecycle(t *testing.T) {
	home := adoptionArtifactHome(t)
	raw := adoptionArtifactBytes(t, []byte("retained history"))
	path, err := WriteFederationAdoptionEnvelope(home, adoptionArtifactUID, raw)
	require.NoError(t, err)
	require.Equal(t, filepath.Join(home, "federation", "adoptions", adoptionArtifactUID+".json"), path)
	for _, entry := range []struct {
		path string
		mode os.FileMode
	}{{filepath.Join(home, "federation"), 0o700}, {filepath.Dir(path), 0o700}, {path, 0o600}} {
		info, err := os.Stat(entry.path)
		require.NoError(t, err)
		if runtime.GOOS != "windows" {
			require.Equal(t, entry.mode, info.Mode().Perm())
		}
	}
	got, err := ReadFederationAdoptionEnvelope(home, adoptionArtifactUID, adoptionArtifactDigest)
	require.NoError(t, err)
	require.Equal(t, raw, got)
	entries, err := os.ReadDir(filepath.Dir(path))
	require.NoError(t, err)
	require.Len(t, entries, 1, "successful atomic writes leave only the envelope")
	require.NoError(t, RemoveFederationAdoptionEnvelope(home, adoptionArtifactUID))
	require.NoError(t, RemoveFederationAdoptionEnvelope(home, adoptionArtifactUID))
	_, err = ReadFederationAdoptionEnvelope(home, adoptionArtifactUID, adoptionArtifactDigest)
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestFederationAdoptionEnvelopeInvalidSelectors(t *testing.T) {
	for _, id := range []string{"", ".", "..", "../outside", "/tmp/outside", `..\outside`, adoptionArtifactUID + "/outside", strings.Repeat("A", 27), "81HZNQ7VFPK1XGD8R5MABCD4EX"} {
		t.Run(id, func(t *testing.T) {
			home := adoptionArtifactHome(t)
			_, err := WriteFederationAdoptionEnvelope(home, id, []byte("data"))
			require.Error(t, err)
			_, err = ReadFederationAdoptionEnvelope(home, id, adoptionArtifactDigest)
			require.Error(t, err)
			require.Error(t, RemoveFederationAdoptionEnvelope(home, id))
			entries, err := os.ReadDir(home)
			require.NoError(t, err)
			require.Empty(t, entries)
		})
	}
	_, err := WriteFederationAdoptionEnvelope("", adoptionArtifactUID, []byte("data"))
	require.Error(t, err)
}

func TestFederationAdoptionEnvelopeReadRefusesWrongDigestAndMalformedJSON(t *testing.T) {
	home := adoptionArtifactHome(t)
	for _, raw := range [][]byte{
		[]byte(`{}`), []byte(`{"digest":`), []byte(`{"digest":"wrong"}`),
		[]byte(`{"digest":"` + adoptionArtifactDigest + `","digest":"` + adoptionArtifactDigest + `"}`),
	} {
		_, err := WriteFederationAdoptionEnvelope(home, adoptionArtifactUID, raw)
		require.NoError(t, err)
		_, err = ReadFederationAdoptionEnvelope(home, adoptionArtifactUID, adoptionArtifactDigest)
		require.Error(t, err)
	}
	_, err := WriteFederationAdoptionEnvelope(home, adoptionArtifactUID, adoptionArtifactBytes(t, nil))
	require.NoError(t, err)
	for _, digest := range []string{"", "wrong", strings.Repeat("0", 64), strings.Repeat("g", 64)} {
		_, err := ReadFederationAdoptionEnvelope(home, adoptionArtifactUID, digest)
		require.Error(t, err)
	}
}

func TestFederationAdoptionEnvelopeBounds(t *testing.T) {
	home := adoptionArtifactHome(t)
	_, err := WriteFederationAdoptionEnvelope(home, adoptionArtifactUID, make([]byte, (64<<20)+1))
	require.Error(t, err)
	entries, err := os.ReadDir(home)
	require.NoError(t, err)
	require.Empty(t, entries, "oversize write must fail before creating directories")
	path, err := WriteFederationAdoptionEnvelope(home, adoptionArtifactUID, adoptionArtifactBytes(t, nil))
	require.NoError(t, err)
	require.NoError(t, os.Truncate(path, (64<<20)+1))
	_, err = ReadFederationAdoptionEnvelope(home, adoptionArtifactUID, adoptionArtifactDigest)
	require.Error(t, err)
	require.NoError(t, RemoveFederationAdoptionEnvelope(home, adoptionArtifactUID), "cleanup does not read the oversized artifact")
}

func TestFederationAdoptionEnvelopeRefusesPublicPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permission-bit contract")
	}
	home := adoptionArtifactHome(t)
	path, err := WriteFederationAdoptionEnvelope(home, adoptionArtifactUID, adoptionArtifactBytes(t, nil))
	require.NoError(t, err)
	require.NoError(t, os.Chmod(path, 0o644)) //nolint:gosec // Deliberately insecure fixture.
	_, err = ReadFederationAdoptionEnvelope(home, adoptionArtifactUID, adoptionArtifactDigest)
	require.Error(t, err, "refuse content that was exposed by an incorrectly restored mode")
	require.NoError(t, os.Chmod(path, 0o600))
	require.NoError(t, os.Chmod(filepath.Dir(path), 0o755)) //nolint:gosec // Deliberately insecure fixture.
	_, err = ReadFederationAdoptionEnvelope(home, adoptionArtifactUID, adoptionArtifactDigest)
	require.Error(t, err)
	_, err = WriteFederationAdoptionEnvelope(home, adoptionArtifactUID, adoptionArtifactBytes(t, nil))
	require.NoError(t, err, "an explicit save secures an existing directory")
	got, err := ReadFederationAdoptionEnvelope(home, adoptionArtifactUID, adoptionArtifactDigest)
	require.NoError(t, err)
	require.Equal(t, adoptionArtifactBytes(t, nil), got)
}

func TestFederationAdoptionEnvelopeRefusesLinksAndDirectories(t *testing.T) {
	for _, component := range []string{"federation", "adoptions", "file"} {
		t.Run(component, func(t *testing.T) {
			home, outside := adoptionArtifactHome(t), t.TempDir()
			path, err := WriteFederationAdoptionEnvelope(home, adoptionArtifactUID, adoptionArtifactBytes(t, nil))
			require.NoError(t, err)
			link := path
			target := filepath.Join(outside, "untouched.json")
			require.NoError(t, os.WriteFile(target, []byte("untouched"), 0o600))
			if component != "file" {
				link, target = filepath.Dir(path), outside
				if component == "federation" {
					link = filepath.Dir(link)
				}
			}
			require.NoError(t, os.RemoveAll(link))
			if err := os.Symlink(target, link); err != nil {
				t.Skipf("symlinks unavailable: %v", err)
			}
			_, err = WriteFederationAdoptionEnvelope(home, adoptionArtifactUID, []byte("replacement"))
			require.Error(t, err)
			_, err = ReadFederationAdoptionEnvelope(home, adoptionArtifactUID, adoptionArtifactDigest)
			require.Error(t, err)
			require.Error(t, RemoveFederationAdoptionEnvelope(home, adoptionArtifactUID))
			got, err := os.ReadFile(filepath.Join(outside, "untouched.json")) //nolint:gosec // outside is owned by this test's TempDir.
			require.NoError(t, err)
			require.Equal(t, "untouched", string(got))
		})
	}
	t.Run("directory target", func(t *testing.T) {
		home := adoptionArtifactHome(t)
		path, err := WriteFederationAdoptionEnvelope(home, adoptionArtifactUID, []byte("original"))
		require.NoError(t, err)
		require.NoError(t, os.Remove(path))
		require.NoError(t, os.Mkdir(path, 0o700))
		_, err = WriteFederationAdoptionEnvelope(home, adoptionArtifactUID, []byte("replacement"))
		require.Error(t, err)
		_, err = ReadFederationAdoptionEnvelope(home, adoptionArtifactUID, adoptionArtifactDigest)
		require.Error(t, err)
		require.Error(t, RemoveFederationAdoptionEnvelope(home, adoptionArtifactUID), "never recursively remove a directory")
	})
}

func TestFederationAdoptionEnvelopeMissingCleanupHasNoSideEffects(t *testing.T) {
	home := adoptionArtifactHome(t)
	require.NoError(t, RemoveFederationAdoptionEnvelope(home, adoptionArtifactUID))
	_, err := ReadFederationAdoptionEnvelope(home, adoptionArtifactUID, adoptionArtifactDigest)
	require.True(t, errors.Is(err, os.ErrNotExist))
	entries, err := os.ReadDir(home)
	require.NoError(t, err)
	require.Empty(t, entries)
}

func FuzzFederationAdoptionEnvelopeInvalidUID(f *testing.F) {
	f.Add("../other")
	f.Add("")
	f.Add("bad\x00uid")
	f.Fuzz(func(t *testing.T, operationUID string) {
		if uid.Valid(operationUID) {
			return
		}
		home := adoptionArtifactHome(t)
		_, err := WriteFederationAdoptionEnvelope(home, operationUID, []byte("data"))
		require.Error(t, err)
		_, err = ReadFederationAdoptionEnvelope(home, operationUID, adoptionArtifactDigest)
		require.Error(t, err)
		require.Error(t, RemoveFederationAdoptionEnvelope(home, operationUID))
	})
}
