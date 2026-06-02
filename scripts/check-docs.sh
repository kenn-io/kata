#!/usr/bin/env bash
set -euo pipefail

missing=0

required_files=(
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
  "docs/development/deploying-docs.md"
  "docs/zensical.toml"
  "docs/vercel.json"
  "docs/vercel-build.sh"
  "docs/zensical-docs.sh"
  "docs/pyproject.toml"
  "docs/uv.lock"
  "docs/design/index.md"
  "docs/design/federation.md"
  "docs/design/hosted-mode.md"
  "docs/design/architecture.md"
  "docs/design/data-model.md"
  "docs/reference/agent-output.md"
  "docs/stylesheets/extra.css"
)

if [[ -d "docs-site" ]]; then
  printf 'docs-site directory should not exist; keep Zensical source under docs/\n' >&2
  missing=1
fi

if [[ -e "zensical.toml" ]]; then
  printf 'Zensical config must live under docs/: zensical.toml\n' >&2
  missing=1
fi

if [[ -e "requirements-docs.txt" ]]; then
  printf 'docs dependencies must live under docs/pyproject.toml, not requirements-docs.txt\n' >&2
  missing=1
fi

if [[ -e "scripts/zensical-docs.sh" ]]; then
  printf 'docs build helper must live under docs/: scripts/zensical-docs.sh\n' >&2
  missing=1
fi

for private_docs in \
  docs/federation.md \
  docs/hosted-mode.md; do
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

  if ! grep -F -- "$expected" "$file" >/dev/null; then
    printf 'missing required docs content in %s: %s\n' "$file" "$expected" >&2
    exit 1
  fi
}

require_line docs/vercel.json '"framework": null'
require_line docs/vercel.json '"installCommand": "uv sync --frozen --no-dev"'
require_line docs/vercel.json '"buildCommand": "uv run --frozen bash ./vercel-build.sh"'
require_line docs/vercel.json '"outputDirectory": "site"'
require_line docs/vercel-build.sh '"$script_dir/zensical-docs.sh" build'
require_line docs/pyproject.toml 'requires-python = ">=3.12"'
require_line docs/pyproject.toml '"zensical==0.0.43"'
require_line docs/pyproject.toml 'package = false'
require_line docs/zensical-docs.sh '"$docs_root/zensical.toml"'
require_line docs/zensical-docs.sh "--exclude './.venv'"
require_line docs/zensical-docs.sh "--exclude './site'"
require_line docs/zensical-docs.sh "--exclude './zensical-public-docs.*'"
require_line docs/zensical-docs.sh "--exclude './.zensical-build.*'"
require_line docs/zensical.toml 'site_name = "kata カタ"'
require_line docs/zensical.toml 'site_url = "https://katatracker.com/"'
require_line docs/zensical.toml 'docs_dir = "docs"'
require_line docs/zensical.toml 'site_dir = "site"'
require_line docs/zensical.toml 'scheme = "slate"'
require_line docs/zensical.toml '{"Design" = ['
require_line docs/zensical.toml '{"Deploying docs" = "development/deploying-docs.md"}'
require_line docs/index.md '# kata カタ: lightweight issue tracker for humans and agents'
require_line docs/development/deploying-docs.md '| Root directory | `docs` |'
require_line docs/development/deploying-docs.md 'Vercel should install with `uv sync --frozen --no-dev`'
require_line docs/development/deploying-docs.md 'Vercel should build with `uv run --frozen bash ./vercel-build.sh`'
require_line docs/development/deploying-docs.md 'Vercel should publish the generated `site/` directory'
require_line docs/development/deploying-docs.md 'vercel link --cwd docs'
require_line docs/development/deploying-docs.md 'make docs-deploy'
require_line Makefile 'docs-deploy:'
require_line Makefile 'vercel deploy --cwd docs --prod'
require_line README.md 'kata close abc4 --done --message "Fixed the login race and verified the relevant tests pass." --commit <sha>'

for stale_reference in Makefile docs/zensical-docs.sh docs/development/deploying-docs.md; do
  if grep -F -- "requirements-docs.txt" "$stale_reference" >/dev/null; then
    printf 'stale requirements-docs.txt reference in %s\n' "$stale_reference" >&2
    exit 1
  fi
done

stale_config="docs/.zensical-build.XXXXXX.toml"
stale_docs="docs/zensical-public-docs.XXXXXX"
vercel_docs_root=""
cleanup_check_docs() {
  rm -rf "$stale_config" "$stale_docs"
  if [[ -n "$vercel_docs_root" ]]; then
    rm -rf "$vercel_docs_root"
  fi
}
trap cleanup_check_docs EXIT

# Guard against macOS mktemp regressions where suffix templates become literal
# repo-local paths and block repeat docs builds.
: > "$stale_config"
mkdir -p "$stale_docs"

rm -rf docs/site

vercel_docs_root="$(mktemp -d)"
mkdir -p "$vercel_docs_root/docs"
(
  cd docs
  tar \
    --exclude './site' \
    --exclude './.ruff_cache' \
    --exclude './.mypy_cache' \
    -cf - .
) | (cd "$vercel_docs_root/docs" && tar -xf -)
(cd "$vercel_docs_root/docs" && bash ./vercel-build.sh)

docs/zensical-docs.sh build

for generated in \
  docs/site/federation/index.html \
  docs/site/hosted-mode/index.html \
  docs/site/superpowers; do
  if [[ -e "$generated" ]]; then
    printf 'generated site contains maintainer-only docs: %s\n' "$generated" >&2
    exit 1
  fi
done

for generated in \
  docs/site/design/index.html \
  docs/site/design/architecture/index.html \
  docs/site/design/data-model/index.html \
  docs/site/design/federation/index.html \
  docs/site/design/hosted-mode/index.html; do
  if [[ ! -e "$generated" ]]; then
    printf 'generated site is missing design docs page: %s\n' "$generated" >&2
    exit 1
  fi
done
