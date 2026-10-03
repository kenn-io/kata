package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/kit/atomicfile"
)

const (
	hooklessBeginComment = "<!-- BEGIN KATA HOOKLESS MUSE -->"
	hooklessBeginYAML    = "# BEGIN KATA HOOKLESS MUSE"
	hooklessEndMarker    = "<!-- END KATA HOOKLESS MUSE -->"
	hooklessAddedNewline = "<!-- KATA HOOKLESS MUSE ADDED PREFIX NEWLINE -->"
)

type instructionArtifact struct {
	Path        string
	Content     string
	Changed     bool
	State       string
	beginMarker string
	// fence is the line install writes before beginMarker, if any.
	fence string
	// retainEmpty keeps the file when uninstall leaves it empty.
	retainEmpty bool
	before      string
	exists      bool
	mode        os.FileMode
}

func newAgentInstructionsCmd() *cobra.Command {
	group := &cobra.Command{
		Use: "instructions", Short: "Manage instruction bundles for hookless harnesses",
		Long: "Manage consumer Muse's standing instructions, skill and scheduled poll specification.\nThese rely on instruction-following, not enforcement; there is no native attention support.\nNo authentication option or scheduled task is installed.",
	}
	for _, verb := range []string{"install", "uninstall", "status"} {
		group.AddCommand(newAgentInstructionsActionCmd(verb))
	}
	return group
}

func newAgentInstructionsActionCmd(verb string) *cobra.Command {
	var home, actor string
	var dryRun bool
	cmd := &cobra.Command{
		Use: verb + " muse", Short: verb + " consumer Muse's hookless instruction bundle",
		Args: cobra.ExactArgs(1),
		ValidArgsFunction: func(_ *cobra.Command, args []string, prefix string) ([]string, cobra.ShellCompDirective) {
			if len(args) == 0 && strings.HasPrefix("muse", prefix) {
				return []string{"muse"}, cobra.ShellCompDirectiveNoFileComp
			}
			return nil, cobra.ShellCompDirectiveNoFileComp
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if args[0] != "muse" {
				return agentHookUsage("hookless instructions support consumer muse only")
			}
			if strings.TrimSpace(home) == "" {
				return agentHookUsage("--home is required")
			}
			if verb == "install" {
				var err error
				actor, err = normalizeNotificationRecipient(actor)
				if err != nil {
					return agentHookUsage("--actor requires an exact actor or actor/teammate inbox address")
				}
			}
			root, err := filepath.Abs(home)
			if err != nil {
				return err
			}
			artifacts, err := planInstructionArtifacts(root, actor, verb)
			if err != nil {
				return err
			}
			if verb != "status" && !dryRun {
				if err := applyInstructionArtifacts(artifacts); err != nil {
					return err
				}
			}
			return printInstructionArtifacts(cmd, verb, dryRun, artifacts)
		},
	}
	cmd.Flags().StringVar(&home, "home", "", "required harness home containing AGENTS.md and workspace/skills")
	if verb == "install" {
		cmd.Flags().StringVar(&actor, "actor", "", "required exact actor or actor/teammate inbox address")
	}
	if verb != "status" {
		cmd.Flags().BoolVar(&dryRun, "dry", false, "print proposed contents without writing files or creating directories")
	}
	return cmd
}

// All artifacts consume the same contract as native prompt injection. Only
// hookless setup and delivery instructions differ between them.
func museInstructionArtifacts(home, actor string) []instructionArtifact {
	start := "\n" + hooklessBeginComment + "\n"
	skillStart := "---\n" + hooklessBeginYAML + "\n"
	end := hooklessEndMarker + "\n"
	briefing := fmt.Sprintf(`At every conversation start and before claiming completion, read this Kata contract.
Run from the bound project workspace with the explicitly selected daemon and actor.
At conversation start, run:

    kata inbox --for %s --json
    kata ready --agent
    kata list --meta work.attention=needs-human --limit 0 --agent
    kata list --meta work.attention=stuck --limit 0 --agent

Handle only the exact inbox recipient. Record proof (commit and test evidence)
before reporting done. Keep work.attention and work.attention_msg current yourself.
Never print tokens or put them in instructions, task prompts, or saved poll state.
These prompts rely on instruction-following, not enforcement. They provide no
native attention support, automatic idle wakeup, or guaranteed session-end hook.

`, "'"+strings.ReplaceAll(actor, "'", "'\"'\"'")+"'")
	return []instructionArtifact{
		{Path: filepath.Join(home, "AGENTS.md"), beginMarker: hooklessBeginComment, retainEmpty: true,
			Content: start + briefing + agentContractText + end},
		{Path: filepath.Join(home, "workspace", "skills", "kata", "SKILL.md"), beginMarker: hooklessBeginYAML, fence: "---",
			Content: skillStart + "name: kata\ndescription: Use at conversation start and before claiming completion for Kata issue work.\n---\n\n" + briefing + agentContractText + end},
		{Path: filepath.Join(home, "workspace", "skills", "kata", "POLL.md"), beginMarker: hooklessBeginComment,
			Content: start + musePollSpec(actor) + briefing + agentContractText + end},
	}
}

func musePollSpec(actor string) string {
	return fmt.Sprintf(`# Recurring task specification for consumer Muse

Ask Muse to create this task through its own scheduled-task UI or agent interface.
Proposed interval: every five minutes. The human confirms interval, bound workspace,
selected daemon, exact recipient %q, and notification destination before enabling it.
Kata does not install cron or create the task. Cancel it in Muse before uninstalling.

Choose authentication with the human before running any poll. Neither option is selected:
- Provision a token file with owner-only mode 0600 in a 0700 directory. The CLI can
  read it through [auth].token_file or KATA_AUTH_TOKEN_FILE; a private launcher may
  instead read it into KATA_AUTH_TOKEN. Never echo it, enable shell tracing, pass
  the token on argv, or store it in poll state. This exposes the secret to the local
  process and filesystem.
- Configure a custom connector to an HTTPS endpoint hosting kata mcp serve --http.
  Supply the separate inbound MCP bearer through the connector's secret settings;
  the bridge keeps its daemon credential server-side. Follow Kata's MCP HTTP transport
  rules. Map the CLI reads below to equivalent connector tools and validate cursor
  and notification behavior before enabling a connector task; this spec is not a connector.

Persist state under a private task-owned directory, separately for each daemon identity,
project identity and exact recipient. Serialize runs (no overlapping polls). State includes
the event cursor and the last successfully delivered request identities: issue ref,
recipient, and the complete notify value (from, teammate and message).
Start with cursor 0 and an empty delivered set. Reading the inbox does not clear it.

On each run, in the confirmed bound workspace:
1. Read kata inbox --for '%s' --json. Keep the complete request identities as a
   candidate snapshot, but do not notify the human yet. Reading the inbox does not
   clear requests.
2. Read kata events --after <cursor> --limit 100 --json. Drain pages while
   next_after_id advances, even when fewer than 100 visible events are returned;
   scoped polling can advance across filtered events. Use each advanced cursor for
   the next read. Stop only when the cursor no longer advances. Events invalidate
   cached views; they are not themselves requests to notify the human.
3. If reset_required is true, discard cached projections. Refetch the inbox, ready
   and both attention views listed below. After successful refresh, resume from
   reset_after_id and drain again. Retain delivered identities for deduplication.
4. Only after the inbox and event reads, including any required reset refresh, all
   succeed, compare the latest inbox snapshot with the delivered set. Notify the
   human only on new or changed requests, never unchanged items or ordinary events.
   Drop identities absent from a successfully read inbox so a cleared request may
   later appear again. Do not clear requests merely on read; handle them, clear with
   kata notify <ref> --to <recipient> --clear, then read back.
5. After all reads and notifications succeed, atomically persist next_after_id
   (or the successfully refreshed reset_after_id) and delivered identities.
   On read, refresh, or delivery failure retain the previous state and retry next run.
   Delivery and state persistence cannot be atomic across Muse and a local file:
   prefer a task delivery idempotency key when available; otherwise a crash may
   redeliver a request. Do not claim exactly-once delivery.

The task follows the same contract as the standing instructions and skill:

`, actor, strings.ReplaceAll(actor, "'", "'\"'\"'"))
}

// resolveInstructionHome resolves OS aliases such as macOS /var that live above
// the managed home, using the nearest existing parent when the home is new.
// The home itself is not resolved, so a symlinked home is still refused.
func resolveInstructionHome(home string) (string, error) {
	parent, suffix := filepath.Dir(home), filepath.Base(home)
	for {
		resolved, err := filepath.EvalSymlinks(parent)
		if err == nil {
			return filepath.Join(resolved, suffix), nil
		}
		if !errors.Is(err, os.ErrNotExist) || filepath.Dir(parent) == parent {
			return "", err
		}
		suffix = filepath.Join(filepath.Base(parent), suffix)
		parent = filepath.Dir(parent)
	}
}

func planInstructionArtifacts(home, actor, verb string) ([]instructionArtifact, error) {
	home, err := resolveInstructionHome(home)
	if err != nil {
		return nil, err
	}
	artifacts := museInstructionArtifacts(home, actor)
	for i := range artifacts {
		a := &artifacts[i]
		if err := checkInstructionPath(a.Path, home); err != nil {
			return nil, err
		}
		var err error
		a.before, a.exists, err = readIfExists(a.Path)
		if err != nil {
			return nil, err
		}
		a.mode = 0o600
		if a.exists {
			info, err := os.Stat(a.Path)
			if err != nil {
				return nil, err
			}
			a.mode = info.Mode().Perm()
		}
		begin, end, err := instructionBlockSpan(a.before, a.beginMarker, a.fence)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", a.Path, err)
		}
		a.State = "absent"
		if begin >= 0 {
			a.State = "installed"
		}
		switch verb {
		case "status":
			a.Content = "" // Status reports ownership on disk, never runtime loading.
		case "uninstall":
			a.Content = a.before
			if begin >= 0 {
				a.Content = a.before[:begin] + a.before[end:]
				a.State = "removed"
			}
		case "install":
			prefix := a.before
			if begin >= 0 {
				prefix = prefix[:begin]
			}
			if i == 0 && prefix != "" && !strings.HasSuffix(prefix, "\n") {
				// Remember that the separator terminates an existing line, so
				// uninstall can restore it without guessing who owns the newline.
				a.Content = strings.Replace(a.Content, a.beginMarker+"\n", a.beginMarker+"\n"+hooklessAddedNewline+"\n", 1)
			}
			if begin >= 0 {
				a.Content = a.before[:begin] + a.Content + a.before[end:]
			} else if i == 0 {
				a.Content = a.before + a.Content
			} else if a.exists && a.before != "" {
				return nil, fmt.Errorf("refusing to replace unowned %s", a.Path)
			}
			a.State = "installed"
		}
		a.Changed = verb != "status" && a.Content != a.before
	}
	return artifacts, nil
}

// instructionBlockSpan locates the managed block by complete marker lines. It
// accepts LF and CRLF line endings and a final line without a newline, so
// marker phrases inside other text, such as an actor name, are not markers.
// It returns -1, -1 when the content has no markers.
func instructionBlockSpan(content, beginMarker, fence string) (int, int, error) {
	begins := markerLines(content, beginMarker)
	ends := markerLines(content, hooklessEndMarker)
	if len(begins) == 0 && len(ends) == 0 {
		return -1, -1, nil
	}
	if len(begins) != 1 || len(ends) != 1 {
		return 0, 0, errors.New("malformed or duplicate hookless markers")
	}
	if ends[0].start < begins[0].end {
		return 0, 0, errors.New("misplaced hookless end marker")
	}
	addedNewline := len(markerLines(content[begins[0].end:ends[0].start], hooklessAddedNewline)) > 0
	begin, err := managedBlockStart(content, begins[0].start, fence, addedNewline)
	if err != nil {
		return 0, 0, err
	}
	return begin, ends[0].end, nil
}

type lineSpan struct{ start, end int }

// markerLines returns the spans of lines equal to marker, each including its
// line ending.
func markerLines(content, marker string) []lineSpan {
	var spans []lineSpan
	start := 0
	for line := range strings.Lines(content) {
		end := start + len(line)
		if strings.TrimRight(line, "\r\n") == marker {
			spans = append(spans, lineSpan{start, end})
		}
		start = end
	}
	return spans
}

// managedBlockStart extends the block before its begin marker to include what
// install wrote there: the fence line, a blank separator, or a recorded newline.
func managedBlockStart(content string, marker int, fence string, addedNewline bool) (int, error) {
	preceding := strings.TrimSuffix(strings.TrimSuffix(content[:marker], "\n"), "\r")
	if fence == "" {
		if preceding != "" && !strings.HasSuffix(preceding, "\n") && !addedNewline {
			return marker, nil
		}
		return len(preceding), nil
	}
	lineStart := strings.LastIndexByte(preceding, '\n') + 1
	if preceding[lineStart:] != fence {
		return 0, fmt.Errorf("missing %q line before hookless begin marker", fence)
	}
	return lineStart, nil
}

func checkInstructionPath(path, home string) error {
	for p := path; ; p = filepath.Dir(p) {
		info, err := os.Lstat(p)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err == nil && (info.Mode()&os.ModeSymlink != 0 || (p == path && !info.Mode().IsRegular()) || (p != path && !info.IsDir())) {
			return fmt.Errorf("instruction path must use regular files and real directories: %s", p)
		}
		if p == home || filepath.Dir(p) == p {
			return nil
		}
	}
}

func applyInstructionArtifacts(artifacts []instructionArtifact) error {
	return applyInstructionArtifactsWithWriter(artifacts, writeInstructionArtifact)
}

func applyInstructionArtifactsWithWriter(artifacts []instructionArtifact, write func(string, string, bool, os.FileMode) error) error {
	// Check every preimage before mutating any file. Each file write is atomic;
	// restore completed writes if a later artifact fails.
	home := filepath.Dir(artifacts[0].Path)
	for _, a := range artifacts {
		if err := checkInstructionPath(a.Path, home); err != nil {
			return err
		}
		content, exists, err := readIfExists(a.Path)
		if err != nil {
			return err
		}
		if exists != a.exists || content != a.before {
			return fmt.Errorf("instruction file changed during planning: %s", a.Path)
		}
	}
	for i, a := range artifacts {
		if !a.Changed {
			continue
		}
		if err := write(a.Path, a.Content, a.retainEmpty, a.mode); err != nil {
			rollbackEnd := i
			if errors.Is(err, atomicfile.ErrPublished) {
				rollbackEnd++
			}
			for _, previous := range artifacts[:rollbackEnd] {
				if previous.Changed {
					err = errors.Join(err, write(previous.Path, previous.before, previous.exists, previous.mode))
				}
			}
			return err
		}
	}
	return nil
}

// keepEmpty writes an empty file instead of removing it. Rollback uses it to
// restore an existing empty preimage; uninstall uses it to retain AGENTS.md.
func writeInstructionArtifact(path, content string, keepEmpty bool, mode os.FileMode) error {
	if content == "" && !keepEmpty {
		err := os.Remove(path)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return atomicfile.WriteFile(path, []byte(content), atomicfile.WithPerm(mode))
}

func printInstructionArtifacts(cmd *cobra.Command, verb string, dryRun bool, artifacts []instructionArtifact) error {
	if currentOutputMode() == outputJSON {
		type response struct {
			Harness         string `json:"harness"`
			Action          string `json:"action"`
			DryRun          bool   `json:"dry_run"`
			NativeAttention bool   `json:"native_attention"`
			Artifacts       any    `json:"artifacts"`
		}
		result := response{Harness: "muse", Action: verb, DryRun: dryRun}
		if dryRun {
			type previewArtifact struct {
				Path    string `json:"path"`
				Content string `json:"content"`
				Changed bool   `json:"changed"`
				State   string `json:"state"`
			}
			outputs := make([]previewArtifact, len(artifacts))
			for i, artifact := range artifacts {
				outputs[i] = previewArtifact{Path: artifact.Path, Content: artifact.Content, Changed: artifact.Changed, State: artifact.State}
			}
			result.Artifacts = outputs
		} else {
			type metadataArtifact struct {
				Path    string `json:"path"`
				Changed bool   `json:"changed"`
				State   string `json:"state"`
			}
			outputs := make([]metadataArtifact, len(artifacts))
			for i, artifact := range artifacts {
				outputs[i] = metadataArtifact{Path: artifact.Path, Changed: artifact.Changed, State: artifact.State}
			}
			result.Artifacts = outputs
		}
		return emitJSON(cmd.OutOrStdout(), result)
	}
	if flags.Quiet {
		return nil
	}
	if _, err := fmt.Fprintf(cmd.OutOrStdout(), "OK instructions %s harness=muse dry_run=%t native_attention=false\n", verb, dryRun); err != nil {
		return err
	}
	for _, a := range artifacts {
		if err := writeAgentKVRow(cmd.OutOrStdout(), agentRowField("path", a.Path), agentRowField("state", a.State), agentRowField("changed", fmt.Sprint(a.Changed))); err != nil {
			return err
		}
		if dryRun {
			if _, err := fmt.Fprint(cmd.OutOrStdout(), a.Content); err != nil {
				return err
			}
		}
	}
	return nil
}
