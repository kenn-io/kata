#!/usr/bin/env bash
# Fail when the committed OpenAPI contract introduces breaking changes for
# existing API clients relative to a base git ref. Complements the drift
# tests, which only prove the committed contract matches the code.
#
# Usage: scripts/openapi-breaking.sh [base-ref]   (default: origin/main)
set -euo pipefail

base_ref="${1:-origin/main}"
specs=(
    api/openapi.yaml
)

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

status=0

api_version() {
    awk '$0 == "info:" { in_info = 1; next }
         in_info && /^  version:/ { print $2; exit }' "$1"
}

version_is_greater() {
    local current="$1"
    local base="$2"
    local current_major current_minor current_patch
    local base_major base_minor base_patch

    [[ "$current" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || return 1
    [[ "$base" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || return 1
    IFS=. read -r current_major current_minor current_patch <<<"$current"
    IFS=. read -r base_major base_minor base_patch <<<"$base"

    ((10#$current_major > 10#$base_major)) && return 0
    ((10#$current_major < 10#$base_major)) && return 1
    ((10#$current_minor > 10#$base_minor)) && return 0
    ((10#$current_minor < 10#$base_minor)) && return 1
    ((10#$current_patch > 10#$base_patch))
}

for spec in "${specs[@]}"; do
    if ! git cat-file -e "$base_ref:$spec" 2>/dev/null; then
        echo "openapi-breaking: skipping $spec (absent at $base_ref)"
        continue
    fi
    base_copy="$tmp/${spec//\//_}"
    git show "$base_ref:$spec" >"$base_copy"
    echo "openapi-breaking: checking $spec against $base_ref"
    if go tool oasdiff breaking "$base_copy" "$spec" --fail-on ERR; then
        continue
    else
        oasdiff_status=$?
    fi
    if ((oasdiff_status != 1)); then
        status=1
        continue
    fi

    base_version="$(api_version "$base_copy")"
    current_version="$(api_version "$spec")"
    if version_is_greater "$current_version" "$base_version"; then
        echo "openapi-breaking: allowing versioned contract change $base_version -> $current_version"
        continue
    fi
    echo "openapi-breaking: breaking changes require a higher info.version" >&2
    status=1
done
exit $status
