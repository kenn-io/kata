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
  [[ -f "$workflow" ]] || fail "release workflow is missing"
  [[ -f "$publish_workflow" ]] || fail "publish release workflow is missing"

  assert_file_contains "$workflow" "permissions:" "workflow permissions"
  assert_file_contains "$workflow" "contents: read" "workflow read-only permission"
  assert_file_contains "$workflow" "actions/upload-artifact" "workflow build artifact upload"
  assert_file_contains "$workflow" 'TAG_NAME="${GITHUB_REF#refs/tags/}"' "workflow tag derivation"
  assert_file_contains "$workflow" 'VERSION="${TAG_NAME#v}"' "workflow bare version derivation"
  assert_file_contains "$workflow" 'scripts/release-archive-name.sh "$VERSION" "$GOOS" "$GOARCH"' "workflow archive naming"
  assert_file_not_contains "$workflow" "KATA_UPDATE_SIGNING_PRIVATE_KEY_HEX" "tag workflow must not read signing secret"
  assert_file_not_contains "$workflow" "sign-release-checksums.go" "tag workflow must not sign assets"
  assert_file_not_contains "$workflow" "gh release create" "tag workflow must not publish releases"
  assert_file_not_contains "$workflow" 'kata_v${VERSION}' "workflow must not use v-prefixed archive names"

  assert_file_contains "$publish_workflow" "workflow_dispatch:" "publish workflow manual trigger"
  assert_file_contains "$publish_workflow" "tag:" "publish workflow tag input"
  assert_file_not_contains "$publish_workflow" "workflow_run:" "publish workflow must not auto-publish tag builds"
  assert_file_not_contains "$publish_workflow" "github.event.workflow_run" "publish workflow must not trust tag-triggered workflow events"
  assert_file_contains "$publish_workflow" "contents: write" "publish workflow release permission"
  assert_file_contains "$publish_workflow" "actions: read" "publish workflow artifact permission"
  assert_file_contains "$publish_workflow" "environment: release-signing" "publish workflow protected environment"
  assert_file_contains "$publish_workflow" "fetch-depth: 0" "publish workflow annotated tag checkout"
  assert_file_contains "$publish_workflow" "fetch-tags: true" "publish workflow tag fetching"
  assert_file_contains "$publish_workflow" 'TAG_NAME="${{ inputs.tag }}"' "publish workflow tag input use"
  assert_file_contains "$publish_workflow" 'git rev-list -n1 "$TAG_NAME"' "publish workflow tag target lookup"
  assert_file_contains "$publish_workflow" 'git merge-base --is-ancestor "$tag_sha" origin/main' "publish workflow protected branch verification"
  assert_file_contains "$publish_workflow" 'actions/workflows/release.yml/runs?event=push&status=completed&per_page=100' "publish workflow release build lookup"
  assert_file_contains "$publish_workflow" '.head_branch == \"$TAG_NAME\"' "publish workflow build tag filter"
  assert_file_contains "$publish_workflow" '.head_sha == \"$tag_sha\"' "publish workflow build sha filter"
  assert_file_contains "$publish_workflow" 'gh run download "$RUN_ID"' "publish workflow downloads build artifacts"
  assert_file_contains "$publish_workflow" 'sha256sum "${archives[@]}" > SHA256SUMS' "publish workflow aggregate checksums"
  assert_file_contains "$publish_workflow" "KATA_UPDATE_SIGNING_PRIVATE_KEY_HEX" "publish workflow signing key secret"
  assert_file_contains "$publish_workflow" "sign-release-checksums.go" "publish workflow checksum signature script"
  assert_file_contains "$publish_workflow" 'signatures=( ./*.sha256.sig )' "publish workflow signature assets"
  assert_file_contains "$publish_workflow" 'gh release create "$TAG_NAME" "${release_assets[@]}" ./SHA256SUMS "${signatures[@]}" --notes-file notes.md --verify-tag' "publish workflow release create"
  assert_file_not_contains "$publish_workflow" 'gh release create "$TAG_NAME" ./kata_*' "publish workflow must not duplicate signature assets"

  local release_create_count
  release_create_count="$(grep -Fch 'gh release create "$TAG_NAME"' "$workflow" "$publish_workflow" | awk '{sum += $1} END {print sum + 0}')"
  [[ "$release_create_count" == "1" ]] || fail "workflows should create the GitHub release exactly once, found $release_create_count"
}

test_release_rejects_missing_version
test_release_rejects_v_prefixed_version
test_release_rejects_non_semver_version
test_release_refuses_dirty_worktree
test_changelog_fallback_includes_first_commit_without_tags
test_changelog_defaults_to_deterministic_fallback
test_changelog_allows_explicit_agent_opt_in
test_release_creates_and_pushes_bare_semver_tag
test_release_workflow_contract

printf 'release script tests passed\n'
