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
  if ! grep -Fq "$needle" "$file"; then
    fail "$context: expected $file to contain [$needle]"
  fi
}

assert_file_not_contains() {
  local file="$1"
  local needle="$2"
  local context="$3"
  if grep -Fq "$needle" "$file"; then
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
  [[ -f "$workflow" ]] || fail "release workflow is missing"

  assert_file_contains "$workflow" "permissions:" "workflow permissions"
  assert_file_contains "$workflow" "contents: write" "workflow release permission"
  assert_file_contains "$workflow" "actions/upload-artifact" "workflow build artifact upload"
  assert_file_contains "$workflow" "actions/download-artifact" "workflow release artifact download"
  assert_file_contains "$workflow" "needs: build" "workflow dependent release job"
  assert_file_contains "$workflow" 'TAG_NAME="${GITHUB_REF#refs/tags/}"' "workflow tag derivation"
  assert_file_contains "$workflow" 'VERSION="${TAG_NAME#v}"' "workflow bare version derivation"
  assert_file_contains "$workflow" 'scripts/release-archive-name.sh "$VERSION" "$GOOS" "$GOARCH"' "workflow archive naming"
  assert_file_contains "$workflow" 'sha256sum kata_* > SHA256SUMS' "workflow aggregate checksums"
  assert_file_contains "$workflow" "KATA_UPDATE_SIGNING_PRIVATE_KEY_HEX" "workflow signing key secret"
  assert_file_contains "$workflow" "sign-release-checksums.go" "workflow checksum signature script"
  assert_file_contains "$workflow" ".sha256.sig" "workflow signature assets"
  assert_file_contains "$workflow" "fetch-depth: 0" "workflow annotated tag checkout"
  assert_file_contains "$workflow" "fetch-tags: true" "workflow tag fetching"
  assert_file_contains "$workflow" 'gh release create "$TAG_NAME" ./kata_* ./SHA256SUMS ./*.sha256.sig --notes-file notes.md' "workflow release create"
  assert_file_not_contains "$workflow" 'kata_v${VERSION}' "workflow must not use v-prefixed archive names"

  local release_create_count
  release_create_count="$(grep -Fc 'gh release create "$TAG_NAME"' "$workflow")"
  [[ "$release_create_count" == "1" ]] || fail "workflow should create the GitHub release exactly once, found $release_create_count"
}

test_release_rejects_missing_version
test_release_rejects_v_prefixed_version
test_release_rejects_non_semver_version
test_release_refuses_dirty_worktree
test_changelog_fallback_includes_first_commit_without_tags
test_release_creates_and_pushes_bare_semver_tag
test_release_workflow_contract

printf 'release script tests passed\n'
