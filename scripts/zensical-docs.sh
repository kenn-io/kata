#!/usr/bin/env bash
set -euo pipefail

command_name="${1:-}"
if [[ "$command_name" != "build" && "$command_name" != "serve" ]]; then
  printf 'usage: %s {build|serve} [zensical args...]\n' "$0" >&2
  exit 2
fi
shift || true

if [[ -x ".venv/bin/zensical" ]]; then
  zensical_bin=".venv/bin/zensical"
elif command -v zensical >/dev/null 2>&1; then
  zensical_bin="zensical"
else
  printf 'zensical not found; install with: python3 -m venv .venv && .venv/bin/pip install -r requirements-docs.txt\n' >&2
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

tmp_docs="$(mktemp -d zensical-public-docs.XXXXXX)"
tmp_config_base="$(mktemp .zensical-build.XXXXXX)"
tmp_config="$tmp_config_base.toml"
if [[ -e "$tmp_config" ]]; then
  printf 'temporary config path already exists: %s\n' "$tmp_config" >&2
  exit 1
fi
mv "$tmp_config_base" "$tmp_config"
tmp_config_base=""

(cd docs && tar --exclude './superpowers' -cf - .) | (cd "$tmp_docs" && tar -xf -)
awk -v docs_dir="$tmp_docs" '
  $0 == "docs_dir = \"docs\"" {
    print "docs_dir = \"" docs_dir "\""
    next
  }
  { print }
' zensical.toml > "$tmp_config"

case "$command_name" in
  build)
    "$zensical_bin" build --strict --config-file "$tmp_config" "$@"
    ;;
  serve)
    "$zensical_bin" serve --config-file "$tmp_config" "$@"
    ;;
esac
