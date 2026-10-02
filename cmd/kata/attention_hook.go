package main

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strings"

	"github.com/spf13/cobra"
	kataclient "go.kenn.io/kata/pkg/client"
	"go.kenn.io/kata/pkg/client/generated"
)

// `kata agent-hooks attention <start|end>` is lifecycle plumbing for
// the work.attention convention. The launcher supplies the tracked issue in
// KATA_REF for both hooks. No session payload or local state is involved.
//
// Legacy invocations exit zero and silently
// ignore invalid arguments, invalid refs, unavailable daemons, stale revisions, and other
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
		Short:              "Track attention at session start and end",
		Long:               "Track work.attention for the issue in KATA_REF.\nStdin is ignored; daemon failures remain silent.",
		Hidden:             true,
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			// Released launchers use an optional ownership marker and expect
			// malformed lifecycle calls to remain silent, including unknown flags.
			if len(args) == 0 || (args[0] != "start" && args[0] != "end") {
				return nil
			}
			if len(args) != 1 && !slices.Equal(args[1:], []string{"--source", legacyAttentionHookSource + args[0]}) {
				return nil
			}
			runAttentionHook(cmd, args[0])
			return nil
		},
	}
}

func runAttentionHook(cmd *cobra.Command, mode string) {
	// Both command entry points validate mode before calling this function.
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

// attnLookup uses empty attention for absent, null, and non-string metadata.
type attnLookup struct {
	kind      attnLookupKind
	attention string
	session   string
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
		if lookup.kind != lookupOpen || lookup.attention != attnValueOK {
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
	apiClient, err := kataclient.NewWithHTTPClient(baseURL, client)
	if err != nil {
		return attnLookup{kind: lookupTransient}
	}
	wire, callErr := apiClient.ShowIssueWithResponse(ctx, &generated.ShowIssueRequestOptions{
		PathParams: &generated.ShowIssuePath{ProjectID: pid, Ref: resolved.RefForAPI},
	})
	if wire == nil {
		return attnLookup{kind: lookupTransient}
	}
	status, body := wire.StatusCode, wire.Body
	if status == http.StatusNotFound {
		return attnLookup{kind: lookupGone}
	}
	if status >= http.StatusBadRequest || callErr != nil {
		return attnLookup{kind: lookupTransient}
	}
	var response metaShowResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return attnLookup{kind: lookupTransient}
	}
	if response.Issue.Status != "open" {
		return attnLookup{kind: lookupGone}
	}
	if response.Issue.Revision <= 0 {
		return attnLookup{kind: lookupTransient}
	}
	return attnLookup{
		kind:      lookupOpen,
		attention: decodeJSONString(response.Issue.Metadata[attentionKey]),
		session:   decodeJSONString(response.Issue.Metadata[attentionSessionKey]),
		revision:  response.Issue.Revision,
	}
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
	rawPatch := make(map[string]any, len(patch))
	for key, value := range patch {
		valueJSON, err := json.Marshal(value)
		if err != nil {
			return attnWriteFailed
		}
		rawPatch[key] = jsontext.Value(valueJSON)
	}
	apiClient, err := kataclient.NewWithHTTPClient(baseURL, client)
	if err != nil {
		return attnWriteFailed
	}
	etag := fmt.Sprintf(`"rev-%d"`, revision)
	response, callErr := apiClient.PatchIssueMetadataWithResponse(ctx, &generated.PatchIssueMetadataRequestOptions{
		PathParams: &generated.PatchIssueMetadataPath{ProjectID: pid, Ref: resolved.RefForAPI},
		Body:       &generated.PatchIssueMetadataBody{Actor: &actor, Patch: rawPatch},
		Header:     &generated.PatchIssueMetadataHeaders{IfMatch: &etag},
	})
	if response == nil {
		return attnWriteFailed
	}
	status := response.StatusCode
	if status == http.StatusPreconditionFailed {
		return attnWriteConflict
	}
	if status >= http.StatusBadRequest || callErr != nil {
		return attnWriteFailed
	}
	return attnWriteApplied
}
