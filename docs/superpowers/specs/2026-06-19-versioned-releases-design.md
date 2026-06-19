# Versioned Releases Design

## Goal

Ship kata as versioned releases starting at `v0.5.0`, with a repeatable local release command, generated changelog text, published GitHub release artifacts, a `kata update` command backed by `go.kenn.io/kit/selfupdate`, and documentation that includes both future release notes and a retroactive project history.

## Current State

kata already has the foundations for release identity:

- `make build` and `make install` bake `git describe --tags --always --dirty` into `go.kenn.io/kata/internal/version.Version`.
- `kata version` reports build version, commit, build date, Go version, and target platform.
- The daemon health responses carry the build version.
- The TUI chrome already receives `internal/tui.kataVersion` and renders it in the title bar.
- `go.kenn.io/kit` is already a dependency and includes `selfupdate` support for GitHub release discovery, checksum validation, archive extraction, and binary replacement.

The missing pieces are the release scripts, release publishing workflow, update command wiring, release documentation, and tests that pin the expected release behavior.

## Release Flow

Add `scripts/changelog.sh` and `scripts/release.sh`.

`scripts/changelog.sh` will use a self-contained variant of the existing local release-script pattern:

- Accept `[version] [start_tag] [extra_instructions]`.
- Default `version` to `NEXT`.
- Auto-detect the previous tag when `start_tag` is empty or `-`.
- If no tag exists, use the full commit history rather than excluding the first commit.
- Feed commit subjects and diff stats to `codex exec` by default, with `CHANGELOG_AGENT=claude` as an alternate.
- Provide a non-AI fallback mode that emits grouped commit subjects so release creation is not blocked when no agent CLI is installed.
- Instruct the agent to output only concise, user-focused Markdown grouped into sections.
- Avoid private repository or workspace names in prompts, tests, and generated fixtures.

`scripts/release.sh` will:

- Require a semantic version argument without the `v` prefix.
- Validate `X.Y.Z`; the first intended release is `0.5.0`.
- Refuse to run if `vX.Y.Z` already exists or the worktree is dirty.
- Generate a changelog with `scripts/changelog.sh`.
- Preview the changelog and ask for confirmation.
- Create an annotated tag whose message body is the accepted changelog.
- Push the tag to `origin`.

The scripts should stay project-local and should not depend on packaging systems that kata does not yet ship.

## Release Publishing

Add `.github/workflows/release.yml` triggered by `v*` tags.

The workflow will:

- Validate tags match `vMAJOR.MINOR.PATCH`.
- Build release archives for Linux, macOS, and Windows on supported `amd64` and `arm64` targets where runner support is practical.
- Declare `permissions: contents: write` because the workflow creates the GitHub release.
- Derive `TAG_NAME` as the full `v0.5.0` tag and `VERSION` as the bare `0.5.0` semver. Name assets with kit's default convention using the bare semver: `kata_${VERSION}_${GOOS}_${GOARCH}.tar.gz`, with `.zip` for Windows.
- Set ldflags for `Version=${TAG_NAME}`, `Commit`, and `BuildDate`. This intentionally differs from the Makefile, which sets only `Version` and lets Go VCS build info populate `Commit` and `BuildDate` for local builds.
- Smoke-test at least native binaries with `kata version` and assert the tag appears.
- Validate archive contents before publishing.
- Generate `SHA256SUMS`.
- Create or update the GitHub release using the annotated tag body as release notes.

This gives `kit/selfupdate` exactly the asset shape it expects without custom asset-name code.

## Self Update

Add a `kata update` command that uses `go.kenn.io/kit/selfupdate` instead of local update logic.

The command will:

- Construct a `selfupdate.Client` with kata's release owner, repository, binary name, current version, and a kata cache directory.
- Use the real GitHub release coordinate, `Owner: "kenn-io"` and `Repo: "kata"`; do not derive this from the Go module path.
- Set `AllowUnsignedChecksums: true` initially because the release workflow will publish archives plus `SHA256SUMS`, not signed update metadata.
- Support a `--check` mode that reports whether an update is available without installing.
- Support `--force` so a development build can be replaced with the latest official release.
- Refetch cached update metadata before installing when `Info.NeedsRefetch()` is true.
- Print concise human output by default, JSON under `--json`, and stable agent output under `--agent`.
- Define `--check` exit semantics explicitly: success means the check ran, regardless of whether an update is available; validation/network failures use existing kata error exit codes. Agent and JSON output carry the update-available boolean.
- Surface non-writable install destinations as clear install errors. kit performs atomic replacement but does not escalate privileges.
- Keep install prompts and command-line UX in kata; only discovery, verification, download, extraction, and install mechanics come from kit.

The first implementation should not invent signing. Once release signing exists, the command can switch from unsigned checksum allowance to trusted public keys.

## TUI Version Display

The TUI already routes `version.Version` into the shared title-bar chrome. The release work should make that behavior explicit and protected:

- Add or tighten tests showing a tagged version such as `v0.5.0` appears in list, detail, and split title bars.
- Keep the existing behavior that an empty version hides the version slot.
- Do not add a separate version footer or settings pane; the title bar is already the right density for this information.

## Changelog Documentation

Add `docs/reference/changelog.md` and link it from the docs navigation if required by the docs system.

The document will have two sections:

- `## Releases`, starting with `v0.5.0` once the first release is cut.
- `## Project History`, a retroactive week-by-week summary generated from pre-release commits.

Add a script mode or companion helper to produce the retroactive history from commit ranges grouped by ISO week. The generated text should stay user-facing: major features, behavior changes, operational capabilities, and notable fixes. It should omit internal review churn and transient bugs fixed inside the same period.

## Tests And Verification Strategy

Use test-first changes for production behavior:

- Script tests should exercise argument validation, no-tag changelog range handling, and dirty-worktree refusal where practical.
- Release workflow syntax can be checked with targeted shell validation and smoke-tested locally for script behavior. Tests must pin the bare-semver asset name contract by comparing generated names with `selfupdate.DefaultAssetName(selfupdate.AssetRequest{BinaryName: "kata", Version: "0.5.0", ...})` for the published platforms.
- `kata update` needs unit tests around check/install command behavior using fake HTTP servers or injected kit clients. Tests must not call live GitHub.
- TUI version tests should render existing views with `v0.5.0` and assert title-bar output.
- Existing build-version e2e tests continue to protect `make build` and `make install`.

Before completing the implementation, run at least:

- `go test ./cmd/kata ./internal/tui`
- `go test ./e2e -run 'MakeBuild|MakeInstall'`
- script-specific shell tests or dry-runs added with the implementation
- `make docs-check` after documentation changes

## Rollout

The first release should be `v0.5.0`.

Cutting the first release will look like:

```bash
scripts/release.sh 0.5.0
```

After the tag push, GitHub Actions publishes archives, checksums, and release notes. Users can then install from release assets, keep using `go install`, or use `kata update` once they have a release-capable binary installed.
