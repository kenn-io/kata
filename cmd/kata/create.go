package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/textsafe"
)

func newCreateCmd() *cobra.Command {
	var src BodySources
	var (
		labels         []string
		parentRefSlice []string
		blocks         []string
		blockedBy      []string
		related        []string
		owner          string
		priority       int
	)
	cmd := &cobra.Command{
		Use:   "create <title>",
		Short: "create a new issue",
		Args:  cobra.ExactArgs(1),
	}
	var idempotencyKey string
	var forceNew bool
	cmd.Flags().StringVar(&src.Body, "body", "", "issue body")
	cmd.Flags().StringVar(&src.File, "body-file", "", "read body from file")
	cmd.Flags().BoolVar(&src.Stdin, "body-stdin", false, "read body from stdin")
	cmd.Flags().StringSliceVar(&labels, "label", nil, "initial label (repeatable)")
	// --parent is at-most-one. We accept it as a slice so duplicate flags
	// produce a parseable error (singletonRefValue) instead of cobra's
	// silent last-wins on StringVar; collapseSingletonRef rejects multiple
	// distinct values explicitly.
	cmd.Flags().Var(newRefSliceValue(&parentRefSlice), "parent",
		"initial parent (must finish before this issue starts; ≤1; "+issueRefHelp+")")
	cmd.Flags().Var(newRefSliceValue(&blocks), "blocks",
		"this issue blocks <ref> (this must finish before <ref> can; repeatable; "+issueRefHelp+")")
	cmd.Flags().Var(newRefSliceValue(&blockedBy), "blocked-by",
		"this issue is blocked by <ref> (<ref> must finish before this; repeatable; "+issueRefHelp+")")
	cmd.Flags().Var(newRefSliceValue(&related), "related",
		"this issue is related to <ref> (symmetric, no ordering; repeatable; "+issueRefHelp+")")
	cmd.Flags().StringVar(&owner, "owner", "", "initial owner")
	cmd.Flags().IntVar(&priority, "priority", 0, "initial priority (0..4; 0 = highest)")
	cmd.Flags().StringVar(&idempotencyKey, "idempotency-key", "", "send Idempotency-Key header for safe retry")
	cmd.Flags().BoolVar(&forceNew, "force-new", false, "bypass look-alike soft-block (idempotency still wins)")

	// RunE is set after flag registration so we can reference cmd.Flags().Changed.
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		src.BodySet = cmd.Flags().Changed("body")
		src.FileSet = cmd.Flags().Changed("body-file")

		title := args[0]
		if strings.TrimSpace(title) == "" {
			return &cliError{Message: "title must not be empty", Kind: kindValidation, ExitCode: ExitValidation}
		}
		if err := validateCreateLabels(labels); err != nil {
			return err
		}
		if cmd.Flags().Changed("priority") && (priority < 0 || priority > 4) {
			return &cliError{
				Message:  "--priority must be between 0 and 4",
				Kind:     kindValidation,
				ExitCode: ExitValidation,
			}
		}

		ctx := cmd.Context()
		start, err := resolveStartPath(flags.Workspace)
		if err != nil {
			return err
		}
		baseURL, err := ensureDaemon(ctx)
		if err != nil {
			return err
		}
		projectID, projectName, err := resolveProjectIDAndName(ctx, baseURL, start)
		if err != nil {
			return err
		}
		body, err := resolveBody(src, cmd.InOrStdin())
		if err != nil {
			code := ExitValidation
			if strings.HasPrefix(err.Error(), "must pass exactly one of") {
				code = ExitUsage
			}
			return &cliError{Message: err.Error(), Kind: kindForExit(code), ExitCode: code}
		}
		actor, _ := resolveActor(flags.As, nil)
		client, err := httpClientFor(ctx, baseURL)
		if err != nil {
			return err
		}

		req := map[string]any{"actor": actor, "title": title, "body": body}
		if cmd.Flags().Changed("owner") {
			req["owner"] = owner
		}
		if cmd.Flags().Changed("priority") {
			req["priority"] = priority
		}
		if len(labels) > 0 {
			req["labels"] = labels
		}
		// Resolve every link-target ref to its wire ref string before
		// building the payload. Refs accept the same forms as `kata show`:
		// bare short_id, qualified ("kata#abc4"), or full ULID. The daemon
		// resolves each ref against the project at request time.
		var links []map[string]any
		var parentRef string
		if cmd.Flags().Changed("parent") {
			r, err := resolveSingletonRefToWire(ctx, baseURL, projectName, projectID, parentRefSlice, "--parent", false)
			if err != nil {
				return err
			}
			parentRef = r
			links = append(links, map[string]any{"type": "parent", "to_ref": r})
		}
		blocksRefs, err := resolveRefSliceToWire(ctx, baseURL, projectName, projectID, blocks, "--blocks")
		if err != nil {
			return err
		}
		for _, r := range blocksRefs {
			links = append(links, map[string]any{"type": "blocks", "to_ref": r})
		}
		blockedByRefs, err := resolveRefSliceToWire(ctx, baseURL, projectName, projectID, blockedBy, "--blocked-by")
		if err != nil {
			return err
		}
		for _, r := range blockedByRefs {
			links = append(links, map[string]any{"type": "blocks", "to_ref": r, "incoming": true})
		}
		relatedRefs, err := resolveRefSliceToWire(ctx, baseURL, projectName, projectID, related, "--related")
		if err != nil {
			return err
		}
		for _, r := range relatedRefs {
			links = append(links, map[string]any{"type": "related", "to_ref": r})
		}
		if len(links) > 0 {
			req["links"] = links
		}
		if forceNew {
			req["force_new"] = true
		}
		headers := map[string]string{}
		if idempotencyKey != "" {
			headers["Idempotency-Key"] = idempotencyKey
		}

		status, bs, err := httpDoJSONWithHeader(ctx, client, http.MethodPost,
			fmt.Sprintf("%s/api/v1/projects/%d/issues", baseURL, projectID),
			headers, req)
		if err != nil {
			return err
		}
		if status >= 400 {
			return apiErrFromBody(status, bs)
		}
		// The /issues create response doesn't carry a `changes` block (it
		// lives on the PATCH path). Synthesize one from the resolved
		// initial-link refs so human-mode `kata create` echoes a
		// `links: +parent <ref>, +blocks <ref>, ...` summary, mirroring
		// the per-link diff a `kata edit` PATCH produces. Reverse
		// direction (`--blocked-by` adding to `blocked_by_added` rather
		// than `blocks_added`) preserves the user's POV.
		applied := initialLinksAsChanges(parentRef, blocksRefs, blockedByRefs, relatedRefs)
		return printMutationWithApplied(cmd, bs, applied)
	}
	return cmd
}

// initialLinksAsChanges builds a synthetic mutationChanges from the
// resolved initial-link refs so create's human output can mirror
// edit's `links: +parent <ref>` diff format. parentRef is "" when no
// `--parent` was passed; the other slices may be nil/empty. Returns
// nil when no link flags were used.
//
// Dedupes each slice before storing so a request like
// `--blocks abc4 --blocks abc4` summarizes as a single `+blocks abc4`,
// matching the daemon's CreateIssue path which dedupes initial links
// before persisting. Without this, the human-mode summary would
// over-report what actually landed.
func initialLinksAsChanges(parentRef string, blocks, blockedBy, related []string) *mutationChanges {
	if parentRef == "" && len(blocks) == 0 && len(blockedBy) == 0 && len(related) == 0 {
		return nil
	}
	c := &mutationChanges{}
	if parentRef != "" {
		ref := parentRef
		c.ParentSet = &linkPeerForChanges{ShortID: ref}
	}
	c.BlocksAdded = stringSliceToPeers(dedupeStrings(blocks))
	c.BlockedByAdded = stringSliceToPeers(dedupeStrings(blockedBy))
	c.RelatedAdded = stringSliceToPeers(dedupeStrings(related))
	return c
}

// dedupeStrings returns a copy of in with duplicate entries dropped,
// preserving first-occurrence order. Returns nil for empty input so
// the caller's omitempty JSON tags do the right thing.
func dedupeStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

// stringSliceToPeers wraps each ref into a linkPeerForChanges with ShortID
// set; used by initialLinksAsChanges so create's synthetic changes shape
// matches the daemon's edit-response shape.
func stringSliceToPeers(refs []string) []linkPeerForChanges {
	if len(refs) == 0 {
		return nil
	}
	out := make([]linkPeerForChanges, 0, len(refs))
	for _, r := range refs {
		out = append(out, linkPeerForChanges{ShortID: r})
	}
	return out
}

func validateCreateLabels(labels []string) error {
	// Reject blank labels client-side so a typo or accidental "--label ''"
	// doesn't silently succeed-with-no-label (hammer-test finding #8). This
	// must run before daemon resolution so validation failures are local.
	for _, l := range labels {
		if strings.TrimSpace(l) == "" {
			return &cliError{
				Message:  "--label must not be empty",
				Kind:     kindValidation,
				ExitCode: ExitValidation,
			}
		}
	}
	return nil
}

// resolveProjectID resolves the project ID by calling
// POST /api/v1/projects/resolve on the daemon.
//
// A global --project value wins and is sent as the project name, allowing
// project-scoped commands to target any project from any directory. Otherwise,
// start_path lets the daemon resolve by alias first and repair stale .kata.toml
// bindings after project rename/merge. A readable local .kata.toml is still
// parsed before the daemon call so malformed config fails with a direct fix-it
// error instead of being hidden behind daemon-side path resolution.
func resolveProjectID(ctx context.Context, baseURL, startPath string) (int64, error) {
	id, _, err := resolveProjectIDAndName(ctx, baseURL, startPath)
	return id, err
}

// resolveProjectIDAndName is resolveProjectID plus the project's canonical
// name. Used by ref-consuming commands that need the project name to format
// qualified IDs ("<project>#<short_id>") for display or for the destructive
// confirmation header.
//
// Wire shape is chosen client-side so the daemon never has to stat the
// client's filesystem (issue #35). Priority order:
//
//  1. --project X → {name: X}. Explicit target — alias-first repair
//     would risk redirecting to a different project.
//  2. .kata.toml readable → {name, alias?}. Daemon does alias-first
//     repair; on rename the client rewrites .kata.toml to the
//     canonical name returned in the response.
//  3. Git workspace, no .kata.toml → {alias}. Daemon does strict
//     alias lookup; unknown alias is 404 (init owns create-by-
//     convention from git remotes — resolve never creates).
//  4. Neither → {start_path}. Legacy local-only fallback.
func resolveProjectIDAndName(ctx context.Context, baseURL, startPath string) (int64, string, error) {
	body, repair, err := buildResolveRequest(startPath)
	if err != nil {
		return 0, "", err
	}
	client, err := httpClientFor(ctx, baseURL)
	if err != nil {
		return 0, "", err
	}
	status, bs, err := httpDoJSON(ctx, client, http.MethodPost,
		baseURL+"/api/v1/projects/resolve", body)
	if err != nil {
		return 0, "", err
	}
	if status >= 400 {
		return 0, "", apiErrFromBody(status, bs)
	}
	var b struct {
		Project struct {
			ID   int64  `json:"id"`
			Name string `json:"name"`
		} `json:"project"`
	}
	if err := json.Unmarshal(bs, &b); err != nil {
		return 0, "", err
	}
	if repair != nil {
		if err := repair(b.Project.Name); err != nil {
			return 0, "", err
		}
	}
	return b.Project.ID, b.Project.Name, nil
}

// buildResolveRequest selects the wire shape for a resolve and returns
// an optional callback for client-side .kata.toml repair on rename.
func buildResolveRequest(startPath string) (map[string]any, func(string) error, error) {
	body := map[string]any{}

	if project := strings.TrimSpace(flags.Project); project != "" {
		body["name"] = project
		return body, nil, nil
	}

	disc, err := config.DiscoverPaths(startPath)
	if err != nil {
		// Tolerate "not exist" so a typo in --workspace still surfaces
		// as a uniform daemon-side error instead of a divergent
		// client-side stat error. Other stat failures (permission,
		// etc.) propagate.
		if errors.Is(err, os.ErrNotExist) {
			return map[string]any{"start_path": startPath}, nil, nil
		}
		return nil, nil, &cliError{
			Message:  err.Error(),
			Kind:     kindValidation,
			ExitCode: ExitValidation,
		}
	}

	var tomlCfg *config.ProjectConfig
	if disc.WorkspaceRoot != "" {
		cfg, err := config.ReadProjectConfig(disc.WorkspaceRoot)
		switch {
		case err == nil:
			tomlCfg = cfg
		case errors.Is(err, config.ErrProjectConfigMissing):
			// no .kata.toml here
		default:
			return nil, nil, &cliError{
				Message:  "read .kata.toml: " + err.Error(),
				Kind:     kindValidation,
				ExitCode: ExitValidation,
			}
		}
	}

	// Alias derivation is best-effort: a broken git config shouldn't
	// block resolve when .kata.toml supplies a name. Without a name to
	// fall back to, the derivation error must surface so the user can
	// fix it instead of seeing an opaque daemon stat error.
	var alias *config.AliasInfo
	if disc.GitRoot != "" || disc.WorkspaceRoot != "" {
		info, derr := config.ComputeAliasIdentity(disc)
		switch {
		case derr == nil:
			alias = &info
		case tomlCfg == nil:
			return nil, nil, &cliError{
				Message:  derr.Error(),
				Kind:     kindValidation,
				ExitCode: ExitValidation,
			}
		}
	}

	if tomlCfg != nil && tomlCfg.Project.Name != "" {
		body["name"] = tomlCfg.Project.Name
		if alias != nil {
			body["alias"] = aliasInputBody(*alias)
		}
		workspaceRoot := disc.WorkspaceRoot
		current := tomlCfg.Project.Name
		repair := func(canonical string) error {
			if canonical == "" || canonical == current {
				return nil
			}
			if err := config.WriteProjectConfig(workspaceRoot, canonical); err != nil {
				return fmt.Errorf("rewrite .kata.toml: %w", err)
			}
			return nil
		}
		return body, repair, nil
	}

	if alias != nil {
		body["alias"] = aliasInputBody(*alias)
		return body, nil, nil
	}

	body["start_path"] = startPath
	return body, nil, nil
}

// aliasInputBody marshals an AliasInfo into the wire shape the daemon
// expects (mirrors api.AliasInput).
func aliasInputBody(info config.AliasInfo) map[string]any {
	return map[string]any{
		"identity":  info.Identity,
		"kind":      info.Kind,
		"root_path": info.RootPath,
	}
}

// printMutation formats a mutation response (issue create/edit/close/reopen)
// according to the active output mode: JSON envelope, quiet (issue short_id
// only), or human-readable one-liner.
//
// In human mode, when the response carries a `changes` block (kata edit
// with link/priority mutations), append a per-line summary so the caller
// can see what landed without parsing JSON. A request whose `changed`
// flag is false renders an explicit "(no changes applied)" tail so a
// no-op edit doesn't look identical to a successful one.
func printMutation(cmd *cobra.Command, bs []byte) error {
	return printMutationWithApplied(cmd, bs, nil)
}

// printMutationWithApplied is printMutation with an optional fallback
// `changes` block. When the response itself doesn't carry one (the
// /issues create path doesn't), pass `applied` describing the
// requested mutations so human-mode output still echoes a `links: ...`
// summary. The wire payload (JSON-mode output) is left unchanged —
// the `applied` argument only seeds the human-mode renderer.
func printMutationWithApplied(cmd *cobra.Command, bs []byte, applied *mutationChanges) error {
	mode := currentOutputMode()
	var b struct {
		Issue struct {
			ShortID string `json:"short_id"`
			Title   string `json:"title"`
			Status  string `json:"status"`
		} `json:"issue"`
		Changed bool             `json:"changed"`
		Changes *mutationChanges `json:"changes,omitempty"`
	}
	if err := json.Unmarshal(bs, &b); err != nil {
		return err
	}
	if mode == outputJSON {
		var buf bytes.Buffer
		if err := emitJSON(&buf, json.RawMessage(bs)); err != nil {
			return err
		}
		_, err := fmt.Fprint(cmd.OutOrStdout(), buf.String())
		return err
	}
	if mode == outputAgent {
		var m agentIssueMutation
		if err := json.Unmarshal(bs, &m); err != nil {
			return err
		}
		verb := commandLeaf(cmd)
		includeChangedTrue := verb == "edit"
		return printAgentMutationDecoded(cmd.OutOrStdout(), verb, m, includeChangedTrue, func(w io.Writer, m agentIssueMutation) error {
			if m.Issue.ClosedReason != nil {
				if err := writeAgentField(w, "Reason", agentValue(*m.Issue.ClosedReason)); err != nil {
					return err
				}
			}
			if verb == "close" {
				for _, evidence := range agentEvidenceSummaries(m) {
					if err := writeAgentField(w, "Evidence", agentValue(evidence)); err != nil {
						return err
					}
				}
			}
			if verb == "edit" && m.Changed {
				if changes := agentEditChangeNames(cmd, b.Changes); len(changes) > 0 {
					if err := writeAgentField(w, "Changes", strings.Join(changes, ", ")); err != nil {
						return err
					}
				}
			}
			return nil
		})
	}
	if flags.Quiet {
		_, err := fmt.Fprintln(cmd.OutOrStdout(), b.Issue.ShortID)
		return err
	}
	if _, err := fmt.Fprintf(cmd.OutOrStdout(), "%s %s [%s]",
		b.Issue.ShortID, textsafe.Line(b.Issue.Title), b.Issue.Status); err != nil {
		return err
	}
	changes := b.Changes
	changed := b.Changed
	if changes == nil && applied != nil && b.Changed {
		// Create-path fallback: the wire response lacks a changes block,
		// but the caller passed the resolved initial-link state. Only
		// fall back when the daemon also flagged the response as
		// changed=true — an idempotent-reuse response (changed=false,
		// returned when an Idempotency-Key matched a prior create)
		// must not synthesize a "links: ..." summary because nothing
		// was applied THIS call. The original create's links surfaced
		// in its OWN response.
		changes = applied
	}
	if summary := summarizeChanges(changes, changed); summary != "" {
		if _, err := fmt.Fprint(cmd.OutOrStdout(), " "+summary); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintln(cmd.OutOrStdout())
	return err
}

// linkPeerForChanges mirrors api.LinkPeer's wire shape so the CLI's
// changes-decoder can pluck the short_id off each link mutation entry. UID
// is decoded too in case the CLI ever needs to disambiguate, but the human
// summary only renders short_id.
type linkPeerForChanges struct {
	UID     string `json:"uid"`
	ShortID string `json:"short_id"`
}

// mutationChanges mirrors the wire `changes` block produced by `kata edit`
// for link mutations. Each entry is a LinkPeer (UID + short_id) so the human
// renderer can show "<short_id>" without resolving anything.
type mutationChanges struct {
	ParentSet        *linkPeerForChanges  `json:"parent_set,omitempty"`
	ParentRemoved    *linkPeerForChanges  `json:"parent_removed,omitempty"`
	BlocksAdded      []linkPeerForChanges `json:"blocks_added,omitempty"`
	BlocksRemoved    []linkPeerForChanges `json:"blocks_removed,omitempty"`
	BlockedByAdded   []linkPeerForChanges `json:"blocked_by_added,omitempty"`
	BlockedByRemoved []linkPeerForChanges `json:"blocked_by_removed,omitempty"`
	RelatedAdded     []linkPeerForChanges `json:"related_added,omitempty"`
	RelatedRemoved   []linkPeerForChanges `json:"related_removed,omitempty"`
}

// summarizeChanges renders a human-mode tail for the printMutation
// one-liner. Returns "" when this response has no link information to
// surface (e.g. plain title-edit, plain create, comment, close).
func summarizeChanges(c *mutationChanges, changed bool) string {
	if c == nil {
		return ""
	}
	parts := make([]string, 0, 8)
	if c.ParentSet != nil && c.ParentRemoved != nil {
		parts = append(parts, fmt.Sprintf("parent %s→%s", c.ParentRemoved.ShortID, c.ParentSet.ShortID))
	} else if c.ParentSet != nil {
		parts = append(parts, fmt.Sprintf("+parent %s", c.ParentSet.ShortID))
	} else if c.ParentRemoved != nil {
		parts = append(parts, fmt.Sprintf("-parent %s", c.ParentRemoved.ShortID))
	}
	for _, p := range c.BlocksAdded {
		parts = append(parts, fmt.Sprintf("+blocks %s", p.ShortID))
	}
	for _, p := range c.BlocksRemoved {
		parts = append(parts, fmt.Sprintf("-blocks %s", p.ShortID))
	}
	for _, p := range c.BlockedByAdded {
		parts = append(parts, fmt.Sprintf("+blocked_by %s", p.ShortID))
	}
	for _, p := range c.BlockedByRemoved {
		parts = append(parts, fmt.Sprintf("-blocked_by %s", p.ShortID))
	}
	for _, p := range c.RelatedAdded {
		parts = append(parts, fmt.Sprintf("+related %s", p.ShortID))
	}
	for _, p := range c.RelatedRemoved {
		parts = append(parts, fmt.Sprintf("-related %s", p.ShortID))
	}
	if len(parts) == 0 {
		// `changes` was present but every entry was an idempotent no-op.
		// Make the no-op explicit so callers don't confuse it with a
		// successful mutation.
		if !changed {
			return "(no changes applied)"
		}
		return ""
	}
	return "links: " + strings.Join(parts, ", ")
}

func agentEditChangeNames(cmd *cobra.Command, changes *mutationChanges) []string {
	names := make([]string, 0, 5)
	for _, item := range []struct {
		flag string
		name string
	}{
		{"title", "title"},
		{"body", "body"},
		{"owner", "owner"},
		{"priority", "priority"},
	} {
		if cmd.Flags().Changed(item.flag) {
			names = append(names, item.name)
		}
	}
	if agentHasLinkChanges(cmd, changes) {
		names = append(names, "links")
	}
	return names
}

func agentHasLinkChanges(cmd *cobra.Command, changes *mutationChanges) bool {
	if changes != nil {
		return changes.ParentSet != nil || changes.ParentRemoved != nil ||
			len(changes.BlocksAdded) > 0 || len(changes.BlocksRemoved) > 0 ||
			len(changes.BlockedByAdded) > 0 || len(changes.BlockedByRemoved) > 0 ||
			len(changes.RelatedAdded) > 0 || len(changes.RelatedRemoved) > 0
	}
	for _, flag := range []string{
		"parent", "blocks", "blocked-by", "related",
		"remove-parent", "remove-blocks", "remove-blocked-by", "remove-related",
	} {
		if cmd.Flags().Changed(flag) {
			return true
		}
	}
	return false
}

func agentEvidenceSummaries(m agentIssueMutation) []string {
	if m.Event == nil || m.Event.Payload == "" {
		return nil
	}
	var payload struct {
		Evidence []struct {
			Type      string   `json:"type"`
			SHA       string   `json:"sha,omitempty"`
			URL       string   `json:"url,omitempty"`
			Command   string   `json:"command,omitempty"`
			Paths     []string `json:"paths,omitempty"`
			Rationale string   `json:"rationale,omitempty"`
			IssueRef  string   `json:"issue_ref,omitempty"`
		} `json:"evidence,omitempty"`
	}
	if err := json.Unmarshal([]byte(m.Event.Payload), &payload); err != nil {
		return nil
	}
	out := make([]string, 0, len(payload.Evidence))
	for _, ev := range payload.Evidence {
		switch ev.Type {
		case "commit":
			out = append(out, "commit:"+ev.SHA)
		case "pr":
			out = append(out, "pr:"+ev.URL)
		case "test":
			out = append(out, "test:"+ev.Command)
		case "reviewed-paths":
			out = append(out, "reviewed-paths:"+strings.Join(ev.Paths, ","))
		case "no-change-audit":
			out = append(out, "no-change-audit:"+ev.Rationale)
		case "duplicate-of":
			out = append(out, "duplicate-of:"+ev.IssueRef)
		case "superseded-by":
			out = append(out, "superseded-by:"+ev.IssueRef)
		}
	}
	return out
}
