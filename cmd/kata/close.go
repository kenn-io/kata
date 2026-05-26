package main

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/spf13/cobra"

	"go.kenn.io/kata/internal/api"
)

func newCloseCmd() *cobra.Command {
	var (
		reason   string
		message  string
		evidence []string
		dryRun   bool

		sugarDone          bool
		sugarWontfix       bool
		sugarAuditNoChange bool
		sugarDuplicateOf   string
		sugarSupersededBy  string
		sugarCommit        string
		sugarPR            string
		sugarTest          string
		sugarReviewed      []string
	)
	cmd := &cobra.Command{
		Use:   "close <issue-ref>",
		Short: "close an issue (asserts the work is complete)",
		Long: `Closing an issue asserts that the work it describes is complete.
This is a stronger claim than a comment. Provide evidence and a
substantive message.

Close each issue as soon as its work is verified, not in a batch at
the end. The daemon throttles >3 sibling closes by one actor under
one parent in 5 minutes (operators can disable this via
[close.throttle] enabled = false in <KATA_HOME>/config.toml), so a
bulk "close everything now" pass will trip the guard.

If you have not completed and tested this work, do not close it.
Instead, label and comment:
    kata edit <ref> --label needs-review
    kata comment <ref> --body "what was attempted, what remains"`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// Resolve sugar -> reason (with conflict checks). Multiple sugar
			// flags are mutually exclusive: a sequential switch would silently
			// keep the first match and drop the rest (`--done --wontfix` would
			// close as done without flagging the conflict).
			sugarSet := []string{}
			if sugarDone {
				sugarSet = append(sugarSet, "done")
			}
			if sugarWontfix {
				sugarSet = append(sugarSet, "wontfix")
			}
			if sugarAuditNoChange {
				sugarSet = append(sugarSet, "audit-no-change")
			}
			if sugarDuplicateOf != "" {
				sugarSet = append(sugarSet, "duplicate")
			}
			if sugarSupersededBy != "" {
				sugarSet = append(sugarSet, "superseded")
			}
			if len(sugarSet) > 1 {
				return fmt.Errorf("flag conflict: multiple reason sugar flags set: %s",
					strings.Join(sugarSet, ", "))
			}
			sugarReason := ""
			if len(sugarSet) == 1 {
				sugarReason = sugarSet[0]
			}
			if sugarReason != "" && reason != "" {
				return fmt.Errorf("flag conflict: --reason=%s and corresponding sugar flag both set", reason)
			}
			if sugarReason != "" {
				reason = sugarReason
			}
			if reason == "" {
				return fmt.Errorf("--reason is required (one of: done, wontfix, duplicate, superseded, audit-no-change)")
			}

			// Resolve sugar -> evidence (appended to user-supplied --evidence values).
			if sugarCommit != "" {
				evidence = append(evidence, "commit:"+sugarCommit)
			}
			if sugarPR != "" {
				evidence = append(evidence, "pr:"+sugarPR)
			}
			if sugarTest != "" {
				evidence = append(evidence, "test:"+sugarTest)
			}
			for _, p := range sugarReviewed {
				evidence = append(evidence, "reviewed-paths:"+p)
			}
			if sugarDuplicateOf != "" {
				evidence = append(evidence, "duplicate-of:"+sugarDuplicateOf)
			}
			if sugarSupersededBy != "" {
				evidence = append(evidence, "superseded-by:"+sugarSupersededBy)
			}

			parsed, err := parseEvidenceFlags(evidence)
			if err != nil {
				return err
			}
			if dup := findDuplicateEvidence(parsed); dup != "" {
				return fmt.Errorf("flag conflict: duplicate evidence item %s (provided via both canonical and sugar)", dup)
			}

			extra := map[string]any{
				"reason":   reason,
				"message":  message,
				"evidence": parsed,
				"dry_run":  dryRun,
			}
			// Route the dry-run banner to stderr only in human mode so
			// machine-parseable output modes stay unprefixed.
			if dryRun && currentOutputMode() == outputHuman && !flags.Quiet {
				_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "close: dry-run (no mutations will occur)")
			}
			return runAction(cmd, args[0], "close", extra)
		},
	}
	cmd.Flags().StringVar(&reason, "reason", "",
		"one of: done, wontfix, duplicate, superseded, audit-no-change")
	cmd.Flags().StringVar(&message, "message", "",
		"substantive message describing scope and verification")
	// StringArrayVar (not StringSliceVar) so commas inside a single
	// --evidence value survive intact: no-change-audit:<text> and
	// test:<cmd> often carry prose or shell snippets with commas, and
	// StringSliceVar would shred them into broken sub-items.
	cmd.Flags().StringArrayVar(&evidence, "evidence", nil,
		"typed evidence, repeatable: commit:<sha>, pr:<url>, test:<cmd>, "+
			"reviewed-paths:<path>, no-change-audit:<text>, duplicate-of:<N>, superseded-by:<N>")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false,
		"validate without mutating; reports the would-be close event")

	cmd.Flags().BoolVar(&sugarDone, "done", false, "sugar for --reason done")
	cmd.Flags().BoolVar(&sugarWontfix, "wontfix", false, "sugar for --reason wontfix")
	cmd.Flags().BoolVar(&sugarAuditNoChange, "audit-no-change", false, "sugar for --reason audit-no-change")
	cmd.Flags().StringVar(&sugarDuplicateOf, "duplicate-of", "", "sugar for --reason duplicate --evidence duplicate-of:<ref>")
	cmd.Flags().StringVar(&sugarSupersededBy, "superseded-by", "", "sugar for --reason superseded --evidence superseded-by:<ref>")
	cmd.Flags().StringVar(&sugarCommit, "commit", "", "sugar for --evidence commit:<sha>")
	cmd.Flags().StringVar(&sugarPR, "pr", "", "sugar for --evidence pr:<url>")
	cmd.Flags().StringVar(&sugarTest, "test", "", "sugar for --evidence test:<command>")
	cmd.Flags().StringArrayVar(&sugarReviewed, "reviewed", nil, "sugar for --evidence reviewed-paths:<path>, repeatable")
	addCommentFlag(cmd)
	return cmd
}

// parseEvidenceFlags turns CLI strings like "commit:abc1234" into the wire
// shape expected by the daemon. reviewed-paths repeats are merged into a
// single evidence item with a paths array, per spec §3.3.
func parseEvidenceFlags(raw []string) ([]api.Evidence, error) {
	var out []api.Evidence
	var reviewedPaths []string
	seenReviewedPath := map[string]struct{}{}
	for _, s := range raw {
		colon := strings.Index(s, ":")
		if colon < 0 {
			return nil, fmt.Errorf("evidence %q: expected <type>:<value>", s)
		}
		kind, value := api.EvidenceType(s[:colon]), s[colon+1:]
		switch kind {
		case api.EvidenceCommit:
			out = append(out, api.Evidence{Type: kind, SHA: value})
		case api.EvidencePR:
			out = append(out, api.Evidence{Type: kind, URL: value})
		case api.EvidenceTest:
			out = append(out, api.Evidence{Type: kind, Command: value})
		case api.EvidenceReviewedPaths:
			if _, dup := seenReviewedPath[value]; dup {
				return nil, fmt.Errorf("evidence reviewed-paths:%s: duplicate path (provided more than once via canonical and/or sugar)", value)
			}
			seenReviewedPath[value] = struct{}{}
			reviewedPaths = append(reviewedPaths, value)
		case api.EvidenceNoChangeAudit:
			out = append(out, api.Evidence{Type: kind, Rationale: value})
		case api.EvidenceDuplicateOf:
			if value == "" {
				return nil, fmt.Errorf("evidence duplicate-of: expected issue ref, got empty value")
			}
			out = append(out, api.Evidence{Type: kind, IssueRef: value})
		case api.EvidenceSupersededBy:
			if value == "" {
				return nil, fmt.Errorf("evidence superseded-by: expected issue ref, got empty value")
			}
			out = append(out, api.Evidence{Type: kind, IssueRef: value})
		default:
			return nil, fmt.Errorf("evidence %q: unknown type %q", s, kind)
		}
	}
	if len(reviewedPaths) > 0 {
		out = append(out, api.Evidence{Type: api.EvidenceReviewedPaths, Paths: reviewedPaths})
	}
	return out, nil
}

// runAction is shared by close and reopen. It resolves the issue reference,
// resolves the project, merges extra fields into the request body, and calls
// the daemon action endpoint. If --comment was passed on the command, the
// comment is appended in a separate POST after the action succeeds.
func runAction(cmd *cobra.Command, raw, action string, extra map[string]any) error {
	comment, err := commentFromFlag(cmd)
	if err != nil {
		return err
	}
	ctx, baseURL, pid, issue, err := resolveIssueRefForCommand(cmd, raw)
	if err != nil {
		return err
	}
	actor, _ := resolveActor(flags.As, nil)
	body := map[string]any{"actor": actor}
	for k, v := range extra {
		body[k] = v
	}
	client, err := httpClientFor(ctx, baseURL)
	if err != nil {
		return err
	}
	status, bs, err := httpDoJSON(ctx, client, http.MethodPost,
		fmt.Sprintf("%s/api/v1/projects/%d/issues/%s/actions/%s", baseURL, pid, url.PathEscape(issue.RefForAPI), action),
		body)
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

// findDuplicateEvidence returns the first duplicate "type:value" pair, or
// "" if none. Detects user-error like `--duplicate-of 7 --evidence duplicate-of:7`.
func findDuplicateEvidence(items []api.Evidence) string {
	seen := map[string]struct{}{}
	for _, e := range items {
		key := fmt.Sprintf("%s:%v", e.Type, evidencePayloadKey(e))
		if _, dup := seen[key]; dup {
			return key
		}
		seen[key] = struct{}{}
	}
	return ""
}

func evidencePayloadKey(e api.Evidence) string {
	switch e.Type {
	case api.EvidenceCommit:
		return e.SHA
	case api.EvidencePR:
		return e.URL
	case api.EvidenceTest:
		return e.Command
	case api.EvidenceNoChangeAudit:
		return e.Rationale
	case api.EvidenceDuplicateOf, api.EvidenceSupersededBy:
		return e.IssueRef
	case api.EvidenceReviewedPaths:
		return strings.Join(e.Paths, ",")
	}
	return ""
}
