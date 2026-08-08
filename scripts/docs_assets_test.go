package scripts_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var docsAssets = []string{
	"README.md",
	"manifest.json",
	"tui/hero.svg",
	"federation-tui/list.svg",
	"federation-tui/select-hub.svg",
	"federation-tui/select-hub-project.svg",
	"federation-tui/preview.svg",
	"federation-tui/result.svg",
	"web-ui/workspace.png",
	"web-ui/issue-detail.png",
	"web-ui/relationships.png",
	"web-ui/daemon-switcher.png",
}

func TestDocsAssetPublisherRejectsInvalidSources(t *testing.T) {
	requireDocsScriptTools(t)

	for _, tc := range []struct {
		name    string
		mutate  func(*testing.T, string)
		message string
	}{
		{
			name: "missing",
			mutate: func(t *testing.T, source string) {
				require.NoError(t, os.Remove(filepath.Join(source, "web-ui", "workspace.png")))
			},
			message: "missing expected asset: web-ui/workspace.png",
		},
		{
			name: "unexpected",
			mutate: func(t *testing.T, source string) {
				require.NoError(t, os.WriteFile(filepath.Join(source, ".env.local"), []byte("TOKEN=example\n"), 0o600))
			},
			message: "unexpected file: .env.local",
		},
		{
			name: "symlink",
			mutate: func(t *testing.T, source string) {
				path := filepath.Join(source, "web-ui", "workspace.png")
				require.NoError(t, os.Remove(path))
				if err := os.Symlink("issue-detail.png", path); err != nil {
					t.Skipf("symlinks unavailable: %v", err)
				}
			},
			message: "must not be a symlink: web-ui/workspace.png",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tempDir := t.TempDir()
			repo := filepath.Join(tempDir, "repo")
			source := filepath.Join(tempDir, "source")
			initDocsTestRepo(t, repo)
			writeDocsAssets(t, source, "asset")
			tc.mutate(t, source)
			publisher := installDocsScript(t, repo, "update-assets-branch.sh")

			cmd := exec.Command("bash", publisher, "--source", source)
			cmd.Dir = repo
			output, err := cmd.CombinedOutput()

			require.Error(t, err, string(output))
			assert.Contains(t, string(output), tc.message)
			assertCommandFails(t, repo, "rev-parse", "--verify", "docs-assets")
		})
	}
}

func TestDocsAssetPublisherCreatesSingleOrphanCommit(t *testing.T) {
	requireDocsScriptTools(t)

	tempDir := t.TempDir()
	repo := filepath.Join(tempDir, "repo")
	source := filepath.Join(tempDir, "source")
	initDocsTestRepo(t, repo)
	writeDocsAssets(t, source, "published")
	publisher := installDocsScript(t, repo, "update-assets-branch.sh")

	cmd := exec.Command("bash", publisher, "--source", source)
	cmd.Dir = repo
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, string(output))

	assert.Equal(t, "1", gitOutput(t, repo, "rev-list", "--count", "docs-assets"))
	rootCommit := strings.Fields(gitOutput(t, repo, "rev-list", "--parents", "-n", "1", "docs-assets"))
	assert.Len(t, rootCommit, 1)
	published := strings.Fields(gitOutput(t, repo, "ls-tree", "-r", "--name-only", "docs-assets"))
	expected := slices.Clone(docsAssets)
	slices.Sort(expected)
	slices.Sort(published)
	assert.Equal(t, expected, published)
}

func TestDocsAssetHydratorForceFetchesAndValidatesRemoteBranch(t *testing.T) {
	requireDocsScriptTools(t)

	tempDir := t.TempDir()
	remote := filepath.Join(tempDir, "remote.git")
	repo := filepath.Join(tempDir, "repo")
	git(t, tempDir, "init", "--bare", remote)
	initDocsTestRepo(t, repo)
	git(t, repo, "remote", "add", "origin", remote)

	oldSource := filepath.Join(tempDir, "old")
	writeDocsAssets(t, oldSource, "old")
	oldCommit := commitAssetTree(t, remote, oldSource, "old docs assets")
	gitBare(t, remote, "update-ref", "refs/heads/docs-assets", oldCommit)
	git(t, repo, "fetch", "origin", "docs-assets:refs/remotes/origin/docs-assets")

	newSource := filepath.Join(tempDir, "new")
	writeDocsAssets(t, newSource, "new")
	newCommit := commitAssetTree(t, remote, newSource, "new docs assets")
	gitBare(t, remote, "update-ref", "refs/heads/docs-assets", newCommit)

	hydrator := installDocsScript(t, repo, "hydrate-assets.sh")
	cmd := exec.Command("bash", hydrator)
	cmd.Dir = repo
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, string(output))

	hydrated, err := os.ReadFile(filepath.Join(repo, "docs", "assets", "screenshots", "web-ui", "workspace.png"))
	require.NoError(t, err)
	assert.Equal(t, "new\n", string(hydrated))
	assert.Equal(t, newCommit, gitOutput(t, repo, "rev-parse", "origin/docs-assets"))
}

func TestWebScreenshotGeneratorRunsFocusedCapture(t *testing.T) {
	requireDocsScriptTools(t)

	tempDir := t.TempDir()
	fakeBin := filepath.Join(tempDir, "bin")
	outputDir := filepath.Join(tempDir, "output")
	argsLog := filepath.Join(tempDir, "bun-args")
	require.NoError(t, os.MkdirAll(fakeBin, 0o755))
	fakeBun := filepath.Join(fakeBin, "bun")
	require.NoError(t, os.WriteFile(fakeBun, []byte(`#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >"$KATA_FAKE_BUN_ARGS"
mkdir -p "$KATA_DOCS_SCREENSHOT_DIR/web-ui"
for image in workspace issue-detail relationships daemon-switcher; do
  printf 'synthetic png\n' >"$KATA_DOCS_SCREENSHOT_DIR/web-ui/$image.png"
done
`), 0o755))

	script := filepath.Join("..", "docs", "screenshots", "generate-web-ui.sh")
	cmd := exec.Command("bash", script, "--out", outputDir)
	cmd.Dir = filepath.Join("..", "web")
	cmd.Env = append(envWithout("PATH", "KATA_DOCS_SCREENSHOT_DIR", "KATA_FAKE_BUN_ARGS"),
		"PATH="+fakeBin+":/usr/bin:/bin",
		"KATA_FAKE_BUN_ARGS="+argsLog,
	)
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, string(output))

	args, err := os.ReadFile(argsLog)
	require.NoError(t, err)
	assert.Equal(t, "run test:e2e -- tests/docs-screenshots.spec.ts\n", string(args))
	for _, image := range []string{"workspace", "issue-detail", "relationships", "daemon-switcher"} {
		info, err := os.Stat(filepath.Join(outputDir, "web-ui", image+".png"))
		require.NoError(t, err)
		assert.Positive(t, info.Size())
	}
}

func requireDocsScriptTools(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("docs asset scripts are Unix deployment tooling")
	}
	for _, command := range []string{"bash", "git", "tar"} {
		if _, err := exec.LookPath(command); err != nil {
			t.Skipf("%s not available: %v", command, err)
		}
	}
}

func envWithout(names ...string) []string {
	blocked := make(map[string]struct{}, len(names))
	for _, name := range names {
		blocked[name] = struct{}{}
	}

	filtered := make([]string, 0, len(os.Environ()))
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if _, ok := blocked[name]; !ok {
			filtered = append(filtered, entry)
		}
	}
	return filtered
}

func initDocsTestRepo(t *testing.T, repo string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(repo, 0o755))
	git(t, repo, "init")
	git(t, repo, "config", "user.name", "Example User")
	git(t, repo, "config", "user.email", "example@example.invalid")
	require.NoError(t, os.WriteFile(filepath.Join(repo, "README.md"), []byte("example repository\n"), 0o644))
	git(t, repo, "add", "README.md")
	git(t, repo, "commit", "-m", "initial fixture")
}

func installDocsScript(t *testing.T, repo, name string) string {
	t.Helper()
	destinationDir := filepath.Join(repo, "docs", "screenshots")
	require.NoError(t, os.MkdirAll(destinationDir, 0o755))
	for _, candidate := range []string{"assets.sh", name} {
		source := filepath.Join("..", "docs", "screenshots", candidate)
		contents, err := os.ReadFile(source)
		if os.IsNotExist(err) && candidate == "assets.sh" {
			continue
		}
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(destinationDir, candidate), contents, 0o755))
	}
	return filepath.Join(destinationDir, name)
}

func writeDocsAssets(t *testing.T, root, content string) {
	t.Helper()
	for _, asset := range docsAssets {
		path := filepath.Join(root, asset)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(content+"\n"), 0o644))
	}
}

func commitAssetTree(t *testing.T, bareRepo, workTree, message string) string {
	t.Helper()
	index := filepath.Join(t.TempDir(), "index")
	environment := append(os.Environ(),
		"GIT_INDEX_FILE="+index,
		"GIT_AUTHOR_NAME=Example User",
		"GIT_AUTHOR_EMAIL=example@example.invalid",
		"GIT_COMMITTER_NAME=Example User",
		"GIT_COMMITTER_EMAIL=example@example.invalid",
	)
	gitBareWorkTree(t, bareRepo, workTree, environment, "add", "-A", ".")
	tree := gitBareWorkTreeOutput(t, bareRepo, workTree, environment, "write-tree")
	return gitBareOutput(t, bareRepo, environment, "commit-tree", tree, "-m", message)
}

func assertCommandFails(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	require.Error(t, cmd.Run())
}

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, string(output))
}

func gitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	output, err := cmd.Output()
	require.NoError(t, err)
	return strings.TrimSpace(string(output))
}

func gitBare(t *testing.T, bareRepo string, args ...string) {
	t.Helper()
	output, err := gitBareCommand(bareRepo, "", nil, args...).CombinedOutput()
	require.NoError(t, err, string(output))
}

func gitBareOutput(t *testing.T, bareRepo string, environment []string, args ...string) string {
	t.Helper()
	output, err := gitBareCommand(bareRepo, "", environment, args...).Output()
	require.NoError(t, err)
	return strings.TrimSpace(string(output))
}

func gitBareWorkTree(t *testing.T, bareRepo, workTree string, environment []string, args ...string) {
	t.Helper()
	output, err := gitBareCommand(bareRepo, workTree, environment, args...).CombinedOutput()
	require.NoError(t, err, string(output))
}

func gitBareWorkTreeOutput(t *testing.T, bareRepo, workTree string, environment []string, args ...string) string {
	t.Helper()
	output, err := gitBareCommand(bareRepo, workTree, environment, args...).Output()
	require.NoError(t, err)
	return strings.TrimSpace(string(output))
}

func gitBareCommand(bareRepo, workTree string, environment []string, args ...string) *exec.Cmd {
	fullArgs := []string{"--git-dir", bareRepo}
	if workTree != "" {
		fullArgs = append(fullArgs, "--work-tree", workTree)
	}
	fullArgs = append(fullArgs, args...)
	cmd := exec.Command("git", fullArgs...)
	if workTree != "" {
		cmd.Dir = workTree
	}
	if environment != nil {
		cmd.Env = environment
	}
	return cmd
}
