# Workspaces and projects

kata separates repository files from issue data. Repositories and workspaces
carry only enough information to resolve a project. The database lives under
`KATA_HOME` unless `KATA_DB` points somewhere else.

## Initialize

```sh
kata init
```

In a git workspace, `kata init` derives the project name from the git remote.
Pass `--project` to choose the name explicitly:

```sh
kata init --project product
```

`kata init` writes `.kata.toml` and ensures `.kata.local.toml` is ignored. The
local file is for per-machine settings such as a remote daemon URL.

## Bind many workspaces to one project

Use the same project name in each workspace:

```sh
cd ~/code/product
kata init --project product

cd ~/code/product-worktree
kata init --project product
```

Both workspaces now resolve to the same project in the same local kata
database. Issue short IDs, labels, links, and events are shared.

## Run from outside a workspace

Use the global `--workspace` flag:

```sh
kata --workspace ~/code/product ready --unowned
kata --workspace ~/code/product show abc4
```

This is useful for scripts, cron jobs, and agents that keep their own working
directory.

## Project commands

List and inspect projects:

```sh
kata projects list
kata projects show product
```

Rename a project:

```sh
kata projects rename product platform
```

Merge accidental duplicates:

```sh
kata projects merge old-repo new-repo --rename-target new-repo
```

Archive and restore a project:

```sh
kata projects remove old-lab
kata projects restore old-lab
```

`projects remove` hides the project from normal resolution but preserves events
for audit.

Detach one alias when a workspace identity was attached to the wrong project:

```sh
kata projects detach github.com/example/wrong
```

Use `kata projects show <project>` before destructive or structural project
operations so you know which project and aliases are affected.

## `.kata.toml`

The committed binding file is intentionally small:

```toml
version = 1

[project]
name = "product"
```

Do not put tokens or host-specific daemon URLs in `.kata.toml`.

## `.kata.local.toml`

The local override file is ignored by git. A common use is routing one
workspace to a remote daemon:

```toml
version = 1

[server]
url = "http://100.64.0.5:7777"
```

`KATA_SERVER` wins over `.kata.local.toml` when both are set.

## Non-git workspaces

kata works without git. Use an explicit project name:

```sh
mkdir ~/scratch/research
cd ~/scratch/research
kata init --project research
```

The issue model does not depend on git commits. Git is only one way to derive a
default project name and one possible source of close evidence.
