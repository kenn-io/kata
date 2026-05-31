#!/usr/bin/env bash
set -euo pipefail

missing=0

required_files=(
  "zensical.toml"
  "requirements-docs.txt"
  "docs-site/index.md"
  "docs-site/get-started/quickstart.md"
  "docs-site/get-started/install.md"
  "docs-site/guide/concepts.md"
  "docs-site/guide/workspaces-projects.md"
  "docs-site/reference/cli.md"
  "docs-site/workflows/agents.md"
  "docs-site/workflows/sharing.md"
  "docs-site/operations/remote-daemon.md"
  "docs-site/operations/federation.md"
  "docs-site/operations/hosted-mode.md"
  "docs-site/operations/backup-restore.md"
  "docs-site/reference/configuration.md"
  "docs-site/development/contributing.md"
  "docs-site/stylesheets/extra.css"
)

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
grep -F 'docs_dir = "docs-site"' zensical.toml >/dev/null
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
