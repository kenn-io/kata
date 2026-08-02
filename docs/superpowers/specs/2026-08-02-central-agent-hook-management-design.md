# Central Agent-Hook Management Design

## Goal

Use `go.kenn.io/kit/agenthook` from kit v0.14.0 for coding-agent hook
configuration while preserving kata's existing public CLI and attention-state
behavior.

## Scope

The existing `kata init --with-hooks` and `kata init --with-codex-hooks` flags
remain the user-facing entry points. They continue to install Claude Code and
Codex hooks respectively. The hidden `kata attention-hook <start|end>` command
and its `KATA_REF`-based behavior remain unchanged.

This change does not add a generic agent selector, expose the other kit agent
profiles, or move kata's runtime hook handling to `agenthook.Handle`.

## Architecture

Kata will replace its local JSON hook merge implementation with thin adapters
that call `agenthook.Install` against a private staging file. Each installed
command carries a hidden, mode-specific `--source kata-agent-hook-<mode>` token;
that token is kit's ownership marker and cannot collide with an unrelated
command that merely mentions `kata attention-hook start`. Each adapter supplies
the existing event, matcher, timeout, and command explicitly:

- Claude Code receives `SessionStart` for `startup|resume|clear` and
  `SessionEnd` for
  `logout|prompt_input_exit|bypass_permissions_disabled|other`.
- Codex receives the existing `SessionStart` hook for
  `startup|resume|clear`, with the existing ten-second timeout. It does not gain
  a `SessionEnd` hook in this change.

Kata retains the policy that workspace hook directories and config files must
not be symlinks. The thin adapter performs the existing fd-relative symlink
checks before calling kit. Kata also retains the Codex-specific advisory when
`.codex/config.toml` contains a top-level `[hooks]` table; that warning is CLI
policy rather than shared config mutation.

The Go module dependency moves from kit v0.9.3 to v0.14.0. Any transitive
dependency changes are accepted only when required by that upgrade.

## Existing Configuration Migration

Kata v0.13.0 installed unmarked Claude and Codex commands. Before the shared
installer runs in the staging file, kata will remove only exact v0.13.0-owned
handlers. For Claude these are:

- executable `kata`;
- arguments `attention-hook start` or `attention-hook end`;
- the corresponding managed event and matcher.

For Codex, the exact adopted handler is the `SessionStart` command
`kata attention-hook start` with the `startup|resume|clear` matcher and
ten-second timeout.

No fuzzy command matching, alias handling, dual reading, or unrelated hook
cleanup is permitted. The shared installer then writes marker-owned command
strings and owns all subsequent reinstalls.

This is a bounded forward migration. It remains through kata v0.15.0 so users
can re-run `kata init` from either of the next two minor releases, and is removed
from main after v0.15.0 is tagged. Issue `m7d5` tracks that removal.

## Failure Behavior

Malformed hook configuration remains a non-fatal `kata init` warning and is
left untouched. Symlinked managed paths remain refused. Unrelated top-level
fields, events, matchers, and hook handlers remain preserved.

If shared installation fails, the corresponding init flag reports the same
human-readable warning class it uses today. Machine-readable output continues
to suppress those warnings. Re-running either flag after a successful install
is a no-op.

Claude start and end registrations require separate commands. The adapter
copies the original config to a private staging file, performs exact legacy
adoption and every kit installation there, then atomically publishes the
complete final config once. Any migration or install failure leaves the live
workspace file byte-for-byte unchanged. Empty legacy groups carrying no other
metadata are pruned in staging; groups with unrelated handlers or fields remain.

## Tests

Tests will assert kata-owned behavior at the CLI and adapter boundaries rather
than retest kit's parser and writer internals. Coverage will verify:

- fresh Claude and Codex installation through the existing flags;
- exact event, matcher, command, and timeout behavior;
- preservation of unrelated configuration;
- idempotent reinstallation;
- migration of exact v0.13.0 Claude handlers without duplication;
- adoption of the exact v0.13.0 Codex handler without duplication;
- preservation of commands that mention the lifecycle command but lack kata's
  unique source marker;
- unchanged live configuration when a later staged install fails;
- refusal of symlinked managed paths;
- non-destructive failure for malformed configuration; and
- preservation of the Codex `[hooks]` advisory.

Implementation-detail tests for kata's removed JSON merge and atomic-replace
helpers will be deleted with those helpers. Existing end-to-end init tests will
remain the primary contract tests.

## Documentation

The CLI and orchestration documentation will keep the same flags and lifecycle
semantics. Text that describes kata's custom JSON mutation or Claude exec-form
handler will be updated to describe the kit-managed command registration,
idempotency, preservation rules, and retained symlink boundary.
