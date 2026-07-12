# Vercel Docs Deployment Context Design

## Problem

The docs project is linked and deployed from the repository root while Vercel
builds from `docs/`. Without a Vercel-specific ignore policy, CLI deployments
send the repository's non-docs source, locally generated `docs/site` output,
and ignored local work artifacts. The project also enables source files outside
the configured root even though the docs build is self-contained.

This makes deployment transfer time unnecessarily sensitive to repository size
and can send local-only material that the build neither needs nor serves.

## Goals

- Limit Vercel CLI deployment inputs to the self-contained docs project.
- Exclude locally generated site output while retaining the small hydrated
  screenshot set required by Vercel's Git-metadata-free build source.
- Prevent ignored repository-local work files from entering deployment source
  manifests.
- Keep the existing repository-root `vercel deploy --prod` workflow.
- Correct the Vercel project setting that exposes source outside `docs/` and
  remove the obsolete ignored-build command.

## Non-goals

- Change the Zensical build or published site contents.
- Reconnect the Git integration or change how production deployments are
  initiated.
- Adopt Vercel prebuilt deployments or archive uploads.
- Add repository dependencies solely to validate ignore syntax.

## Considered Approaches

### 1. Repository-root allowlist plus project-setting correction

Commit a root `.vercelignore` that excludes everything by default, includes
`docs/`, and then excludes docs-local generated and internal-only paths. Disable
outside-root source inclusion in the Vercel project and clear its stale ignored
build command.

This is the recommended approach. It preserves the established root-linked
CLI workflow while making the deployment boundary explicit and reviewable.

### 2. Link and deploy from `docs/`

This could make the CLI source root smaller without an allowlist, but the Vercel
project already has `docs` as its configured root. Previous work found that
deploying with `--cwd docs` resolves that root twice. Relinking would also make
the local setup diverge from the documented monorepo workflow.

### 3. Build locally and deploy `--prebuilt`

This would transfer only build output, especially with archive mode, but changes
where production builds run and how Vercel build-time environment is supplied.
It also duplicates the current remote build contract and is unnecessary for a
small static documentation project.

## Design

Add a repository-root `.vercelignore` using Vercel's allowlist form:

- Ignore all root entries.
- Re-include the `docs` directory.
- Exclude `docs/site`, temporary Zensical build inputs, caches, virtual
  environments, local environment files, docs-local Vercel state, Python cache
  output, and `docs/superpowers` planning files.

The allowlist is the durable repository control. Disabling the project's
outside-root source option is defense in depth and aligns the dashboard with the
self-contained build contract. Clearing the obsolete ignored-build command
removes a reference to a helper that no longer exists.

The existing build remains unchanged: Vercel receives docs source plus the
hydrated screenshots prepared by `scripts/update-docs.sh`, runs
`uv sync --frozen --no-dev`, invokes `docs/vercel-build.sh`, and publishes the
generated `site/` directory. Screenshot assets must remain in the upload because
Vercel excludes `.git`; without either Git metadata or pre-hydrated assets,
`hydrate-assets.sh` cannot read the dedicated assets branch.

## Validation

Extend the parser-backed Vercel configuration checker to read `.vercelignore`
and evaluate representative deployment paths. The checker will require docs
source, configuration files, and hydrated screenshots to remain included while
rejecting repository Go source, generated `docs/site` output, caches, virtual
environments, local environment files, Vercel state, Python cache output, local
planning artifacts, and temporary Zensical paths.

The test is behavioral at the policy boundary: it parses ordered ignore and
negation rules and checks path classification rather than grepping for literal
configuration text. Existing docs checks will invoke it through their normal
entry point.

After updating the live project settings, query the project API to confirm the
outside-root option is false and the ignored-build command is empty. A later
production deployment can verify the real Vercel source manifest shrinks; the
PR itself will not trigger a production deployment.

## Operational Safety

The docs build does not read outside `docs/`, so disabling outside-root access
does not remove a required input. The deployment helper prepares screenshot
assets before invoking Vercel, and the allowlist preserves them. The output
directory is rebuilt remotely, so excluding local `docs/site` cannot change
published content. Explicit exclusions for `.env*.local`, `.vercel`, caches,
virtual environments, and temporary paths prevent re-including sensitive or
machine-local state beneath the allowed `docs/` tree.
