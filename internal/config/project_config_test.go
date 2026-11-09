package config_test

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/testfix"
)

func setupKataProjectDir(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, ".kata.toml")
	require.NoError(t, os.WriteFile(path, []byte(body), 0o644)) //nolint:gosec // test fixture matches production .kata.toml mode
	return dir
}

func writeAndReadConfig(t *testing.T, name string) *config.ProjectConfig {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, config.WriteProjectConfig(dir, name))
	cfg, err := config.ReadProjectConfig(dir)
	require.NoError(t, err)
	return cfg
}

func TestReadProjectConfig_Roundtrip(t *testing.T) {
	dir := setupKataProjectDir(t, `version = 1

[project]
name = "kata"
`)

	cfg, err := config.ReadProjectConfig(dir)
	require.NoError(t, err)
	assert.Equal(t, 1, cfg.Version)
	assert.Equal(t, "kata", cfg.Project.Name)
}

func TestReadProjectConfig_IgnoresLegacyIdentity(t *testing.T) {
	dir := setupKataProjectDir(t, `version = 1

[project]
identity = "github.com/example/foo"
name = "foo"
`)

	cfg, err := config.ReadProjectConfig(dir)
	require.NoError(t, err)
	assert.Equal(t, "foo", cfg.Project.Name)
}

func TestReadProjectConfig_DerivesNameFromLegacyIdentityOnly(t *testing.T) {
	dir := setupKataProjectDir(t, `version = 1

[project]
identity = "github.com/example/foo"
`)

	cfg, err := config.ReadProjectConfig(dir)
	require.NoError(t, err)
	assert.Equal(t, "foo", cfg.Project.Name)
}

func TestReadProjectConfig_Missing(t *testing.T) {
	cfg, err := config.ReadProjectConfig(t.TempDir())
	assert.Nil(t, cfg)
	assert.ErrorIs(t, err, config.ErrProjectConfigMissing)
}

func TestReadProjectConfig_RejectsBadVersion(t *testing.T) {
	dir := setupKataProjectDir(t, `version = 2

[project]
name = "y"
`)

	_, err := config.ReadProjectConfig(dir)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported .kata.toml version")
}

func TestReadProjectConfig_RejectsBlankName(t *testing.T) {
	dir := setupKataProjectDir(t, `version = 1

[project]
name = "   "
`)

	_, err := config.ReadProjectConfig(dir)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "project.name")
}

func TestWriteProjectConfig_WritesHeaderAndName(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, config.WriteProjectConfig(dir, "foo"))
	bs, err := os.ReadFile(filepath.Join(dir, ".kata.toml")) //nolint:gosec // test fixture under TempDir
	require.NoError(t, err)
	assert.Contains(t, string(bs), `name = "foo"`)
	assert.True(t, strings.HasPrefix(string(bs), "# This repository tracks its work in kata.\n# Coding agents: read `kata quickstart --format contract` before changing code here.\n"))
	assert.NotContains(t, string(bs), "identity")

	cfg, err := config.ReadProjectConfig(dir)
	require.NoError(t, err)
	assert.Equal(t, "foo", cfg.Project.Name)
}

func TestEnsureProjectConfigHeader(t *testing.T) {
	header := "# This repository tracks its work in kata.\n# Coding agents: read `kata quickstart --format contract` before changing code here.\n"
	body := "# Keep this comment.\nversion = 1\n\n[project]\nname = \"spoke-project\"\n\n[server]\nurl = \"http://127.0.0.1:7777\"\n"
	for _, tc := range []struct {
		name    string
		content string
		want    string
		changed bool
	}{
		{"headerless", body, header + body, true},
		{"UTF-8 BOM", "\xef\xbb\xbf" + body, "\xef\xbb\xbf" + header + body, true},
		{"CRLF", strings.ReplaceAll(body, "\n", "\r\n"), header + strings.ReplaceAll(body, "\n", "\r\n"), true},
		{"comment above moved header", "# User comment.\n" + header + body, "# User comment.\n" + header + body, false},
		{"reworded", "# Read kata quickstart --format contract first.\n" + body, "# Read kata quickstart --format contract first.\n" + body, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := setupKataProjectDir(t, tc.content)
			changed, err := config.EnsureProjectConfigHeader(dir)
			require.NoError(t, err)
			assert.Equal(t, tc.changed, changed)
			content, err := os.ReadFile(filepath.Join(dir, ".kata.toml")) //nolint:gosec // test fixture under TempDir
			require.NoError(t, err)
			assert.Equal(t, tc.want, string(content))
			cfg, err := config.ReadProjectConfig(dir)
			require.NoError(t, err)
			assert.Equal(t, "spoke-project", cfg.Project.Name)
			if !tc.changed {
				return
			}
			changed, err = config.EnsureProjectConfigHeader(dir)
			require.NoError(t, err)
			assert.False(t, changed)
			again, err := os.ReadFile(filepath.Join(dir, ".kata.toml")) //nolint:gosec // test fixture under TempDir
			require.NoError(t, err)
			assert.Equal(t, content, again)
		})
	}
}

func TestEnsureProjectConfigHeader_WriteFailurePreservesBinding(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("directory permissions don't block writes on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root can write to read-only directories")
	}
	body := "version = 1\n[project]\nname = \"spoke-project\"\n"
	dir := setupKataProjectDir(t, body)
	require.NoError(t, os.Chmod(dir, 0o555))       //nolint:gosec // read-only directory blocks the atomic rename
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) }) //nolint:gosec // restore so TempDir cleanup can remove it

	changed, err := config.EnsureProjectConfigHeader(dir)
	require.Error(t, err)
	assert.False(t, changed)
	content, err := os.ReadFile(filepath.Join(dir, ".kata.toml")) //nolint:gosec // test fixture under TempDir
	require.NoError(t, err)
	assert.Equal(t, body, string(content))
}

func TestWriteProjectConfig_WritableFileInReadOnlyDirectory(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("directory permissions don't block writes")
	}
	dir := setupKataProjectDir(t, "version = 1\n[project]\nname = \"old-project\"\n")
	require.NoError(t, os.Chmod(dir, 0o555))       //nolint:gosec // fixture permits writes only to the existing binding
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) }) //nolint:gosec // restore fixture permissions for cleanup

	require.NoError(t, config.WriteProjectConfig(dir, "spoke-project"))
	cfg, err := config.ReadProjectConfig(dir)
	require.NoError(t, err)
	assert.Equal(t, "spoke-project", cfg.Project.Name)
}

func TestWriteProjectConfig_RejectsBlankName(t *testing.T) {
	err := config.WriteProjectConfig(t.TempDir(), " ")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "name")
}

func TestProjectConfig_RejectsControlCharactersInName(t *testing.T) {
	for _, tc := range []struct {
		name string
		toml string
		want string
	}{
		{name: "bad\nname", toml: `"bad\nname"`, want: "non-printable"},
		{name: "bad\x1b]52;c;AAAA\x07name", toml: `"bad\u001b]52;c;AAAA\u0007name"`, want: "non-printable"},
	} {
		t.Run("write", func(t *testing.T) {
			err := config.WriteProjectConfig(t.TempDir(), tc.name)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
		t.Run("read", func(t *testing.T) {
			dir := setupKataProjectDir(t, "version = 1\n\n[project]\nname = "+tc.toml+"\n")
			_, err := config.ReadProjectConfig(dir)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

func TestWriteProjectConfig_PreservesName(t *testing.T) {
	cfg := writeAndReadConfig(t, "Kata Tracker %20 github.com/example/foo")
	assert.Equal(t, "Kata Tracker %20 github.com/example/foo", cfg.Project.Name)
}

func TestReadProjectConfig_AcceptsOptionalServerBlock(t *testing.T) {
	dir := setupKataProjectDir(t, `version = 1

[project]
name = "kata"

[server]
url = "http://127.0.0.1:7777"
`)
	cfg, err := config.ReadProjectConfig(dir)
	require.NoError(t, err)
	assert.Equal(t, "http://127.0.0.1:7777", cfg.Server.URL)
}

func TestReadProjectConfig_NoServerBlockYieldsZeroValue(t *testing.T) {
	dir := setupKataProjectDir(t, `version = 1

[project]
name = "kata"
`)
	cfg, err := config.ReadProjectConfig(dir)
	require.NoError(t, err)
	assert.Empty(t, cfg.Server.URL)
}

func TestReadProjectConfig_WarnsForCommittedDaemonSelection(t *testing.T) {
	dir := setupKataProjectDir(t, `version = 1
[project]
name = "spoke-project"
[server]
daemon = "work"
`)
	warnings, err := os.CreateTemp(t.TempDir(), "stderr")
	require.NoError(t, err)
	t.Cleanup(func() { _ = warnings.Close() })
	stderr := os.Stderr
	os.Stderr = warnings
	t.Cleanup(func() { os.Stderr = stderr })

	cfg, err := config.ReadProjectConfig(dir)

	require.NoError(t, err)
	assert.Equal(t, "spoke-project", cfg.Project.Name)
	output, err := os.ReadFile(warnings.Name()) //nolint:gosec // test-owned capture file
	require.NoError(t, err)
	assert.Contains(t, string(output), "server.daemon in .kata.toml is ignored")
	assert.Contains(t, string(output), "move it to .kata.local.toml")
}

func TestFindProjectConfig_FromSubdirectory(t *testing.T) {
	root := setupKataProjectDir(t, `version = 1

[project]
name = "kata"
`)
	sub := filepath.Join(root, "internal", "tui")
	require.NoError(t, os.MkdirAll(sub, 0o755)) //nolint:gosec // test fixture under TempDir

	cfg, foundDir, err := config.FindProjectConfig(sub)
	require.NoError(t, err)
	assert.Equal(t, root, foundDir)
	assert.Equal(t, "kata", cfg.Project.Name)
}

func TestFindProjectConfig_FromExactDir(t *testing.T) {
	root := setupKataProjectDir(t, `version = 1

[project]
name = "kata"
`)
	cfg, foundDir, err := config.FindProjectConfig(root)
	require.NoError(t, err)
	assert.Equal(t, root, foundDir)
	assert.Equal(t, "kata", cfg.Project.Name)
}

func TestFindProjectConfig_DoesNotCrossGitRoot(t *testing.T) {
	outer := setupKataProjectDir(t, `version = 1

[project]
name = "example-project"
`)
	repo := filepath.Join(outer, "repo-a")
	require.NoError(t, os.MkdirAll(repo, 0o755)) //nolint:gosec // test fixture under TempDir
	testfix.MkDotGit(t, repo)

	cfg, foundDir, err := config.FindProjectConfig(repo)
	assert.Nil(t, cfg)
	assert.Empty(t, foundDir)
	assert.ErrorIs(t, err, config.ErrProjectConfigMissing)
}

func TestFindProjectConfig_MissingReturnsSentinel(t *testing.T) {
	root := t.TempDir()
	cfg, foundDir, err := config.FindProjectConfig(root)
	assert.Nil(t, cfg)
	assert.Empty(t, foundDir)
	assert.ErrorIs(t, err, config.ErrProjectConfigMissing)
}

func TestFindProjectConfig_PropagatesParseError(t *testing.T) {
	root := setupKataProjectDir(t, "this is not toml = = =")
	cfg, foundDir, err := config.FindProjectConfig(root)
	assert.Nil(t, cfg)
	assert.Empty(t, foundDir)
	require.Error(t, err)
	assert.NotErrorIs(t, err, config.ErrProjectConfigMissing)
}
