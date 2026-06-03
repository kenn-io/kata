#!/usr/bin/env bash
# Regenerate docs screenshot assets and update the local docs-assets branch.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
ASSETS_BRANCH="${KATA_DOCS_ASSETS_BRANCH:-docs-assets}"
PUSH=false

usage() {
    cat <<EOF
Usage: $(basename "$0") [--push]

Regenerate docs screenshots and update the local $ASSETS_BRANCH branch to a
single orphan commit. Pass --push to force-push that branch to origin.
EOF
}

while [[ $# -gt 0 ]]; do
    case "$1" in
        --push)
            PUSH=true
            shift
            ;;
        -h|--help)
            usage
            exit 0
            ;;
        *)
            echo "Unknown option: $1" >&2
            usage >&2
            exit 1
            ;;
    esac
done

TMP_ROOT="$(mktemp -d)"
WORKTREE="$TMP_ROOT/worktree"
ASSETS_DIR="$TMP_ROOT/assets"
TMP_BRANCH="docs-assets-build-$$"

cleanup() {
    set +e
    if git -C "$REPO_ROOT" worktree list --porcelain | grep -q "worktree $WORKTREE"; then
        git -C "$REPO_ROOT" worktree remove --force "$WORKTREE" >/dev/null 2>&1
    fi
    git -C "$REPO_ROOT" branch -D "$TMP_BRANCH" >/dev/null 2>&1
    rm -rf "$TMP_ROOT"
}
trap cleanup EXIT

"$SCRIPT_DIR/generate-federation-tui.sh" --out "$ASSETS_DIR"

git -C "$REPO_ROOT" worktree add --detach "$WORKTREE" HEAD >/dev/null
(
    cd "$WORKTREE"
    git switch --orphan "$TMP_BRANCH" >/dev/null
    git rm -r --cached . >/dev/null 2>&1 || true
    find . -mindepth 1 -maxdepth 1 ! -name .git -exec rm -rf {} +
    cp -R "$ASSETS_DIR"/. .
    git add .
    git \
        -c user.name="${GIT_AUTHOR_NAME:-kata docs bot}" \
        -c user.email="${GIT_AUTHOR_EMAIL:-docs-bot@example.invalid}" \
        commit -m "docs assets" >/dev/null
    git rev-parse HEAD >"$TMP_ROOT/commit"
)

ASSET_COMMIT="$(cat "$TMP_ROOT/commit")"
git -C "$REPO_ROOT" update-ref "refs/heads/$ASSETS_BRANCH" "$ASSET_COMMIT"

echo "Updated $ASSETS_BRANCH -> $ASSET_COMMIT"

if [[ "$PUSH" == true ]]; then
    git -C "$REPO_ROOT" push --force origin "$ASSETS_BRANCH"
fi
