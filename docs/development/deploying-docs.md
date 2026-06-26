# Deploying docs

The public docs site is a static Zensical build. Vercel does not need native
Zensical framework support. Configure the Vercel project with `docs/` as its
root directory, install the Python docs toolchain with uv, run the Zensical
build wrapper, and publish the generated `site/` directory.

## Vercel project

Create a Vercel project from the Git repository with these settings:

| Setting | Value |
| --- | --- |
| Production branch | `main` |
| Framework preset | `Other` |
| Root directory | `docs` |
| Install command | `uv sync --frozen --no-dev` |
| Build command | `uv run --frozen bash ./vercel-build.sh` |
| Output directory | `site` |

Vercel should install with `uv sync --frozen --no-dev`.
Vercel should build with `uv run --frozen bash ./vercel-build.sh`.
Vercel should publish the generated `site/` directory.

The build wrapper also copies every nav-listed Markdown document into `site/`.
That keeps source-form docs available from the same deployment as the rendered
page: for example, `/get-started/quickstart.md` serves the Markdown source that
generated `/get-started/quickstart/`.

## Repository config

Prefer committing the deployment settings instead of relying only on dashboard
state. `docs/vercel.json` keeps Vercel builds reproducible from `main`:

```json
{
  "$schema": "https://openapi.vercel.sh/vercel.json",
  "framework": null,
  "installCommand": "uv sync --frozen --no-dev",
  "buildCommand": "uv run --frozen bash ./vercel-build.sh",
  "outputDirectory": "site"
}
```

The docs directory also carries its own uv project metadata in
`docs/pyproject.toml` and `docs/uv.lock`. The Zensical project config lives in
`docs/zensical.toml` so the docs deployment files stay together:

```toml
[project]
name = "kata-docs"
version = "0.0.0"
requires-python = ">=3.12"
dependencies = [
  "zensical==0.0.43",
]

[tool.uv]
package = false
```

Update `docs/pyproject.toml` and refresh `docs/uv.lock` together whenever the
docs toolchain changes.

## CLI deployment

After the Vercel GitHub integration is disconnected, deploy the docs project
from the command line. Link the repository root to the existing Vercel project
once. The Vercel project root directory remains `docs`, so do not link or
deploy from inside `docs/`:

```sh
vercel link
```

Then deploy the current committed workspace to production from the repository
root:

```sh
scripts/update-docs.sh
```

The helper regenerates and pushes the `docs-assets` screenshot branch, hydrates
local screenshots, builds the docs, runs the docs checks, and deploys with
Vercel. It does not commit source changes; commit or stash non-ignored docs
edits before running it.

If you need to run only the Vercel deploy step:

```sh
make docs-deploy
```

The Make target runs:

```sh
vercel deploy --prod
```

Useful Vercel references:

- [Project configuration with `vercel.json`](https://vercel.com/docs/project-configuration/vercel-json)
- [Build customization](https://vercel.com/docs/builds)
- [Python dependency formats and uv support](https://vercel.com/docs/functions/runtimes/python)
