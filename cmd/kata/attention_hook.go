package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

// `kata attention-hook <start|end>` is launcher-only lifecycle plumbing for
// the work.attention convention. The launcher supplies the tracked issue in
// KATA_REF for both hooks. No session payload or local state is involved.
//
// Hooks must never break the harness: every mode exits zero and silently
// ignores invalid refs, unavailable daemons, stale revisions, and other
// internal failures.

const (
	attnValueOK         = "ok"
	attnValueNeedsHuman = "needs-human"
	attnHandoffMsg      = "session ended without hand-off"
	attnWriteAttempts   = 2
)

func newAttentionHookCmd() *cobra.Command {
	return &cobra.Command{
		Use:                "attention-hook <start|end>",
		Short:              "launcher attention lifecycle plumbing (installed by kata init --with-hooks)",
		Hidden:             true,
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			// Treat every malformed invocation as a silent no-op. In particular,
			// dash-leading args must not reach Cobra's non-zero flag-error path.
			if len(args) != 1 {
				return nil
			}
			runAttentionHook(cmd, args[0])
			return nil
		},
	}
}

func runAttentionHook(cmd *cobra.Command, mode string) {
	if mode != "start" && mode != "end" {
		return
	}

	// Claude Code exposes the workspace it launched from even when hook cwd
	// differs. Prefer it as the normal project-resolution anchor when present.
	if projectDir := strings.TrimSpace(os.Getenv("CLAUDE_PROJECT_DIR")); projectDir != "" {
		previous := flags.Workspace
		flags.Workspace = projectDir
		defer func() { flags.Workspace = previous }()
	}

	d := &liveAttnDaemon{cmd: cmd}
	switch mode {
	case "start":
		attnStart(d, os.Getenv("KATA_REF"))
	case "end":
		attnEnd(d, os.Getenv("KATA_REF"))
	}
}

type attnLookupKind uint8

const (
	lookupTransient attnLookupKind = iota
	lookupGone
	lookupOpen
)

type attnLookup struct {
	kind      attnLookupKind
	attention string
	hasAttn   bool
	revision  int64
}

type attnDaemon interface {
	lookup(ref string) attnLookup
	setMetaIfRevision(ref string, patch map[string]string, revision int64) attnWriteResult
}

type attnWriteResult uint8

const (
	attnWriteFailed attnWriteResult = iota
	attnWriteApplied
	attnWriteConflict
)

func attentionRef(kataRef string) (string, bool) {
	ref := strings.TrimSpace(kataRef)
	return ref, ref != "" && !strings.HasPrefix(ref, "-")
}

// attnStart establishes the launcher's active-session baseline. It mutates
// only work.attention and only if the open lookup revision is still current.
func attnStart(d attnDaemon, kataRef string) {
	ref, ok := attentionRef(kataRef)
	if !ok {
		return
	}
	for range attnWriteAttempts {
		lookup := d.lookup(ref)
		if lookup.kind != lookupOpen {
			return
		}
		if d.setMetaIfRevision(ref, map[string]string{attentionKey: attnValueOK}, lookup.revision) != attnWriteConflict {
			return
		}
	}
}

// attnEnd escalates an issue only when the lookup snapshot is open and its
// work.attention value is exactly ok. If-Match makes the two-key hand-off
// patch atomic with respect to any concurrent metadata change.
func attnEnd(d attnDaemon, kataRef string) {
	ref, ok := attentionRef(kataRef)
	if !ok {
		return
	}
	for range attnWriteAttempts {
		lookup := d.lookup(ref)
		if lookup.kind != lookupOpen || !lookup.hasAttn || lookup.attention != attnValueOK {
			return
		}
		if d.setMetaIfRevision(ref, map[string]string{
			attentionKey:    attnValueNeedsHuman,
			attentionMsgKey: attnHandoffMsg,
		}, lookup.revision) != attnWriteConflict {
			return
		}
	}
}

// liveAttnDaemon resolves refs and reads/writes metadata through the running
// daemon using the same workspace/project routing as ordinary CLI commands.
type liveAttnDaemon struct {
	cmd *cobra.Command
}

func (l *liveAttnDaemon) lookup(ref string) attnLookup {
	ctx, baseURL, pid, resolved, err := resolveIssueRefForCommand(l.cmd, ref)
	if err != nil {
		return attnLookup{kind: lookupTransient}
	}
	client, err := httpClientFor(ctx, baseURL)
	if err != nil {
		return attnLookup{kind: lookupTransient}
	}
	status, body, err := httpDoJSON(ctx, client, http.MethodGet,
		fmt.Sprintf("%s/api/v1/projects/%d/issues/%s", baseURL, pid, url.PathEscape(resolved.RefForAPI)), nil)
	if err != nil {
		return attnLookup{kind: lookupTransient}
	}
	if status == http.StatusNotFound {
		return attnLookup{kind: lookupGone}
	}
	if status >= http.StatusBadRequest {
		return attnLookup{kind: lookupTransient}
	}
	var response struct {
		Issue struct {
			Status   string                     `json:"status"`
			Metadata map[string]json.RawMessage `json:"metadata"`
			Revision int64                      `json:"revision"`
		} `json:"issue"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return attnLookup{kind: lookupTransient}
	}
	if response.Issue.Status != "open" {
		return attnLookup{kind: lookupGone}
	}
	if response.Issue.Revision <= 0 {
		return attnLookup{kind: lookupTransient}
	}
	lookup := attnLookup{kind: lookupOpen, revision: response.Issue.Revision}
	if raw, ok := response.Issue.Metadata[attentionKey]; ok {
		lookup.hasAttn = true
		_ = json.Unmarshal(raw, &lookup.attention)
	}
	return lookup
}

func (l *liveAttnDaemon) setMetaIfRevision(ref string, patch map[string]string, revision int64) attnWriteResult {
	ctx, baseURL, pid, resolved, err := resolveIssueRefForCommand(l.cmd, ref)
	if err != nil {
		return attnWriteFailed
	}
	client, err := httpClientFor(ctx, baseURL)
	if err != nil {
		return attnWriteFailed
	}
	actor, _ := resolveActor(ctx, flags.As, nil)
	rawPatch := make(map[string]json.RawMessage, len(patch))
	for key, value := range patch {
		valueJSON, err := json.Marshal(value)
		if err != nil {
			return attnWriteFailed
		}
		rawPatch[key] = json.RawMessage(valueJSON)
	}
	status, _, err := httpDoJSONHeaders(ctx, client, http.MethodPost,
		fmt.Sprintf("%s/api/v1/projects/%d/issues/%s/metadata", baseURL, pid, url.PathEscape(resolved.RefForAPI)),
		map[string]any{
			"actor": actor,
			"patch": rawPatch,
		}, map[string]string{"If-Match": fmt.Sprintf(`"rev-%d"`, revision)})
	if err != nil {
		return attnWriteFailed
	}
	if status == http.StatusPreconditionFailed {
		return attnWriteConflict
	}
	if status >= http.StatusBadRequest {
		return attnWriteFailed
	}
	return attnWriteApplied
}
