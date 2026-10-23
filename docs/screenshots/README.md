# Docs screenshots

The documentation screenshots are generated from disposable synthetic Kata
daemons and issue data. No developer workspace or installed Kata state is read.

For a manual terminal capture, preserve the pane's trailing spaces and normalize
its ANSI stream before passing it to Freeze 0.2.2:

```bash
tmux capture-pane -N -e -p -t example-session > /tmp/example-capture.ansi
python3 docs/screenshots/normalize-ansi.py < /tmp/example-capture.ansi | \
  freeze --language ansi -o /tmp/example-capture.svg
```

The trailing spaces carry full-width row backgrounds. Freeze 0.2.2 ignores
selective foreground and background resets (`39` and `49`), which can dim plain
titles or extend a background into the next row. The normalizer replaces those
resets with full resets and restores the remaining active attributes. It
preserves the captured colors, text, and padding.

Run the converter tests with
`python3 -m unittest discover -s docs/screenshots -p test_normalize_ansi.py`.

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
The deployment helper pins the generated asset commit in
`KATA_DOCS_ASSETS_COMMIT`, then `docs/screenshots/hydrate-assets.sh` validates
and archives that exact object before replacing the ignored screenshot
directory. Standalone deployment resolves the remote branch to an immutable
commit once and archives that object. The local Vercel build consumes those
pre-hydrated files, then deployment uploads the resulting prebuilt site.

Pass `--push` only after reviewing the generated files and source changes when
you intend to replace the remote orphan branch.
