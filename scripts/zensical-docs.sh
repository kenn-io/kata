#!/usr/bin/env bash
set -euo pipefail

command_name="${1:-}"
if [[ "$command_name" != "build" && "$command_name" != "serve" ]]; then
  printf 'usage: %s {build|serve} [zensical args...]\n' "$0" >&2
  exit 2
fi
shift || true

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "$script_dir/.." && pwd)"
site_dir="${KATA_DOCS_SITE_DIR:-site}"

if [[ -n "${VIRTUAL_ENV:-}" && -x "$VIRTUAL_ENV/bin/zensical" ]]; then
  zensical_bin="$VIRTUAL_ENV/bin/zensical"
elif [[ -x "$repo_root/.venv/bin/zensical" ]]; then
  zensical_bin="$repo_root/.venv/bin/zensical"
elif [[ -x "$repo_root/docs/.venv/bin/zensical" ]]; then
  zensical_bin="$repo_root/docs/.venv/bin/zensical"
elif command -v zensical >/dev/null 2>&1; then
  zensical_bin="zensical"
else
  printf 'zensical not found; install with: cd docs && uv sync --frozen --no-dev\n' >&2
  exit 127
fi

tmp_docs=""
tmp_config_base=""
tmp_config=""

cleanup() {
  if [[ -n "$tmp_docs" ]]; then
    rm -rf "$tmp_docs"
  fi
  if [[ -n "$tmp_config" ]]; then
    rm -f "$tmp_config"
  fi
  if [[ -n "$tmp_config_base" ]]; then
    rm -f "$tmp_config_base"
  fi
}
trap cleanup EXIT INT TERM

tmp_docs_name="$(cd "$repo_root" && mktemp -d zensical-public-docs.XXXXXX)"
tmp_docs="$repo_root/$tmp_docs_name"
tmp_config_base_name="$(cd "$repo_root" && mktemp .zensical-build.XXXXXX)"
tmp_config_base="$repo_root/$tmp_config_base_name"
tmp_config="$tmp_config_base.toml"
tmp_config_name="$tmp_config_base_name.toml"
if [[ -e "$tmp_config" ]]; then
  printf 'temporary config path already exists: %s\n' "$tmp_config" >&2
  exit 1
fi
mv "$tmp_config_base" "$tmp_config"
tmp_config_base=""

(
  cd "$repo_root/docs"
  tar \
    --exclude './.venv' \
    --exclude './site' \
    --exclude './.ruff_cache' \
    --exclude './.mypy_cache' \
    --exclude './superpowers' \
    -cf - .
) | (cd "$tmp_docs" && tar -xf -)
awk -v docs_dir="$tmp_docs_name" -v site_dir="$site_dir" '
  $0 == "docs_dir = \"docs\"" {
    print "docs_dir = \"" docs_dir "\""
    next
  }
  $0 == "site_dir = \"site\"" {
    print "site_dir = \"" site_dir "\""
    next
  }
  { print }
' "$repo_root/docs/zensical.toml" > "$tmp_config"

case "$command_name" in
  build)
    (cd "$repo_root" && "$zensical_bin" build --strict --config-file "$tmp_config_name" "$@")
    ;;
  serve)
    (cd "$repo_root" && "$zensical_bin" serve --config-file "$tmp_config_name" "$@")
    ;;
esac
