package release

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildSourceArchive_TaggedReleaseUsesExactTagAndProductionAssets(t *testing.T) {
	repo := newSourceFixture(t, "release tree\n", "production-assets")
	out := filepath.Join(t.TempDir(), "kata_0.14.2_source.tar.gz")

	meta, err := BuildSourceArchive(context.Background(), SourceArchiveOptions{
		RepoRoot: repo, Version: "0.14.2", Tag: "v0.14.2", Output: out,
	})

	require.NoError(t, err)
	assert.Equal(t, "0.14.2", meta.Version)
	assert.Equal(t, "v0.14.2", meta.Tag)
	assert.False(t, meta.Snapshot)
	entries := readSourceArchive(t, out)
	assert.Equal(t, "release tree\n", string(entries["kata-0.14.2/README.md"].body))
	asset := string(entries["kata-0.14.2/internal/web/dist/index.html"].body)
	assert.Contains(t, asset, "production-assets")
	assert.NotContains(t, asset, "compilation stub")
	for name, entry := range entries {
		assert.True(t, strings.HasPrefix(name, "kata-0.14.2/"), name)
		assert.Zero(t, entry.uid, name)
		assert.Zero(t, entry.gid, name)
		assert.Equal(t, meta.BuildDate, entry.modTime, name)
	}
}

func TestBuildSourceArchive_SnapshotUsesHEADAndSnapshotVersion(t *testing.T) {
	repo := newSourceFixture(t, "snapshot tree\n", "production-assets")
	require.NoError(t, os.WriteFile(filepath.Join(repo, "README.md"), []byte("snapshot head\n"), 0o600))
	runGit(t, repo, "add", "README.md")
	runGitEnv(t, repo, []string{"GIT_AUTHOR_DATE=2026-08-08T01:02:04Z", "GIT_COMMITTER_DATE=2026-08-08T01:02:04Z"}, "commit", "-m", "snapshot head")
	out := filepath.Join(t.TempDir(), "snapshot.tar.gz")

	meta, err := BuildSourceArchive(context.Background(), SourceArchiveOptions{
		RepoRoot: repo, Version: "0.14.2-SNAPSHOT", Snapshot: true, Output: out,
	})

	require.NoError(t, err)
	assert.True(t, meta.Snapshot)
	assert.Empty(t, meta.Tag)
	entries := readSourceArchive(t, out)
	assert.Equal(t, "snapshot head\n", string(entries["kata-0.14.2-SNAPSHOT/README.md"].body))
}

func TestBuildSourceArchive_RealReleaseRejectsMissingOrMismatchedTag(t *testing.T) {
	repo := newSourceFixture(t, "release tree\n", "production-assets")
	for _, tc := range []struct {
		name string
		tag  string
	}{
		{name: "missing", tag: ""},
		{name: "mismatched", tag: "v0.14.3"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := BuildSourceArchive(context.Background(), SourceArchiveOptions{
				RepoRoot: repo, Version: "0.14.2", Tag: tc.tag, Output: filepath.Join(t.TempDir(), "source.tar.gz"),
			})
			require.Error(t, err)
		})
	}
}

func TestBuildSourceArchive_IsByteDeterministic(t *testing.T) {
	repo := newSourceFixture(t, "release tree\n", "production-assets")
	outA := filepath.Join(t.TempDir(), "a.tar.gz")
	outB := filepath.Join(t.TempDir(), "b.tar.gz")
	metaA, err := BuildSourceArchive(context.Background(), SourceArchiveOptions{RepoRoot: repo, Version: "0.14.2", Tag: "v0.14.2", Output: outA})
	require.NoError(t, err)
	metaB, err := BuildSourceArchive(context.Background(), SourceArchiveOptions{RepoRoot: repo, Version: "0.14.2", Tag: "v0.14.2", Output: outB})
	require.NoError(t, err)
	bytesA, err := os.ReadFile(outA) //nolint:gosec // test-owned temporary path
	require.NoError(t, err)
	bytesB, err := os.ReadFile(outB) //nolint:gosec // test-owned temporary path
	require.NoError(t, err)
	assert.Equal(t, bytesA, bytesB)
	assert.Equal(t, metaA.SHA256, metaB.SHA256)
	sum := sha256.Sum256(bytesA)
	assert.Equal(t, hex.EncodeToString(sum[:]), metaA.SHA256)
}

func TestBuildSourceArchive_IgnoresGitLineEndingConfiguration(t *testing.T) {
	repo := newSourceFixture(t, "release tree\n", "production-assets")
	runGit(t, repo, "config", "core.autocrlf", "true")
	runGit(t, repo, "config", "core.eol", "crlf")
	out := filepath.Join(t.TempDir(), "source.tar.gz")

	_, err := BuildSourceArchive(context.Background(), SourceArchiveOptions{
		RepoRoot: repo, Version: "0.14.2", Tag: "v0.14.2", Output: out,
	})

	require.NoError(t, err)
	entries := readSourceArchive(t, out)
	assert.Equal(t, "release tree\n", string(entries["kata-0.14.2/README.md"].body))
}

func TestBuildSourceArchive_IncludesVendoredDependencies(t *testing.T) {
	repo := newBuildableSourceFixture(t, false)
	out := filepath.Join(t.TempDir(), "source.tar.gz")

	_, err := BuildSourceArchive(context.Background(), SourceArchiveOptions{
		RepoRoot: repo, Version: "0.14.2", Tag: "v0.14.2", Output: out,
	})

	require.NoError(t, err)
	entries := readSourceArchive(t, out)
	assert.Contains(t, entries, "kata-0.14.2/vendor/modules.txt")
	assert.Contains(t, entries, "kata-0.14.2/vendor/example.test/dependency/dependency.go")
}

func TestVerifySourceArchive_BuildsCoreBinaryFromCleanTree(t *testing.T) {
	repo := newBuildableSourceFixture(t, false)
	out := filepath.Join(t.TempDir(), "source.tar.gz")
	meta, err := BuildSourceArchive(context.Background(), SourceArchiveOptions{RepoRoot: repo, Version: "0.14.2", Tag: "v0.14.2", Output: out})
	require.NoError(t, err)
	require.NoError(t, VerifySourceArchive(context.Background(), out, meta))
}

func TestVerifySourceArchive_RejectsCompilationStub(t *testing.T) {
	repo := newBuildableSourceFixture(t, true)
	out := filepath.Join(t.TempDir(), "source.tar.gz")
	meta, err := BuildSourceArchive(context.Background(), SourceArchiveOptions{RepoRoot: repo, Version: "0.14.2", Tag: "v0.14.2", Output: out})
	require.NoError(t, err)
	require.Error(t, VerifySourceArchive(context.Background(), out, meta))
}

func TestVerificationBinaryPath(t *testing.T) {
	root := filepath.Join("tmp", "verify")
	assert.Equal(t, filepath.Join(root, "kata.exe"), verificationBinaryPath(root, "windows"))
	assert.Equal(t, filepath.Join(root, "kata"), verificationBinaryPath(root, "linux"))
}

func TestRenderHomebrewCoreFormula_UsesSourceMetadata(t *testing.T) {
	templatePath := filepath.Join(t.TempDir(), "kata.rb.tmpl")
	require.NoError(t, os.WriteFile(templatePath, []byte("version={{ .Version }} sha={{ .SHA256 }} built={{ .BuildDate }}\n"), 0o600))
	output := filepath.Join(t.TempDir(), "Formula", "kata.rb")
	meta := SourceArchiveMetadata{Version: "0.14.2", SHA256: "abc123", BuildDate: "2026-08-08T01:02:03Z"}

	require.NoError(t, RenderHomebrewCoreFormula(templatePath, output, meta))
	got, err := os.ReadFile(output) //nolint:gosec // test-owned temporary path
	require.NoError(t, err)
	assert.Equal(t, "version=0.14.2 sha=abc123 built=2026-08-08T01:02:03Z\n", string(got))
}

func TestRenderHomebrewCoreFormula_ProducesValidRuby(t *testing.T) {
	ruby, err := exec.LookPath("ruby")
	if err != nil {
		t.Skip("ruby is not installed")
	}
	templatePath := filepath.Join("..", "..", "packaging", "homebrew-core", "kata.rb.tmpl")
	output := filepath.Join(t.TempDir(), "kata.rb")
	meta := SourceArchiveMetadata{
		Version: "0.14.2", SHA256: strings.Repeat("a", 64), BuildDate: "2026-08-08T01:02:03Z",
	}
	require.NoError(t, RenderHomebrewCoreFormula(templatePath, output, meta))
	cmd := exec.Command(ruby, "-c", output) //nolint:gosec // fixed test tool and generated fixture path
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "%s", out)
}

type archiveTestEntry struct {
	body    []byte
	uid     int
	gid     int
	modTime string
}

func readSourceArchive(t *testing.T, path string) map[string]archiveTestEntry {
	t.Helper()
	f, err := os.Open(path) //nolint:gosec // test-owned temporary path
	require.NoError(t, err)
	defer func() { require.NoError(t, f.Close()) }()
	gz, err := gzip.NewReader(f)
	require.NoError(t, err)
	defer func() { require.NoError(t, gz.Close()) }()
	tr := tar.NewReader(gz)
	entries := make(map[string]archiveTestEntry)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		body, err := io.ReadAll(tr)
		require.NoError(t, err)
		entries[hdr.Name] = archiveTestEntry{body: body, uid: hdr.Uid, gid: hdr.Gid, modTime: hdr.ModTime.UTC().Format("2006-01-02T15:04:05Z07:00")}
	}
	return entries
}

func newSourceFixture(t *testing.T, readme, productionAsset string) string {
	t.Helper()
	repo := t.TempDir()
	runGit(t, repo, "init", "-q")
	runGit(t, repo, "config", "user.name", "Example User")
	runGit(t, repo, "config", "user.email", "example@example.test")
	goMod := `module go.kenn.io/kata

go 1.26.3

require example.test/dependency v0.0.0

replace example.test/dependency => ./third_party/dependency
`
	require.NoError(t, os.WriteFile(filepath.Join(repo, "go.mod"), []byte(goMod), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(repo, "fixture.go"), []byte("package kata\n\nimport _ \"example.test/dependency\"\n"), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(repo, "third_party", "dependency"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(repo, "third_party", "dependency", "go.mod"), []byte("module example.test/dependency\n\ngo 1.26.3\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(repo, "third_party", "dependency", "dependency.go"), []byte("package dependency\n\nconst Name = \"fixture\"\n"), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(repo, "internal/web/dist"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(repo, "README.md"), []byte(readme), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(repo, "internal/web/dist/index.html"), []byte("compilation stub\n"), 0o600))
	runGit(t, repo, "add", ".")
	runGitEnv(t, repo, []string{"GIT_AUTHOR_DATE=2026-08-08T01:02:03Z", "GIT_COMMITTER_DATE=2026-08-08T01:02:03Z"}, "commit", "-m", "release fixture")
	runGit(t, repo, "tag", "v0.14.2")
	require.NoError(t, os.WriteFile(filepath.Join(repo, "internal/web/dist/index.html"), []byte(productionAsset+"\n"), 0o600))
	return repo
}

func newBuildableSourceFixture(t *testing.T, stub bool) string {
	t.Helper()
	asset := "production-assets"
	if stub {
		asset = "compilation stub"
	}
	repo := newSourceFixture(t, "buildable\n", asset)
	require.NoError(t, os.MkdirAll(filepath.Join(repo, "internal/version"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(repo, "internal/version/version.go"), []byte("package version\nvar Version = \"dev\"\nvar Commit = \"unknown\"\nvar BuildDate = \"unknown\"\nvar Distribution string\n"), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(repo, "internal/web"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(repo, "internal/web/embed.go"), []byte("package web\nimport _ \"embed\"\n//go:embed dist/index.html\nvar Asset string\n"), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(repo, "cmd/kata"), 0o750))
	mainSource := `package main
import (
  "encoding/json"
  "fmt"
  "os"
  "strings"
  "example.test/dependency"
  "go.kenn.io/kata/internal/version"
  "go.kenn.io/kata/internal/web"
)
func main() {
	_ = dependency.Name
  if len(os.Args) > 1 && os.Args[1] == "_web-assets-check" {
    if strings.Contains(web.Asset, "compilation stub") { os.Exit(1) }
    return
  }
  if len(os.Args) > 2 && os.Args[1] == "version" && os.Args[2] == "--json" || len(os.Args) > 2 && os.Args[1] == "--json" && os.Args[2] == "version" {
    _ = json.NewEncoder(os.Stdout).Encode(map[string]string{"version": version.Version, "commit": version.Commit, "built": version.BuildDate, "distribution": version.Distribution})
    return
  }
  fmt.Fprintln(os.Stderr, "unsupported")
  os.Exit(2)
}
`
	require.NoError(t, os.WriteFile(filepath.Join(repo, "cmd/kata/main.go"), []byte(mainSource), 0o600))
	runGit(t, repo, "add", ".")
	runGitEnv(t, repo, []string{"GIT_AUTHOR_DATE=2026-08-08T01:02:03Z", "GIT_COMMITTER_DATE=2026-08-08T01:02:03Z"}, "commit", "-m", "buildable fixture")
	runGit(t, repo, "tag", "-f", "v0.14.2")
	return repo
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	runGitEnv(t, dir, nil, args...)
}

func runGitEnv(t *testing.T, dir string, extraEnv []string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...) //nolint:gosec // fixed test helper arguments
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), extraEnv...)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "%s", out)
}
