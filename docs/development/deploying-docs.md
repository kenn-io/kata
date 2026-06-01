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

Keep the Zensical version aligned with `requirements-docs.txt` until the
repository fully moves docs dependency management to uv.

## Verification

Before changing the Vercel project or merging deployment config, verify the
same build path locally:

```sh
cd docs
uv sync --frozen --no-dev
uv run --frozen bash ./vercel-build.sh
```

The build should finish with generated static files under `docs/site/`. The
normal repository docs check remains:

```sh
make docs-check
```

Useful Vercel references:

- [Project configuration with `vercel.json`](https://vercel.com/docs/project-configuration/vercel-json)
- [Build customization](https://vercel.com/docs/builds)
- [Python dependency formats and uv support](https://vercel.com/docs/functions/runtimes/python)
