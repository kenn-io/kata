---
name: testing-without-tautologies
description: Use when creating, editing, fixing, or reviewing tests; when adding assertions, fakes, unit tests, testenv integration tests, e2e tests, CLI tests, TUI golden snapshots, or changing tests after failures.
---

# Testing Without Tautologies

## Core Idea

Tests should fail when protected behavior breaks. A passing test helps only if it can catch a real problem.

Before writing or changing a test, ask: "What production change should make this test fail?" If you cannot answer, redesign the test.

This pairs with the repo's Test First rule: the failing test you write before the implementation must fail *because the behavior is missing*, not because of a typo or an assertion on scaffolding. Watch it fail for the right reason.

## Quality Gate

Before writing the test body, answer these:

- **Who uses this?** Prefer the HTTP API contract, CLI output and exit codes, persisted DB state, emitted events, rendered TUI, or caller-visible results. Avoid private state.
- **What example proves it?** Use concrete inputs and literal expected outputs. Do not compute expected values with production logic.
- **What break would this catch?** Name the wrong branch, missing side effect, wrong argument, boundary case, or contract violation.
- **Do we own it?** Test our choices at framework, SDK, database, and service boundaries. Do not re-test documented dependency mechanics.
- **Can you state it?** Given this setup, when the user/system does X, then Y observable behavior changes. If Y is not assertable, the test is not ready.

## Required Checks

Apply these checks to every new or modified test:

1. **Assert observable effects**
   - Check returned values, persisted rows, HTTP response bodies and status codes, emitted events (`/events` NDJSON), CLI stdout/stderr/exit codes, rendered TUI output, errors, or auth outcomes.
   - A no-assertion test is acceptable only when the failure mode is the subject, such as "this constructor rejects invalid input." Prefer explicit assertions anyway.

2. **Prefer real collaborators over doubles**
   - This repo is built for it: `internal/testenv.New(t)` boots a real daemon over loopback TCP with a real SQLite store; `internal/testfix` provides real git repos and `.kata.toml` fixtures. Use them instead of hand-rolled fakes of the storage or daemon layers.
   - Reserve fakes for genuinely external boundaries (a remote hub, GitHub, an embedding provider) and for the TUI's client seam.

3. **Make fakes specific**
   - When a fake stands in for a boundary, verify arguments, call counts, and which branch was taken when they are part of the contract.
   - Do not let a fake accept any input when the code must pass one value.

4. **Separate branch doubles**
   - Do not reuse one fake handler for success, error, incomplete, unauthorized, or other mutually exclusive paths.
   - Give each branch its own spy/fake so the wrong branch cannot satisfy the expectation.

5. **Do not mock the subject**
   - Fake dependencies, boundaries, and slow or nondeterministic collaborators.
   - Do not replace the handler, store method, command, model update function, or workflow under test.

6. **Investigate failures before changing expectations**
   - Do not flip expected values just to make a failing test pass.
   - First decide whether the production change is intended. Then update the test to describe the new contract.
   - Golden snapshots are the sharpest version of this: never run `go test ./internal/tui/ -update-goldens` to silence a failure without reading the golden diff and confirming every changed line is an intended rendering change.

7. **Avoid mirror assertions**
   - Do not compute expected values with the same logic under test.
   - Use literals, hand-checked fixtures, small examples, or invariant/property assertions (`pgregory.net/rapid` is already a dependency for property and stress tests).
   - Keep test logic simple enough to review by inspection.

8. **Do not test upstream functionality**
   - Do not prove that Huma, cobra, bubbletea, SQLite, testify, or a generated client works as documented.
   - Example: do not test Huma path params, query strings, status codes, or OpenAPI wiring unless our code adds behavior there (the committed OpenAPI/generated-client drift tests are the intended way to protect those artifacts — don't duplicate them).
   - Test our boundary contract instead: route registration, value handoff to domain code, error shapes (`internal/api` error folding is ours), auth decisions, and response shape.
   - For surprising upstream behavior, write a narrow characterization test around our integration point and name the upstream assumption in a comment.
   - For fake external services (fake hub, fake GitHub), test consumer behavior; if the fake itself is load-bearing, add a contract check that keeps it honest.

9. **Avoid blindingly obvious current-code assertions**
   - Do not test that the implementation is written the way it is written now.
   - Skip tests for plain constructor assignment, getters, trivial forwarding, constants, and data-only structs.
   - Test them only when they validate, normalize, default, derive, copy, enforce permissions, handle errors, cause side effects, or protect compatibility.
   - Prefer the first consumer-visible result that depends on the fields.

10. **No content-assertion shell tests**
    - Never assert shell script, workflow, or config behavior by grepping the file for implementation text (repo rule). Exercise the command or use the tool's own validation; if that's impractical, document the manual check instead.

11. **NEVER write negative-existence refactoring tests**
    - When cleaning up code, do not add tests asserting that a function, method, file, flag, route, or symbol *no longer exists* (grepping the source tree, reflecting on types, asserting a file is absent, asserting a package doesn't compile a name).
    - These are tautologies about the current shape of the codebase: they pin an implementation detail, break legitimate future reintroduction, and catch no behavior. The compiler already fails on references to removed Go symbols; deleted behavior is protected by the *positive* tests of what replaced it.
    - If a removal has a user-visible contract (a CLI flag now rejected, an endpoint now 404/410, a config key now erroring), test that observable behavior instead — the response the user gets, not the absence of code.

## Test Level

Use the narrowest test that can catch the break. The ladder in this repo:

- **Package unit tests** next to the code, often against a real in-memory/temp SQLite store. Cheapest; use for domain logic, parsing, folding, ordering.
- **`internal/testenv` integration tests**: real daemon + HTTP client + DB per test. Use when the behavior spans handler ↔ store ↔ events, or depends on daemon config (`testenv.Option`s like auth or close-throttle).
- **`cmd/kata` CLI tests**: assert user-visible output, exit codes, and written files — the CLI is a public contract.
- **`e2e/`**: full workflows over HTTP (lifecycle, federation, hooks, remote-client). Keep them non-tautological: assert the workflow result, stored state, or API contract, not just that nothing crashed. Respect `-short` for slow ones.
- **TUI golden snapshots** (`internal/tui`, `-update-goldens`): freeze time and color mode as the harness does; treat golden diffs as contract changes to review, not noise to regenerate.
- **rapid property/stress tests** for invariants under generated inputs (see the federation stress harness).

Tests run with `-shuffle=on`, so no test may depend on execution order or state left behind by another test.

## Repo Hygiene That Interacts With Tests

- **Determinism**: use `testfix.InitGitRepo` (fixed author), frozen clocks like the TUI's `snapshotFixedNow`, and `t.TempDir()`/per-test daemons — never the host's git config, wall clock, or a shared daemon.
- **No private project data**: fixtures and test names use neutral placeholders (`spoke-project`, `hub-project`, `example-workspace`, `daemon.example`), never real customer/host/repo names.

## Mutation Check

Before finishing, mentally mutate the production code. At least one relevant test should fail for each realistic mutation.

- Wrong constant or argument.
- Wrong branch handler (success double satisfying the error path).
- Missing state change (200 returned but row not updated — necessary but not sufficient).
- Empty/default return.
- Missing side effect (no event emitted, no file written).
- Broken fake at a boundary our code should notice.
- Renamed or rearranged private fields with behavior preserved.
- Missing validation for zero, empty, nil, unauthorized, or malformed input.

If none fail, the test is probably tautological.

## Red Flags

- Reuses the same setup/assertion object, guaranteeing equality.
- Can fail only through panic, nil deref, or daemon crash.
- Still matters if only Huma/bubbletea/SQLite remains and all our code is deleted.
- Translates a constructor, getter, setter, mapper, or wrapper line by line.
- Exists for coverage without checking side effects, boundaries, or outcomes.
- Hides expected values behind loops, formatters, builders, or helpers.
- Asserts that removed code stays removed (file absent, symbol gone, grep finds nothing) instead of testing the replacement's behavior.
- Asserts only the action endpoint's status code without re-reading the state it claims to change.
- Regenerated goldens or flipped literals in the same commit that changed behavior, with no note on why the new contract is right.
