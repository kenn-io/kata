#!/usr/bin/env bash
# Capture the production Web UI against disposable synthetic Kata state.
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "$script_dir/../.." && pwd)"
output_dir="$repo_root/docs/assets/screenshots"

usage() {
  cat <<EOF
Usage: $(basename "$0") [--out DIR]

Generate deterministic Web UI documentation screenshots into DIR/web-ui.
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --out)
      [[ $# -ge 2 ]] || { printf 'ERROR: --out requires a directory\n' >&2; exit 2; }
      output_dir="$2"
      shift 2
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

if ! command -v bun >/dev/null 2>&1; then
  printf 'ERROR: bun is required for Web UI docs screenshots\n' >&2
  exit 1
fi

mkdir -p "$output_dir/web-ui"
(
  cd "$repo_root/web"
  KATA_DOCS_SCREENSHOT_DIR="$output_dir" \
    bun run test:e2e -- tests/docs-screenshots.spec.ts
)

for image in workspace issue-detail relationships daemon-switcher; do
  path="$output_dir/web-ui/$image.png"
  if [[ ! -s "$path" ]]; then
    printf 'ERROR: Web UI screenshot is missing or empty: %s\n' "$path" >&2
    exit 1
  fi
done

printf 'Generated Web UI screenshots in %s/web-ui\n' "$output_dir"
