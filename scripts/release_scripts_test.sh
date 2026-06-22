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

test_verify_release_tag_rejects_tag_outside_origin_main() {
  local repo="$tmp_root/release-tag-policy"
  local remote="$tmp_root/release-tag-policy.git"
  init_repo "$repo"
  git -C "$repo" branch -M main
  git init -q --bare "$remote"
  git -C "$repo" remote add origin "$remote"
  git -C "$repo" push -q -u origin main

  git -C "$repo" switch -q -c unreviewed
  printf 'unreviewed\n' >"$repo/unreviewed.txt"
  git -C "$repo" add unreviewed.txt
  git -C "$repo" commit -q -m "feat: unreviewed release code"
  git -C "$repo" tag -a v0.5.0 -m "Release 0.5.0"

  local output status
  set +e
  output="$(run_in_repo "$repo" "$repo_root/scripts/verify-release-tag.sh" v0.5.0 2>&1)"
  status=$?
  set -e

  [[ $status -ne 0 ]] || fail "release tag verifier should reject tags outside origin/main"
  assert_contains "$output" "not contained in origin/main" "unreviewed release tag"
}

test_verify_release_tag_accepts_tag_on_origin_main() {
  local repo="$tmp_root/release-tag-valid"
  local remote="$tmp_root/release-tag-valid.git"
  local github_output="$tmp_root/release-tag-valid-output"
  init_repo "$repo"
  git -C "$repo" branch -M main
  git init -q --bare "$remote"
  git -C "$repo" remote add origin "$remote"
  git -C "$repo" push -q -u origin main
  git -C "$repo" tag -a v0.5.0 -m "Release 0.5.0"
  git -C "$repo" push -q origin v0.5.0
  local tag_sha
  tag_sha="$(git -C "$repo" rev-parse v0.5.0^{commit})"

  local output
  output="$(run_in_repo "$repo" env GITHUB_OUTPUT="$github_output" EXPECTED_TAG_SHA="$tag_sha" "$repo_root/scripts/verify-release-tag.sh" v0.5.0)"

  assert_contains "$output" "Release tag v0.5.0 verified" "valid release tag"
  assert_contains "$(cat "$github_output")" "tag_name=v0.5.0" "valid release tag outputs"
  assert_contains "$(cat "$github_output")" "version=0.5.0" "valid release tag outputs"
  assert_contains "$(cat "$github_output")" "tag_sha=$tag_sha" "valid release tag outputs"
}

test_verify_release_tag_rejects_workflow_sha_mismatch() {
  local repo="$tmp_root/release-tag-sha"
  local remote="$tmp_root/release-tag-sha.git"
  init_repo "$repo"
  git -C "$repo" branch -M main
  git init -q --bare "$remote"
  git -C "$repo" remote add origin "$remote"
  git -C "$repo" push -q -u origin main
  git -C "$repo" tag -a v0.5.0 -m "Release 0.5.0"

  printf 'next\n' >"$repo/next.txt"
  git -C "$repo" add next.txt
  git -C "$repo" commit -q -m "feat: next change"
  git -C "$repo" push -q origin main
  local wrong_sha
  wrong_sha="$(git -C "$repo" rev-parse HEAD)"

  local output status
  set +e
  output="$(run_in_repo "$repo" env EXPECTED_TAG_SHA="$wrong_sha" "$repo_root/scripts/verify-release-tag.sh" v0.5.0 2>&1)"
  status=$?
  set -e

  [[ $status -ne 0 ]] || fail "release tag verifier should reject workflow SHA mismatch"
  assert_contains "$output" "does not match completed workflow SHA" "workflow SHA mismatch"
}

test_verify_release_tag_rejects_tag_moved_after_validation() {
  local repo="$tmp_root/release-tag-moved"
  local remote="$tmp_root/release-tag-moved.git"
  init_repo "$repo"
  git -C "$repo" branch -M main
  git init -q --bare "$remote"
  git -C "$repo" remote add origin "$remote"
  git -C "$repo" push -q -u origin main
  git -C "$repo" tag -a v0.5.0 -m "Release 0.5.0"
  git -C "$repo" push -q origin v0.5.0
  local validated_sha
  validated_sha="$(git -C "$repo" rev-parse v0.5.0^{commit})"

  run_in_repo "$repo" env EXPECTED_TAG_SHA="$validated_sha" "$repo_root/scripts/verify-release-tag.sh" v0.5.0 >/dev/null

  printf 'moved tag\n' >"$repo/moved.txt"
  git -C "$repo" add moved.txt
  git -C "$repo" commit -q -m "feat: move release tag"
  git -C "$repo" push -q origin main
  git -C "$repo" tag -f -a v0.5.0 -m "Moved release 0.5.0"
  git -C "$repo" push -q --force origin v0.5.0

  local output status
  set +e
  output="$(run_in_repo "$repo" env EXPECTED_TAG_SHA="$validated_sha" "$repo_root/scripts/verify-release-tag.sh" v0.5.0 2>&1)"
  status=$?
  set -e

  [[ $status -ne 0 ]] || fail "release tag verifier should reject a tag moved after validation"
  assert_contains "$output" "does not match completed workflow SHA" "moved release tag"
}

test_install_checksum_cannot_be_skipped() {
  local dir="$tmp_root/install-checksum"
  mkdir -p "$dir"
  printf 'archive payload\n' >"$dir/kata_0.5.0_linux_amd64.tar.gz"
  printf '%s  %s\n' \
    "0000000000000000000000000000000000000000000000000000000000000000" \
    "kata_0.5.0_linux_amd64.tar.gz" >"$dir/SHA256SUMS"

  local output status
  set +e
  output="$(KATA_SKIP_CHECKSUM=1 bash -c '
    source "$1"
    verify_checksum "$2" "$3" kata_0.5.0_linux_amd64.tar.gz
  ' bash "$repo_root/scripts/install.sh" "$dir/kata_0.5.0_linux_amd64.tar.gz" "$dir/SHA256SUMS" 2>&1)"
  status=$?
  set -e

  [[ $status -ne 0 ]] || fail "installer checksum verification should fail even when KATA_SKIP_CHECKSUM=1 is set"
  assert_contains "$output" "Checksum verification failed" "installer checksum enforcement"
  assert_not_contains "$output" "Checksum verification skipped" "installer checksum enforcement"
}

test_release_rejects_missing_version
test_release_rejects_v_prefixed_version
test_release_rejects_non_semver_version
test_release_refuses_dirty_worktree
test_changelog_fallback_includes_first_commit_without_tags
test_changelog_defaults_to_deterministic_fallback
test_changelog_allows_explicit_agent_opt_in
test_release_creates_and_pushes_bare_semver_tag
test_verify_release_tag_rejects_tag_outside_origin_main
test_verify_release_tag_accepts_tag_on_origin_main
test_verify_release_tag_rejects_workflow_sha_mismatch
test_verify_release_tag_rejects_tag_moved_after_validation
test_install_checksum_cannot_be_skipped

printf 'release script tests passed\n'
