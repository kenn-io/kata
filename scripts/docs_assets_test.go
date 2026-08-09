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

func TestDocsScreenshotGeneratorRejectsUnsafeOutputDirectories(t *testing.T) {
	requireDocsScriptTools(t)

	t.Run("empty output", func(t *testing.T) {
		tempDir := t.TempDir()
		repo := filepath.Join(tempDir, "repo")
		initDocsTestRepo(t, repo)
		generator := installDocsGeneratorFixture(t, repo)
		sentinel := filepath.Join(repo, "must-survive")
		require.NoError(t, os.WriteFile(sentinel, []byte("keep\n"), 0o600))

		cmd := exec.Command("bash", generator, "--out", "") //nolint:gosec // fixed test-owned script under TempDir
		cmd.Dir = repo
		output, err := cmd.CombinedOutput()

		require.Error(t, err, string(output))
		assert.FileExists(t, sentinel)
		assert.Contains(t, string(output), "refusing unsafe docs screenshot output directory")
	})

	t.Run("existing unowned directory", func(t *testing.T) {
		tempDir := t.TempDir()
		repo := filepath.Join(tempDir, "repo")
		initDocsTestRepo(t, repo)
		generator := installDocsGeneratorFixture(t, repo)
		unowned := filepath.Join(tempDir, "unowned")
		require.NoError(t, os.MkdirAll(unowned, 0o700))
		sentinel := filepath.Join(unowned, "must-survive")
		require.NoError(t, os.WriteFile(sentinel, []byte("keep\n"), 0o600))

		cmd := exec.Command("bash", generator, "--out", unowned) //nolint:gosec // fixed test-owned script and TempDir output
		cmd.Dir = repo
		output, err := cmd.CombinedOutput()

		require.Error(t, err, string(output))
		assert.FileExists(t, sentinel)
		assert.Contains(t, string(output), "refusing to replace unowned docs screenshot directory")
	})

	t.Run("symlink", func(t *testing.T) {
		tempDir := t.TempDir()
		repo := filepath.Join(tempDir, "repo")
		initDocsTestRepo(t, repo)
		generator := installDocsGeneratorFixture(t, repo)
		target := filepath.Join(tempDir, "target")
		require.NoError(t, os.MkdirAll(target, 0o700))
		sentinel := filepath.Join(target, "must-survive")
		require.NoError(t, os.WriteFile(sentinel, []byte("keep\n"), 0o600))
		link := filepath.Join(tempDir, "output-link")
		if err := os.Symlink(target, link); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}

		cmd := exec.Command("bash", generator, "--out", link) //nolint:gosec // fixed test-owned script and TempDir output
		cmd.Dir = repo
		output, err := cmd.CombinedOutput()

		require.Error(t, err, string(output))
		assert.FileExists(t, sentinel)
		assert.Contains(t, string(output), "output directory symlink")
	})
}

func TestDocsScreenshotGeneratorCreatesAndReplacesOwnedOutput(t *testing.T) {
	requireDocsScriptTools(t)

	tempDir := t.TempDir()
	repo := filepath.Join(tempDir, "repo")
	initDocsTestRepo(t, repo)
	generator := installDocsGeneratorFixture(t, repo)
	outputDir := filepath.Join(tempDir, "generated")

	for range 2 {
		cmd := exec.Command("bash", generator, "--out", outputDir) //nolint:gosec // fixed test-owned script and TempDir output
		cmd.Dir = repo
		output, err := cmd.CombinedOutput()
		require.NoError(t, err, string(output))
		for _, asset := range docsAssets {
			assert.FileExists(t, filepath.Join(outputDir, asset))
		}
	}

	staging, err := filepath.Glob(filepath.Join(tempDir, ".kata-docs-screenshots.*"))
	require.NoError(t, err)
	assert.Empty(t, staging)
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

			cmd := exec.Command("bash", publisher, "--source", source) //nolint:gosec // test-owned script and TempDir source
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

	cmd := exec.Command("bash", publisher, "--source", source) //nolint:gosec // test-owned script and TempDir source
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
	git(t, repo, "branch", "docs-assets", "refs/remotes/origin/docs-assets")
	hydrator := installDocsScript(t, repo, "hydrate-assets.sh")
	cmd := exec.Command("bash", hydrator) //nolint:gosec // test-owned script installed under TempDir
	cmd.Dir = repo
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, string(output))
	hydrated, err := os.ReadFile(filepath.Join(repo, "docs", "assets", "screenshots", "web-ui", "workspace.png")) //nolint:gosec // test-owned path under TempDir
	require.NoError(t, err)
	assert.Equal(t, "old\n", string(hydrated))

	newSource := filepath.Join(tempDir, "new")
	writeDocsAssets(t, newSource, "new")
	newCommit := commitAssetTree(t, remote, newSource, "new docs assets")
	gitBare(t, remote, "update-ref", "refs/heads/docs-assets", newCommit)

	cmd = exec.Command("bash", hydrator, "--force") //nolint:gosec // test-owned script installed under TempDir
	cmd.Dir = repo
	output, err = cmd.CombinedOutput()
	require.NoError(t, err, string(output))

	hydrated, err = os.ReadFile(filepath.Join(repo, "docs", "assets", "screenshots", "web-ui", "workspace.png")) //nolint:gosec // test-owned path under TempDir
	require.NoError(t, err)
	assert.Equal(t, "new\n", string(hydrated))
	assert.Equal(t, newCommit, gitOutput(t, repo, "rev-parse", "origin/docs-assets"))
}

func TestDocsAssetHydratorUsesPinnedCommitWithoutRefreshingRemote(t *testing.T) {
	requireDocsScriptTools(t)

	tempDir := t.TempDir()
	remote := filepath.Join(tempDir, "remote.git")
	repo := filepath.Join(tempDir, "repo")
	git(t, tempDir, "init", "--bare", remote)
	initDocsTestRepo(t, repo)
	git(t, repo, "remote", "add", "origin", remote)

	pinnedSource := filepath.Join(tempDir, "pinned")
	writeDocsAssets(t, pinnedSource, "pinned")
	pinnedCommit := commitAssetTree(t, remote, pinnedSource, "pinned docs assets")
	gitBare(t, remote, "update-ref", "refs/heads/docs-assets", pinnedCommit)
	git(t, repo, "fetch", "origin", "docs-assets:refs/remotes/origin/docs-assets")

	replacementSource := filepath.Join(tempDir, "replacement")
	writeDocsAssets(t, replacementSource, "replacement")
	replacementCommit := commitAssetTree(t, remote, replacementSource, "replacement docs assets")
	gitBare(t, remote, "update-ref", "refs/heads/docs-assets", replacementCommit)

	hydrator := installDocsScript(t, repo, "hydrate-assets.sh")
	cmd := exec.Command("bash", hydrator, "--force") //nolint:gosec // test-owned script installed under TempDir
	cmd.Dir = repo
	cmd.Env = append(envWithout("KATA_DOCS_ASSETS_COMMIT"), "KATA_DOCS_ASSETS_COMMIT="+pinnedCommit)
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, string(output))

	hydrated, err := os.ReadFile(filepath.Join(repo, "docs", "assets", "screenshots", "web-ui", "workspace.png")) //nolint:gosec // test-owned path under TempDir
	require.NoError(t, err)
	assert.Equal(t, "pinned\n", string(hydrated))
	assert.Equal(t, pinnedCommit, gitOutput(t, repo, "rev-parse", "origin/docs-assets"))
	assert.Equal(t, replacementCommit, gitBareOutput(t, remote, nil, "rev-parse", "refs/heads/docs-assets"))
}

func TestWebScreenshotGeneratorRunsFocusedCapture(t *testing.T) {
	requireDocsScriptTools(t)

	tempDir := t.TempDir()
	fakeBin := filepath.Join(tempDir, "bin")
	outputDir := filepath.Join(tempDir, "output")
	argsLog := filepath.Join(tempDir, "bun-args")
	require.NoError(t, os.MkdirAll(fakeBin, 0o700))
	fakeBun := filepath.Join(fakeBin, "bun")
	//nolint:gosec // test-owned shell fixture must be executable.
	require.NoError(t, os.WriteFile(fakeBun, []byte(`#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >"$KATA_FAKE_BUN_ARGS"
mkdir -p "$KATA_DOCS_SCREENSHOT_DIR/web-ui"
for image in workspace issue-detail relationships daemon-switcher; do
  printf 'synthetic png\n' >"$KATA_DOCS_SCREENSHOT_DIR/web-ui/$image.png"
done
`), 0o700))

	script := filepath.Join("..", "docs", "screenshots", "generate-web-ui.sh")
	cmd := exec.Command("bash", script, "--out", outputDir) //nolint:gosec // fixed repository script and TempDir output
	cmd.Dir = filepath.Join("..", "web")
	cmd.Env = append(envWithout("PATH", "KATA_DOCS_SCREENSHOT_DIR", "KATA_FAKE_BUN_ARGS"),
		"PATH="+fakeBin+":/usr/bin:/bin",
		"KATA_FAKE_BUN_ARGS="+argsLog,
	)
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, string(output))

	args, err := os.ReadFile(argsLog) //nolint:gosec // test-owned path under TempDir
	require.NoError(t, err)
	assert.Equal(t, "run test:e2e -- tests/docs-screenshots.spec.ts\n", string(args))
	for _, image := range []string{"workspace", "issue-detail", "relationships", "daemon-switcher"} {
		info, err := os.Stat(filepath.Join(outputDir, "web-ui", image+".png"))
		require.NoError(t, err)
		assert.Positive(t, info.Size())
	}
}

func TestFederationScreenshotGeneratorConnectsToListenerAuthority(t *testing.T) {
	requireDocsScriptTools(t)
	for _, command := range []string{"freeze", "tmux"} {
		if _, err := exec.LookPath(command); err != nil {
			t.Skipf("%s not available: %v", command, err)
		}
	}

	tempDir := t.TempDir()
	outputDir := filepath.Join(tempDir, "output")
	fakeKata := filepath.Join(tempDir, "kata")
	//nolint:gosec // test-owned shell fixture must be executable.
	require.NoError(t, os.WriteFile(fakeKata, []byte(`#!/usr/bin/env bash
set -euo pipefail
if [[ " $* " == *" daemon start --foreground "* ]]; then
  printf 'kata daemon: listening on 127.0.0.1:45678\n'
  trap 'exit 0' TERM INT
  while :; do sleep 1; done
fi
if [[ " $* " == *" init "* && -n "${KATA_SERVER:-}" && "$KATA_SERVER" != "http://127.0.0.1:45678" ]]; then
  printf 'unexpected live docs daemon authority: %s\n' "$KATA_SERVER" >&2
  exit 9
fi
if [[ " $* " == *" create "* ]]; then
  printf 'OK create abc4\n'
  exit 0
fi
if [[ " $* " == *" tui "* ]]; then
  if [[ "${!#}" != "abc4" ]]; then
    printf 'docs TUI was not opened at the generated issue ref: %s\n' "${!#}" >&2
    exit 10
  fi
  printf '%s\n' 'ship federation TUI enrollment'
  IFS= read -r -n 1 _ || true
  printf '%s\n' 'review remote daemon catalog config'
  IFS= read -r -n 1 _ || true
  printf '%s\n' \
    'Project: demo-spoke-project' \
    'show selected spoke project everywhere' \
    'kata / federation' \
    'selected project: demo-spoke-project' \
    'Select local spoke project' \
    'Select hub daemon' \
    'Select hub project' \
    'demo-hub-project' \
    'Enrollment Preview' \
    'Operation: adopt existing local project' \
    'Confirm Adoption' \
    'Enrollment Result'
  trap 'exit 0' TERM INT
  while :; do sleep 1; done
fi
exit 0
`), 0o700))

	script := filepath.Join("..", "docs", "screenshots", "generate-federation-tui.sh")
	cmd := exec.Command("bash", script, "--out", outputDir) //nolint:gosec // fixed repository script and TempDir output
	cmd.Dir = filepath.Join("..", "scripts")
	cmd.Env = append(envWithout("KATA_BIN", "KATA_SERVER", "KATA_AUTH_TOKEN", "KATA_HOME"),
		"KATA_BIN="+fakeKata,
	)
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, string(output))
	for _, asset := range []string{
		"tui/hero.svg",
		"federation-tui/list.svg",
		"federation-tui/select-hub.svg",
		"federation-tui/select-hub-project.svg",
		"federation-tui/preview.svg",
		"federation-tui/result.svg",
	} {
		info, err := os.Stat(filepath.Join(outputDir, asset))
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
	require.NoError(t, os.MkdirAll(repo, 0o700))
	git(t, repo, "init")
	git(t, repo, "config", "user.name", "Example User")
	git(t, repo, "config", "user.email", "example@example.invalid")
	require.NoError(t, os.WriteFile(filepath.Join(repo, "README.md"), []byte("example repository\n"), 0o600))
	git(t, repo, "add", "README.md")
	git(t, repo, "commit", "-m", "initial fixture")
}

func installDocsScript(t *testing.T, repo, name string) string {
	t.Helper()
	destinationDir := filepath.Join(repo, "docs", "screenshots")
	require.NoError(t, os.MkdirAll(destinationDir, 0o700))
	for _, candidate := range []string{"assets.sh", name} {
		source := filepath.Join("..", "docs", "screenshots", candidate)
		contents, err := os.ReadFile(source) //nolint:gosec // candidate comes from the fixed script allowlist above
		if os.IsNotExist(err) && candidate == "assets.sh" {
			continue
		}
		require.NoError(t, err)
		//nolint:gosec // copied test shell fixtures must be executable.
		require.NoError(t, os.WriteFile(filepath.Join(destinationDir, candidate), contents, 0o700))
	}
	return filepath.Join(destinationDir, name)
}

func installDocsGeneratorFixture(t *testing.T, repo string) string {
	t.Helper()
	generator := installDocsScript(t, repo, "generate.sh")
	destinationDir := filepath.Dir(generator)
	for name, contents := range map[string]string{
		"generate-federation-tui.sh": `#!/usr/bin/env bash
set -euo pipefail
[[ "${1:-}" == "--out" && -n "${2:-}" ]]
mkdir -p "$2/tui" "$2/federation-tui"
printf 'svg\n' >"$2/tui/hero.svg"
for image in list select-hub select-hub-project preview result; do
  printf 'svg\n' >"$2/federation-tui/$image.svg"
done
`,
		"generate-web-ui.sh": `#!/usr/bin/env bash
set -euo pipefail
[[ "${1:-}" == "--out" && -n "${2:-}" ]]
mkdir -p "$2/web-ui"
for image in workspace issue-detail relationships daemon-switcher; do
  printf 'png\n' >"$2/web-ui/$image.png"
done
`,
	} {
		//nolint:gosec // copied test shell fixtures must be executable.
		require.NoError(t, os.WriteFile(filepath.Join(destinationDir, name), []byte(contents), 0o700))
	}
	return generator
}

func writeDocsAssets(t *testing.T, root, content string) {
	t.Helper()
	for _, asset := range docsAssets {
		path := filepath.Join(root, asset)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
		require.NoError(t, os.WriteFile(path, []byte(content+"\n"), 0o600))
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
	cmd := exec.Command("git", args...) //nolint:gosec // test-controlled Git arguments from fixed call sites
	cmd.Dir = dir
	require.Error(t, cmd.Run())
}

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...) //nolint:gosec // test-controlled Git arguments from fixed call sites
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, string(output))
}

func gitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...) //nolint:gosec // test-controlled Git arguments from fixed call sites
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
	cmd := exec.Command("git", fullArgs...) //nolint:gosec // test-controlled Git arguments and TempDir repositories
	if workTree != "" {
		cmd.Dir = workTree
	}
	if environment != nil {
		cmd.Env = environment
	}
	return cmd
}
