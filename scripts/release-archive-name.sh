#!/usr/bin/env bash
set -euo pipefail

if [[ $# -ne 3 ]]; then
  printf 'usage: %s <bare-version> <goos> <goarch>\n' "$0" >&2
  exit 2
fi

version="$1"
goos="$2"
goarch="$3"
ext=".tar.gz"
if [[ "$goos" == "windows" ]]; then
  ext=".zip"
fi

printf 'kata_%s_%s_%s%s\n' "$version" "$goos" "$goarch" "$ext"
