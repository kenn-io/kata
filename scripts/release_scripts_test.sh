#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
tmp_root="$(mktemp -d)"
trap 'rm -rf "$tmp_root"' EXIT

fail() {
  printf 'FAIL: %s\n' "$*" >&2
  exit 1
}

assert_contains() {
  local haystack="$1"
  local needle="$2"
  local context="$3"
  if [[ "$haystack" != *"$needle"* ]]; then
    fail "$context: expected to find [$needle] in [$haystack]"
  fi
}

assert_not_contains() {
  local haystack="$1"
  local needle="$2"
  local context="$3"
  if [[ "$haystack" == *"$needle"* ]]; then
    fail "$context: did not expect to find [$needle] in [$haystack]"
  fi
}

assert_file_contains() {
  local file="$1"
  local needle="$2"
  local context="$3"
  if ! grep -Fq -- "$needle" "$file"; then
    fail "$context: expected $file to contain [$needle]"
  fi
}

assert_file_not_contains() {
  local file="$1"
  local needle="$2"
  local context="$3"
  if grep -Fq -- "$needle" "$file"; then
    fail "$context: did not expect $file to contain [$needle]"
  fi
}

init_repo() {
  local dir="$1"
  mkdir -p "$dir"
  git -C "$dir" init -q
  git -C "$dir" config user.name "Example User"
  git -C "$dir" config user.email "example@example.test"
  printf 'example-workspace\n' >"$dir/README.md"
  git -C "$dir" add README.md
  git -C "$dir" commit -q -m "feat: add task list"
}

run_in_repo() {
  local dir="$1"
  shift
  (
    cd "$dir"
    "$@"
  )
}

test_release_rejects_missing_version() {
  local repo="$tmp_root/missing-version"
  init_repo "$repo"

  local output status
  set +e
  output="$(run_in_repo "$repo" "$repo_root/scripts/release.sh" 2>&1)"
  status=$?
  set -e

  [[ $status -ne 0 ]] || fail "release.sh without version should fail"
  assert_contains "$output" "usage:" "missing version"
}

test_release_rejects_v_prefixed_version() {
  local repo="$tmp_root/v-prefixed"
  init_repo "$repo"

  local output status
  set +e
  output="$(run_in_repo "$repo" "$repo_root/scripts/release.sh" v0.5.0 2>&1)"
  status=$?
  set -e

  [[ $status -ne 0 ]] || fail "release.sh should reject v-prefixed versions"
  assert_contains "$output" "bare version" "v-prefixed version"
}

test_release_rejects_non_semver_version() {
  local repo="$tmp_root/non-semver"
  init_repo "$repo"

  local output status
  set +e
  output="$(run_in_repo "$repo" "$repo_root/scripts/release.sh" 0.5 2>&1)"
  status=$?
  set -e

  [[ $status -ne 0 ]] || fail "release.sh should reject non-semver versions"
  assert_contains "$output" "X.Y.Z" "non-semver version"
}

test_release_refuses_dirty_worktree() {
  local repo="$tmp_root/dirty"
  init_repo "$repo"
  printf 'dirty\n' >"$repo/dirty.txt"

  local output status
  set +e
  output="$(run_in_repo "$repo" env CHANGELOG_AGENT=none "$repo_root/scripts/release.sh" 0.5.0 2>&1)"
  status=$?
  set -e

  [[ $status -ne 0 ]] || fail "release.sh should reject dirty worktrees"
  assert_contains "$output" "worktree is dirty" "dirty worktree"
  assert_not_contains "$output" "Changes" "dirty worktree should fail before changelog generation"
}

test_changelog_fallback_includes_first_commit_without_tags() {
  local repo="$tmp_root/changelog"
  init_repo "$repo"

  local output
  output="$(run_in_repo "$repo" env CHANGELOG_AGENT=none "$repo_root/scripts/changelog.sh" NEXT -)"

  assert_contains "$output" "### Changes" "fallback changelog heading"
  assert_contains "$output" "feat: add task list" "fallback changelog commit"
}

test_changelog_defaults_to_deterministic_fallback() {
  local repo="$tmp_root/changelog-default"
  local fake_bin="$tmp_root/fake-bin"
  init_repo "$repo"
  mkdir -p "$fake_bin"
  cat >"$fake_bin/codex" <<'EOF'
#!/usr/bin/env bash
echo "AI changelog was invoked"
EOF
  chmod +x "$fake_bin/codex"

  local output
  output="$(run_in_repo "$repo" env PATH="$fake_bin:$PATH" "$repo_root/scripts/changelog.sh" NEXT -)"

  assert_contains "$output" "### Changes" "default changelog heading"
  assert_contains "$output" "feat: add task list" "default changelog commit"
  assert_not_contains "$output" "AI changelog was invoked" "default changelog must not invoke agent"
}

test_changelog_allows_explicit_agent_opt_in() {
  local repo="$tmp_root/changelog-agent"
  local fake_bin="$tmp_root/fake-bin-agent"
  init_repo "$repo"
  mkdir -p "$fake_bin"
  cat >"$fake_bin/codex" <<'EOF'
#!/usr/bin/env bash
echo "AI changelog was invoked"
EOF
  chmod +x "$fake_bin/codex"

  local output
  output="$(run_in_repo "$repo" env PATH="$fake_bin:$PATH" CHANGELOG_AGENT=codex "$repo_root/scripts/changelog.sh" NEXT -)"

  assert_contains "$output" "AI changelog was invoked" "explicit changelog agent"
}

test_binary_install_script_contract() {
  local installer="$repo_root/scripts/install.sh"
  [[ -f "$installer" ]] || fail "binary install script is missing"

  assert_file_contains "$installer" 'REPO="kenn-io/kata"' "installer repository"
  assert_file_contains "$installer" 'BINARY_NAME="kata"' "installer binary name"
  assert_file_contains "$installer" 'https://github.com/${REPO}/releases/latest' "installer latest release discovery"
  assert_file_contains "$installer" 'filename="${BINARY_NAME}_${version#v}_${platform}.tar.gz"' "installer tar asset name"
  assert_file_contains "$installer" 'filename="${BINARY_NAME}_${version#v}_${platform}.zip"' "installer windows asset name"
  assert_file_contains "$installer" 'download "${base_url}/SHA256SUMS"' "installer checksum download"
  assert_file_contains "$installer" 'verify_checksum "$archive_path" "$tmpdir/SHA256SUMS" "$filename"' "installer checksum verification"
  assert_file_contains "$installer" 'KATA_SKIP_CHECKSUM=1' "installer checksum bypass escape hatch"
  assert_file_contains "$installer" 'kata update --check' "installer update follow-up"
  assert_file_contains "$installer" 'if [[ "${BASH_SOURCE[0]-}" == "${0}" || -z "${BASH_SOURCE[0]-}" ]]; then' "installer source guard"
}

test_install_redirect_and_docs_contract() {
  local vercel="$repo_root/docs/vercel.json"
  [[ -f "$vercel" ]] || fail "docs vercel config is missing"

  assert_file_contains "$vercel" '"source": "/install.sh"' "vercel install redirect source"
  assert_file_contains "$vercel" '"destination": "https://raw.githubusercontent.com/kenn-io/kata/main/scripts/install.sh"' "vercel install redirect destination"
  assert_file_contains "$vercel" '"permanent": false' "vercel install redirect permanence"
  assert_file_contains "$repo_root/docs/get-started/install.md" 'curl -fsSL https://katatracker.com/install.sh | bash' "install docs curl path"
  assert_file_contains "$repo_root/docs/get-started/install.md" 'The installer verifies the downloaded archive against `SHA256SUMS` before installing it.' "install docs checksum explanation"
  assert_file_contains "$repo_root/docs/get-started/install.md" 'Linux `.deb` and `.rpm` packages are also published for `amd64` and `arm64`.' "install docs Linux packages"
  assert_file_contains "$repo_root/README.md" 'curl -fsSL https://katatracker.com/install.sh | bash' "README curl path"
  assert_file_contains "$repo_root/README.md" 'Linux `.deb` and `.rpm` packages are published for `amd64` and `arm64`.' "README Linux packages"
  assert_file_not_contains "$repo_root/README.md" 'Pre-built binaries are not published yet.' "README stale binary wording"
}

test_release_creates_and_pushes_bare_semver_tag() {
  local repo="$tmp_root/release"
  local remote="$tmp_root/origin.git"
  init_repo "$repo"
  git init -q --bare "$remote"
  git -C "$repo" remote add origin "$remote"

  local output
  output="$(printf 'y\n' | run_in_repo "$repo" env CHANGELOG_AGENT=none "$repo_root/scripts/release.sh" 0.5.0)"

  assert_contains "$output" "Release v0.5.0" "release preview"
  git -C "$repo" rev-parse -q --verify refs/tags/v0.5.0 >/dev/null || fail "local tag v0.5.0 missing"
  git -C "$remote" rev-parse -q --verify refs/tags/v0.5.0 >/dev/null || fail "remote tag v0.5.0 missing"
}

test_release_workflow_contract() {
  local workflow="$repo_root/.github/workflows/release.yml"
  local publish_workflow="$repo_root/.github/workflows/publish-release.yml"
  local goreleaser="$repo_root/.goreleaser.yaml"
  [[ -f "$workflow" ]] || fail "release workflow is missing"
  [[ ! -f "$publish_workflow" ]] || fail "publish release workflow should be replaced by GoReleaser release workflow"
  [[ -f "$goreleaser" ]] || fail "GoReleaser config is missing"

  assert_file_contains "$workflow" "permissions:" "workflow permissions"
  assert_file_contains "$workflow" "contents: write" "workflow release permission"
  assert_file_contains "$workflow" 'TAG_NAME="${GITHUB_REF#refs/tags/}"' "workflow tag derivation"
  assert_file_contains "$workflow" 'version=${TAG_NAME#v}' "workflow bare version output"
  assert_file_contains "$workflow" 'gh release create "$TAG_NAME"' "workflow creates GitHub release"
  assert_file_contains "$workflow" "--notes-from-tag" "workflow uses annotated tag notes"
  assert_file_contains "$workflow" "goreleaser/goreleaser-action" "workflow runs GoReleaser"
  assert_file_contains "$workflow" "args: release --clean" "workflow GoReleaser release command"
  assert_file_not_contains "$workflow" "actions/upload-artifact" "workflow must not use custom archive upload"
  assert_file_not_contains "$workflow" "scripts/release-archive-name.sh" "workflow must not hand-build archive names"
  assert_file_not_contains "$workflow" "KATA_UPDATE_SIGNING_PRIVATE_KEY_HEX" "tag workflow must not read signing secret"
  assert_file_not_contains "$workflow" "sign-release-checksums.go" "tag workflow must not sign assets"
  assert_file_not_contains "$workflow" 'kata_v${VERSION}' "workflow must not use v-prefixed archive names"

  assert_file_contains "$goreleaser" "project_name: kata" "GoReleaser project name"
  assert_file_contains "$goreleaser" "main: ./cmd/kata" "GoReleaser main package"
  assert_file_contains "$goreleaser" "binary: kata" "GoReleaser binary name"
  assert_file_contains "$goreleaser" "- CGO_ENABLED=0" "GoReleaser static builds"
  assert_file_contains "$goreleaser" "go.kenn.io/kata/internal/version.Version=v{{ .Version }}" "GoReleaser version ldflag"
  assert_file_contains "$goreleaser" "go.kenn.io/kata/internal/version.Commit={{ .ShortCommit }}" "GoReleaser commit ldflag"
  assert_file_contains "$goreleaser" "go.kenn.io/kata/internal/version.BuildDate={{ .Date }}" "GoReleaser build date ldflag"
  assert_file_contains "$goreleaser" "{{- .Version }}_" "GoReleaser bare semver archive names"
  assert_file_contains "$goreleaser" "- linux" "GoReleaser linux target"
  assert_file_contains "$goreleaser" "- darwin" "GoReleaser darwin target"
  assert_file_contains "$goreleaser" "- windows" "GoReleaser windows target"
  assert_file_contains "$goreleaser" "- amd64" "GoReleaser amd64 target"
  assert_file_contains "$goreleaser" "- arm64" "GoReleaser arm64 target"
  assert_file_contains "$goreleaser" "name_template: SHA256SUMS" "GoReleaser checksum file"
  assert_file_contains "$goreleaser" "nfpms:" "GoReleaser Linux packages"
  assert_file_contains "$goreleaser" "- deb" "GoReleaser deb package"
  assert_file_contains "$goreleaser" "- rpm" "GoReleaser rpm package"
  assert_file_not_contains "$goreleaser" ".sha256.sig" "GoReleaser must not publish signature assets"

  local release_create_count
  release_create_count="$(grep -Fch 'gh release create "$TAG_NAME"' "$workflow" | awk '{sum += $1} END {print sum + 0}')"
  [[ "$release_create_count" == "1" ]] || fail "workflows should create the GitHub release exactly once, found $release_create_count"
}

test_release_rejects_missing_version
test_release_rejects_v_prefixed_version
test_release_rejects_non_semver_version
test_release_refuses_dirty_worktree
test_changelog_fallback_includes_first_commit_without_tags
test_changelog_defaults_to_deterministic_fallback
test_changelog_allows_explicit_agent_opt_in
test_binary_install_script_contract
test_install_redirect_and_docs_contract
test_release_creates_and_pushes_bare_semver_tag
test_release_workflow_contract

printf 'release script tests passed\n'
