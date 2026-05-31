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

tmp_docs="$(mktemp -d zensical-public-docs.XXXXXX)"
tmp_config="$(mktemp .zensical-build.XXXXXX.toml)"

cleanup() {
  rm -rf "$tmp_docs" "$tmp_config"
}
trap cleanup EXIT INT TERM

(cd docs && tar --exclude './superpowers' -cf - .) | (cd "$tmp_docs" && tar -xf -)
sed "s#docs_dir = \"docs\"#docs_dir = \"$tmp_docs\"#" zensical.toml > "$tmp_config"

case "$command_name" in
  build)
    "$zensical_bin" build --strict --config-file "$tmp_config" "$@"
    ;;
  serve)
    "$zensical_bin" serve --config-file "$tmp_config" "$@"
    ;;
esac
