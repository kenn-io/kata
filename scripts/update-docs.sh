#!/usr/bin/env bash
# Regenerate docs screenshots, verify the docs build, and deploy to Vercel.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
DRY_RUN=false

usage() {
  cat <<EOF
Usage: $(basename "$0") [--dry-run]

Regenerate and push docs screenshot assets, build and check the docs, then
deploy the current committed workspace to production Vercel.

Run this after docs source changes have already been committed.
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --dry-run)
      DRY_RUN=true
      shift
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      printf 'unknown option: %s\n' "$1" >&2
      usage >&2
      exit 2
      ;;
  esac
done

require_cmd() {
  if ! command -v "$1" >/dev/null 2>&1; then
    printf 'ERROR: %s is required for docs deployment\n' "$1" >&2
    exit 1
  fi
}

run() {
  printf '+'
  printf ' %q' "$@"
  printf '\n'
  if [[ "$DRY_RUN" == false ]]; then
    "$@"
  fi
}

require_clean_tracked_tree() {
  local status
  status="$(git status --porcelain --untracked-files=all)"
  if [[ -n "$status" ]]; then
    printf 'ERROR: uncommitted, non-ignored changes are present. Commit or stash docs source changes before deploying.\n' >&2
    printf '%s\n' "$status" >&2
    exit 1
  fi
}

require_cmd bash
require_cmd git
require_cmd make
require_cmd vercel

cd "$REPO_ROOT"
if [[ "$DRY_RUN" == false ]]; then
  require_clean_tracked_tree
fi

run make docs-install
run bash docs/screenshots/update-assets-branch.sh --push
assets_branch="${KATA_DOCS_ASSETS_BRANCH:-docs-assets}"
if [[ "$DRY_RUN" == false ]]; then
  assets_commit="$(git rev-parse --verify "refs/heads/$assets_branch^{commit}")"
  printf '+ pin docs assets %s\n' "$assets_commit"
else
  assets_commit="<generated-$assets_branch-commit>"
  printf '+ pin docs assets to the generated %s commit\n' "$assets_branch"
fi
run env "KATA_DOCS_ASSETS_COMMIT=$assets_commit" bash docs/screenshots/hydrate-assets.sh --force
run make docs-build
run make docs-check
run env "KATA_DOCS_ASSETS_COMMIT=$assets_commit" make docs-deploy
