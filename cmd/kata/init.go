package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/textsafe"
)

// initOptions holds the flags specific to `kata init`.
type initOptions struct {
	Project    string
	Replace    bool
	Reassign   bool
	WithAgents bool
}

// callInitOpts is the parameter bag passed to callInit.
type callInitOpts struct {
	Project    string
	Replace    bool
	Reassign   bool
	WithAgents bool
}

// cliError is a structured error that carries an exit code for main().
//
// Kind is the coarse classification used by the --json error envelope so
// scripts can branch on a stable taxonomy instead of grepping the
// human-readable message. Code is the daemon-supplied per-error tag
// (e.g. "issue_not_found"); empty when the error originated client-side.
// Message is the human-readable text. ExitCode is what main() exits with.
type cliError struct {
	Message  string
	Kind     errKind
	Code     string
	ExitCode int
}

func (e *cliError) Error() string { return e.Message }

// errKind is the coarse classification surfaced in the --json error
// envelope. Maps roughly onto the spec §4.7 exit codes but is named
// for the kind of failure rather than the numeric exit, so JSON
// consumers can branch on a stable identifier.
type errKind string

const (
	kindUsage         errKind = "usage"
	kindValidation    errKind = "validation"
	kindNotFound      errKind = "not_found"
	kindConflict      errKind = "conflict"
	kindConfirm       errKind = "confirm"
	kindDaemonUnavail errKind = "daemon_unavailable"
	kindInternal      errKind = "internal"
)

// kindForExit maps an exit code to the conventional errKind. Used when
// a non-cliError reaches main and we still want to emit a JSON
// envelope under --json.
func kindForExit(exit int) errKind {
	switch exit {
	case ExitUsage:
		return kindUsage
	case ExitValidation:
		return kindValidation
	case ExitNotFound:
		return kindNotFound
	case ExitConflict:
		return kindConflict
	case ExitConfirm:
		return kindConfirm
	case ExitDaemonUnavail:
		return kindDaemonUnavail
	}
	return kindInternal
}

// kindForStatus maps an HTTP status to the conventional errKind. The
// daemon-supplied error code is reserved for future per-code overrides.
func kindForStatus(status int) errKind {
	switch status {
	case http.StatusBadRequest:
		return kindValidation
	case http.StatusNotFound:
		return kindNotFound
	case http.StatusConflict:
		return kindConflict
	case http.StatusPreconditionFailed:
		return kindConfirm
	}
	return kindInternal
}

// newInitCmd returns the cobra.Command for `kata init`.
func newInitCmd() *cobra.Command {
	var opts initOptions

	cmd := &cobra.Command{
		Use:   "init",
		Short: "bind workspace to a project",
		Long: `Initialize kata in this workspace.

Writes a committed .kata.toml that binds the workspace to a project
name. The daemon derives the name from a git remote when one is present;
pass --project to choose the project name explicitly.

Also adds .kata.local.toml to .gitignore so a developer's per-machine
overrides (e.g., a remote daemon URL via [server] url = "...") never
get committed.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			baseURL, err := ensureDaemon(cmd.Context())
			if err != nil {
				return fmt.Errorf("daemon: %w", err)
			}
			startPath, err := resolveStartPath(flags.Workspace)
			if err != nil {
				return fmt.Errorf("resolve workspace: %w", err)
			}
			out, err := callInit(cmd.Context(), baseURL, startPath, callInitOpts(opts))
			if err != nil {
				return err
			}
			_, err = fmt.Fprint(cmd.OutOrStdout(), out)
			return err
		},
	}

	cmd.Flags().BoolVar(&opts.Replace, "replace", false, "overwrite .kata.toml binding when it conflicts")
	cmd.Flags().BoolVar(&opts.Reassign, "reassign", false, "move an existing alias to this project")
	cmd.Flags().BoolVar(&opts.WithAgents, "with-agents", false, "write kata agent guidance into AGENTS.md in the workspace")

	return cmd
}

// callInit dispatches `kata init` between the path-free flow (client
// derives the project name locally, daemon registers project, client writes
// files) and the path-based flow (daemon does everything).
//
// Path-free runs whenever the client can resolve the name locally —
// from .kata.toml, --project, or a discoverable git workspace. That's
// the contract that lets a daemon on another host serve init without
// filesystem access to the client workspace. The client falls back to
// the path-based request only when local derivation can't produce an
// name, so the daemon (or its absence) emits the validation error.
func callInit(ctx context.Context, baseURL, startPath string, opts callInitOpts) (string, error) {
	if opts.Project == "" {
		opts.Project = flags.Project
	}
	derived, err := localDerive(ctx, startPath, opts)
	switch {
	case err == nil:
		return runNameInit(ctx, baseURL, derived, opts)
	case errors.Is(err, config.ErrNameConflict):
		return "", &cliError{
			Message:  err.Error(),
			Kind:     kindConflict,
			Code:     "project_binding_conflict",
			ExitCode: ExitConflict,
		}
	case errors.Is(err, config.ErrNoNameSource):
		return runStartPathInit(ctx, baseURL, startPath, opts)
	default:
		return "", &cliError{
			Message:  err.Error(),
			Kind:     kindValidation,
			ExitCode: ExitValidation,
		}
	}
}

// localInit captures everything callInit needs to run the path-free
// flow: the chosen name, the discovered roots (so .kata.toml lands
// at the workspace/git root rather than the cwd), the existing
// .kata.toml binding (so we can skip a redundant write), the absolute
// start path used as a final fallback for the write location, and
// optional alias metadata so the daemon can attach an alias without
// stat'ing the client's filesystem.
type localInit struct {
	Choice       config.NameChoice
	Disc         config.DiscoveredPaths
	ExistingToml *config.ProjectConfig
	StartPath    string
	Alias        *config.AliasInfo
}

// localDerive runs the same name-selection logic the daemon uses
// in path-based init, but on the client's filesystem. Errors from
// PickInitName (conflict, no-source) are returned unwrapped so
// callInit can dispatch on them. Alias metadata is computed
// best-effort: when the workspace can't yield an alias, the daemon
// still gets name but no alias attach happens.
func localDerive(ctx context.Context, startPath string, opts callInitOpts) (localInit, error) {
	disc, err := config.DiscoverPaths(startPath)
	if err != nil {
		return localInit{}, err
	}
	var tomlCfg *config.ProjectConfig
	if disc.WorkspaceRoot != "" {
		cfg, err := config.ReadProjectConfig(disc.WorkspaceRoot)
		switch {
		case err == nil:
			tomlCfg = cfg
		case errors.Is(err, config.ErrProjectConfigMissing):
			// Discovered workspace root, but file vanished between the
			// walk and the read; treat as no-toml so we fall through
			// to the next identity source.
		default:
			return localInit{}, err
		}
	}
	choice, err := config.PickInitName(ctx, disc, tomlCfg, opts.Project, opts.Replace)
	if err != nil {
		return localInit{}, err
	}
	alias, err := computeAliasInfo(ctx, disc, startPath)
	if err != nil {
		return localInit{}, err
	}
	return localInit{
		Choice:       choice,
		Disc:         disc,
		ExistingToml: tomlCfg,
		StartPath:    startPath,
		Alias:        alias,
	}, nil
}

// computeAliasInfo derives the alias metadata the daemon needs to
// attach an alias on the client's behalf. Mirrors the daemon-side
// path-based init: when the workspace has neither a git ancestor
// nor a .kata.toml ancestor, we synthesize a workspace root at the
// start path so ComputeAliasIdentity has something to anchor on
// (matching the path-based local:// fallback).
func computeAliasInfo(ctx context.Context, disc config.DiscoveredPaths, startPath string) (*config.AliasInfo, error) {
	if disc.GitRoot == "" && disc.WorkspaceRoot == "" {
		disc.WorkspaceRoot = startPath
	}
	info, err := config.ComputeAliasIdentity(ctx, disc)
	if err != nil {
		return nil, err
	}
	return &info, nil
}

// runNameInit POSTs the derived name to the daemon, then
// writes .kata.toml and .gitignore on the client's filesystem. The
// daemon never sees the client's workspace path.
func runNameInit(ctx context.Context, baseURL string, in localInit, opts callInitOpts) (string, error) {
	reqBody := map[string]any{
		"name": in.Choice.Name,
	}
	if opts.Replace {
		reqBody["replace"] = true
	}
	if opts.Reassign {
		reqBody["reassign"] = true
	}
	if in.Alias != nil {
		reqBody["alias"] = map[string]any{
			"identity": in.Alias.Identity,
			"kind":     in.Alias.Kind,
		}
	}
	bs, err := postProjects(ctx, baseURL, reqBody)
	if err != nil {
		return "", err
	}

	var resp struct {
		Project struct {
			Name string `json:"name"`
		} `json:"project"`
		Created bool `json:"created"`
	}
	if err := json.Unmarshal(bs, &resp); err != nil {
		return "", fmt.Errorf("decode response: %w", err)
	}

	dest := config.WriteDestination(in.Disc, in.StartPath)
	tomlChanged := needsTomlWrite(in.ExistingToml, resp.Project.Name)
	if tomlChanged {
		if err := config.WriteProjectConfig(dest, resp.Project.Name); err != nil {
			return "", fmt.Errorf("write .kata.toml: %w", err)
		}
	}
	gitignoreChanged, err := ensureGitignoreEntry(dest, ".kata.local.toml")
	if err != nil {
		warnGitignoreUpdate(err)
	}
	agentsChanged := false
	if opts.WithAgents {
		agentsChanged, err = applyAgentGuidance(dest)
		if err != nil {
			warnAgentsUpdate(err)
		}
	}

	return formatInitOutput(bs, resp.Project.Name, dest, resp.Created, resp.Created || tomlChanged || gitignoreChanged || agentsChanged)
}

// runStartPathInit is the fallback used when the client cannot derive a name
// locally (no .kata.toml, no --project, no git). It
// preserves today's behavior: the daemon walks its own filesystem,
// writes .kata.toml, and reports back the workspace root so the client
// places .gitignore beside it.
func runStartPathInit(ctx context.Context, baseURL, startPath string, opts callInitOpts) (string, error) {
	reqBody := map[string]any{"start_path": startPath}
	if opts.Project != "" {
		reqBody["name"] = opts.Project
	}
	if opts.Replace {
		reqBody["replace"] = true
	}
	if opts.Reassign {
		reqBody["reassign"] = true
	}
	bs, err := postProjects(ctx, baseURL, reqBody)
	if err != nil {
		return "", err
	}

	var resp struct {
		Project struct {
			Name string `json:"name"`
		} `json:"project"`
		WorkspaceRoot string `json:"workspace_root,omitempty"`
		Created       bool   `json:"created"`
	}
	if err := json.Unmarshal(bs, &resp); err != nil {
		return "", fmt.Errorf("decode response: %w", err)
	}

	gitignoreDir := resp.WorkspaceRoot
	if gitignoreDir == "" {
		gitignoreDir = startPath
	}
	gitignoreChanged, err := ensureGitignoreEntry(gitignoreDir, ".kata.local.toml")
	if err != nil {
		warnGitignoreUpdate(err)
	}
	agentsChanged := false
	if opts.WithAgents {
		agentsChanged, err = applyAgentGuidance(gitignoreDir)
		if err != nil {
			warnAgentsUpdate(err)
		}
	}

	// The path-based daemon flow writes workspace files remotely and exposes no
	// local file-change bit today; project creation is the closest stable signal.
	return formatInitOutput(bs, resp.Project.Name, gitignoreDir, resp.Created, resp.Created || gitignoreChanged || agentsChanged)
}

func warnGitignoreUpdate(err error) {
	if currentOutputMode() != outputHuman {
		return
	}
	fmt.Fprintf(os.Stderr, "kata: warning: could not update .gitignore: %v\n", err)
}

func warnAgentsUpdate(err error) {
	if currentOutputMode() != outputHuman {
		return
	}
	fmt.Fprintf(os.Stderr, "kata: warning: could not update AGENTS.md: %v\n", err)
}

// warnBeadsConflict tells the user kata declined to edit a file in place
// because it still carries a beads integration block, and points at the sidecar
// proposal kata wrote instead. Suppressed in machine-output modes, matching
// warnGitignoreUpdate.
func warnBeadsConflict(originalPath, sidecarPath string) {
	if currentOutputMode() != outputHuman {
		return
	}
	cwd, err := os.Getwd()
	if err != nil {
		cwd = ""
	}
	fmt.Fprint(os.Stderr, beadsConflictMessage(cwd, originalPath, sidecarPath))
}

// beadsConflictMessage builds the human hint for a declined in-place edit.
// Paths are rendered relative to cwd when they sit under it, else absolute, so
// the `mv` command is copy-pasteable no matter which directory init was invoked
// from — the write destination is the workspace root, which need not be cwd.
func beadsConflictMessage(cwd, originalPath, sidecarPath string) string {
	label := filepath.Base(originalPath)
	sidecar := displayPath(cwd, sidecarPath)
	original := displayPath(cwd, originalPath)
	return fmt.Sprintf(
		"kata: %s still contains a beads integration block; left it untouched.\n"+
			"      Wrote a migrated copy to %s (beads block removed, kata guidance added).\n"+
			"      Review it, then `mv %s %s` to adopt — or delete it to keep the original.\n",
		label, sidecar, shellQuote(sidecar), shellQuote(original))
}

// displayPath renders abs for a shell hint: relative to cwd when abs sits under
// it, else abs unchanged. An empty or unusable cwd yields the absolute path.
func displayPath(cwd, abs string) string {
	if cwd == "" {
		return abs
	}
	rel, err := filepath.Rel(cwd, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return abs
	}
	return rel
}

// postProjects POSTs the request and returns the raw response body on
// success. Non-2xx responses are decoded into a *cliError so callers
// can return them directly.
func postProjects(ctx context.Context, baseURL string, reqBody any) ([]byte, error) {
	client, err := httpClientFor(ctx, baseURL)
	if err != nil {
		return nil, fmt.Errorf("client: %w", err)
	}
	status, bs, err := httpDoJSON(ctx, client, http.MethodPost, baseURL+"/api/v1/projects", reqBody)
	if err != nil {
		return nil, fmt.Errorf("POST /api/v1/projects: %w", err)
	}
	if status >= 300 {
		return nil, apiErrFromBody(status, bs)
	}
	return bs, nil
}

// needsTomlWrite reports whether .kata.toml needs to be written: true
// when no toml exists yet or its name doesn't match the chosen value.
// Mirrors the daemon-side guard so re-running init in the
// same workspace is a no-op rather than a redundant rewrite.
func needsTomlWrite(existing *config.ProjectConfig, name string) bool {
	if existing == nil {
		return true
	}
	return existing.Project.Name != name
}

// formatInitOutput renders the selected output mode for init, shared between
// the path-free and path-based flows.
func formatInitOutput(bs []byte, name, workspace string, projectCreated, changed bool) (string, error) {
	switch currentOutputMode() {
	case outputJSON:
		var buf bytes.Buffer
		if err := emitJSON(&buf, json.RawMessage(bs)); err != nil {
			return "", fmt.Errorf("emit json: %w", err)
		}
		return buf.String(), nil
	case outputAgent:
		return fmt.Sprintf("OK init project=%s workspace=%s changed=%t\n",
			agentValue(name), agentValue(workspace), changed), nil
	}
	action := "bound"
	if projectCreated {
		action = "created and bound"
	}
	return fmt.Sprintf("%s project %s\n", action, textsafe.Line(name)), nil
}

// resolveStartPath returns the absolute path to use as the daemon's
// start_path. Relative paths are resolved against the CLI's current working
// directory so the daemon (which may have a different cwd) doesn't end up
// binding or writing .kata.toml in the wrong place.
func resolveStartPath(workspace string) (string, error) {
	if workspace == "" {
		return os.Getwd()
	}
	return filepath.Abs(workspace)
}

// apiErrFromBody decodes a daemon ErrorEnvelope and returns a *cliError with
// the appropriate exit code. Falls back to a raw-body error when the envelope
// can't be decoded so the caller still has debugging context.
func apiErrFromBody(status int, bs []byte) *cliError {
	var env struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(bs, &env); err != nil {
		return &cliError{
			Message:  errors.New(string(bs)).Error(),
			Code:     "",
			Kind:     kindForStatus(status),
			ExitCode: mapStatusToExit(status, ""),
		}
	}
	return &cliError{
		Message:  env.Error.Message,
		Code:     env.Error.Code,
		Kind:     kindForStatus(status),
		ExitCode: mapStatusToExit(status, env.Error.Code),
	}
}

// mapStatusToExit maps an HTTP status to a CLI exit code. The code parameter
// is reserved for future per-code overrides (e.g. distinguishing
// project_not_found from project_not_initialized within 404s).
func mapStatusToExit(status int, _ string) int {
	switch status {
	case http.StatusBadRequest:
		return ExitValidation
	case http.StatusNotFound:
		return ExitNotFound
	case http.StatusConflict:
		return ExitConflict
	case http.StatusPreconditionFailed:
		return ExitConfirm
	default:
		return ExitInternal
	}
}

// ensureGitignoreEntry appends a single line to <dir>/.gitignore if the entry
// is not already present. It creates the file if absent and returns whether the
// file changed. Re-running on a file that already lists the entry is a no-op.
func ensureGitignoreEntry(dir, entry string) (bool, error) {
	path := filepath.Join(dir, ".gitignore")
	existing, err := os.ReadFile(path) //nolint:gosec
	switch {
	case err == nil:
		// Walk lines so we don't false-match a substring inside a longer
		// pattern (e.g. ".kata.local.toml.bak").
		for _, line := range strings.Split(string(existing), "\n") {
			if strings.TrimSpace(line) == entry {
				return false, nil
			}
		}
		// Preserve trailing-newline convention: if the file ends without
		// a newline, add one before appending so we don't merge our line
		// into theirs.
		var prefix string
		if len(existing) > 0 && existing[len(existing)-1] != '\n' {
			prefix = "\n"
		}
		f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644) //nolint:gosec // .gitignore is world-readable by convention; mode is unused by O_APPEND on existing files but golangci-lint flags it
		if err != nil {
			return false, err
		}
		defer func() { _ = f.Close() }()
		if _, err := f.WriteString(prefix + entry + "\n"); err != nil {
			return false, err
		}
		return true, nil
	case errors.Is(err, os.ErrNotExist):
		if err := os.WriteFile(path, []byte(entry+"\n"), 0o644); err != nil { //nolint:gosec
			return false, err
		}
		return true, nil
	default:
		return false, err
	}
}

// agentsBlockBegin and agentsBlockEnd delimit the section kata manages inside
// an agent guidance file. They let init refresh kata's guidance in place across
// re-runs without disturbing anything else a project keeps in that file.
const (
	agentsBlockBegin = "<!-- BEGIN KATA (managed by `kata init --with-agents`) -->"
	agentsBlockEnd   = "<!-- END KATA -->"
)

// beadsBlockBeginPrefix and beadsBlockEnd match the integration block Beads
// writes into AGENTS.md/CLAUDE.md. The begin marker carries a trailing
// version/profile/hash, so we match on its prefix and scan to the end marker.
const (
	beadsBlockBeginPrefix = "<!-- BEGIN BEADS INTEGRATION"
	beadsBlockEnd         = "<!-- END BEADS INTEGRATION -->"
)

// agentsProposalSuffix names the sidecar kata writes when it refuses to edit a
// file in place because it still carries a beads integration block. The user
// reviews it and moves it over the original when ready.
const agentsProposalSuffix = ".kata-proposed"

// agentsBlockBody is the guidance kata injects between the markers. It mirrors
// the contract `kata quickstart` prints, kept short so it complements rather
// than duplicates the full quickstart text.
const agentsBlockBody = "## kata issue tracker\n\n" +
	"This project uses [kata](https://github.com/kenn-io/kata) as its shared issue\n" +
	"ledger. Run `kata quickstart` at the start of each session for the full agent\n" +
	"contract. The short version:\n\n" +
	"- Search before creating: `kata search \"<keywords>\" --agent`.\n" +
	"- Prefer updating existing issues over duplicates (`kata comment`, `kata label add`, `kata edit`).\n" +
	"- Default to `--agent` for ordinary reads and mutations; use `--json` only when a script needs structured data.\n" +
	"- Close only verified work: `kata close <ref> --done --message \"<scope + verification>\" --commit <sha>`.\n" +
	"- If work is incomplete, label `needs-review` and comment what remains rather than closing.\n" +
	"- Never `kata delete` or `kata purge` without explicit user authorization.\n"

// agentsManagedBlock returns the full marker-delimited block kata writes.
func agentsManagedBlock() string {
	return agentsBlockBegin + "\n" + agentsBlockBody + agentsBlockEnd
}

// applyAgentGuidance is the entry point for `--with-agents`. It writes kata's
// block into AGENTS.md and, when migrating off beads, sidesteps a destructive
// edit: if a file still carries a beads integration block, kata leaves it
// untouched and writes a <file>.kata-proposed copy (beads block removed, kata
// block added) for the user to adopt or discard. CLAUDE.md is only considered
// in that conflict case, and only when it is a real file rather than a symlink
// (kata's own convention points CLAUDE.md at AGENTS.md).
func applyAgentGuidance(dir string) (bool, error) {
	changed := false

	agentsPath := filepath.Join(dir, "AGENTS.md")
	// Refuse a symlinked AGENTS.md before reading it. Following the link would
	// let a hostile repo copy an outside file's content into the workspace via
	// the migration sidecar, or rewrite the link target. CLAUDE.md gets the same
	// treatment through regularFileWithBeads.
	if fi, lerr := os.Lstat(agentsPath); lerr == nil && fi.Mode()&os.ModeSymlink != 0 {
		return changed, fmt.Errorf("refusing to manage symlinked %s", agentsPath)
	}
	content, exists, err := readIfExists(agentsPath)
	if err != nil {
		return changed, err
	}
	if exists && hasBeadsBlock(content) {
		sidecar, err := writeMigrationProposal(agentsPath, content)
		if err != nil {
			return changed, err
		}
		warnBeadsConflict(agentsPath, sidecar)
		changed = true
	} else {
		c, err := ensureAgentsBlock(agentsPath, content, exists)
		if err != nil {
			return changed, err
		}
		changed = changed || c
	}

	claudePath := filepath.Join(dir, "CLAUDE.md")
	if claude, ok := regularFileWithBeads(claudePath); ok {
		sidecar, err := writeMigrationProposal(claudePath, claude)
		if err != nil {
			return changed, err
		}
		warnBeadsConflict(claudePath, sidecar)
		changed = true
	}

	return changed, nil
}

// ensureAgentsBlock writes kata's block into an agent guidance file whose
// current content is already known. It creates the file when absent, appends
// the block when the file has no kata markers, and rewrites the block in place
// when its content drifts. A begin marker with no matching end marker is an
// error rather than a guessed span. Returns whether the file changed.
func ensureAgentsBlock(path, content string, exists bool) (bool, error) {
	updated, err := upsertKataBlock(content)
	if err != nil {
		return false, err
	}
	if exists && updated == content {
		return false, nil
	}
	if exists {
		if err := rewriteGuidanceFile(path, []byte(updated)); err != nil {
			return false, err
		}
	} else if err := writeNewGuidanceFile(path, []byte(updated)); err != nil {
		return false, err
	}
	return true, nil
}

// upsertKataBlock returns content with kata's managed block present: appended
// (with a blank line of separation) when absent, or rewritten in place when a
// stale block already exists. A non-empty result always ends with a newline.
func upsertKataBlock(content string) (string, error) {
	block := agentsManagedBlock()
	begin := strings.Index(content, agentsBlockBegin)
	if begin == -1 {
		var prefix string
		switch {
		case len(content) == 0, strings.HasSuffix(content, "\n\n"):
		case strings.HasSuffix(content, "\n"):
			prefix = "\n"
		default:
			prefix = "\n\n"
		}
		return content + prefix + block + "\n", nil
	}
	rel := strings.Index(content[begin:], agentsBlockEnd)
	if rel == -1 {
		return "", fmt.Errorf("agent guidance file has %q without a matching %q", agentsBlockBegin, agentsBlockEnd)
	}
	end := begin + rel + len(agentsBlockEnd)
	if content[begin:end] == block {
		return content, nil
	}
	return content[:begin] + block + content[end:], nil
}

// writeMigrationProposal writes a <path><suffix> copy of an existing file with
// the beads block stripped and kata's block added, leaving the original
// untouched. Returns the sidecar path.
func writeMigrationProposal(path, content string) (string, error) {
	proposed, err := upsertKataBlock(stripBeadsBlock(content))
	if err != nil {
		return "", err
	}
	sidecar := path + agentsProposalSuffix
	if err := writeNewGuidanceFile(sidecar, []byte(proposed)); err != nil {
		return "", err
	}
	return sidecar, nil
}

// writeNewGuidanceFile creates path with data, refusing to follow a symlink a
// hostile repo may have planted there. O_EXCL fails (EEXIST) when anything —
// regular file or symlink — already occupies the path, so kata never redirects
// its write onto a victim file the path merely points at.
func writeNewGuidanceFile(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644) //nolint:gosec
	if err != nil {
		return err
	}
	_, werr := f.Write(data)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	return werr
}

// rewriteGuidanceFile overwrites an existing regular file, refusing to write
// through a symlink so kata never rewrites a file the path merely points at.
func rewriteGuidanceFile(path string, data []byte) error {
	fi, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("refusing to write through symlinked %s", path)
	}
	return os.WriteFile(path, data, 0o644) //nolint:gosec
}

// hasBeadsBlock reports whether content carries a complete beads integration
// block (both markers, in order).
func hasBeadsBlock(content string) bool {
	begin := strings.Index(content, beadsBlockBeginPrefix)
	if begin == -1 {
		return false
	}
	return strings.Contains(content[begin:], beadsBlockEnd)
}

// stripBeadsBlock removes the beads integration block and collapses the blank
// space it leaves at the seam. Content without a complete block is returned
// unchanged. A non-empty result ends with a newline.
func stripBeadsBlock(content string) string {
	begin := strings.Index(content, beadsBlockBeginPrefix)
	if begin == -1 {
		return content
	}
	rel := strings.Index(content[begin:], beadsBlockEnd)
	if rel == -1 {
		return content
	}
	end := begin + rel + len(beadsBlockEnd)
	prefix := strings.TrimRight(content[:begin], "\n")
	suffix := strings.TrimLeft(content[end:], "\n")
	switch {
	case prefix == "":
		return suffix
	case suffix == "":
		return prefix + "\n"
	default:
		return prefix + "\n\n" + suffix
	}
}

// readIfExists returns a file's content and whether it exists. A missing file
// is reported as (",", false, nil); other read errors propagate.
func readIfExists(path string) (string, bool, error) {
	bs, err := os.ReadFile(path) //nolint:gosec
	switch {
	case err == nil:
		return string(bs), true, nil
	case errors.Is(err, os.ErrNotExist):
		return "", false, nil
	default:
		return "", false, err
	}
}

// regularFileWithBeads reports the content of path when it is a regular file
// (not a symlink or directory) that carries a beads integration block. kata
// skips symlinked CLAUDE.md so it never rewrites a file that merely points at
// AGENTS.md.
func regularFileWithBeads(path string) (string, bool) {
	fi, err := os.Lstat(path)
	if err != nil || fi.Mode()&os.ModeSymlink != 0 || fi.IsDir() {
		return "", false
	}
	bs, err := os.ReadFile(path) //nolint:gosec
	if err != nil {
		return "", false
	}
	content := string(bs)
	if !hasBeadsBlock(content) {
		return "", false
	}
	return content, true
}
