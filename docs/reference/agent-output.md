# Agent output format

Agent mode (`--agent`, or `--format agent`) is a compact, stable text contract
for coding agents. It exists alongside JSON, not instead of it: agent mode is
for short action acknowledgements that a human can scan and an agent can
pattern-match without a JSON parser, while `--json` remains the complete
machine-readable API. See [output modes](../guide/concepts.md#output-modes) for
the high-level picture and the [CLI reference](cli.md) for per-command flags.

This page documents the agent format as a contract. Tooling may rely on the
guarantees described here.

## Selecting the mode

```sh
kata list --agent                 # agent text
kata list --json                  # full JSON envelope
kata list --format agent          # equivalent to --agent
kata list --format human          # default terminal output
```

- `--json` and `--agent` are aliases for `--format json` and `--format agent`.
- Passing several output-mode flags that resolve to the **same** mode is valid
  (`--format agent --agent`). Flags that resolve to **different** modes are a
  usage error before the command runs.
- `--quiet` is orthogonal — a suppression flag, not a mode. In agent mode it may
  suppress the `OK ...` success header where a command already has quiet
  behavior, but it never suppresses an `ERR ...` line.
- `--format` is reserved globally for output mode. `kata import` selects its
  source format with `--source-format <kata|beads>`.

## Stability contract

Agent output is plain UTF-8 with no ANSI styling. Its shape is stable:

- In non-quiet agent mode the first token is always `OK` for success or `ERR`
  for failure. Success goes to stdout; failure goes to stderr with a nonzero
  exit code.
- The second token is the command or record kind: `create`, `comment`, `list`,
  `show`, `event`, and so on. The literal `kata` appears only on a top-level
  parse error before a subcommand runs.
- Remaining first-line fields are stable positional fields where this document
  specifies them, and `key=value` otherwise. **Field order is part of the
  contract.**
- New fields may be appended to a line or block. Existing field names, first
  tokens, and their meanings do not change without an agent-format version bump.
- List-valued fields are comma-separated with no spaces (`labels=bug,safari`).
- Nullable free-form fields are **omitted when absent**. Agent output never
  emits sentinel values such as `owner=null` or `owner=unowned`, because a user
  could legitimately choose those exact strings as an owner name.
- Values containing whitespace, quotes, or control characters are double-quoted
  with Go/JSON-style escaping; simple tokens stay unquoted.

### Version

The current agent format version is `1`. A breaking change bumps this integer.
Discover it without parsing JSON:

```text
$ kata version --agent
OK version version=0.0.0 agent_format=1
```

The same value appears as `agent_format` in `kata version --json`. Ordinary
command output does not repeat the version, to keep transcripts small.

## Errors

```text
ERR comment validation: comment body is required
Hint: pass --body, --body-file, or --body-stdin
```

- The first line is `ERR <command> <kind>: <message>`.
- `<kind>` reuses the CLI error taxonomy: `usage`, `validation`, `not_found`,
  `conflict`, `confirm`, `daemon_unavailable`, `internal`.
- Optional follow-up lines use fixed field names such as `Hint:`, `Code:`, and
  `Exit-Code:`.
- A top-level parse error, before any subcommand runs, uses `kata` as the
  command token: `ERR kata usage: unknown command "cretae"`.
- In `--format json` errors remain the JSON error envelope; in `--format human`
  they remain the existing human text.

## Success shapes

### Mutations

A mutation line names what happened and which issue changed, with `changed=` and
`reused=` flags where they apply, followed by block lines:

```text
OK create abc4
Issue: abc4 "Fix login race"
Status: open
```

```text
OK close abc4
Issue: abc4 "Fix login race"
Status: closed
Reason: done
Evidence: commit:3a401a8
```

```text
OK delete abc4
Issue: abc4 "Fix login race"
Status: deleted
Undo: kata restore abc4 --agent
```

An `Undo:` line is emitted only when the follow-up is factual and mechanically
correct. Generic `Next:` guidance is intentionally not part of the format.

### Reads

A read emits an `OK <command> count=<n>` header followed by one row per record:

```text
OK list count=3
- issue=abc4 status=open priority=2 owner=wesm labels=bug,safari title="Fix login race"
- issue=def7 status=open labels=architecture title="Control channel"
- issue=j9k2 status=closed title="Old task"
```

Empty reads emit the header with `count=0` and no rows:

```text
OK search count=0 query="login race"
```

### Events

Non-tail reads use a header plus rows; tail mode is stream-safe and emits exactly
one line per event:

```text
OK events count=2 next_after_id=44
- id=42 type=issue.created issue=abc4 actor=wesm
- id=43 type=issue.commented issue=abc4 actor=codex
```

```text
OK event id=42 type=issue.created issue=abc4 actor=wesm
```

`kata events --tail --json` is newline-delimited JSON; `kata events --tail
--agent` is one agent line per event and never emits multi-line blocks.

## Multiline text

Bodies and comments are preserved in fenced `text` blocks:

````text
OK show abc4
Issue: abc4 "Fix login race"
Status: open
Body:
```text
Safari can double-submit the callback.
```
````

- No silent truncation is allowed. If truncation is ever introduced, the
  metadata fields immediately precede the field they describe and are emitted as
  a pair — `<Field>-Truncated: true` and `<Field>-Bytes: <full-byte-count>` —
  never one without the other.
- A fenced block uses at least three backticks and the `text` info string. If
  the content itself contains a triple-backtick sequence, a longer fence is
  chosen so the block stays valid Markdown.

## Guarantees

- `--json` is never weakened or removed; it stays the complete structured API.
- Agent mode does not change daemon API responses — it is purely a CLI rendering
  of the same data.
- Agent mode is not the default.
- Interactive and foreground commands (`kata tui`, `kata daemon start`) have no
  meaningful final transcript line and reject agent mode with a usage error
  rather than launching.
