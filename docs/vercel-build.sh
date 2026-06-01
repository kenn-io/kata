#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
KATA_DOCS_SITE_DIR="docs/site" "$script_dir/../scripts/zensical-docs.sh" build
