# Agent Output Format Implementation Plan

> **For agentic workers:** REQUIRED: Use superpowers:subagent-driven-development (if subagents available) or superpowers:executing-plans to implement this plan. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Implement the approved `--format agent` CLI output contract for kata while preserving existing human and JSON behavior.

**Architecture:** Add one shared output-mode layer under `cmd/kata` that resolves `--format`, `--json`, and `--agent`, centralizes agent quoting/fencing/error emission, and exposes small render helpers. Convert command printers to dispatch on the resolved mode without changing daemon HTTP requests or response schemas.

**Tech Stack:** Go 1.26, Cobra/pflag, existing kata daemon HTTP API, existing `cmd/kata` test fixtures, `testify`.

---

## File Structure

- Create `cmd/kata/output_mode.go`: output mode enum, flag resolution, conflict validation, agent value quoting, line sanitization, fenced text helper, shared agent error emitter, and generic row helpers.
- Create `cmd/kata/output_mode_test.go`: focused unit tests for output mode resolution, agent quoting, fences, and agent error emission.
- Modify `cmd/kata/main.go`: add `--format` and `--agent`, resolve output mode, route errors through agent/json/human emitters, and update help tests.
- Modify `cmd/kata/helpers.go`: keep `emitJSON`; add version constant `agentFormatVersion = 1` if it is not in `output_mode.go`.
- Modify command files with existing renderers: `create.go`, `comment.go`, `close.go`, `reopen.go`, `assign.go`, `label.go`, `delete.go`, `restore.go`, `list.go`, `show.go`, `search.go`, `ready.go`, `events.go`, `digest.go`, `audit_closes.go`, `whoami.go`, `version.go`, `health.go`, `daemon_cmd.go`, `daemon_logs.go`, `export.go`, `import.go`, `beads_import.go`, `projects.go`, `quickstart.go`, `tui_cmd.go`.
- Modify tests adjacent to each changed command, preferring focused additions to existing `*_test.go` files.
- Modify `README.md` and quickstart text in `cmd/kata/quickstart.go` to recommend `--agent` for transcript-friendly agent use and `--json` for complete structured parsing.

## Implementation Notes

- During the `kata import` deprecation window, route the single root `--format` flag by value:
  - `human|json|agent` means output mode.
  - `kata|beads` means legacy import source format only for `kata import`, only when `--source-format` is absent.
  - The namespaces do not overlap. Add a code comment near this resolver so future cleanup is obvious.
- Preserve `--json` output exactly except for adding `agent_format` to `kata version --json`.
- JSON errors stay on stderr as JSON. Agent errors stay on stderr as `ERR ...`. Human errors keep `kata: ...`.
- Do not change daemon APIs. Any data needed for agent output must come from existing response bodies or already-resolved CLI context.
- For absent nullable free-form values such as owner, omit the field. Do not print sentinels like `owner=unowned`.

### Task 1: Output Mode Resolution And Agent Error Contract

**Files:**
- Create: `cmd/kata/output_mode.go`
- Create: `cmd/kata/output_mode_test.go`
- Modify: `cmd/kata/main.go`
- Modify: `cmd/kata/main_test.go`

- [ ] **Step 1: Write failing tests for output mode resolution**

Add table tests in `cmd/kata/output_mode_test.go`:

```go
func TestResolveOutputMode(t *testing.T) {
	cases := []struct {
		name    string
		format  string
		json    bool
		agent   bool
		want    outputMode
		wantErr string
	}{
		{name: "default human", want: outputHuman},
		{name: "format json", format: "json", want: outputJSON},
		{name: "json alias", json: true, want: outputJSON},
		{name: "agent alias", agent: true, want: outputAgent},
		{name: "matching agent flags", format: "agent", agent: true, want: outputAgent},
		{name: "conflicting modes", format: "human", json: true, wantErr: "conflicting output modes"},
		{name: "bad format", format: "xml", wantErr: "unsupported output format"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveOutputModeValues(tc.format, tc.json, tc.agent)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}
```

- [ ] **Step 2: Run the focused tests and verify they fail**

Run: `go test ./cmd/kata -run 'TestResolveOutputMode|TestRoot_HelpListsUniversalFlags'`

Expected: fail because `outputMode`, `resolveOutputModeValues`, `--format`, and `--agent` do not exist.

- [ ] **Step 3: Implement output mode types and flags**

In `cmd/kata/output_mode.go`, add:

```go
type outputMode string

const (
	outputHuman outputMode = "human"
	outputJSON  outputMode = "json"
	outputAgent outputMode = "agent"
)

const agentFormatVersion = 1

func resolveOutputModeValues(format string, jsonFlag, agentFlag bool) (outputMode, error) {
	var selected []outputMode
	if format != "" {
		switch outputMode(format) {
		case outputHuman, outputJSON, outputAgent:
			selected = append(selected, outputMode(format))
		default:
			return "", &cliError{
				Message:  "unsupported output format " + strconv.Quote(format) + " (want human, json, or agent)",
				Kind:     kindUsage,
				ExitCode: ExitUsage,
			}
		}
	}
	if jsonFlag {
		selected = append(selected, outputJSON)
	}
	if agentFlag {
		selected = append(selected, outputAgent)
	}
	if len(selected) == 0 {
		return outputHuman, nil
	}
	first := selected[0]
	for _, mode := range selected[1:] {
		if mode != first {
			return "", &cliError{Message: "conflicting output modes", Kind: kindUsage, ExitCode: ExitUsage}
		}
	}
	return first, nil
}
```

Extend `globalFlags` in `main.go` with `Format string`, `Agent bool`, and resolved `Mode outputMode`. Register:

```go
cmd.PersistentFlags().StringVar(&flags.Format, "format", "", "output format: human|json|agent")
cmd.PersistentFlags().BoolVar(&flags.Agent, "agent", false, "emit concise agent-readable text")
```

Update `PersistentPreRunE` to resolve and store `flags.Mode`. During Task 7, extend this resolver so `kata import --format kata|beads` is accepted as a temporary legacy source-format value.

- [ ] **Step 4: Update error routing**

Change `emitError` signature to accept `mode outputMode` or add a small wrapper:

```go
func emitErrorForMode(w io.Writer, err error, mode outputMode, runEReached bool) {
	switch mode {
	case outputJSON:
		emitJSONError(w, err, runEReached)
	case outputAgent:
		emitAgentError(w, err, commandNameForError(runEReached))
	default:
		emitHumanError(w, err, runEReached)
	}
}
```

For parse errors before a subcommand dispatches, use `kata` as the second token. For command errors, use the command's `CommandPath()` final component where available.

- [ ] **Step 5: Add tests for agent errors**

In `cmd/kata/main_test.go`, add tests that call the error emitter directly:

```go
func TestEmitError_AgentMode_CommandError(t *testing.T) {
	cli := &cliError{Message: "comment body is required", Kind: kindValidation, ExitCode: ExitValidation}
	var buf bytes.Buffer
	emitAgentError(&buf, "comment", cli)
	assert.Contains(t, buf.String(), "ERR comment validation: comment body is required\n")
}
```

Also add an integration-style parse error test:

Run command: `executeRootCapture(t, context.Background(), "--agent", "cretae")`

Expected stderr starts with `ERR kata usage:`.

- [ ] **Step 6: Run focused tests**

Run: `go test ./cmd/kata -run 'TestResolveOutputMode|TestEmitError|TestRoot_HelpListsUniversalFlags'`

Expected: pass.

- [ ] **Step 7: Commit**

```bash
git add cmd/kata/main.go cmd/kata/main_test.go cmd/kata/output_mode.go cmd/kata/output_mode_test.go
git commit -m "Add CLI output mode resolution"
```

### Task 2: Agent Quoting, Field, And Fence Helpers

**Files:**
- Modify: `cmd/kata/output_mode.go`
- Modify: `cmd/kata/output_mode_test.go`

- [ ] **Step 1: Write failing tests for agent value encoding**

Add tests:

```go
func TestAgentValue_Quoting(t *testing.T) {
	assert.Equal(t, "abc4", agentValue("abc4"))
	assert.Equal(t, strconv.Quote("Fix login race"), agentValue("Fix login race"))
	assert.Equal(t, strconv.Quote(`quoted "title"`), agentValue(`quoted "title"`))
	assert.Equal(t, strconv.Quote("bad\nline"), agentValue("bad\nline"))
}

func TestAgentFencedText_ExtendsFenceForBackticks(t *testing.T) {
	got := agentFencedText("``` inside")
	assert.Contains(t, got, "````text\n")
	assert.True(t, strings.HasSuffix(got, "\n````\n"))
}
```

- [ ] **Step 2: Run tests and verify they fail**

Run: `go test ./cmd/kata -run 'TestAgentValue|TestAgentFencedText'`

Expected: fail because helpers do not exist.

- [ ] **Step 3: Implement helpers**

In `output_mode.go`, add:

```go
func agentValue(s string) string {
	clean := textsafe.Line(s)
	if clean == "" {
		return `""`
	}
	if strings.IndexFunc(clean, func(r rune) bool {
		return unicode.IsSpace(r) || r == '"' || r == '\\' || unicode.IsControl(r)
	}) >= 0 {
		return strconv.Quote(clean)
	}
	return clean
}

func writeAgentField(w io.Writer, name, value string) error {
	_, err := fmt.Fprintf(w, "%s: %s\n", name, value)
	return err
}

func agentFencedText(s string) string {
	fence := "```"
	for strings.Contains(s, fence) {
		fence += "`"
	}
	return fence + "text\n" + textsafe.Block(s) + "\n" + fence + "\n"
}
```

- [ ] **Step 4: Run focused tests**

Run: `go test ./cmd/kata -run 'TestAgentValue|TestAgentFencedText'`

Expected: pass.

- [ ] **Step 5: Commit**

```bash
git add cmd/kata/output_mode.go cmd/kata/output_mode_test.go
git commit -m "Add agent output formatting helpers"
```

### Task 3: Version, Whoami, And Simple Local Commands

**Files:**
- Modify: `cmd/kata/version.go`
- Modify: `cmd/kata/version_test.go`
- Modify: `cmd/kata/whoami.go`
- Modify: `cmd/kata/health.go`
- Modify: `cmd/kata/daemon_cmd.go`
- Modify: `cmd/kata/daemon_cmd_test.go`

- [ ] **Step 1: Write failing version tests**

In `version_test.go`, add:

```go
func TestVersion_AgentIncludesFormatVersion(t *testing.T) {
	resetFlags(t)
	out := string(executeRoot(t, newRootCmd(), "--agent", "version"))
	assert.Contains(t, out, "OK version ")
	assert.Contains(t, out, "agent_format=1")
}

func TestVersion_JSONIncludesAgentFormat(t *testing.T) {
	resetFlags(t)
	out := executeRoot(t, newRootCmd(), "--json", "version")
	var got map[string]any
	require.NoError(t, json.Unmarshal(out, &got))
	assert.Equal(t, float64(agentFormatVersion), got["agent_format"])
}
```

- [ ] **Step 2: Run tests and verify failure**

Run: `go test ./cmd/kata -run 'TestVersion_'`

Expected: fail because version does not support agent mode or JSON `agent_format`.

- [ ] **Step 3: Implement version and whoami agent output**

In `version.go`, branch:

```go
case flags.Mode == outputAgent:
	_, err := fmt.Fprintf(out, "OK version version=%s agent_format=%d\n",
		agentValue(version.Version), agentFormatVersion)
	return err
```

Add `agent_format` to JSON payload.

In `whoami.go`, add:

```go
if flags.Mode == outputAgent {
	_, err := fmt.Fprintf(cmd.OutOrStdout(), "OK whoami actor=%s source=%s\n",
		agentValue(actor), agentValue(source))
	return err
}
```

- [ ] **Step 4: Add and implement health/daemon status agent tests**

Use existing daemon status test patterns. Assert `--agent` emits `OK daemon status=...` and `health --agent` emits `OK health`.

- [ ] **Step 5: Run focused tests**

Run: `go test ./cmd/kata -run 'TestVersion_|TestWhoami|TestHealth|TestDaemon'`

Expected: pass.

- [ ] **Step 6: Commit**

```bash
git add cmd/kata/version.go cmd/kata/version_test.go cmd/kata/whoami.go cmd/kata/health.go cmd/kata/daemon_cmd.go cmd/kata/daemon_cmd_test.go
git commit -m "Add agent output for local status commands"
```

### Task 4: Shared Agent Mutation Renderer

**Files:**
- Modify: `cmd/kata/output_mode.go`
- Modify: `cmd/kata/create.go`
- Modify: `cmd/kata/comment.go`
- Modify: `cmd/kata/close.go`
- Modify: `cmd/kata/reopen.go`
- Modify: `cmd/kata/assign.go`
- Modify: `cmd/kata/label.go`
- Modify: `cmd/kata/delete.go`
- Modify: `cmd/kata/restore.go`
- Test: `cmd/kata/create_test.go`, `cmd/kata/comment_test.go`, `cmd/kata/close_reopen_test.go`, `cmd/kata/assign_test.go`, `cmd/kata/label_test.go`, `cmd/kata/delete_test.go`, `cmd/kata/restore_test.go`, `cmd/kata/purge_test.go`

- [ ] **Step 1: Write failing tests for representative mutations**

Add tests for:

- `kata create --agent`: starts `OK create <short_id>`, includes `Issue:` and `Status: open`.
- `kata create --agent --idempotency-key K` twice: second includes `reused=true changed=false`.
- `kata comment --agent`: `OK comment <short_id>` and `Comment: appended`.
- `kata close --agent`: `OK close <short_id>`, `Status: closed`, `Reason: done`.
- `kata reopen --agent`: `OK reopen <short_id>`, `Status: open`.
- `kata assign --agent`: `Owner: wesm`.
- `kata unassign --agent`: `Owner-Cleared: true`.
- `kata label add --agent`: `OK label <short_id> changed=true`.
- `kata delete --agent`: `Undo: kata restore <short_id> --agent`.
- `kata restore --agent`: `Deleted: false`.

- [ ] **Step 2: Run tests and verify failure**

Run: `go test ./cmd/kata -run 'Test(Create|Comment|Close|Reopen|Assign|Unassign|Label|Delete|Restore).*Agent'`

Expected: fail because commands still render human/JSON only.

- [ ] **Step 3: Implement generic mutation agent output**

In `output_mode.go`, define a small decoded shape:

```go
type agentIssueMutation struct {
	Issue struct {
		ShortID      string  `json:"short_id"`
		QualifiedID  string  `json:"qualified_id"`
		Title        string  `json:"title"`
		Status       string  `json:"status"`
		ClosedReason *string `json:"closed_reason"`
		Owner        *string `json:"owner"`
		DeletedAt    *string `json:"deleted_at"`
	} `json:"issue"`
	Changed bool `json:"changed"`
	Reused  bool `json:"reused,omitempty"`
}
```

Add helpers:

```go
func printAgentMutation(cmd *cobra.Command, verb string, bs []byte, extra func(io.Writer, agentIssueMutation) error) error
```

The first line shape is:

```go
OK <verb> <short_id> [reused=true] [changed=false]
```

Print `Issue: <short_id> "<title>"` and `Status: <status>` when available.

- [ ] **Step 4: Wire lifecycle commands**

Update existing printers:

- `printMutationWithApplied`: when `flags.Mode == outputAgent`, call `printAgentMutation` with verb from `cmd.CommandPath()` final component.
- `comment.go`: after successful POST, agent mode should decode response and call a comment-specific helper that prints `Comment: appended`.
- `printAssignMutation`: agent mode emits `Owner:` for assign and `Owner-Cleared: true` for unassign.
- `printLabelMutation` / `printLabelRemoved`: agent mode emits `Action: added|removed`.
- `printDestructive`: agent mode emits delete/purge shapes.
- `restore.go`: remove the current early human-only print path for agent mode and route through mutation renderer with `Deleted: false`.

- [ ] **Step 5: Preserve quiet and JSON behavior**

Run existing non-agent tests after each command group. Ensure `--quiet` still prints exactly what it did before, and `--json` remains parseable.

- [ ] **Step 6: Run focused tests**

Run: `go test ./cmd/kata -run 'Test(Create|Comment|Close|Reopen|Assign|Unassign|Label|Delete|Restore|Purge)'`

Expected: pass.

- [ ] **Step 7: Commit**

```bash
git add cmd/kata/output_mode.go cmd/kata/create.go cmd/kata/comment.go cmd/kata/close.go cmd/kata/reopen.go cmd/kata/assign.go cmd/kata/label.go cmd/kata/delete.go cmd/kata/restore.go cmd/kata/*_test.go
git commit -m "Add agent output for issue mutations"
```

### Task 5: Agent Output For List, Search, Ready, Labels, And Show

**Files:**
- Modify: `cmd/kata/list.go`
- Modify: `cmd/kata/search.go`
- Modify: `cmd/kata/ready.go`
- Modify: `cmd/kata/label.go`
- Modify: `cmd/kata/show.go`
- Test: `cmd/kata/list_test.go`, `cmd/kata/search_test.go`, `cmd/kata/ready_test.go`, `cmd/kata/label_test.go`, `cmd/kata/show_test.go`

- [ ] **Step 1: Write failing tests for read outputs**

Add tests:

- `list --agent` emits `OK list count=N`; rows omit owner when absent.
- `list --agent` with title `quoted "title"` emits an escaped quoted value.
- empty `search --agent` emits only `OK search count=0 query="..."`.
- `ready --agent` emits rows without owner when absent.
- `labels --agent` emits `OK labels count=N`.
- `show --agent` emits `Labels: bug,safari`, fenced `Body:`, comment row followed by column-0 fenced text, and no `Owner:` when absent.

- [ ] **Step 2: Run focused tests and verify failure**

Run: `go test ./cmd/kata -run 'Test(List|Search|Ready|Labels|Show).*Agent'`

Expected: fail because agent read renderers do not exist.

- [ ] **Step 3: Implement list/search/ready row helpers**

Use agent row helpers so row code stays small:

```go
func writeAgentKVRow(w io.Writer, fields ...agentField) error
```

Omit fields whose value pointer is nil. For list-valued fields, join with `,` and no spaces.

- [ ] **Step 4: Implement show agent renderer**

Decode the existing show response. Emit:

````text
OK show abc4
Issue: abc4 "Title"
Status: open
Owner: wesm        # only when non-nil/non-empty
Labels: bug,safari # only when labels exist
Priority: 2        # only when set
Body:
```text
...
```
````

For each comment:

````text
- author=<value> created_at=<value>
```text
comment body
```
````

Fence starts at column 0.

For links, emit only fields available in the existing show response. Current
link rows include `type` and `issue`; do not fake `title=` because link peer
titles are not present in the response and this plan forbids daemon API changes
or extra per-link fetches for agent formatting.

- [ ] **Step 5: Run focused tests**

Run: `go test ./cmd/kata -run 'Test(List|Search|Ready|Labels|Show)'`

Expected: pass.

- [ ] **Step 6: Commit**

```bash
git add cmd/kata/list.go cmd/kata/search.go cmd/kata/ready.go cmd/kata/label.go cmd/kata/show.go cmd/kata/*_test.go
git commit -m "Add agent output for issue reads"
```

### Task 6: Agent Output For Events, Digest, And Audit

**Files:**
- Modify: `cmd/kata/events.go`
- Modify: `cmd/kata/digest.go`
- Modify: `cmd/kata/audit_closes.go`
- Test: `cmd/kata/events_test.go`, `cmd/kata/digest_test.go`, `cmd/kata/audit_closes_test.go`

- [ ] **Step 1: Write failing events tests**

Add tests:

- `events --agent` emits `OK events count=N next_after_id=M` and rows like `- id=42 type=issue.created issue=abc4 actor=wesm`.
- reset response in one-shot agent mode emits `OK events reset_required=true reset_after_id=N`.
- `events --tail --agent` emits one line per event: `OK event id=... type=... issue=... actor=...`.
- `events --tail --json` remains NDJSON.

- [ ] **Step 2: Run events tests and verify failure**

Run: `go test ./cmd/kata -run 'TestEvents.*Agent|TestEvents.*JSON'`

Expected: agent tests fail, JSON tests pass.

- [ ] **Step 3: Implement events agent renderers**

In `runEventsPoll`, branch on `flags.Mode == outputAgent` before human output. In `streamOnce` or the frame write boundary, branch tail output mode:

```go
if flags.Mode == outputAgent {
	fmt.Fprintf(out, "OK event id=%d type=%s", id, agentValue(eventType))
	...
}
```

Keep one line per event in tail mode. Do not emit multiline blocks.

- [ ] **Step 4: Add digest and audit agent tests**

For `digest --agent` and `audit closes --agent`, assert only the stable shape:

- Header starts with `OK digest` / `OK audit`.
- Rows use `key=value`.
- No ANSI escape bytes.

- [ ] **Step 5: Implement digest/audit agent renderers**

Follow existing human decoders, but use compact key/value rows. Do not include fields that require extra daemon calls.

- [ ] **Step 6: Run focused tests**

Run: `go test ./cmd/kata -run 'TestEvents|TestDigest|TestAuditCloses'`

Expected: pass.

- [ ] **Step 7: Commit**

```bash
git add cmd/kata/events.go cmd/kata/digest.go cmd/kata/audit_closes.go cmd/kata/events_test.go cmd/kata/digest_test.go cmd/kata/audit_closes_test.go
git commit -m "Add agent output for event and audit reads"
```

### Task 7: Import, Export, Projects, And Daemon Logs

**Files:**
- Modify: `cmd/kata/import.go`
- Modify: `cmd/kata/beads_import.go`
- Modify: `cmd/kata/export.go`
- Modify: `cmd/kata/projects.go`
- Modify: `cmd/kata/daemon_logs.go`
- Test: `cmd/kata/import_test.go`, `cmd/kata/beads_import_test.go`, `cmd/kata/export_test.go`, `cmd/kata/projects_test.go`, `cmd/kata/daemon_logs_test.go`

- [ ] **Step 1: Write failing import format migration tests**

Add tests:

- `kata import --source-format beads` selects beads import path.
- During deprecation, `kata import --format beads --agent` is accepted as legacy source format.
- `kata import --format beads --source-format beads` fails with a usage error.
- `kata import --format agent --source-format kata` selects agent output and kata JSONL source.

- [ ] **Step 2: Run import tests and verify failure**

Run: `go test ./cmd/kata -run 'TestImport.*Format|TestBeadsImport'`

Expected: fail because `--source-format` and the routing rules do not exist.

- [ ] **Step 3: Implement import source-format migration**

In `import.go`:

- Add local `sourceFormat string`.
- Remove the import command's local `--format` registration. Root owns `--format`.
- Update the root output-mode resolver to accept `flags.Format == "kata"` or `"beads"` only when the final command name is `import`; in that case, set `flags.Mode = outputHuman` unless `--json` or `--agent` also selected output mode through their aliases.
- In `import.go`, consume `flags.Format` as the legacy source-format value only when it is `kata` or `beads` and `--source-format` is absent.
- Parse by value with a comment:

```go
// During the deprecation window, kata|beads in the root --format slot
// means source format. human|json|agent belongs to the root output mode
// namespace. The value sets are disjoint, so routing is unambiguous.
```

If both `--source-format` and legacy `--format kata|beads` are set, return a usage `cliError`.

- [ ] **Step 4: Implement import/export agent output**

Kata JSONL import:

```text
OK import source_format=kata target=<db_path>
```

Beads import:

```text
OK import source_format=beads project=<id_or_name> created=<n> updated=<n> unchanged=<n> comments=<n> links=<n>
```

Export:

```text
OK export output=<path>
```

- [ ] **Step 5: Implement projects and daemon logs agent output**

Projects reads use `OK projects count=N`; project mutations use `OK project action=<verb> ...`. `daemon logs --agent` emits one line per log record and no multiline blocks.

- [ ] **Step 6: Run focused tests**

Run: `go test ./cmd/kata -run 'TestImport|TestBeadsImport|TestExport|TestProjects|TestDaemonLogs'`

Expected: pass.

- [ ] **Step 7: Commit**

```bash
git add cmd/kata/import.go cmd/kata/beads_import.go cmd/kata/export.go cmd/kata/projects.go cmd/kata/daemon_logs.go cmd/kata/*_test.go
git commit -m "Add agent output for maintenance commands"
```

### Task 8: TUI Rejection, Quickstart, README, And Help Text

**Files:**
- Modify: `cmd/kata/tui_cmd.go`
- Modify: `cmd/kata/tui_cmd_test.go`
- Modify: `cmd/kata/quickstart.go`
- Modify: `cmd/kata/quickstart_test.go`
- Modify: `README.md`

- [ ] **Step 1: Write failing TUI and quickstart tests**

Add tests:

- `kata tui --agent` fails with usage and does not launch TUI.
- `kata quickstart` mentions `--agent`.
- `kata agent-instructions` mentions `--agent`.
- Quickstart still mentions `--json` for complete structured parsing.

- [ ] **Step 2: Run focused tests and verify failure**

Run: `go test ./cmd/kata -run 'TestTUI.*Agent|TestQuickstart'`

Expected: fail until TUI rejection and text are updated.

- [ ] **Step 3: Implement TUI rejection**

In `newTUICmd` RunE, before launching:

```go
if flags.Mode == outputAgent {
	return &cliError{Message: "kata tui does not support --agent; run without output formatting", Kind: kindUsage, ExitCode: ExitUsage}
}
```

- [ ] **Step 4: Update quickstart and README**

Replace agent examples that currently say "prefer `--json` for reads and writes" with:

```text
Use --agent for concise action summaries in agent logs.
Use --json when your script needs complete structured data.
```

Keep any JSON-specific polling examples as JSON where parsing full structure is required.

- [ ] **Step 5: Run focused tests**

Run: `go test ./cmd/kata -run 'TestTUI|TestQuickstart|TestRoot_HelpListsUniversalFlags'`

Expected: pass.

- [ ] **Step 6: Commit**

```bash
git add cmd/kata/tui_cmd.go cmd/kata/tui_cmd_test.go cmd/kata/quickstart.go cmd/kata/quickstart_test.go README.md
git commit -m "Document agent output mode"
```

### Task 9: Contract Sweep And Full Test Run

**Files:**
- Modify as needed based on failures.

- [ ] **Step 1: Search for missed `flags.JSON` branches**

Run:

```bash
rg -n "flags\\.JSON|flags\\.Quiet|emitJSON|print[A-Za-z].*\\(cmd.*bs|fmt\\.Fprint" cmd/kata
```

Expected: every output-producing branch either dispatches through output mode or is intentionally human-only with a test-backed reason.

- [ ] **Step 2: Add missing focused tests**

For any missed command, add the smallest agent-mode test that pins its first line and any critical fields.

- [ ] **Step 3: Run command package tests**

Run: `go test ./cmd/kata`

Expected: pass.

- [ ] **Step 4: Run full repo tests**

Run: `go test ./...`

Expected: pass.

- [ ] **Step 5: Run final formatting/checks**

Run:

```bash
gofmt -w cmd/kata
git diff --check
```

Expected: no diff-check errors.

- [ ] **Step 6: Commit final fixes if any**

```bash
git add .
git commit -m "Complete agent output mode coverage"
```

Skip this commit only if there were no changes after the previous task commits.

### Task 10: Close Kata Issue

**Files:**
- No source changes.

- [ ] **Step 1: Confirm final commit and clean worktree**

Run:

```bash
git status --short
git rev-parse --short HEAD
```

Expected: clean status and a commit SHA to reference.

- [ ] **Step 2: Close the kata issue with evidence**

Use the issue ref corresponding to GitHub issue #46 in local kata, if present. If the local kata issue does not exist, comment in the final response instead of closing an unrelated local issue.

If present:

```bash
kata close <ref> --done \
  --message "Implemented agent output format with --format agent/--agent, stable ERR/OK contract, docs, and tests." \
  --commit <sha> \
  --test "go test ./..."
```

Expected: issue closes only after tests pass.

- [ ] **Step 3: Final response**

Report:

- Final commit SHA.
- Test commands run.
- Any intentional compatibility notes, especially the `kata import --format kata|beads` deprecation window.
