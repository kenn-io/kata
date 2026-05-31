# CLI reference

This page summarizes the command surface. Run `kata <command> --help` for the
current flag list in your installed binary.

## Global flags

| Flag | Meaning |
| --- | --- |
| `--workspace <path>` | Resolve project context from a specific workspace. |
| `--project <name>` | Select a project explicitly for project-scoped commands. |
| `--as <actor>` | Override the actor for this command. |
| `--agent` | Emit concise agent-readable text. |
| `--json` | Emit machine-readable JSON. |
| `--format human|json|agent` | Select output mode explicitly. |
| `--quiet` | Suppress non-essential output. |

## Issue lifecycle

Create:

```sh
kata create <title> \
  [--body TEXT | --body-file PATH | --body-stdin] \
  [--label LABEL] \
  [--owner NAME] \
  [--priority 0..4] \
  [--parent <ref>] \
  [--blocks <ref>] \
  [--blocked-by <ref>] \
  [--related <ref>] \
  [--idempotency-key KEY] \
  [--force-new]
```

List and inspect:

```sh
kata list [--status open|closed|all] [--limit N]
kata list [--label LABEL] [--no-label LABEL] [--owner NAME] [--unowned]
kata show <issue-ref>
kata search <query> [--limit N] [--include-deleted]
```

Edit:

```sh
kata edit <issue-ref> \
  [--title TEXT] \
  [--body TEXT] \
  [--owner NAME] \
  [--priority 0..4 | --priority -] \
  [--parent <ref>] \
  [--blocks <ref>] \
  [--blocked-by <ref>] \
  [--related <ref>] \
  [--remove-parent <ref>] \
  [--remove-blocks <ref>] \
  [--remove-blocked-by <ref>] \
  [--remove-related <ref>] \
  [--comment TEXT]
```

Comment:

```sh
kata comment <ref> [--body TEXT | --body-file PATH | --body-stdin]
```

Close:

```sh
kata close <ref> --done --message <text> \
  [--commit <sha>] \
  [--pr <url>] \
  [--test <command>] \
  [--reviewed <path>] \
  [--evidence <type:value>]
```

Other close reasons:

```sh
kata close <ref> --wontfix --message <rationale>
kata close <ref> --duplicate-of <ref> --message <pointer>
kata close <ref> --superseded-by <ref> --message <pointer>
kata close <ref> --audit-no-change \
  --message <scope-and-verification> \
  --evidence "no-change-audit:<rationale>" \
  --reviewed <path>
```

Reopen:

```sh
kata reopen <ref> [--comment TEXT]
```

Delete, restore, and purge:

```sh
kata delete <ref> --force --confirm "DELETE <qualified-id>"
kata restore <ref>
kata purge <ref> --force --confirm "PURGE <qualified-id>"
```

`delete` is reversible with `restore`; `purge` is irreversible. The
confirmation string is the issue's qualified short ID, for example
`DELETE kata#abc4`. Agents must not run `delete` or `purge` unless the user
explicitly asks for that exact operation and ref.

## Labels, ownership, and claiming

```sh
kata label add <ref> <label> [--comment TEXT]
kata label rm <ref> <label> [--comment TEXT]
kata labels

kata assign <ref> <owner> [--comment TEXT]
kata unassign <ref> [--comment TEXT]
kata claim <ref> [--force] [--comment TEXT]
```

`kata claim` atomically sets ownership to the current actor and fails if the
issue is already owned by someone else unless `--force` is used.

## Ready work

```sh
kata ready [--limit N] [--unowned] [--owner NAME]
kata ready [--label LABEL] [--no-label LABEL]
kata ready --all
```

`ready` returns open issues that do not have an open blocking predecessor.
Filters combine with AND logic. `--all` lists ready issues across every
non-archived project and cannot be combined with those filters or `--project`.

## Events and audit

```sh
kata events [--after N] [--limit N]
kata events --tail [--last-event-id N]
kata digest --since 24h [--until ...] [--project-id N | --all-projects] [--actor NAME ...]
kata audit closes [--actor NAME] [--reason done|wontfix|duplicate|superseded|audit-no-change]
```

`kata digest` groups recent activity by actor. `kata audit closes` is for
reviewing close discipline and finding lazy or duplicate closes.

## Projects

```sh
kata projects list
kata projects show <project>
kata projects rename <project> <name>
kata projects merge <source> <target> [--rename-target NAME]
kata projects remove <project> [--force]
kata projects restore <project>
kata projects detach <alias-identity>
```

## Daemon and diagnostics

```sh
kata daemon start [--listen <host:port>] [--insecure-readonly]
kata daemon status
kata daemon stop
kata daemon reload
kata daemon logs --hooks [--tail]
kata health
kata whoami
kata quickstart
kata version
kata tui
```

Local commands auto-start the daemon when appropriate. `daemon start` runs in
the foreground and is used for explicit service setups. `kata agent-instructions`
is an alias for `kata quickstart`.

## Backup and import

```sh
kata export [--project NAME] [--project-id N] [--output PATH]
kata export --allow-running-daemon --output PATH

kata import --input PATH --target PATH [--force]
kata import --source-format beads
```

The kata-format `import` creates a fresh database at the target path; it is not a
merge operation. The `--source-format beads` form is different: it drives the
`bd` CLI and merges into the current project. See
[Migrating from Beads](../guide/migrating-from-beads.md).

## Remote and identity tokens

```sh
kata tokens create --actor <actor> [--name <name>]
kata tokens list
kata tokens revoke <id>
```

Identity tokens are used when a remote/shared daemon has
`require_token_identity = true`.

## Federation

```sh
kata federation identity
kata federation enable --project <project>
kata federation enroll --project <project> --spoke-instance <uid> --hub-url <url>
kata federation join --project <project> --hub-url <url> --hub-project-id <id> \
  --token <token> [--push]
kata federation join --project <existing-project> --hub-url <url> \
  --hub-project-id <id> --token <token> --push --adopt-existing
kata federation status
kata federation enrollments list
kata federation revoke <enrollment-id>
kata federation lease acquire <issue-ref> [--ttl 30m]
kata federation lease release <issue-ref>
kata federation quarantine skip <id> --confirm "SKIP FEDERATION BATCH <id>" --reason <text>
```

`--adopt-existing` is a current-state cutover. It removes the local project's
pre-adoption event history from the live event stream and queues fresh snapshots
for federation. Run `kata --project <project> export --output <path>.jsonl`
first if you need to retain that local event timeline.

Federation is an operator workflow. Most users never need these commands.
Issue edits on push-enabled federated spokes remain local-first; use
`kata federation lease acquire` only when you want exclusive coordination on an
issue. A live lease held by another actor blocks non-comment mutations until it
is released or expires.

## Ref forms

Issue refs accept a bare short ID, a qualified short ID, or a full ULID:

```text
abc4
kata#abc4
01HZNQ7VFPK1XGD8R5MABCD4EX
```

Legacy numeric refs no longer resolve.
