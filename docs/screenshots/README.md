# Docs screenshots

The documentation screenshots are generated from disposable synthetic Kata
daemons and issue data. No developer workspace or installed Kata state is read.

`make docs-screenshots` regenerates the complete local preview set under the
ignored `docs/assets/screenshots/` directory. The aggregate generator runs both
the federation TUI capture and the focused Playwright Web UI capture. To write
somewhere else, run:

```bash
docs/screenshots/generate.sh --out /tmp/kata-docs-screenshots
```

A custom destination must either not exist or be a complete asset directory
created by this generator. Existing unrelated directories, symlinks, empty
paths, and broad repository paths are rejected. Generation happens in a
sibling temporary directory; only a validated result replaces the destination.

The Web UI fixture normalizes generated issue identifiers and relative times
to stable documentation values before capture. Each image must also reach two
identical rendered frames, so rerunning the generator with unchanged source
produces byte-identical PNGs instead of asset-branch churn.

`make docs-assets-branch` regenerates the same set and updates the local
`docs-assets` branch. The publisher accepts only the declared screenshot files,
rejects empty files and symlinks, and creates a fresh one-commit orphan branch.
For an already generated and reviewed directory, use:

```bash
docs/screenshots/update-assets-branch.sh \
  --source /tmp/kata-docs-screenshots
```

The `docs-assets` branch is intentionally an orphan branch with one commit.
Generated images stay out of `main`; docs pages reference them through
`/assets/screenshots/...`. Local preview reads the ignored generated files.
Production builds run `docs/screenshots/hydrate-assets.sh`, which force-fetches
and validates the branch before replacing the ignored screenshot directory.

Pass `--push` only after reviewing the generated files and source changes when
you intend to replace the remote orphan branch.
