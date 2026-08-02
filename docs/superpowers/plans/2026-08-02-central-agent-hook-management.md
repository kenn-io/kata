# Central Agent-Hook Management Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan directly in the current agent, task-by-task. Never use subagent-driven development. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace kata's duplicated Claude and Codex hook configuration logic with `go.kenn.io/kit/agenthook` v0.14.0 while preserving the existing CLI and attention lifecycle.

**Approved spec/design:** `docs/superpowers/specs/2026-08-02-central-agent-hook-management-design.md`

**Architecture:** A small kata adapter retains workspace symlink policy and invokes `agenthook.Install` with explicit workspace config paths, ownership markers, events, matchers, and timeouts. Claude keeps a bounded exact-shape migration for v0.13.0 command-plus-arguments handlers; Codex keeps its kata-specific TOML advisory. Runtime dispatch remains `kata attention-hook <start|end>`.

**Tech Stack:** Go, Cobra, `go.kenn.io/kit/agenthook` v0.14.0, `testify`, existing CLI end-to-end harness.

## Global Constraints

- Keep `kata init --with-hooks` and `kata init --with-codex-hooks` unchanged.
- Keep `kata attention-hook <start|end>` and its `KATA_REF` behavior unchanged.
- Do not expose additional kit agent profiles or use `agenthook.Handle`.
- Claude installs start and end lifecycle hooks; Codex installs only its existing start hook with a ten-second timeout.
- Refuse symlinked managed directories and config files before invoking kit.
- Preserve the Codex `.codex/config.toml` `[hooks]` advisory.
- Migrate only exact v0.13.0 Claude handlers; issue `m7d5` removes the migration after kata v0.15.0.
- Preserve unrelated hook configuration and leave syntactically malformed or structurally invalid configuration untouched.
- Do not add database migrations or persisted-schema changes.
- Do not test kit internals, deleted-symbol absence, or dependency version strings.

---

### Task 1: Centralize Codex Hook Installation

**Files:**
- Create: `cmd/kata/init_agent_hooks.go`
- Modify: `cmd/kata/init_hooks_codex.go`
- Modify: `cmd/kata/init_hooks_codex_test.go`
- Modify: `go.mod`
- Modify: `go.sum`

**Interfaces:**
- Produces: `installAttentionHook(agent agenthook.Agent, configPath, mode string, hook agenthook.Hook) (bool, error)` for both agent adapters.
- Produces: `planAttentionHook(agent agenthook.Agent, configPath, mode string, hook agenthook.Hook) error` for full-file validation before the Claude legacy migration writes.
- Produces: `refuseSymlinkComponents(root *os.Root, rels ...string) error` as kata's workspace boundary.
- Produces: `validateManagedHookJSON(configPath string, events ...agenthook.Event) error` to retain kata's strict rejection of JSON `null` at managed structural boundaries while kit owns all other parsing and mutation.
- Preserves: `applyCodexHooks(dir string) (bool, []string, error)` for callers in `init.go`.

- [ ] **Step 1: Write the failing ownership-marker test**

Add this behavior test to `cmd/kata/init_hooks_codex_test.go`. The production break it catches is an installer that compares the complete command instead of replacing a kata-owned command whose executable path changed.

```go
func TestApplyCodexHooks_ReplacesOwnedCommandAfterExecutablePathChanges(t *testing.T) {
	dir := t.TempDir()
	writeCodexHooks(t, dir, map[string]any{
		"hooks": map[string]any{
			"SessionStart": []any{map[string]any{
				"matcher": codexSessionStartMatcher,
				"hooks": []any{map[string]any{
					"type":    "command",
					"command": "/opt/example/bin/kata attention-hook start",
					"timeout": json.Number("10"),
				}},
			}},
		},
	})

	changed, warnings, err := applyCodexHooks(dir)
	require.NoError(t, err)
	assert.True(t, changed)
	assert.Empty(t, warnings)

	file := readCodexHooks(t, dir)
	groups := file["hooks"].(map[string]any)["SessionStart"].([]any)
	require.Len(t, groups, 1)
	handlers := groups[0].(map[string]any)["hooks"].([]any)
	assert.Equal(t, []any{expectedCodexHandler()}, handlers)
}
```

Before running RED, update `expectedCodexHandler` for the shared profile's
cross-platform command contract:

```go
func expectedCodexHandler() map[string]any {
	return map[string]any{
		"type":           "command",
		"command":        "kata attention-hook start",
		"commandWindows": "kata attention-hook start",
		"timeout":        json.Number("10"),
	}
}
```

Add a collision-preservation fixture containing `notify attention-hook start`;
the kata-specific marker must leave it untouched while replacing
`/opt/example/bin/kata attention-hook start`.

- [ ] **Step 2: Run the focused test and verify RED**

Run:

```bash
go test ./cmd/kata -run '^TestApplyCodexHooks_ReplacesOwnedCommandAfterExecutablePathChanges$' -count=1
```

Expected: FAIL because the current deep-equality upsert preserves the old-path handler and appends a second handler group.

- [ ] **Step 3: Upgrade kit and add the shared adapter**

Run:

```bash
go get go.kenn.io/kit@v0.14.0
go mod tidy
```

Create `cmd/kata/init_agent_hooks.go` with the common installation seam:

```go
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"go.kenn.io/kit/agenthook"
)

func installAttentionHook(
	agent agenthook.Agent,
	configPath, mode string,
	hook agenthook.Hook,
) (bool, error) {
	result, err := agenthook.Install(agent, agenthook.InstallOptions{
		ConfigPath: configPath,
		Executable: "kata",
		Arguments:  []string{"attention-hook", mode},
		Marker:     "kata attention-hook " + mode,
		Hooks:      []agenthook.Hook{hook},
	})
	return result.Changed, err
}

func planAttentionHook(
	agent agenthook.Agent,
	configPath, mode string,
	hook agenthook.Hook,
) error {
	_, err := agenthook.PlanInstall(agent, agenthook.InstallOptions{
		ConfigPath: configPath,
		Executable: "kata",
		Arguments:  []string{"attention-hook", mode},
		Marker:     "kata attention-hook " + mode,
		Hooks:      []agenthook.Hook{hook},
	})
	return err
}

func validateManagedHookJSON(configPath string, events ...agenthook.Event) error {
	data, err := os.ReadFile(configPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var root map[string]any
	if err := decoder.Decode(&root); err != nil {
		return err
	}
	if root == nil {
		return errors.New("hook config root must be an object")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("multiple JSON values")
		}
		return err
	}
	rawHooks, exists := root["hooks"]
	if !exists {
		return nil
	}
	hooks, ok := rawHooks.(map[string]any)
	if !ok {
		return errors.New("hook config hooks must be an object")
	}
	for _, event := range events {
		if raw, exists := hooks[string(event)]; exists && raw == nil {
			return fmt.Errorf("hook config event %s must be an array", event)
		}
	}
	return nil
}

func refuseSymlinkComponents(root *os.Root, rels ...string) error {
	for _, rel := range rels {
		fi, err := root.Lstat(rel)
		switch {
		case errors.Is(err, os.ErrNotExist):
			return nil
		case err != nil:
			return err
		case fi.Mode()&os.ModeSymlink != 0:
			return fmt.Errorf("refusing to manage symlinked %s", filepath.Join(root.Name(), rel))
		}
	}
	return nil
}
```

Replace Codex's custom JSON upsert/write path with:

```go
func applyCodexHooks(dir string) (bool, []string, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return false, nil, err
	}
	defer func() { _ = root.Close() }()
	if err := refuseSymlinkComponents(root, ".codex", ".codex/hooks.json"); err != nil {
		return false, nil, err
	}
	configPath := filepath.Join(root.Name(), ".codex", "hooks.json")
	if err := validateManagedHookJSON(configPath, agenthook.EventSessionStart); err != nil {
		return false, nil, err
	}
	warnings := codexConfigHooksWarnings(root)
	changed, err := installAttentionHook(
		agenthook.AgentCodex,
		configPath,
		"start",
		agenthook.Hook{
			Event: agenthook.EventSessionStart,
			Matcher: codexSessionStartMatcher,
			Timeout: 10 * time.Second,
		},
	)
	return changed, warnings, err
}
```

Keep `codexConfigHooksWarnings`, `writeCodexConfig` test setup, and the public return signature. Delete Codex's local JSON decoder, shape walker, upsert, encoder, and atomic writer calls. Adjust imports and move `refuseSymlinkComponents` out of the Claude file to the shared adapter.

- [ ] **Step 4: Run focused Codex tests and verify GREEN**

Run:

```bash
gofmt -w cmd/kata/init_agent_hooks.go cmd/kata/init_hooks_codex.go cmd/kata/init_hooks_codex_test.go
go test ./cmd/kata -run '^(TestApplyCodexHooks_|TestE2E_InitWithCodexHooks)' -count=1
```

Expected: PASS. If a test only asserts the removed local parser/writer implementation rather than kata's adapter contract, delete or narrow it; retain fresh install, preservation, idempotency, legacy matcher narrowing, malformed/invalid config, symlink refusal, warning, and ownership-marker coverage.

- [ ] **Step 5: Inspect the dependency delta**

Run:

```bash
git diff -- go.mod go.sum
go mod verify
```

Expected: `go.kenn.io/kit v0.14.0` is selected, checksums verify, and no unrelated direct dependency is added.

- [ ] **Step 6: Commit the Codex adapter**

Use the mandatory commit skill, then run:

```bash
git add go.mod go.sum cmd/kata/init_agent_hooks.go cmd/kata/init_hooks_codex.go cmd/kata/init_hooks_codex_test.go
git commit -m "Use shared agent-hook management for Codex" \
  -m "Codex hook configuration duplicated parsing and ownership logic that kit v0.14.0 now maintains centrally. Keep kata's workspace symlink boundary and TOML advisory while delegating marker-based config mutation to the shared profile." \
  -m $'Generated with OpenAI Codex (gpt-5.6-sol)\nCo-authored-by: OpenAI Codex <noreply@openai.com>'
```

Do not bypass hooks.

---

### Task 2: Centralize Claude Installation and Migrate v0.13 Entries

**Files:**
- Modify: `cmd/kata/init_hooks.go`
- Modify: `cmd/kata/init_hooks_test.go`
- Modify: `cmd/kata/init_with_hooks_e2e_test.go`
- Modify: `cmd/kata/init_with_codex_hooks_e2e_test.go`

**Interfaces:**
- Consumes: `installAttentionHook` and `refuseSymlinkComponents` from Task 1.
- Preserves: `applyClaudeHooks(dir string) (bool, error)` for callers in `init.go`.
- Produces: a bounded exact v0.13 migration internal to the Claude adapter; issue `m7d5` owns its removal.

- [ ] **Step 1: Change the expected Claude command shape and add the migration regression test**

Change `expectedHookHandler` in `cmd/kata/init_hooks_test.go` to the shared command-string representation:

```go
func expectedHookHandler(mode string) map[string]any {
	return map[string]any{
		"type":    "command",
		"command": "kata attention-hook " + mode,
	}
}
```

Add a test that seeds both exact v0.13 handlers plus an unrelated handler and verifies convergence without duplication:

```go
func TestApplyClaudeHooks_MigratesExactV013Handlers(t *testing.T) {
	dir := t.TempDir()
	legacy := func(mode string) map[string]any {
		return map[string]any{
			"type": "command", "command": "kata",
			"args": []any{"attention-hook", mode},
		}
	}
	unrelated := map[string]any{"type": "command", "command": "notify-session"}
	writeSettings(t, dir, map[string]any{
		"hooks": map[string]any{
			"SessionStart": []any{map[string]any{
				"matcher": "startup|resume|clear",
				"hooks": []any{legacy("start"), unrelated},
			}},
			"SessionEnd": []any{map[string]any{
				"matcher": "logout|prompt_input_exit|bypass_permissions_disabled|other",
				"hooks": []any{legacy("end")},
			}},
		},
	})

	changed, err := applyClaudeHooks(dir)
	require.NoError(t, err)
	assert.True(t, changed)

	hooks := readSettings(t, dir)["hooks"].(map[string]any)
	assert.Equal(t, []any{
		map[string]any{
			"matcher": "startup|resume|clear",
			"hooks": []any{unrelated},
		},
		map[string]any{
			"matcher": "startup|resume|clear",
			"hooks": []any{expectedHookHandler("start")},
		},
	}, hooks["SessionStart"])
	assert.Equal(t, []any{map[string]any{
		"matcher": "logout|prompt_input_exit|bypass_permissions_disabled|other",
		"hooks": []any{expectedHookHandler("end")},
	}}, hooks["SessionEnd"])
}
```

Also add an ownership-marker test analogous to Task 1 using `/opt/example/bin/kata attention-hook start`; it must converge to the canonical command and retain unrelated handlers.

Before implementation, add table-driven negative fixtures for the migration
ownership boundary. Each fixture starts from the legacy-shaped handler and
changes exactly one property: alternate matcher, command other than `kata`, an
extra argument, or an extra handler field such as `timeout`. After
`applyClaudeHooks`, each modified fixture must still be present alongside the
new managed hook. These rows prove the migration owns only the exact three-key
v0.13 handler (`type`, `command`, and `args`).

Add a malformed-file regression that combines an exact legacy handler with an
unrelated event whose value is an object instead of an event array. Assert that
`applyClaudeHooks` returns an error and the config remains byte-for-byte
unchanged. The production break it catches is migration writing before kit has
validated the complete file.

- [ ] **Step 2: Run the focused tests and verify RED**

Run:

```bash
go test ./cmd/kata -run '^(TestApplyClaudeHooks_FreshWorkspace|TestApplyClaudeHooks_MigratesExactV013Handlers|TestApplyClaudeHooks_PreservesNonExactLegacyHandlers|TestApplyClaudeHooks_MalformedUnrelatedEventDoesNotPartiallyMigrate|TestApplyClaudeHooks_ReplacesOwnedCommandAfterExecutablePathChanges)$' -count=1
```

Expected: FAIL because the current installer writes command-plus-arguments entries and does not replace marker-owned command strings after an executable path change.

- [ ] **Step 3: Implement the exact forward migration**

Retain only the small JSON read/encode and atomic replacement pieces needed for shipped-state migration. Add a pure traversal that removes a handler only when all of these match: managed event, managed matcher, an exact three-key handler object, type `command`, command `kata`, and the exact two-element semantic command `attention-hook <mode>` represented by the existing arguments array.

The traversal shape is:

```go
func removeLegacyClaudeHandlers(settings map[string]any) (bool, error) {
	changed := false
	for _, spec := range claudeHookSpecs() {
		groups, err := legacyClaudeEventGroups(settings, spec.Event)
		if err != nil {
			return false, err
		}
		if groups == nil {
			continue
		}
		for _, rawGroup := range groups {
			group, ok := rawGroup.(map[string]any)
			if !ok || group["matcher"] != spec.Matcher {
				continue
			}
			handlers, ok := group["hooks"].([]any)
			if !ok {
				continue
			}
			kept := handlers[:0]
			for _, rawHandler := range handlers {
				if isLegacyClaudeHandler(rawHandler, spec.Mode) {
					changed = true
					continue
				}
				kept = append(kept, rawHandler)
			}
			group["hooks"] = kept
		}
	}
	return changed, nil
}
```

`legacyClaudeEventGroups` must return `nil, nil` for absent fields and an error for a malformed managed shape, so the real adapter leaves invalid files untouched. `isLegacyClaudeHandler` must require `len(handler) == 3` in addition to exact field values; it must not treat arbitrary `kata` commands, alternate matchers, extra arguments, or extra handler fields as owned.

When the migration changes data, encode with the existing number-preserving JSON path and atomically replace the real workspace file before shared installation. Keep the migration comment explicit: shipped in v0.13.0, retained through v0.15.0, removed by issue `m7d5`.

- [ ] **Step 4: Delegate normal Claude installation to kit**

Reduce `applyClaudeHooks` to symlink validation, `validateManagedHookJSON` for
`SessionStart` and `SessionEnd`, complete-file kit preflight, exact migration,
and two shared calls. Resolve the config path once and pass it to validation,
migration, both preflights, and both installs. Before migration writes, call
`planAttentionHook` for both desired Claude registrations against the original
file; if either plan fails, return without changing the file:

```go
if err := planAttentionHook(agenthook.AgentClaude, configPath, "start", startHook); err != nil {
	return false, err
}
if err := planAttentionHook(agenthook.AgentClaude, configPath, "end", endHook); err != nil {
	return false, err
}
```

Then migrate and install with those same `startHook` and `endHook` values:

```go
startChanged, err := installAttentionHook(
	agenthook.AgentClaude,
	configPath,
	"start",
	agenthook.Hook{
		Event: agenthook.EventSessionStart,
		Matcher: "startup|resume|clear",
	},
)
if err != nil {
	return false, err
}
endChanged, err := installAttentionHook(
	agenthook.AgentClaude,
	configPath,
	"end",
	agenthook.Hook{
		Event: agenthook.EventSessionEnd,
		Matcher: "logout|prompt_input_exit|bypass_permissions_disabled|other",
	},
)
if err != nil {
	return false, err
}
return migrated || startChanged || endChanged, nil
```

Delete the old normal-path hook renderer, generic upsert, and config creation logic. Do not retain a second normal installer alongside kit.

- [ ] **Step 5: Run focused Claude and combined end-to-end tests**

Run:

```bash
gofmt -w cmd/kata/init_hooks.go cmd/kata/init_hooks_test.go cmd/kata/init_with_hooks_e2e_test.go cmd/kata/init_with_codex_hooks_e2e_test.go
go test ./cmd/kata -run '^(TestApplyClaudeHooks_|TestE2E_InitWithHooks_|TestE2E_InitWithCodexHooks_Composes)' -count=1
```

Expected: PASS. Update end-to-end expected Claude handlers to command strings. Remove normal-path atomic-writer tests that now exercise kit rather than kata; the migration regression covers kata's remaining owned traversal and write seam.

- [ ] **Step 6: Run mutation checks against the migration test**

Confirm the Step 1 table fails if each ownership guard is mentally removed:
matcher equality, command equality, argument length/content, handler key count,
and preservation of unrelated handlers in the same group. Add no post-hoc test
cases here; every executable row was written and observed failing before
production code. Do not reproduce kit's ownership tests.

- [ ] **Step 7: Commit the Claude adapter**

Use the mandatory commit skill, then run:

```bash
git add cmd/kata/init_hooks.go cmd/kata/init_hooks_test.go cmd/kata/init_with_hooks_e2e_test.go cmd/kata/init_with_codex_hooks_e2e_test.go
git commit -m "Use shared agent-hook management for Claude" \
  -m "Claude hook configuration now shares kit's marker-based installer, eliminating kata's normal-path JSON upsert logic. Migrate only the exact v0.13 command-plus-arguments entries after a complete-file preflight, and track removal of that bounded upgrade path in issue m7d5." \
  -m $'Generated with OpenAI Codex (gpt-5.6-sol)\nCo-authored-by: OpenAI Codex <noreply@openai.com>'
```

Do not bypass hooks.

---

### Task 3: Align User Documentation and Verify the Migration

**Files:**
- Modify: `docs/reference/cli.md`
- Modify: `docs/workflows/agents.md`
- Modify: `docs/operations/agent-orchestration.md`
- Modify: `docs/changelog.md`

**Interfaces:**
- Consumes: the unchanged public flags and lifecycle behavior from Tasks 1 and 2.
- Produces: user-facing descriptions of shared command registrations without exposing implementation-only migration details.

- [ ] **Step 1: Update user-facing hook descriptions**

Replace claims that Claude uses command-plus-arguments “exec-form” entries with accurate command-hook wording. Keep the security statement that installed commands invoke the trusted `kata` binary rather than a repository-controlled script. Preserve the documented event matchers, Codex start-only limitation, wrapper recipe, idempotency, preservation, and symlink refusal.

Add an unreleased changelog bullet similar to:

```markdown
- Centralized Claude Code and Codex hook configuration on kit's shared
  agent-hook manager while preserving the existing init flags, lifecycle
  matchers, and attention behavior.
```

- [ ] **Step 2: Check documentation consistency**

Run:

```bash
rg -n "exec-form|with-hooks|with-codex-hooks|attention-hook" \
  docs/reference/cli.md docs/workflows/agents.md \
  docs/operations/agent-orchestration.md docs/changelog.md
```

Expected: no remaining claim that the newly installed Claude entry has separate `command` and `args`; all flags and lifecycle descriptions remain present.

- [ ] **Step 3: Run scoped and full verification**

Before executing branch code, use the required isolate-production skill. Then run:

```bash
go test ./cmd/kata -run '^(TestApplyClaudeHooks_|TestApplyCodexHooks_|TestE2E_InitWithHooks_|TestE2E_InitWithCodexHooks_)' -count=1
go test ./cmd/kata -count=1
go test ./... -count=1
go vet ./...
go mod verify
git diff --check
```

Expected: every command exits zero with no unexpected warnings. If the full suite requires an external service, report the exact skipped or unavailable target; do not point tests at a live daemon or user data directory.

- [ ] **Step 4: Inspect the final diff and removed duplication**

Run:

```bash
git status --short
git diff --stat 9856a95
git diff --check 9856a95
rg -n "ensureCodexHooksFile|upsertCodexHook|upsertClaudeHook" cmd/kata
```

Expected: only scoped migration, adapter, dependency, test, plan, and
documentation changes remain in the complete committed-plus-working-tree diff.
The complete diff has no whitespace errors. The `rg` command returns no
matches; this is a one-time verification, not a test.

- [ ] **Step 5: Commit documentation and final cleanup**

Use the mandatory commit skill, then run:

```bash
git add docs/reference/cli.md docs/workflows/agents.md docs/operations/agent-orchestration.md docs/changelog.md
git commit -m "Document shared agent-hook management" \
  -m "The hook flags and attention lifecycle stay stable, but Claude's managed config entry is now a shared command registration rather than kata's custom exec-form mutation. Keep user guidance accurate without exposing the bounded internal migration." \
  -m $'Generated with OpenAI Codex (gpt-5.6-sol)\nCo-authored-by: OpenAI Codex <noreply@openai.com>'
```

Do not bypass hooks. After the commit, run the complete-tree checks again:

```bash
git diff --check 9856a95..HEAD
git diff --stat 9856a95..HEAD
git status --short
```

Expected: the complete committed diff has no whitespace errors and the working
tree is clean.

- [ ] **Step 6: Close the implementation issue with evidence**

After all verification and commits succeed, run:

```bash
kata_commit=$(git rev-parse HEAD)
kata close 7y6d --done \
  --message "Replaced kata's Claude and Codex hook config mutation with kit v0.14.0 agenthook management while retaining the public flags, lifecycle behavior, symlink boundary, Codex advisory, and a bounded exact v0.13 migration; focused and full Go verification pass." \
  --commit "$kata_commit"
```

Leave cleanup issue `m7d5` open until its post-v0.15.0 removal condition is satisfied.
