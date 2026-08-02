# Central Agent-Hook Management Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan directly in the current agent, task-by-task. Never use subagent-driven development. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace kata's duplicated Claude and Codex hook configuration logic with `go.kenn.io/kit/agenthook` v0.14.0 while preserving the existing CLI and attention lifecycle.

**Approved spec/design:** `docs/superpowers/specs/2026-08-02-central-agent-hook-management-design.md`

**Architecture:** Kata copies the current workspace config to a private staging file, adopts only exact v0.13.0 handlers there, lets kit install every desired registration, and atomically publishes the complete final file once. Managed commands carry a mode-specific `--source kata-agent-hook-<mode>` token used as kit's unique ownership marker. Workspace symlink refusal and the Codex TOML advisory remain kata policy; runtime behavior remains `kata attention-hook <start|end>`.

**Tech Stack:** Go, Cobra, `go.kenn.io/kit/agenthook` v0.14.0, `testify`, existing CLI end-to-end harness.

## Global Constraints

- Keep `kata init --with-hooks` and `kata init --with-codex-hooks` unchanged.
- Keep direct `kata attention-hook <start|end>` invocations and `KATA_REF` behavior unchanged.
- Do not expose additional kit profiles or use `agenthook.Handle`.
- Claude installs start and end hooks; Codex installs only its existing start hook with a ten-second timeout.
- Refuse symlinked managed directories and config files before staging.
- Preserve the Codex `.codex/config.toml` `[hooks]` advisory, but emit it only after successful installation.
- Use `--source kata-agent-hook-<mode>` as the unique ownership marker; lifecycle command text without that token is unrelated.
- Adopt only exact v0.13.0 Claude and Codex handlers. Issue `m7d5` removes both bounded migrations after kata v0.15.0.
- Build the complete migrated result in staging and publish it with one atomic workspace write.
- Preserve unrelated config and leave malformed config byte-for-byte untouched.
- Do not test kit internals, deleted-symbol absence, or dependency version strings.

---

### Task 1: Centralize Codex Installation and Shared Infrastructure

**Files:**
- Create: `cmd/kata/init_agent_hooks.go`
- Modify: `cmd/kata/init_hooks.go`
- Modify: `cmd/kata/init_hooks_codex.go`
- Modify: `cmd/kata/init_hooks_codex_test.go`
- Modify: `cmd/kata/attention_hook.go`
- Modify: `cmd/kata/attention_hook_unit_test.go`
- Modify: `go.mod`
- Modify: `go.sum`

**Interfaces:**
- Produces: `installAttentionHooks(root *os.Root, rel string, agent agenthook.Agent, migrate agentHookConfigMigration, registrations ...attentionHookRegistration) (bool, error)`.
- Produces: `attentionHookInstallOptions(configPath, mode string, hook agenthook.Hook) agenthook.InstallOptions` with the source marker.
- Produces: `removeExactAgentHook(...) (bool, error)` for bounded adoption in staging.
- Produces: `parseAttentionHookArgs(args []string) (string, bool)` for direct and managed hidden invocations.
- Preserves: `applyCodexHooks(dir string) (bool, []string, error)`.

- [x] **Step 1: Write and run the failing ownership test**

The test seeded `/opt/example/bin/kata attention-hook start` plus an unrelated lifecycle-text command. It failed because the old installer compared complete handlers and omitted `commandWindows`.

- [x] **Step 2: Upgrade kit**

Executed:

```bash
go get go.kenn.io/kit@v0.14.0
go mod tidy
go mod verify
```

Only the direct kit version and its checksums changed.

- [x] **Step 3: Delegate Codex config mutation to kit**

The shared options use executable/arguments construction rather than raw shell interpolation:

```go
source := attentionHookSource + mode
opts := agenthook.InstallOptions{
	ConfigPath: configPath,
	Executable: "kata",
	Arguments:  []string{"attention-hook", mode, "--source", source},
	Marker:     "--source " + source,
	Hooks:      []agenthook.Hook{hook},
}
```

`parseAttentionHookArgs` accepts the existing one-argument form and only the exact source token for managed three-argument invocations.

- [x] **Step 4: Add review regressions before correcting ownership**

Added failing coverage for:

- preservation of `notify "kata attention-hook start failed"`;
- exact v0.13 Codex adoption;
- `commandWindows` output;
- hidden source-token parsing; and
- suppression of the success advisory when installation fails.

- [x] **Step 5: Stage and atomically publish complete configs**

`installAttentionHooks` writes the original bytes to a private temporary file, applies exact migration callbacks there, invokes `agenthook.Install` for every registration there, reads the final bytes, and calls `atomicReplaceSettings` once. Any migration or install error returns before the live config changes.

- [x] **Step 6: Verify focused Codex behavior**

Executed against isolated scratch state:

```bash
go test ./cmd/kata -run '^(TestParseAttentionHookArgs_|TestAttentionHookCommand_|TestApplyCodexHooks_)' -count=1
```

Expected and observed: PASS.

---

### Task 2: Centralize Claude Installation and Exact Legacy Adoption

**Files:**
- Modify: `cmd/kata/init_agent_hooks.go`
- Modify: `cmd/kata/init_hooks.go`
- Modify: `cmd/kata/init_hooks_test.go`
- Modify: `cmd/kata/init_with_hooks_e2e_test.go`
- Modify: `cmd/kata/init_with_codex_hooks_e2e_test.go`

**Interfaces:**
- Consumes: staged `installAttentionHooks` and exact `removeExactAgentHook` from Task 1.
- Preserves: `applyClaudeHooks(dir string) (bool, error)`.
- Produces: `migrateLegacyClaudeHooks(settings map[string]any) (bool, error)` through kata v0.15.0.

- [x] **Step 1: Write all migration-boundary tests before implementation**

The RED suite covered:

- exact start/end command-plus-arguments migration;
- preservation for alternate matcher, alternate command, extra argument, and extra handler field;
- preservation of unrelated handlers in the same group;
- marker-owned replacement after executable-path changes;
- malformed unrelated events leaving the original bytes unchanged; and
- injected failure on the second staged install leaving the original bytes unchanged.

- [x] **Step 2: Implement exact migration in staging**

The exact Claude handler is:

```go
map[string]any{
	"type":    "command",
	"command": "kata",
	"args":    []any{"attention-hook", mode},
}
```

`removeExactAgentHook` uses whole-map equality. It prunes a now-empty group only when that group has exactly `matcher` and `hooks`; groups with unrelated metadata remain.

- [x] **Step 3: Install both Claude registrations before publication**

```go
registrations := []attentionHookRegistration{
	{
		mode: "start",
		hook: agenthook.Hook{
			Event: agenthook.EventSessionStart,
			Matcher: "startup|resume|clear",
		},
	},
	{
		mode: "end",
		hook: agenthook.Hook{
			Event: agenthook.EventSessionEnd,
			Matcher: "logout|prompt_input_exit|bypass_permissions_disabled|other",
		},
	},
}
```

Both kit writes target staging; one fd-relative atomic rename publishes the final workspace file.

- [x] **Step 4: Verify focused Claude and combined CLI behavior**

Executed against isolated scratch state:

```bash
go test ./cmd/kata -run '^(TestApplyClaudeHooks_|TestE2E_InitWithHooks_|TestE2E_InitWithCodexHooks_|TestAttentionHook)' -count=1
```

Expected and observed: PASS.

- [x] **Step 5: Commit the complete review correction and Claude adapter**

Committed after full verification with subject:

```text
Atomically migrate shared agent hooks
```

The body must explain the unique source marker, exact v0.13 adoption for both agents, and one-write staging guarantee. Include required attribution and never bypass hooks.

---

### Task 3: Align Documentation, Verify, and Close Work

**Files:**
- Modify: `docs/reference/cli.md`
- Modify: `docs/workflows/agents.md`
- Modify: `docs/operations/agent-orchestration.md`
- Modify: `docs/changelog.md`
- Modify: `docs/superpowers/specs/2026-08-02-central-agent-hook-management-design.md`
- Modify: `docs/superpowers/plans/2026-08-02-central-agent-hook-management.md`

**Interfaces:**
- Consumes: unchanged public flags and lifecycle behavior.
- Produces: accurate command-hook documentation without internal migration detail in user workflows.

- [x] **Step 1: Update user-facing descriptions**

Replace Claude “exec-form” claims with command-hook wording. Preserve lifecycle matchers, Codex start-only limitation, launcher wrapper, idempotency, config preservation, and symlink refusal. Add an unreleased changelog bullet for shared management.

- [x] **Step 2: Check documentation consistency**

Run:

```bash
rg -n "exec-form|with-hooks|with-codex-hooks|attention-hook" \
  docs/reference/cli.md docs/workflows/agents.md \
  docs/operations/agent-orchestration.md docs/changelog.md
```

Expected: no claim that newly installed Claude handlers separate `command` and `args`; public flags and lifecycle descriptions remain.

- [x] **Step 3: Run complete isolated verification**

Run with scratch `HOME`, `KATA_HOME`, `GOMODCACHE`, and `GOCACHE`:

```bash
go test ./cmd/kata -count=1
go test ./... -count=1
go vet ./...
go mod verify
git diff --check 9856a95
```

Expected: all commands exit zero. Existing macOS sqlite-vector deprecation warnings are non-failing baseline output.

- [x] **Step 4: Commit docs and any final cleanup**

Use the mandatory commit skill with a rationale-focused body and required attribution. Do not bypass hooks.

- [x] **Step 5: Verify the complete committed tree**

Run:

```bash
git diff --check 9856a95..HEAD
git diff --stat 9856a95..HEAD
git status --short
rg -n "ensureCodexHooksFile|upsertCodexHook|upsertClaudeHook" cmd/kata
```

Expected: the complete committed diff has no whitespace errors, the working tree is clean, and the removed local upsert symbols have no matches.

- [x] **Step 6: Record and close work**

Comment on and close RoboRev jobs `9338`, `9343`, `9344`, and `9345` only after their fixes and tests are committed. Then close kata issue `7y6d` with the final implementation commit and leave cleanup issue `m7d5` open.
