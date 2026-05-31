#!/usr/bin/env bash
set -euo pipefail

missing=0

required_files=(
  "zensical.toml"
  "requirements-docs.txt"
  "docs/index.md"
  "docs/get-started/quickstart.md"
  "docs/get-started/install.md"
  "docs/guide/concepts.md"
  "docs/guide/workspaces-projects.md"
  "docs/guide/migrating-from-beads.md"
  "docs/reference/cli.md"
  "docs/workflows/agents.md"
  "docs/workflows/sharing.md"
  "docs/operations/remote-daemon.md"
  "docs/operations/federation.md"
  "docs/operations/hosted-mode.md"
  "docs/operations/backup-restore.md"
  "docs/reference/configuration.md"
  "docs/development/contributing.md"
  "docs/stylesheets/extra.css"
)

if [[ -d "docs-site" ]]; then
  printf 'docs-site directory should not exist; keep Zensical source under docs/\n' >&2
  missing=1
fi

for file in "${required_files[@]}"; do
  if [[ ! -f "$file" ]]; then
    printf 'missing required docs file: %s\n' "$file" >&2
    missing=1
  fi
done

if [[ "$missing" -ne 0 ]]; then
  exit 1
fi

grep -F 'site_name = "kata"' zensical.toml >/dev/null
grep -F 'site_url = "https://katatracker.com/"' zensical.toml >/dev/null
grep -F 'docs_dir = "docs"' zensical.toml >/dev/null
grep -F 'site_dir = "site"' zensical.toml >/dev/null
grep -F 'scheme = "slate"' zensical.toml >/dev/null

if [[ -x ".venv/bin/zensical" ]]; then
  .venv/bin/zensical build --strict
elif command -v zensical >/dev/null 2>&1; then
  zensical build --strict
else
  printf 'zensical not found; install with: python3 -m venv .venv && .venv/bin/pip install -r requirements-docs.txt\n' >&2
  exit 127
fi
