package pgstore

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
	gitenv "go.kenn.io/kit/git/env"
)

const schemaContractDir = "../testdata/schema-contracts"

var schemaMarkerName = regexp.MustCompile(`^([0-9]{6})-([0-9a-f]{64})\.txt$`)

func schemaDigest(t *testing.T, catalog [][4]string, manifest []any, migrations []Migration) string {
	t.Helper()
	data, err := json.Marshal([]any{catalog, manifest, migrations})
	require.NoError(t, err)
	return fmt.Sprintf("%x", sha256.Sum256(data))
}

func schemaManifest() []any {
	return []any{canonicalSchemaSQL, vectorSchemaSQL, canonicalTableColumns, optionalVectorTableColumns, canonicalIndexes, optionalVectorIndexes, canonicalTriggers, canonicalFunctions, []string{canonicalColumnFingerprint, canonicalConstraintFingerprint, canonicalIndexFingerprint, vectorColumnFingerprint, vectorConstraintFingerprint, vectorIndexFingerprint, canonicalTriggerFingerprint, canonicalFunctionFingerprint, canonicalTextSearchFingerprint}}
}

func sqliteCatalog(t *testing.T) [][4]string {
	t.Helper()
	store, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "schema.db"))
	require.NoError(t, err)
	defer func() { require.NoError(t, store.Close()) }()
	rows, err := store.QueryContext(t.Context(), `SELECT type, name, tbl_name, COALESCE(sql, '') FROM sqlite_master ORDER BY type, name, tbl_name, sql`)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	var catalog [][4]string
	for rows.Next() {
		var row [4]string
		require.NoError(t, rows.Scan(&row[0], &row[1], &row[2], &row[3]))
		catalog = append(catalog, row)
	}
	require.NoError(t, rows.Err())
	return catalog
}

func markerName(version int, digest string) string {
	return fmt.Sprintf("%06d-%s.txt", version, digest)
}

func checkMarkers(dir string, version int, digest string, base []string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	baseSet := make(map[string]bool, len(base))
	for _, name := range base {
		baseSet[name] = true
	}
	seen := map[int]string{}
	highest := version
	for _, entry := range entries {
		name := entry.Name()
		m := schemaMarkerName.FindStringSubmatch(name)
		if entry.IsDir() || m == nil {
			return fmt.Errorf("invalid schema marker name %s", name)
		}
		data, err := os.ReadFile(filepath.Join(dir, name)) //nolint:gosec // G304: names come from the marker directory.
		if err != nil {
			return err
		}
		summary := strings.TrimSuffix(string(data), "\n")
		if strings.TrimSpace(summary) == "" || strings.ContainsAny(summary, "\r\n") {
			return fmt.Errorf("schema marker %s needs a one-line summary", name)
		}
		n, _ := strconv.Atoi(m[1])
		if previous := seen[n]; previous != "" {
			baseMarker := previous
			if baseSet[name] {
				baseMarker = name
			}
			if baseSet[baseMarker] {
				return fmt.Errorf("schema version %d has multiple markers %s and %s; preserve base marker %s and move to version %d: %s", n, previous, name, baseMarker, max(highest, n)+1, markerName(max(highest, n)+1, digest))
			}
			return fmt.Errorf("schema version %d has multiple markers %s and %s; keep one marker for version %d", n, previous, name, n)
		}
		seen[n] = name
		highest = max(highest, n)
	}
	for _, name := range base {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			return fmt.Errorf("restore base schema marker %s: %w", name, err)
		}
	}
	if highest > version {
		return fmt.Errorf("marker exceeds current version %d; move to version %d: %s", version, highest+1, markerName(highest+1, digest))
	}
	want := markerName(version, digest)
	existing := seen[version]
	if existing == "" {
		return fmt.Errorf("missing schema marker; add %s with a one-line summary", want)
	}
	if existing != want {
		if baseSet[existing] {
			return fmt.Errorf("preserve base marker %s and move to version %d: %s", existing, highest+1, markerName(highest+1, digest))
		}
		if base != nil {
			return fmt.Errorf("replace draft marker %s with %s and keep version %d", existing, want, version)
		}
		return fmt.Errorf("replace draft marker %s with %s and keep version %d; if it is on the base, preserve it and move to version %d: %s", existing, want, version, highest+1, markerName(highest+1, digest))
	}
	return nil
}

func baseSchemaMarkers(t *testing.T, dir, ref string, env []string) ([]string, error) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", "ls-tree", "-r", "--name-only", ref, "internal/db/testdata/schema-contracts/") //nolint:gosec // G204: the ref is supplied by the developer or CI.
	cmd.Dir, cmd.Env = dir, env
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("read schema markers at %q: %w", ref, err)
	}
	names := []string{}
	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		if line != "" {
			names = append(names, filepath.Base(line))
		}
	}
	return names, nil
}

func TestSchemaVersionContract(t *testing.T) {
	version := db.CurrentSchemaVersion()
	var migrations []Migration
	for _, m := range Migrations() {
		if m.ToVersion == version {
			migrations = append(migrations, m)
		}
	}
	digest := schemaDigest(t, sqliteCatalog(t), schemaManifest(), migrations)
	var base []string
	if ref := os.Getenv("SCHEMA_VERSION_BASE_REF"); ref != "" {
		var err error
		base, err = baseSchemaMarkers(t, "../../..", ref, nil)
		require.NoError(t, err)
	}
	require.NoError(t, checkMarkers(schemaContractDir, version, digest, base))
}

func TestSchemaVersionMarkers(t *testing.T) {
	a, b := strings.Repeat("a", 64), strings.Repeat("b", 64)
	current, replacement := markerName(50, a), markerName(50, b)
	cases := []struct {
		name  string
		files map[string]string
		base  []string
		want  string
		exact bool
	}{
		{"match", map[string]string{replacement: "Summary.\n"}, nil, "", false},
		{"missing", nil, nil, fmt.Sprintf("missing schema marker; add %s with a one-line summary", replacement), true},
		{"draft", map[string]string{current: "Summary.\n"}, []string{}, fmt.Sprintf("replace draft marker %s with %s and keep version 50", current, replacement), true},
		{"base", map[string]string{current: "Summary.\n"}, []string{current}, fmt.Sprintf("preserve base marker %s and move to version 51: %s", current, markerName(51, b)), false},
		{"deleted base", map[string]string{replacement: "Summary.\n"}, []string{current}, "restore base schema marker", false},
		{"unknown base", map[string]string{current: "Summary.\n"}, nil, fmt.Sprintf("replace draft marker %s with %s and keep version 50; if it is on the base, preserve it and move to version 51: %s", current, replacement, markerName(51, b)), false},
		{"duplicate", map[string]string{current: "One\n", replacement: "Two\n"}, []string{current}, fmt.Sprintf("schema version 50 has multiple markers %s and %s; preserve base marker %s and move to version 51: %s", current, replacement, current, markerName(51, b)), false},
		{"duplicate base second", map[string]string{current: "One\n", replacement: "Two\n"}, []string{replacement}, fmt.Sprintf("schema version 50 has multiple markers %s and %s; preserve base marker %s and move to version 51: %s", current, replacement, replacement, markerName(51, b)), false},
		{"duplicate drafts", map[string]string{current: "One\n", replacement: "Two\n"}, []string{}, "keep one marker for version 50", false},
		{"future", map[string]string{markerName(51, a): "Future\n"}, nil, fmt.Sprintf("marker exceeds current version 50; move to version 52: %s", markerName(52, b)), false},
		{"bad name", map[string]string{"bad.txt": "Summary\n"}, nil, "invalid schema marker", false},
		{"empty", map[string]string{replacement: "\n"}, nil, "one-line summary", false},
		{"multiline", map[string]string{replacement: "one\ntwo\n"}, nil, "one-line summary", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			for name, contents := range tc.files {
				require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(contents), 0600))
			}
			err := checkMarkers(dir, 50, b, tc.base)
			if tc.want == "" {
				require.NoError(t, err)
			} else if tc.exact {
				require.EqualError(t, err, tc.want)
			} else {
				require.ErrorContains(t, err, tc.want)
			}
		})
	}
}

func TestSchemaVersionDigestInputs(t *testing.T) {
	require.Contains(t, schemaManifest(), any(canonicalSchemaSQL))
	require.Contains(t, schemaManifest(), any(vectorSchemaSQL))
	catalog := [][4]string{{"table", "example", "example", "CREATE TABLE example (id TEXT)"}}
	manifest := []any{map[string]string{"example": "id"}, []string{"fingerprint"}}
	migrations := []Migration{{FromVersion: 49, ToVersion: 50, Name: "000050_example.up.sql", SQL: "CREATE TABLE example (id TEXT)"}}
	original := schemaDigest(t, catalog, manifest, migrations)
	require.NotEqual(t, schemaDigest(t, catalog, []any{"CREATE TABLE t (v INTEGER DEFAULT 1)"}, migrations), schemaDigest(t, catalog, []any{"CREATE TABLE t (v INTEGER DEFAULT 2)"}, migrations))
	require.NotEqual(t, original, schemaDigest(t, [][4]string{{"table", "example", "example", "CREATE TABLE example (id INTEGER)"}}, manifest, migrations))
	require.NotEqual(t, original, schemaDigest(t, catalog, []any{map[string]string{"example": "other"}, []string{"fingerprint"}}, migrations))
	require.NotEqual(t, original, schemaDigest(t, catalog, manifest, []Migration{{FromVersion: 49, ToVersion: 50, Name: "000050_example.up.sql", SQL: "changed"}}))
}

func TestSchemaVersionGitBase(t *testing.T) {
	repo := t.TempDir()
	env := append(gitenv.StripAll(os.Environ()), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+filepath.Join(t.TempDir(), "config"))
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), "git", args...) //nolint:gosec // G204: arguments are fixed test fixture commands.
		cmd.Dir, cmd.Env = repo, env
		output, err := cmd.CombinedOutput()
		require.NoError(t, err, string(output))
		return strings.TrimSpace(string(output))
	}
	git("init", "-q")
	markerDir := filepath.Join(repo, "internal/db/testdata/schema-contracts")
	require.NoError(t, os.MkdirAll(markerDir, 0700))
	marker := markerName(50, strings.Repeat("a", 64))
	require.NoError(t, os.WriteFile(filepath.Join(markerDir, marker), []byte("Summary\n"), 0600))
	require.NoError(t, os.WriteFile(filepath.Join(repo, "unrelated.txt"), []byte("Unrelated\n"), 0600))
	git("add", ".")
	git("-c", "user.name=Test", "-c", "user.email=test@example.com", "-c", "core.hooksPath=/dev/null", "commit", "-qm", "base")
	base, err := baseSchemaMarkers(t, repo, "HEAD", env)
	require.NoError(t, err)
	require.Equal(t, []string{marker}, base)
	_, err = baseSchemaMarkers(t, repo, "missing-ref", env)
	require.ErrorContains(t, err, `read schema markers at "missing-ref"`)
}
