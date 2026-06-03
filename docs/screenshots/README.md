# Docs screenshots

`make docs-screenshots` regenerates local preview screenshots under the ignored
`docs/assets/screenshots/` directory.

`make docs-assets-branch` regenerates those same screenshots from disposable
simulated kata daemons and updates the local `docs-assets` branch.

The `docs-assets` branch is intentionally an orphan branch with one commit.
Generated SVGs stay out of `main`; docs pages reference them through
`/assets/screenshots/...`. Local preview reads the ignored generated files.
Production builds hydrate that directory from `docs-assets`.

Pass `--push` to `docs/screenshots/update-assets-branch.sh` only when you want
to force-push the regenerated asset branch.
