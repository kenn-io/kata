# Project Create CLI Design

## Goal

Add a CLI path for creating a kata daemon project by name without requiring a
repository, workspace binding, or `kata init`.

## User Contract

`kata projects create <name>` creates the named project in the selected daemon
or returns the existing active project with that name. The command is
idempotent, so setup scripts can run it safely before other project-scoped
commands.

The command does not write `.kata.toml`, does not update `.gitignore`, does not
write agent guidance, and does not attach or reassign workspace aliases. Users
who want to bind the current workspace still use `kata init`.

Output follows existing project command conventions:

- Human: `created project #<id> (<name>)` or `project #<id> (<name>) already exists`.
- Agent: one `OK project action=create ... created=<bool>` row.
- JSON: the daemon response, including `project` and `created`.

## Architecture

The daemon already exposes the needed primitive through path-free
`POST /api/v1/projects` with a `name` body and no `alias`. The CLI should reuse
that endpoint instead of adding a second HTTP route or database method.

The implementation belongs in `cmd/kata/projects.go` as a new `create`
subcommand registered under `projects`. It should use the existing
`postProjects` helper so response handling and daemon error mapping stay
consistent with `kata init`.

## Data Flow

1. Cobra parses `kata projects create <name>`.
2. The CLI trims the joined name argument and rejects an empty value before any
   daemon call.
3. The CLI starts or selects the daemon with `ensureDaemon`, then posts
   `{"name": "<name>"}` to `/api/v1/projects`.
4. The daemon validates the name, creates the project when absent, or returns
   the existing active project when present.
5. The CLI renders the response in human, agent, or JSON mode.

## Error Handling

Name validation errors come from the daemon for consistency with all other
project creation paths, except the client rejects an all-whitespace argument as
usage/validation before posting.

Archived-name conflicts keep the daemon's existing `project_archived` behavior:
the command should surface the same hint to restore the project or choose a
different name.

Because this command never sends alias metadata, `--reassign` and `--replace`
do not apply and are intentionally not exposed.

## Testing

Add CLI tests before implementation:

- `projects create` posts only a `name` body, creates a project, and does not
  write workspace binding files.
- Re-running `projects create` for an active project reports `created=false`.
- `--json` returns the daemon response with `project` and `created`.
- `--agent` emits a stable single-line machine-readable result.
- Whitespace-only names fail before the HTTP request.

Docs should update the CLI reference project section and a getting-started or
workflow page that currently implies project creation requires `kata init`.
