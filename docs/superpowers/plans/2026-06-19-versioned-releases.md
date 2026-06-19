# Versioned Releases Implementation Plan

> **For agentic workers:** REQUIRED: Use superpowers:subagent-driven-development (if subagents available) or superpowers:executing-plans to implement this plan. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add kata's repeatable versioned release workflow, starting with `v0.5.0`, including release scripts, GitHub release artifacts, a kit-backed `kata update` command, TUI version coverage, and changelog documentation.

**Architecture:** Keep release artifact naming in a tiny tested helper that both the release workflow and tests can use, so kit self-update compatibility is pinned to code. Use `go.kenn.io/kit/selfupdate` for update discovery and installation, with kata owning only CLI output, prompts, cache path, and error wrapping. Keep changelog generation script-local with an AI path plus deterministic fallback.

**Tech Stack:** Go 1.26, Cobra, Bubble Tea/Lip Gloss TUI, `go.kenn.io/kit/selfupdate`, Bash release scripts, GitHub Actions, Zensical docs.

---

## File Structure

- Create: `internal/release/assets.go` for release asset-name and platform helpers.
- Create: `internal/release/assets_test.go` to compare kata asset names against `selfupdate.DefaultAssetName`.
- Create: `scripts/changelog.sh` for release-note generation and fallback changelog output.
- Create: `scripts/release.sh` for local tag creation and push.
- Create: `scripts/release-archive-name.sh` so shell workflows use the same tested naming logic.
- Create: `scripts/release_scripts_test.sh` for shell-level script behavior tests.
- Modify: `Makefile` to add a `release-scripts-test` target if useful for local verification.
- Create: `.github/workflows/release.yml` for tag-triggered artifact publishing.
- Create: `cmd/kata/update.go` for the `kata update` command.
- Create: `cmd/kata/update_test.go` for CLI output, exit, and injected updater tests.
- Modify: `cmd/kata/main.go` to register `newUpdateCmd()`.
- Modify: `cmd/kata/version_test.go` to remove the private sibling-project test name.
- Modify: `internal/tui/list_render_test.go` and/or `internal/tui/detail_redesign_test.go` for tagged version coverage.
- Create: `docs/reference/changelog.md`.
- Modify: `docs/zensical.toml`, `docs/get-started/install.md`, `docs/index.md`, and `scripts/check-docs.sh` for changelog and release install docs.

## Task 1: Pin Release Asset Naming

**Files:**
- Create: `internal/release/assets.go`
- Create: `internal/release/assets_test.go`
- Create: `scripts/release-archive-name.sh`
- Test: `internal/release/assets_test.go`

- [ ] **Step 1: Write the failing Go test**

Create `internal/release/assets_test.go` with a table that compares kata-generated names to kit defaults:

```go
package release

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"go.kenn.io/kit/selfupdate"
)

func TestAssetNameMatchesKitDefault(t *testing.T) {
	for _, tc := range []struct {
		goos, goarch, ext string
	}{
		{"linux", "amd64", ".tar.gz"},
		{"linux", "arm64", ".tar.gz"},
		{"darwin", "amd64", ".tar.gz"},
		{"darwin", "arm64", ".tar.gz"},
		{"windows", "amd64", ".zip"},
		{"windows", "arm64", ".zip"},
	} {
		t.Run(tc.goos+"_"+tc.goarch, func(t *testing.T) {
			req := selfupdate.AssetRequest{
				BinaryName: "kata",
				Version:    "0.5.0",
				GOOS:       tc.goos,
				GOARCH:     tc.goarch,
				Extension:  tc.ext,
			}
			assert.Equal(t, selfupdate.DefaultAssetName(req), AssetName("0.5.0", tc.goos, tc.goarch))
		})
	}
}

func TestAssetNameUsesBareSemver(t *testing.T) {
	assert.Equal(t, "kata_0.5.0_linux_amd64.tar.gz", AssetName("0.5.0", "linux", "amd64"))
	assert.NotContains(t, AssetName("0.5.0", "linux", "amd64"), "v0.5.0")
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/release`

Expected: FAIL because `internal/release` does not exist.

- [ ] **Step 3: Implement the minimal helper**

Create `internal/release/assets.go`:

```go
package release

import "go.kenn.io/kit/selfupdate"

const BinaryName = "kata"

func AssetName(version, goos, goarch string) string {
	ext := ".tar.gz"
	if goos == "windows" {
		ext = ".zip"
	}
	return selfupdate.DefaultAssetName(selfupdate.AssetRequest{
		BinaryName: BinaryName,
		Version:    version,
		GOOS:       goos,
		GOARCH:     goarch,
		Extension:  ext,
	})
}
```

- [ ] **Step 4: Add shell bridge**

Create executable `scripts/release-archive-name.sh`:

```bash
#!/usr/bin/env bash
set -euo pipefail

if [[ $# -ne 3 ]]; then
  printf 'usage: %s <bare-version> <goos> <goarch>\n' "$0" >&2
  exit 2
fi

version="$1"
goos="$2"
goarch="$3"
ext=".tar.gz"
if [[ "$goos" == "windows" ]]; then
  ext=".zip"
fi

printf 'kata_%s_%s_%s%s\n' "$version" "$goos" "$goarch" "$ext"
```

Then extend `internal/release/assets_test.go` with an `exec.Command` test that runs the shell script and compares its output to `AssetName`.

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/release`

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/release scripts/release-archive-name.sh
git commit -m "build: pin release asset names"
```

## Task 2: Add Changelog And Release Scripts

**Files:**
- Create: `scripts/changelog.sh`
- Create: `scripts/release.sh`
- Create: `scripts/release_scripts_test.sh`
- Modify: `Makefile`
- Test: `scripts/release_scripts_test.sh`

- [ ] **Step 1: Write failing shell tests**

Create `scripts/release_scripts_test.sh` with temp Git repositories and assertions for:

- `scripts/release.sh` rejects missing version.
- `scripts/release.sh` rejects `v0.5.0` and accepts only bare `0.5.0` shape.
- `scripts/release.sh` refuses a dirty worktree before attempting changelog generation.
- `scripts/changelog.sh NEXT -` handles a repo with no prior tags and includes the first commit in fallback mode.
- `scripts/changelog.sh` supports deterministic fallback via `CHANGELOG_AGENT=none`.

Use neutral repo data such as `example-workspace` and commit subjects like `feat: add task list`.

- [ ] **Step 2: Run tests to verify they fail**

Run: `bash scripts/release_scripts_test.sh`

Expected: FAIL because the scripts do not exist.

- [ ] **Step 3: Implement `scripts/changelog.sh`**

Implement:

- `set -euo pipefail`.
- Arguments: `VERSION="${1:-NEXT}"`, `START_TAG="${2:-}"`, `EXTRA_INSTRUCTIONS="${3:-}"`.
- Range selection:
  - explicit start tag: `START_TAG..HEAD`;
  - detected previous tag: `PREV_TAG..HEAD`;
  - no tag: no range for `git log`, and diff stat from empty tree.
- Agent selection:
  - `CHANGELOG_AGENT=codex` default when `codex` exists;
  - `CHANGELOG_AGENT=claude` when requested;
  - `CHANGELOG_AGENT=none` or missing agent falls back to deterministic grouped commit output.
- Prompt text must say kata is a local-first issue tracker and must not mention private sibling repo names.

Fallback output should be valid Markdown, for example:

```markdown
### Changes

- feat: add task list (abc1234)
- fix: handle empty workspace (def5678)
```

- [ ] **Step 4: Implement `scripts/release.sh`**

Implement:

- Validate bare `X.Y.Z`.
- Refuse existing tag `v$VERSION`.
- Refuse dirty worktree with `git diff-index --quiet HEAD --`.
- Generate changelog to a temp file with `scripts/changelog.sh "$VERSION" "-" "$EXTRA_INSTRUCTIONS"`.
- Preview and confirm.
- Create annotated tag `v$VERSION` with title `Release $VERSION` and changelog body.
- Push to `origin`.

- [ ] **Step 5: Add Makefile target**

Add `release-scripts-test` to `.PHONY` and implement:

```make
release-scripts-test:
	bash scripts/release_scripts_test.sh
```

- [ ] **Step 6: Run tests to verify they pass**

Run: `bash scripts/release_scripts_test.sh`

Expected: PASS.

Run: `make release-scripts-test`

Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add Makefile scripts/changelog.sh scripts/release.sh scripts/release_scripts_test.sh
git commit -m "build: add release scripts"
```

## Task 3: Add GitHub Release Workflow

**Files:**
- Create: `.github/workflows/release.yml`
- Modify: `scripts/release_scripts_test.sh`
- Test: `scripts/release_scripts_test.sh`, `go test ./internal/release`

- [ ] **Step 1: Extend failing tests for workflow contracts**

In `scripts/release_scripts_test.sh`, add checks that `.github/workflows/release.yml`:

- exists;
- contains `permissions:` and `contents: write`;
- derives `TAG_NAME="${GITHUB_REF#refs/tags/}"`;
- derives `VERSION="${TAG_NAME#v}"`;
- calls `scripts/release-archive-name.sh "$VERSION" "$GOOS" "$GOARCH"`;
- includes `SHA256SUMS`;
- does not use `v${VERSION}` in archive names.

Run: `bash scripts/release_scripts_test.sh`

Expected: FAIL because workflow does not exist.

- [ ] **Step 2: Implement workflow**

Create `.github/workflows/release.yml`:

- Trigger on `push.tags: ['v*']`.
- Set `permissions: contents: write`.
- Validate `^v[0-9]+\.[0-9]+\.[0-9]+$`.
- Build matrix:
  - Linux `amd64` and `arm64` on Ubuntu runners.
  - macOS `amd64` and `arm64` on macOS.
  - Windows `amd64` and `arm64` only if runner/toolchain support is practical; otherwise start with `amd64` and document `arm64` as intentionally deferred in a workflow comment.
- Build with:

```bash
TAG_NAME="${GITHUB_REF#refs/tags/}"
VERSION="${TAG_NAME#v}"
COMMIT="$(printf '%s' "$GITHUB_SHA" | cut -c1-8)"
BUILD_DATE="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
LDFLAGS="-s -w -X go.kenn.io/kata/internal/version.Version=${TAG_NAME} -X go.kenn.io/kata/internal/version.Commit=${COMMIT} -X go.kenn.io/kata/internal/version.BuildDate=${BUILD_DATE}"
```

- Archive using `scripts/release-archive-name.sh "$VERSION" "$GOOS" "$GOARCH"`.
- Validate archive contents.
- Smoke-test native Linux `amd64` archive with `kata version` and grep for `$TAG_NAME`.
- Generate `SHA256SUMS`.
- Read annotated tag body.
- Use a pinned release action or `gh release create` with `GH_TOKEN: ${{ github.token }}`.

- [ ] **Step 3: Run tests**

Run: `bash scripts/release_scripts_test.sh`

Expected: PASS.

Run: `go test ./internal/release`

Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add .github/workflows/release.yml scripts/release_scripts_test.sh
git commit -m "ci: publish tagged releases"
```

## Task 4: Add Kit-Backed Update Command

**Files:**
- Create: `cmd/kata/update.go`
- Create: `cmd/kata/update_test.go`
- Modify: `cmd/kata/main.go`
- Test: `cmd/kata/update_test.go`, `cmd/kata/main_test.go`

- [ ] **Step 1: Write failing registration and output tests**

In `cmd/kata/update_test.go`, add tests for:

- root registers `update`;
- `kata update --check` prints "kata is up to date" when injected client returns nil;
- `kata update --check` exits 0 and prints current/latest when update exists;
- `kata update --check --json` emits `kata_api_version`, `current_version`, `latest_version`, `update_available`;
- `kata update --check --agent` emits stable fields such as `OK update update_available=true current=v0.4.0 latest=v0.5.0`;
- `kata update` refetches with `Force: true` before install when the cached-info predicate returns true;
- install errors from non-writable destinations are wrapped into a clear `cliError` with `ExitInternal`;
- the constructed default client uses `Owner: "kenn-io"`, `Repo: "kata"`, `BinaryName: "kata"`, `AllowUnsignedChecksums: true`.

Use an injected fake client. Do not call live GitHub. Because kit's cached-info marker is unexported, add a package variable such as `updateInfoNeedsRefetch = func(info *selfupdate.Info) bool { return info.NeedsRefetch() }` and override it in the refetch test.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./cmd/kata -run 'Update|Root.*update'`

Expected: FAIL because `newUpdateCmd` does not exist.

- [ ] **Step 3: Implement update client seam**

In `cmd/kata/update.go`, define:

```go
type updateClient interface {
	Check(context.Context, selfupdate.CheckOptions) (*selfupdate.Info, error)
	Install(context.Context, *selfupdate.Info, selfupdate.InstallOptions) error
}

var newSelfUpdateClient = func(current string) (updateClient, error) {
	home, err := config.KataHome()
	if err != nil {
		return nil, err
	}
	return selfupdate.Client{
		Owner:                  "kenn-io",
		Repo:                   "kata",
		BinaryName:             "kata",
		CurrentVersion:         current,
		CacheDir:               filepath.Join(home, "cache", "update"),
		AllowUnsignedChecksums: true,
	}, nil
}
```

- [ ] **Step 4: Implement command behavior**

Implement `newUpdateCmd()` with flags:

- `--check`: check only, no install.
- `--force`, `-f`: pass `selfupdate.CheckOptions{Force: true}` and permit replacing dev builds.
- `--yes`, `-y`: skip confirmation prompt for install.

Output contract:

- Human check no update: `kata is up to date (<current>)`.
- Human check update: `update available: <current> -> <latest>`.
- JSON: object with `current_version`, `latest_version`, `update_available`, `asset_name`, `is_dev_build`.
- Agent: `OK update update_available=<bool> current=<current> latest=<latest-or-empty>`.

Install path:

- Call `Check`.
- If nil, print up-to-date and return nil.
- If `--check`, print available and return nil.
- If `updateInfoNeedsRefetch(info)`, call `Check` again with `Force: true`.
- Prompt unless `--yes` or non-human mode.
- Call `Install`.
- Wrap install errors as `cliError{Kind: kindInternal, ExitCode: ExitInternal}` with message `install update: ...`.

- [ ] **Step 5: Register command**

Add `newUpdateCmd()` to the `subs` slice in `cmd/kata/main.go` near `newVersionCmd()`.

- [ ] **Step 6: Run tests**

Run: `go test ./cmd/kata -run 'Update|Root.*update|Version'`

Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add cmd/kata/main.go cmd/kata/update.go cmd/kata/update_test.go
git commit -m "feat: add self update command"
```

## Task 5: Tighten Version And TUI Tests

**Files:**
- Modify: `cmd/kata/version_test.go`
- Modify: `internal/tui/list_render_test.go`
- Modify: `internal/tui/detail_redesign_test.go`
- Test: `cmd/kata/version_test.go`, `internal/tui/*`

- [ ] **Step 1: Write failing or tightening tests**

Rename the existing version human-format test to a neutral name such as `TestVersion_HumanFormatIncludesBuildMetadata`.

Add tests:

```go
func TestTitleBarShowsTaggedVersion(t *testing.T) {
	useNoColor(t)
	got := stripANSI(renderTitleBar(100, scope{projectName: "kata"}, "v0.5.0"))
	assertContains(t, got, "kata カタ · v0.5.0", "title bar missing tagged version")
}
```

Extend detail and split title-bar tests to use `v0.5.0` and assert it appears.

- [ ] **Step 2: Run tests to verify current behavior**

Run: `go test ./cmd/kata -run Version`

Expected: PASS after rename only.

Run: `go test ./internal/tui -run 'TitleBar.*Version|DetailTitleBar|SplitTitleBar'`

Expected: PASS if existing implementation already renders the version.

- [ ] **Step 3: Implement only if needed**

If tests fail because version is absent in a view, update the relevant `renderTitleBar` caller to pass `chrome.version`. Do not add a second version display.

- [ ] **Step 4: Run tests**

Run: `go test ./cmd/kata -run Version`

Run: `go test ./internal/tui -run 'TitleBar|DetailTitleBar|SplitTitleBar'`

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cmd/kata/version_test.go internal/tui/list_render_test.go internal/tui/detail_redesign_test.go
git commit -m "test: pin release version display"
```

## Task 6: Add Changelog And Install Documentation

**Files:**
- Create: `docs/reference/changelog.md`
- Modify: `docs/zensical.toml`
- Modify: `docs/get-started/install.md`
- Modify: `docs/index.md`
- Modify: `scripts/check-docs.sh`
- Test: `scripts/check-docs.sh`, docs build if available

- [ ] **Step 1: Write failing docs checks**

Modify `scripts/check-docs.sh` required files and content checks to require:

- `docs/reference/changelog.md`
- nav entry `{"Changelog" = "reference/changelog.md"}`
- install docs mention GitHub release binaries and `kata update`
- changelog docs mention `v0.5.0`

Run: `bash scripts/check-docs.sh`

Expected: FAIL because docs are missing.

- [ ] **Step 2: Add changelog page**

Create `docs/reference/changelog.md`:

```markdown
# Changelog

## Releases

### v0.5.0

The first versioned kata release. Release notes will be generated from the annotated tag when the release is cut.

## Project History

### 2026-W18

- Bootstrapped kata's local-first issue tracker architecture, project binding, SQLite storage, daemon API, and CLI lifecycle commands.

...
```

Generate week-by-week entries from `git log --date=short --pretty=format:'%ad%x09%s' --no-merges`, but keep the final prose curated and user-facing. Do not include private workspace names.

- [ ] **Step 3: Update docs nav and install docs**

In `docs/zensical.toml`, add `{"Changelog" = "reference/changelog.md"}` under `Reference`.

In `docs/get-started/install.md`, replace "Pre-built release binaries are not published yet" with:

- GitHub releases are available after `v0.5.0`.
- `go install go.kenn.io/kata/cmd/kata@latest` remains supported.
- `kata update --check` checks for updates.
- `kata update` installs an available release from GitHub release assets.

In `docs/index.md`, update preview language if needed to reflect versioned pre-1.0 releases instead of no stable release process.

- [ ] **Step 4: Run docs checks**

Run: `bash scripts/check-docs.sh`

Expected: PASS. If it invokes docs build and dependencies are missing, run `make docs-install` or record the missing dependency.

- [ ] **Step 5: Commit**

```bash
git add docs/reference/changelog.md docs/zensical.toml docs/get-started/install.md docs/index.md scripts/check-docs.sh
git commit -m "docs: add release changelog"
```

## Task 7: Final Verification

**Files:**
- No new files unless verification reveals a bug.

- [ ] **Step 1: Run focused tests**

Run:

```bash
go test ./internal/release
go test ./cmd/kata -run 'Update|Version|Root'
go test ./internal/tui -run 'TitleBar|DetailTitleBar|SplitTitleBar'
bash scripts/release_scripts_test.sh
```

Expected: PASS.

- [ ] **Step 2: Run e2e build-version verification**

Run:

```bash
go test ./e2e -run 'MakeBuild|MakeInstall' -count=1
```

Expected: PASS and non-vacuous test output including the three `TestMake...` tests.

- [ ] **Step 3: Run docs check**

Run:

```bash
make docs-check
```

Expected: PASS.

- [ ] **Step 4: Run broader relevant suite**

Run:

```bash
go test ./cmd/kata ./internal/tui ./internal/release
```

Expected: PASS.

- [ ] **Step 5: Check worktree**

Run: `git status --short`

Expected: clean.

- [ ] **Step 6: Update kata issue**

Comment on `yevv` with:

- commits made;
- test commands run and results;
- whether the first release can be cut with `scripts/release.sh 0.5.0`.

Do not close `yevv` until the implementation is complete and verified.
