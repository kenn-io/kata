package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
)

func newEditCmd() *cobra.Command {
	var (
		title    string
		body     string
		owner    string
		priority string

		parentRefSlice       []string
		blocks               []string
		blockedBy            []string
		related              []string
		removeParentRefSlice []string
		removeBlocks         []string
		removeBlockedBy      []string
		removeRelated        []string
	)
	cmd := &cobra.Command{
		Use:   "edit <issue-ref>",
		Short: "edit issue title/body/owner/priority and relationships",
		Args:  cobra.ExactArgs(1),
	}
	cmd.Flags().StringVar(&title, "title", "", "new title")
	cmd.Flags().StringVar(&body, "body", "", "new body")
	cmd.Flags().StringVar(&owner, "owner", "", "new owner")
	cmd.Flags().StringVar(&priority, "priority", "",
		"new priority (0..4; 0 = highest). Pass '-' to clear.")

	// --parent and --remove-parent are at-most-one. We accept them as
	// StringSliceVar so duplicate flags are visible to collapseSingletonRef
	// rather than silently last-winning under cobra's StringVar.
	cmd.Flags().Var(newRefSliceValue(&parentRefSlice), "parent",
		"set parent to <ref> (replaces existing; ≤1; <ref> must finish before this issue starts; "+issueRefHelp+")")
	cmd.Flags().Var(newRefSliceValue(&blocks), "blocks",
		"this issue blocks <ref> (this must finish before <ref> can; repeatable; "+issueRefHelp+")")
	cmd.Flags().Var(newRefSliceValue(&blockedBy), "blocked-by",
		"this issue is blocked by <ref> (<ref> must finish before this; repeatable; "+issueRefHelp+")")
	cmd.Flags().Var(newRefSliceValue(&related), "related",
		"this issue is related to <ref> (symmetric, no ordering; repeatable; "+issueRefHelp+")")
	cmd.Flags().Var(newRefSliceValue(&removeParentRefSlice), "remove-parent",
		"remove parent (strict: ref must equal the current parent or 409; "+issueRefHelp+")")
	cmd.Flags().Var(newRefSliceValue(&removeBlocks), "remove-blocks",
		"remove blocks→<ref> (idempotent: no error if no such link or <ref> is missing; repeatable; "+issueRefHelp+")")
	cmd.Flags().Var(newRefSliceValue(&removeBlockedBy), "remove-blocked-by",
		"remove blocked-by←<ref> (idempotent; repeatable; "+issueRefHelp+")")
	cmd.Flags().Var(newRefSliceValue(&removeRelated), "remove-related",
		"remove related↔<ref> (idempotent; repeatable; "+issueRefHelp+")")
	addCommentFlag(cmd)

	// RunE is set after flag registration so we can reference cmd.Flags().Changed.
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		comment, err := commentFromFlag(cmd)
		if err != nil {
			return err
		}
		payload := map[string]any{}
		if cmd.Flags().Changed("title") {
			if strings.TrimSpace(title) == "" {
				return &cliError{
					Message:  "--title must not be empty (omit the flag to keep the existing title)",
					Kind:     kindValidation,
					ExitCode: ExitValidation,
				}
			}
			payload["title"] = title
		}
		if cmd.Flags().Changed("body") {
			payload["body"] = body
		}
		if cmd.Flags().Changed("owner") {
			payload["owner"] = owner
		}

		if cmd.Flags().Changed("priority") {
			v, cleared, err := parseEditPriority(priority)
			if err != nil {
				return err
			}
			if cleared {
				payload["clear_priority"] = true
			} else {
				payload["set_priority"] = *v
			}
		}

		// Resolve the URL issue early so we have ctx/baseURL/pid available
		// to resolve link-target refs (short_id, qualified short_id, or ULID).
		ctx, baseURL, pid, issue, err := resolveIssueRefForCommand(cmd, args[0])
		if err != nil {
			return err
		}

		// --parent and --remove-parent are at-most-one but accept any of
		// short_id, qualified, or ULID. resolveSingletonRefToWire rejects
		// only when distinct refs resolve to *different* issues, so
		// equivalent forms (e.g. `--parent abc4 --parent kata#abc4`) succeed.
		// issue.ProjectName is the URL issue's canonical project — link
		// targets must belong to that same project (cross-project refs
		// require the ULID form, not `<other>#abc4`).
		var parentRef, removeParentRef string
		if cmd.Flags().Changed("parent") {
			parentRef, err = resolveSingletonRefToWire(ctx, baseURL, issue.ProjectName, pid, parentRefSlice, "--parent", false)
			if err != nil {
				return err
			}
		}
		if cmd.Flags().Changed("remove-parent") {
			removeParentRef, err = resolveSingletonRefToWire(ctx, baseURL, issue.ProjectName, pid, removeParentRefSlice, "--remove-parent", true)
			if err != nil {
				return err
			}
		}
		linksDelta, err := buildLinksDelta(ctx, cmd, baseURL, issue.ProjectName, pid,
			parentRef, blocks, blockedBy, related,
			removeParentRef, removeBlocks, removeBlockedBy, removeRelated)
		if err != nil {
			return err
		}
		if linksDelta != nil {
			payload["links_delta"] = linksDelta
		}

		// Pure idempotent-remove of refs that resolved to "no such issue"
		// would arrive here with payload == {} (no field flags, no link
		// delta). Treat that as a successful no-op locally rather than
		// sending an empty PATCH the daemon would reject. Examples:
		//   kata edit 1 --remove-blocks <missing-uid-prefix>
		//   kata edit 1 --remove-related 99
		// Both are documented as idempotent; producing a synthetic
		// no-op response keeps that contract for non-numeric refs too.
		// Numeric refs that resolved to nothing also benefit (they
		// previously short-circuited at the daemon).
		anyLinkFlagSet := cmd.Flags().Changed("parent") || cmd.Flags().Changed("blocks") ||
			cmd.Flags().Changed("blocked-by") || cmd.Flags().Changed("related") ||
			cmd.Flags().Changed("remove-parent") || cmd.Flags().Changed("remove-blocks") ||
			cmd.Flags().Changed("remove-blocked-by") || cmd.Flags().Changed("remove-related")
		if len(payload) == 0 && anyLinkFlagSet {
			// Fetch the issue so the "no changes applied" line can echo
			// the issue's title/status — printMutation reads those off
			// the response body. One extra GET avoids a wasted PATCH the
			// daemon would 400 on as an empty mutation.
			client, err := httpClientFor(ctx, baseURL)
			if err != nil {
				return err
			}
			_, bs, err := httpDoJSON(ctx, client, http.MethodGet,
				fmt.Sprintf("%s/api/v1/projects/%d/issues/%s", baseURL, pid, url.PathEscape(issue.RefForAPI)),
				nil)
			if err != nil {
				return err
			}
			actor, _ := resolveActor(flags.As, nil)
			if err := postFollowupComment(ctx, client, baseURL, pid, issue.RefForAPI, actor, comment); err != nil {
				return err
			}
			return printMutation(cmd, syntheticNoopFromShow(bs))
		}

		// At least one mutation must be present, mirroring the daemon's check
		// but surfaced client-side so an empty edit doesn't waste a roundtrip.
		// `actor` is added below and doesn't count toward "real" mutations.
		hasMutation := len(payload) > 0
		if !hasMutation {
			return &cliError{
				Message: "pass at least one of --title, --body, --owner, --priority, " +
					"--parent, --blocks, --blocked-by, --related, " +
					"--remove-parent, --remove-blocks, --remove-blocked-by, --remove-related",
				Kind:     kindValidation,
				ExitCode: ExitValidation,
			}
		}
		actor, _ := resolveActor(flags.As, nil)
		payload["actor"] = actor
		client, err := httpClientFor(ctx, baseURL)
		if err != nil {
			return err
		}
		status, bs, err := httpDoJSON(ctx, client, http.MethodPatch,
			fmt.Sprintf("%s/api/v1/projects/%d/issues/%s", baseURL, pid, url.PathEscape(issue.RefForAPI)),
			payload)
		if err != nil {
			return err
		}
		if status >= 400 {
			return apiErrFromBody(status, bs)
		}
		if err := postFollowupComment(ctx, client, baseURL, pid, issue.RefForAPI, actor, comment); err != nil {
			return err
		}
		return printMutation(cmd, bs)
	}
	return cmd
}

// buildLinksDelta translates the edit command's link flags into a wire-format
// links_delta map. Returns nil when no link flag was passed. Resolves every
// ref (short_id, qualified, or ULID) to its wire ref string before building
// the payload, then runs client-side conflict checks so an obviously-broken
// delta never reaches the daemon.
//
// currentProject is the canonical name of the URL issue's project. Every
// link target must belong to that project; a qualified `<other>#abc4` ref
// is rejected up front so it can't silently target the wrong issue.
func buildLinksDelta(
	ctx context.Context,
	cmd *cobra.Command,
	baseURL, currentProject string, projectID int64,
	parentRef string,
	blocks, blockedBy, related []string,
	removeParentRef string,
	removeBlocks, removeBlockedBy, removeRelated []string,
) (map[string]any, error) {
	parentSet := cmd.Flags().Changed("parent")
	parentRm := cmd.Flags().Changed("remove-parent")
	if !parentSet && !parentRm &&
		len(blocks) == 0 && len(blockedBy) == 0 && len(related) == 0 &&
		len(removeBlocks) == 0 && len(removeBlockedBy) == 0 && len(removeRelated) == 0 {
		return nil, nil
	}
	if parentSet && parentRm {
		return nil, &cliError{
			Message:  "--parent and --remove-parent cannot be used in the same call",
			Kind:     kindValidation,
			ExitCode: ExitValidation,
		}
	}

	// parentRef / removeParentRef arrived already resolved from the
	// at-most-one collapse helper. Multi-valued flags resolve here
	// (each entry independently). Errors short-circuit the whole edit
	// so a malformed ref never lands a partial mutation.
	var (
		blocksRefs, blockedByRefs, relatedRefs                   []string
		removeBlocksRefs, removeBlockedByRefs, removeRelatedRefs []string
		err                                                      error
	)
	if blocksRefs, err = resolveRefSliceToWire(ctx, baseURL, currentProject, projectID, blocks, "--blocks"); err != nil {
		return nil, err
	}
	if blockedByRefs, err = resolveRefSliceToWire(ctx, baseURL, currentProject, projectID, blockedBy, "--blocked-by"); err != nil {
		return nil, err
	}
	if relatedRefs, err = resolveRefSliceToWire(ctx, baseURL, currentProject, projectID, related, "--related"); err != nil {
		return nil, err
	}
	// Remove flags are idempotent at the contract level: removing a link
	// that doesn't exist is a no-op. The daemon's resolver tolerates
	// soft-deleted peers (the link row is real); idempotent remove of a
	// completely-missing peer is handled daemon-side too.
	if removeBlocksRefs, err = resolveRefSliceToWireIdempotentRemove(ctx, baseURL, currentProject, projectID, removeBlocks, "--remove-blocks"); err != nil {
		return nil, err
	}
	if removeBlockedByRefs, err = resolveRefSliceToWireIdempotentRemove(ctx, baseURL, currentProject, projectID, removeBlockedBy, "--remove-blocked-by"); err != nil {
		return nil, err
	}
	if removeRelatedRefs, err = resolveRefSliceToWireIdempotentRemove(ctx, baseURL, currentProject, projectID, removeRelated, "--remove-related"); err != nil {
		return nil, err
	}

	// Conflict checks compare canonical UIDs so equivalent forms of the
	// same ref (short_id vs ULID, qualified vs bare) collide even when
	// the user spells them differently. Each pair is canonicalized only
	// when both sides are non-empty, so the common case (one of the two
	// flags set) skips the extra daemon roundtrips entirely.
	if conflict, err := firstResolvedOverlap(ctx, baseURL, projectID,
		blocksRefs, removeBlocksRefs); err != nil {
		return nil, err
	} else if conflict != "" {
		return nil, &cliError{
			Message:  fmt.Sprintf("--blocks and --remove-blocks both target %s", conflict),
			Kind:     kindValidation,
			ExitCode: ExitValidation,
		}
	}
	if conflict, err := firstResolvedOverlap(ctx, baseURL, projectID,
		blockedByRefs, removeBlockedByRefs); err != nil {
		return nil, err
	} else if conflict != "" {
		return nil, &cliError{
			Message:  fmt.Sprintf("--blocked-by and --remove-blocked-by both target %s", conflict),
			Kind:     kindValidation,
			ExitCode: ExitValidation,
		}
	}
	if conflict, err := firstResolvedOverlap(ctx, baseURL, projectID,
		relatedRefs, removeRelatedRefs); err != nil {
		return nil, err
	} else if conflict != "" {
		return nil, &cliError{
			Message:  fmt.Sprintf("--related and --remove-related both target %s", conflict),
			Kind:     kindValidation,
			ExitCode: ExitValidation,
		}
	}

	delta := map[string]any{}
	if parentSet {
		delta["set_parent"] = parentRef
	}
	if parentRm {
		delta["remove_parent"] = removeParentRef
	}
	if len(blocksRefs) > 0 {
		delta["add_blocks"] = blocksRefs
	}
	if len(blockedByRefs) > 0 {
		delta["add_blocked_by"] = blockedByRefs
	}
	if len(relatedRefs) > 0 {
		delta["add_related"] = relatedRefs
	}
	if len(removeBlocksRefs) > 0 {
		delta["remove_blocks"] = removeBlocksRefs
	}
	if len(removeBlockedByRefs) > 0 {
		delta["remove_blocked_by"] = removeBlockedByRefs
	}
	if len(removeRelatedRefs) > 0 {
		delta["remove_related"] = removeRelatedRefs
	}
	if len(delta) == 0 {
		return nil, nil
	}
	return delta, nil
}

// syntheticNoopFromShow extracts the issue subobject from a show
// response body and wraps it in a MutationResponse-shaped envelope
// with changed=false plus an empty `changes` block. summarizeChanges
// requires the changes key to be present-but-empty AND changed=false
// to render the "(no changes applied)" tail; absent changes prints
// no tail at all, which would look identical to a normal field edit.
// Used when every requested link mutation resolved to a no-op locally
// (idempotent --remove-* against missing peers) so we honor the
// idempotent contract without sending an empty PATCH the daemon
// would reject.
func syntheticNoopFromShow(showBody []byte) []byte {
	var src struct {
		Issue json.RawMessage `json:"issue"`
	}
	_ = json.Unmarshal(showBody, &src)
	resp := map[string]any{
		"issue":   src.Issue,
		"changed": false,
		"changes": map[string]any{},
	}
	bs, _ := json.Marshal(resp)
	return bs
}

// firstResolvedOverlap canonicalizes every ref in adds and removes to its
// issue's UID (via a daemon GET) and returns the first ref in `removes`
// whose canonical UID also appears in `adds`. Used by buildLinksDelta to
// catch contradictory delta pairs like `--blocks abc4 --remove-blocks
// 01HZ…` where the ULID resolves to abc4 — string-equality alone would
// miss the conflict and let an obviously contradictory mutation reach
// the daemon.
//
// Refs that fail to resolve (issue doesn't exist) are skipped; the
// underlying remove flags are documented as idempotent, and a typo on
// the add side would surface through the daemon's own validation. The
// canonical wire string (passed back from the add side) is returned as
// the conflict label so error messages match what the user typed.
func firstResolvedOverlap(ctx context.Context, baseURL string, projectID int64, adds, removes []string) (string, error) {
	if len(adds) == 0 || len(removes) == 0 {
		return "", nil
	}
	addUIDs, err := refsToUIDs(ctx, baseURL, projectID, adds)
	if err != nil {
		return "", err
	}
	removeUIDs, err := refsToUIDs(ctx, baseURL, projectID, removes)
	if err != nil {
		return "", err
	}
	if len(addUIDs) == 0 || len(removeUIDs) == 0 {
		return "", nil
	}
	have := make(map[string]string, len(addUIDs))
	for ref, uid := range addUIDs {
		if uid != "" {
			have[uid] = ref
		}
	}
	for ref, uid := range removeUIDs {
		if uid == "" {
			continue
		}
		if _, ok := have[uid]; ok {
			return ref, nil
		}
	}
	return "", nil
}

// refsToUIDs resolves every ref to its issue's UID via a daemon GET.
// Refs that fail to resolve (404, soft-deleted under the default
// include filter) map to an empty string so callers can treat them as
// "could not canonicalize, skip from conflict checks". Network errors
// (other than not-found) propagate so the caller doesn't silently
// proceed with an inconsistent view.
func refsToUIDs(ctx context.Context, baseURL string, projectID int64, refs []string) (map[string]string, error) {
	out := make(map[string]string, len(refs))
	if len(refs) == 0 {
		return out, nil
	}
	client, err := httpClientFor(ctx, baseURL)
	if err != nil {
		return nil, err
	}
	for _, ref := range refs {
		if _, seen := out[ref]; seen {
			continue
		}
		path := fmt.Sprintf("%s/api/v1/projects/%d/issues/%s", baseURL, projectID, url.PathEscape(ref))
		status, bs, err := httpDoJSON(ctx, client, http.MethodGet, path, nil)
		if err != nil {
			return nil, err
		}
		if status == http.StatusNotFound {
			out[ref] = ""
			continue
		}
		if status >= 400 {
			return nil, apiErrFromBody(status, bs)
		}
		var body struct {
			Issue struct {
				UID string `json:"uid"`
			} `json:"issue"`
		}
		if err := json.Unmarshal(bs, &body); err != nil {
			return nil, err
		}
		out[ref] = body.Issue.UID
	}
	return out, nil
}

// parseEditPriority interprets the --priority value: "-" clears, an integer
// 0..4 sets. Returns (value, cleared, err).
func parseEditPriority(raw string) (*int64, bool, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "-" {
		return nil, true, nil
	}
	n, err := strconv.ParseInt(trimmed, 10, 64)
	if err != nil {
		return nil, false, &cliError{
			Message:  "--priority must be an integer 0..4 or '-' to clear",
			Kind:     kindValidation,
			ExitCode: ExitValidation,
		}
	}
	if n < 0 || n > 4 {
		return nil, false, &cliError{
			Message:  "--priority must be between 0 and 4",
			Kind:     kindValidation,
			ExitCode: ExitValidation,
		}
	}
	return &n, false, nil
}
