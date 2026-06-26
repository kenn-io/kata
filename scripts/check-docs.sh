#!/usr/bin/env bash
set -euo pipefail

missing=0

required_files=(
  "docs/index.md"
  "docs/get-started/quickstart.md"
  "docs/get-started/install.md"
  "docs/changelog.md"
  "docs/guide/concepts.md"
  "docs/guide/workspaces-projects.md"
  "docs/guide/migrating-from-beads.md"
  "docs/reference/cli.md"
  "docs/workflows/agents.md"
  "docs/workflows/sharing.md"
  "docs/operations/remote-daemon.md"
  "docs/operations/github-sync.md"
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
  "docs/scripts/check_vercel_redirects.py"
  "docs/scripts/check_public_markdown_sources.py"
  "docs/scripts/copy_public_markdown_sources.py"
  "docs/scripts/public_markdown_sources.py"
  "scripts/update-docs.sh"
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

if [[ -f "scripts/update-docs.sh" && ! -x "scripts/update-docs.sh" ]]; then
  printf 'docs deploy helper must be executable: scripts/update-docs.sh\n' >&2
  missing=1
fi

if [[ "$missing" -ne 0 ]]; then
  exit 1
fi

python3 docs/scripts/check_vercel_redirects.py

stale_config="docs/.zensical-build.XXXXXX.toml"
stale_docs="docs/zensical-public-docs.XXXXXX"
vercel_docs_root=""
missing_assets_docs_root=""
cleanup_check_docs() {
  rm -rf "$stale_config" "$stale_docs"
  if [[ -n "$vercel_docs_root" ]]; then
    rm -rf "$vercel_docs_root"
  fi
  if [[ -n "$missing_assets_docs_root" ]]; then
    rm -rf "$missing_assets_docs_root"
  fi
}
trap cleanup_check_docs EXIT

# Guard against macOS mktemp regressions where suffix templates become literal
# repo-local paths and block repeat docs builds.
: > "$stale_config"
mkdir -p "$stale_docs"

rm -rf docs/site

missing_assets_docs_root="$(mktemp -d)"
mkdir -p "$missing_assets_docs_root/docs"
(
  cd docs
  tar \
    --exclude './assets/screenshots' \
    --exclude './site' \
    --exclude './.ruff_cache' \
    --exclude './.mypy_cache' \
    -cf - .
) | (cd "$missing_assets_docs_root/docs" && tar -xf -)
missing_assets_log="$missing_assets_docs_root/vercel-build.log"
if (cd "$missing_assets_docs_root/docs" && uv run --frozen bash ./vercel-build.sh >"$missing_assets_log" 2>&1); then
  printf 'docs Vercel build without hydrated screenshots should fail\n' >&2
  exit 1
fi
if ! grep -F -- "docs screenshots not hydrated" "$missing_assets_log" >/dev/null; then
  printf 'missing screenshot failure did not mention hydration\n' >&2
  cat "$missing_assets_log" >&2
  exit 1
fi

bash docs/screenshots/hydrate-assets.sh

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
(cd "$vercel_docs_root/docs" && uv run --frozen bash ./vercel-build.sh)
(cd "$vercel_docs_root/docs" && python3 scripts/check_public_markdown_sources.py)

(cd docs && uv run --frozen bash ./zensical-docs.sh build)
(cd docs && python3 scripts/check_public_markdown_sources.py)

for generated in \
  docs/site/.env.local \
  docs/site/.vercel \
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
