package config_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/testfix"
)

func TestDiscoverPaths_FindsKataTomlAndGit(t *testing.T) {
	root := t.TempDir()
	testfix.MkDotGit(t, root)
	testfix.WriteKataToml(t, root, "x")
	sub := filepath.Join(root, "a", "b")
	require.NoError(t, os.MkdirAll(sub, 0o755)) //nolint:gosec // test fixture under TempDir.

	d, err := config.DiscoverPaths(sub)
	require.NoError(t, err)
	assert.Equal(t, root, d.WorkspaceRoot)
	assert.Equal(t, root, d.GitRoot)
}

func TestDiscoverPaths_KataTomlInSubdirOfGit(t *testing.T) {
	root := t.TempDir()
	testfix.MkDotGit(t, root)
	sub := filepath.Join(root, "subproject")
	require.NoError(t, os.MkdirAll(sub, 0o755)) //nolint:gosec // test fixture under TempDir.
	testfix.WriteKataToml(t, sub, "x")

	d, err := config.DiscoverPaths(sub)
	require.NoError(t, err)
	assert.Equal(t, sub, d.WorkspaceRoot)
	assert.Equal(t, root, d.GitRoot)
}

func TestDiscoverPaths_NeitherFound(t *testing.T) {
	d, err := config.DiscoverPaths(t.TempDir())
	require.NoError(t, err)
	assert.Empty(t, d.WorkspaceRoot)
	assert.Empty(t, d.GitRoot)
}

func TestDiscoverPaths_StartPathMissingErrors(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist", "deeper")
	_, err := config.DiscoverPaths(missing)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "stat")
}

func TestDiscoverPaths_StartPathIsFileWalksFromParent(t *testing.T) {
	root := t.TempDir()
	testfix.MkDotGit(t, root)
	testfix.WriteKataToml(t, root, "x")
	filePath := filepath.Join(root, "README.md")
	require.NoError(t, os.WriteFile(filePath, []byte("hi"), 0o644)) //nolint:gosec // test fixture

	d, err := config.DiscoverPaths(filePath)
	require.NoError(t, err)
	assert.Equal(t, root, d.WorkspaceRoot)
	assert.Equal(t, root, d.GitRoot)
}

func TestNormalizeRemoteURL(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"https://github.com/wesm/kata.git", "github.com/wesm/kata"},
		{"https://github.com/wesm/kata", "github.com/wesm/kata"},
		{"https://user:pass@github.com/wesm/kata.git", "github.com/wesm/kata"},
		{"git@github.com:wesm/kata.git", "github.com/wesm/kata"},
		{"ssh://git@gitlab.com/team/repo.git", "gitlab.com/team/repo"},
		// Percent-encoded paths (Azure DevOps, spaces in org/project names)
		{"rtstnz@vs-ssh.visualstudio.com:v3/rtstnz/AI%20Experiments/AI%20Experiments", "vs-ssh.visualstudio.com/v3/rtstnz/AI-Experiments/AI-Experiments"},
		{"https://dev.azure.com/org/My%20Project/_git/My%20Repo.git", "dev.azure.com/org/My-Project/_git/My-Repo"},
	}
	for _, tc := range cases {
		got, err := config.NormalizeRemoteURL(tc.in)
		require.NoError(t, err, tc.in)
		assert.Equal(t, tc.want, got, tc.in)
	}
}

func TestComputeAliasIdentity_GitWithRemote(t *testing.T) {
	dir := testfix.InitGitRepo(t)
	testfix.RunGit(t, dir, "remote", "add", "origin", "https://github.com/wesm/kata.git")

	a, err := config.ComputeAliasIdentity(t.Context(), config.DiscoveredPaths{GitRoot: dir})
	require.NoError(t, err)
	assert.Equal(t, "github.com/wesm/kata", a.Identity)
	assert.Equal(t, "git", a.Kind)
}

func TestComputeAliasIdentity_RespectsCanceledContext(t *testing.T) {
	dir := testfix.InitGitRepo(t)
	testfix.RunGit(t, dir, "remote", "add", "origin", "https://github.com/wesm/kata.git")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := config.ComputeAliasIdentity(ctx, config.DiscoveredPaths{GitRoot: dir})
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
}

func TestComputeAliasIdentity_GitNoRemote(t *testing.T) {
	dir := testfix.InitGitRepo(t)

	a, err := config.ComputeAliasIdentity(t.Context(), config.DiscoveredPaths{GitRoot: dir})
	require.NoError(t, err)
	assert.Equal(t, "local://"+dir, a.Identity)
	assert.Equal(t, "local", a.Kind)
}

func TestComputeAliasIdentity_NonGitWorkspace(t *testing.T) {
	ws := t.TempDir()
	a, err := config.ComputeAliasIdentity(t.Context(), config.DiscoveredPaths{WorkspaceRoot: ws})
	require.NoError(t, err)
	assert.Equal(t, "local://"+ws, a.Identity)
	assert.Equal(t, "local", a.Kind)
}

func TestComputeAliasIdentity_Neither(t *testing.T) {
	_, err := config.ComputeAliasIdentity(t.Context(), config.DiscoveredPaths{})
	require.Error(t, err)
}

func TestValidateIdentity(t *testing.T) {
	cases := []struct {
		in   string
		ok   bool
		hint string
	}{
		{"github.com/wesm/kata", true, ""},
		{"local:///abs/path", true, ""},
		{"a_b.c-d:foo/bar", true, ""},
		{"", false, "non-empty"},
		{"  spaces in middle  ", false, "whitespace"},
		{"has space", false, "whitespace"},
		{"https://u:p@host/x", false, "credential"},
	}
	for _, tc := range cases {
		err := config.ValidateIdentity(tc.in)
		if tc.ok {
			assert.NoError(t, err, tc.in)
		} else {
			require.Error(t, err, tc.in)
			assert.Contains(t, err.Error(), tc.hint, tc.in)
		}
	}
}

func TestPickInitName_KataTomlOnly(t *testing.T) {
	cfg := &config.ProjectConfig{}
	cfg.Project.Name = "kata"

	got, err := config.PickInitName(t.Context(), config.DiscoveredPaths{}, cfg, "", false)
	require.NoError(t, err)
	assert.Equal(t, "kata", got.Name)
}

func TestPickInitName_KataTomlMatchingInputName(t *testing.T) {
	cfg := &config.ProjectConfig{}
	cfg.Project.Name = "kata"

	got, err := config.PickInitName(t.Context(), config.DiscoveredPaths{}, cfg, "kata", false)
	require.NoError(t, err)
	assert.Equal(t, "kata", got.Name)
}

func TestPickInitName_KataTomlConflictWithoutReplace(t *testing.T) {
	cfg := &config.ProjectConfig{}
	cfg.Project.Name = "kata"

	_, err := config.PickInitName(t.Context(), config.DiscoveredPaths{}, cfg, "other", false)
	require.Error(t, err)
	assert.ErrorIs(t, err, config.ErrNameConflict)
}

func TestPickInitName_KataTomlConflictWithReplace(t *testing.T) {
	cfg := &config.ProjectConfig{}
	cfg.Project.Name = "kata"

	got, err := config.PickInitName(t.Context(), config.DiscoveredPaths{}, cfg, "other", true)
	require.NoError(t, err)
	assert.Equal(t, "other", got.Name)
}

func TestPickInitName_InputName(t *testing.T) {
	got, err := config.PickInitName(t.Context(), config.DiscoveredPaths{}, nil, "custom", false)
	require.NoError(t, err)
	assert.Equal(t, "custom", got.Name)
}

func TestPickInitName_FromGitRoot(t *testing.T) {
	dir := testfix.InitGitRepo(t)
	testfix.RunGit(t, dir, "remote", "add", "origin", "https://github.com/wesm/kata.git")

	got, err := config.PickInitName(t.Context(), config.DiscoveredPaths{GitRoot: dir}, nil, "", false)
	require.NoError(t, err)
	assert.Equal(t, "kata", got.Name)
}

func TestPickInitName_FromLocalWorkspace(t *testing.T) {
	dir := t.TempDir()

	got, err := config.PickInitName(t.Context(), config.DiscoveredPaths{WorkspaceRoot: dir}, nil, "", false)
	require.NoError(t, err)
	assert.Equal(t, filepath.Base(dir), got.Name)
}

func TestPickInitName_NoSource(t *testing.T) {
	_, err := config.PickInitName(t.Context(), config.DiscoveredPaths{}, nil, "", false)
	require.Error(t, err)
	assert.ErrorIs(t, err, config.ErrNoNameSource)
}

func TestPickInitName_KataTomlEmptyNameFallsBackToSource(t *testing.T) {
	cfg := &config.ProjectConfig{}
	cfg.Project.Name = ""
	dir := testfix.InitGitRepo(t)
	testfix.RunGit(t, dir, "remote", "add", "origin", "https://github.com/wesm/kata.git")

	got, err := config.PickInitName(t.Context(), config.DiscoveredPaths{GitRoot: dir}, cfg, "", false)
	require.NoError(t, err)
	assert.Equal(t, "kata", got.Name)
}

func TestValidateAliasInfo(t *testing.T) {
	cases := []struct {
		name string
		info config.AliasInfo
		ok   bool
		hint string
	}{
		{
			name: "git alias passes ValidateIdentity rules",
			info: config.AliasInfo{Identity: "github.com/wesm/kata", Kind: "git"},
			ok:   true,
		},
		{
			name: "local alias with spaces in path is allowed",
			info: config.AliasInfo{Identity: "local:///Users/me/My Project", Kind: "local"},
			ok:   true,
		},
		{
			name: "local alias with unicode is allowed",
			info: config.AliasInfo{Identity: "local:///私の/プロジェクト", Kind: "local"},
			ok:   true,
		},
		{
			name: "git alias with whitespace is rejected",
			info: config.AliasInfo{Identity: "has space", Kind: "git"},
			ok:   false,
			hint: "whitespace",
		},
		{
			name: "unknown kind is rejected",
			info: config.AliasInfo{Identity: "github.com/wesm/kata", Kind: "bogus"},
			ok:   false,
			hint: "kind",
		},
		{
			name: "empty kind is rejected",
			info: config.AliasInfo{Identity: "github.com/wesm/kata", Kind: ""},
			ok:   false,
			hint: "kind",
		},
		{
			name: "empty identity is rejected",
			info: config.AliasInfo{Identity: "", Kind: "git"},
			ok:   false,
			hint: "identity",
		},
		{
			name: "local alias missing prefix is rejected",
			info: config.AliasInfo{Identity: "/Users/me/proj", Kind: "local"},
			ok:   false,
			hint: "local://",
		},
		{
			name: "local alias bare prefix is rejected",
			info: config.AliasInfo{Identity: "local://", Kind: "local"},
			ok:   false,
			hint: "local://",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := config.ValidateAliasInfo(tc.info)
			if tc.ok {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.hint)
		})
	}
}
