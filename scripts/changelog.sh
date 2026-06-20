#!/usr/bin/env bash
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

git_log() {
  if [[ -n "$range_spec" ]]; then
    git log --no-merges "$@" "$range_spec"
  else
    git log --no-merges "$@"
  fi
}

fallback_changelog() {
  printf '### Changes\n\n'
  local log_output
  log_output="$(git_log --pretty=format:'%s%x09%h')"
  if [[ -z "$log_output" ]]; then
    printf '%s\n' '- No commits since the previous release.'
    return
  fi
  while IFS=$'\t' read -r subject short_hash; do
    [[ -n "$subject" ]] || continue
    printf '%s\n' "- ${subject} (${short_hash})"
  done <<<"$log_output"
}

agent="${CHANGELOG_AGENT:-}"
if [[ -z "$agent" ]]; then
  agent="none"
fi

if [[ "$agent" == "none" ]]; then
  fallback_changelog
  exit 0
fi

prompt_file="$(mktemp)"
log_file="$(mktemp)"
trap 'rm -f "$prompt_file" "$log_file"' EXIT

git_log --date=short --pretty=format:'%ad%x09%h%x09%s' >"$log_file"

{
  printf 'Write concise Markdown release notes for kata %s.\n\n' "$version"
  printf 'kata is a local-first issue tracker with a daemon, CLI, TUI, federation, and documentation.\n'
  printf 'Use user-facing language, group related changes when useful, and avoid private workspace or repository names.\n'
  if [[ -n "$extra_instructions" ]]; then
    printf '\nAdditional instructions:\n%s\n' "$extra_instructions"
  fi
  printf '\nCommits:\n'
  cat "$log_file"
} >"$prompt_file"

case "$agent" in
  codex)
    if command -v codex >/dev/null 2>&1; then
      codex exec <"$prompt_file" || fallback_changelog
    else
      fallback_changelog
    fi
    ;;
  claude)
    if command -v claude >/dev/null 2>&1; then
      claude <"$prompt_file" || fallback_changelog
    else
      fallback_changelog
    fi
    ;;
  *)
    printf 'unknown CHANGELOG_AGENT %q; using deterministic fallback\n' "$agent" >&2
    fallback_changelog
    ;;
esac
