package daemon

import (
	"crypto/sha256"
	"encoding/json"
	"encoding/json/jsontext"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/mod/semver"
)

func TestCheckAPIVersionRecord(t *testing.T) {
	const a = `{"info":{"version":"0.45.0"},"paths":{"/a":{}}}`
	const b = `{"info":{"version":"0.45.0"},"paths":{"/b":{}}}`
	const hashA = "a796a0b24a661cae44ecfaf054e4e53ee32d80f219424fb40c319a1d036aba5f"
	const hashB = "95abbaa7a1794b458dae61670a105f01339b2978a65310dc6575f987536650c9"
	for _, tc := range []struct {
		name, doc, version string
		markers            []string
		contains           []string
	}{
		{"collision", a, "0.45.0", []string{"0.45.0." + hashA + ".sha256", "0.45.0." + hashB + ".sha256"}, []string{"0.46.0", "cmd/kata/api_compat.go", "cmd/kata/close_attachment.go", "cmd/kata/teammate.go", "internal/mcp/", "internal/tui/client.go", "internal/recurrence/recurrenceclient/"}},
		{"identical", a, "0.45.0", []string{"0.45.0." + hashA + ".sha256"}, nil},
		{"stale", b, "0.45.0", []string{"0.45.0." + hashA + ".sha256"}, []string{"0.46.0"}},
		{"next recorded", b, "0.46.0", []string{"0.45.0." + hashA + ".sha256", "0.46.0." + hashB + ".sha256"}, nil},
		{"next occupied", a, "0.45.0", []string{"0.45.0." + hashA + ".sha256", "0.45.0." + hashB + ".sha256", "0.46.0." + hashB + ".sha256"}, []string{"0.47.0"}},
		{"missing", a, "0.45.0", nil, []string{"0.45.0." + hashA + ".sha256"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			for _, name := range tc.markers {
				require.NoError(t, os.WriteFile(filepath.Join(dir, name), nil, 0600))
			}
			err := checkAPIVersionRecord(dir, tc.version, []byte(tc.doc))
			if len(tc.contains) == 0 {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			for _, s := range tc.contains {
				require.Contains(t, err.Error(), s)
			}
		})
	}
}

func TestAPISchemaVersionMatchesRecordedContract(t *testing.T) {
	doc, err := OpenAPIDocument().MarshalJSON()
	require.NoError(t, err)
	require.NoError(t, checkAPIVersionRecord("testdata/api-contracts", APISchemaVersion, doc))
}

func checkAPIVersionRecord(dir, version string, doc []byte) error {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(doc, &root); err != nil {
		return err
	}
	var info map[string]json.RawMessage
	if err := json.Unmarshal(root["info"], &info); err != nil {
		return err
	}
	info["version"] = json.RawMessage(`""`)
	var err error
	root["info"], err = json.Marshal(info)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(root)
	if err != nil {
		return err
	}
	canonical := jsontext.Value(raw)
	if err := canonical.Format(jsontext.ReorderRawObjects(true), jsontext.CanonicalizeRawInts(false), jsontext.CanonicalizeRawFloats(true)); err != nil {
		return err
	}
	digest := sha256.Sum256(canonical)
	marker := fmt.Sprintf("%s.%x.sha256", version, digest)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	count, match := 0, false
	next := "v" + version
	for _, entry := range entries {
		name := entry.Name()
		parts := strings.Split(name, ".")
		if len(parts) != 5 || parts[4] != "sha256" || len(parts[3]) != 64 {
			continue
		}
		recorded := "v" + strings.Join(parts[:3], ".")
		if !semver.IsValid(recorded) {
			continue
		}
		if semver.Compare(recorded, next) > 0 {
			next = recorded
		}
		if strings.Join(parts[:3], ".") == version {
			count++
			match = match || name == marker
		}
	}
	if count == 1 && match {
		return nil
	}
	if count == 0 {
		return fmt.Errorf("API contract has no record; add empty marker %s", filepath.Join(dir, marker))
	}
	minor, err := strconv.Atoi(strings.TrimPrefix(semver.MajorMinor(next), semver.Major(next)+"."))
	if err != nil {
		return err
	}
	suggested := fmt.Sprintf("%s.%d.0", strings.TrimPrefix(semver.Major(next), "v"), minor+1)
	return fmt.Errorf("API contract version %s has conflicting or stale records; take %s in APISchemaVersion, api/openapi.yaml, and the version-history row, and move newly introduced feature minimums in cmd/kata/api_compat.go, cmd/kata/close_attachment.go, cmd/kata/teammate.go, internal/mcp/, internal/tui/client.go, and internal/recurrence/recurrenceclient/; move this branch's marker and recompute it for the combined document", version, suggested)
}
