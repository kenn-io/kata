#!/usr/bin/env bash
# Generate release notes since the last release using an agent by default.
set -euo pipefail

version="${1:-NEXT}"
start_tag="${2:-}"
extra_instructions="${3:-}"

git rev-parse --show-toplevel >/dev/null

range_spec=""
if [[ -n "$start_tag" && "$start_tag" != "-" ]]; then
  range_spec="${start_tag}..HEAD"
else
  if prev_tag="$(git describe --tags --abbrev=0 2>/dev/null)"; then
    range_spec="${prev_tag}..HEAD"
  fi
fi

git_diff_stat() {
  if [[ -n "$range_spec" ]]; then
    git diff --stat "$range_spec"
  else
    local empty_tree
    empty_tree="$(git hash-object -t tree /dev/null)"
    git diff --stat "$empty_tree" HEAD
  fi
}

git_log() {
  if [[ -n "$range_spec" ]]; then
    git log --no-merges "$@" "$range_spec"
  else
    git log --no-merges "$@"
  fi
}

fallback_changelog() {
  local log_output
  log_output="$(git_log --pretty=format:'%s%x09%h')"
  if [[ -z "$log_output" ]]; then
    printf '### Changes\n\n'
    printf '%s\n' '- No commits since the previous release.'
    return
  fi

  local features=""
  local improvements=""
  local fixes=""

  while IFS=$'\t' read -r subject short_hash; do
    [[ -n "$subject" ]] || continue
    local entry="- ${subject} (${short_hash})"
    case "$subject" in
      feat:*|feat\(*\):*|feature:*|feature\(*\):*)
        features+="${entry}"$'\n'
        ;;
      fix:*|fix\(*\):*|bugfix:*|bugfix\(*\):*)
        fixes+="${entry}"$'\n'
        ;;
      docs:*|docs\(*\):*|doc:*|doc\(*\):*)
        ;;
      *)
        improvements+="${entry}"$'\n'
        ;;
    esac
  done <<<"$log_output"

  local printed=0
  if [[ -n "$features" ]]; then
    printf '### New Features\n\n%s\n' "$features"
    printed=1
  fi
  if [[ -n "$improvements" ]]; then
    [[ $printed -eq 0 ]] || printf '\n'
    printf '### Improvements\n\n%s\n' "$improvements"
    printed=1
  fi
  if [[ -n "$fixes" ]]; then
    [[ $printed -eq 0 ]] || printf '\n'
    printf '### Bug Fixes\n\n%s\n' "$fixes"
    printed=1
  fi
  if [[ $printed -eq 0 ]]; then
    printf '### Improvements\n\n'
    printf '%s\n' '- No user-facing changes in this commit range.'
  fi
}

agent="${CHANGELOG_AGENT:-codex}"

if [[ "$agent" == "none" ]]; then
  fallback_changelog
  exit 0
fi

prompt_file="$(mktemp)"
log_file="$(mktemp)"
diff_file="$(mktemp)"
notes_file="$(mktemp)"
err_file="$(mktemp)"
trap 'rm -f "$prompt_file" "$log_file" "$diff_file" "$notes_file" "$err_file"' EXIT

git_log --date=short --pretty=format:'%ad%x09%h%x09%s' >"$log_file"
git_diff_stat >"$diff_file"

{
  printf 'Write concise Markdown release notes for kata %s.\n\n' "$version"
  printf 'IMPORTANT: Do not use tools, run shell commands, search, or read files.\n'
  printf 'All required information is provided below. Analyze the commit log and diff summary only.\n\n'
  printf 'kata is a local-first issue tracker with a daemon, CLI, TUI, federation, and documentation.\n'
  printf 'Use user-facing language and avoid private workspace or repository names.\n'
  printf 'Group changes into these Markdown sections when applicable:\n'
  printf '%s\n' '- ### New Features'
  printf '%s\n' '- ### Improvements'
  printf '%s\n' '- ### Bug Fixes'
  printf 'Use only sections that have entries.\n'
  printf 'Skip internal refactoring unless it affects users.\n'
  printf 'Do NOT mention documentation-only changes, including docs, changelog, release notes, README, website copy, screenshots, or generated docs asset updates, unless documentation is the user-facing product being released.\n'
  printf 'Do NOT mention bugs that were introduced and fixed within this same release cycle.\n'
  printf 'Keep descriptions brief, one line each, and use present tense.\n'
  printf 'Output only the release notes, with no preamble.\n'
  if [[ -n "$extra_instructions" ]]; then
    printf '\nAdditional instructions:\n%s\n' "$extra_instructions"
  fi
  printf '\nCommits:\n'
  cat "$log_file"
  printf '\n\nDiff summary:\n'
  cat "$diff_file"
} >"$prompt_file"

run_agent() {
  case "$agent" in
    codex)
      if ! command -v codex >/dev/null 2>&1; then
        printf 'codex not found; install codex or set CHANGELOG_AGENT=none for deterministic fallback\n' >&2
        return 127
      fi
      local codex_rust_log
      codex_rust_log="${CHANGELOG_CODEX_RUST_LOG:-${RUST_LOG:-error,codex_core::rollout::list=off}}"
      RUST_LOG="$codex_rust_log" codex exec \
        --json \
        --skip-git-repo-check \
        --sandbox read-only \
        -c reasoning_effort=high \
        -o "$notes_file" \
        - >/dev/null <"$prompt_file" 2>"$err_file"
      ;;
    claude)
      if ! command -v claude >/dev/null 2>&1; then
        printf 'claude not found; install claude or set CHANGELOG_AGENT=none for deterministic fallback\n' >&2
        return 127
      fi
      claude --print <"$prompt_file" >"$notes_file" 2>"$err_file"
      ;;
    *)
      printf 'unknown CHANGELOG_AGENT %q; expected codex, claude, or none\n' "$agent" >&2
      return 2
      ;;
  esac
}

agent_status=0
set +e
run_agent
agent_status=$?
set -e

if [[ "$agent_status" -ne 0 || ! -s "$notes_file" ]]; then
  printf '%s failed to generate changelog\n' "$agent" >&2
  if [[ "${CHANGELOG_DEBUG:-0}" == "1" && -s "$err_file" ]]; then
    cat "$err_file" >&2
  elif [[ -s "$err_file" ]]; then
    filtered_err="$(grep -E -v 'rollout path for thread|failed to record rollout items: failed to queue rollout items: channel closed|^mcp startup: no servers$|^WARNING: proceeding, even though we could not update PATH:' "$err_file" || true)"
    if [[ -n "$filtered_err" ]]; then
      printf '%s\n' "$filtered_err" >&2
    else
      printf 'Set CHANGELOG_DEBUG=1 to print full agent logs.\n' >&2
    fi
  fi
  exit 1
fi

case "$agent" in
  codex|claude)
    if [[ "${CHANGELOG_DEBUG:-0}" == "1" && -s "$err_file" ]]; then
      cat "$err_file" >&2
    fi
    cat "$notes_file"
    ;;
esac
