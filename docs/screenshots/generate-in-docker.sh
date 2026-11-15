#!/usr/bin/env bash
# Run generate.sh inside docs/screenshots/Dockerfile and write the complete
# docs-assets tree to OUTPUT_DIR/assets. The host needs only Bash and Docker.
set -euo pipefail

output="${1:?usage: generate-in-docker.sh OUTPUT_DIR}"
script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "$script_dir/../.." && pwd)"
image=kata-docs-screenshots

mkdir -p "$output"
output="$(cd "$output" && pwd -P)"
docker build -t "$image" "$script_dir"
# Run as the caller so every generated file stays removable without root.
docker run --rm --init --user "$(id -u):$(id -g)" -e HOME=/tmp \
    -v "$repo_root:/src" -v "$output:/out" -w /src "$image" \
    bash -c 'cd web && bun install --frozen-lockfile && cd .. && bash docs/screenshots/generate.sh --out /out/assets'
