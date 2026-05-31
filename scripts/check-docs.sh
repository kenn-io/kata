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

for private_docs in \
  docs/federation.md \
  docs/hosted-mode.md \
  docs/superpowers; do
  if [[ -e "$private_docs" ]]; then
    printf 'maintainer-only docs must live outside docs/: %s\n' "$private_docs" >&2
    missing=1
  fi
done

for file in "${required_files[@]}"; do
  if [[ ! -f "$file" ]]; then
    printf 'missing required docs file: %s\n' "$file" >&2
    missing=1
  fi
done

if [[ "$missing" -ne 0 ]]; then
  exit 1
fi

require_line() {
  local file="$1"
  local expected="$2"

  if ! grep -F "$expected" "$file" >/dev/null; then
    printf 'missing required docs content in %s: %s\n' "$file" "$expected" >&2
    exit 1
  fi
}

require_line zensical.toml 'site_name = "kata カタ"'
require_line zensical.toml 'site_url = "https://katatracker.com/"'
require_line zensical.toml 'docs_dir = "docs"'
require_line zensical.toml 'site_dir = "site"'
require_line zensical.toml 'scheme = "slate"'
require_line docs/index.md '# kata カタ'
require_line README.md 'kata close abc4 --done --message "Fixed the login race and verified the relevant tests pass." --commit <sha>'

rm -rf site

if [[ -x ".venv/bin/zensical" ]]; then
  .venv/bin/zensical build --strict
elif command -v zensical >/dev/null 2>&1; then
  zensical build --strict
else
  printf 'zensical not found; install with: python3 -m venv .venv && .venv/bin/pip install -r requirements-docs.txt\n' >&2
  exit 127
fi

for generated in \
  site/federation/index.html \
  site/hosted-mode/index.html \
  site/superpowers; do
  if [[ -e "$generated" ]]; then
    printf 'generated site contains maintainer-only docs: %s\n' "$generated" >&2
    exit 1
  fi
done
