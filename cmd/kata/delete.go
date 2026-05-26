package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

// newDeleteCmd returns the cobra.Command for `kata delete`.
//
// Spec §3.5 / §4.4: deletion is gated by --force and an X-Kata-Confirm header
// whose value is the exact string "DELETE <qualified>". The CLI accepts the
// header value via --confirm (noninteractive) or builds it from a TTY prompt
// where the user types the short_id.
func newDeleteCmd() *cobra.Command {
	var force bool
	var confirm string
	cmd := &cobra.Command{
		Use:   "delete <issue-ref>",
		Short: "soft-delete an issue (reversible via kata restore)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !force {
				return &cliError{
					Message: "deletion requires --force; use `kata restore` to undo if you change your mind",
					Code:    "validation",
					Kind:    kindValidation, ExitCode: ExitValidation,
				}
			}
			ctx, baseURL, pid, issue, err := resolveIssueRefForCommand(cmd, args[0])
			if err != nil {
				return err
			}
			// Resolve the issue's display short_id so the X-Kata-Confirm
			// header carries the daemon-expected "<project>#<short_id>"
			// form even when the user passed a ULID.
			issue, err = hydrateRefWithQualified(ctx, baseURL, pid, issue, false)
			if err != nil {
				return err
			}
			expected := fmt.Sprintf("DELETE %s", issue.QualifiedID)
			confirm, err = resolveConfirm(cmd, confirm, expected,
				fmt.Sprintf("Type %q to confirm: ", expected), confirmPromptFull)
			if err != nil {
				return err
			}
			return runDestructive(cmd, baseURL, pid, issue.RefForAPI, issue.QualifiedID, "delete", confirm, nil)
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "required to perform the soft delete")
	cmd.Flags().StringVar(&confirm, "confirm", "", `exact confirmation string ("DELETE <short_id>")`)
	return cmd
}

// confirmMatcher decides whether the user's TTY input satisfies the prompt.
// Both delete and purge now use confirmPromptFull (the full
// "VERB <project>#<short_id>" string) so the X-Kata-Confirm header — which
// must match that exact form — works whether the user typed in interactively
// or passed it noninteractively via --confirm.
type confirmMatcher func(line, expected string) bool

// confirmPromptFull accepts only the exact expected string.
func confirmPromptFull(line, expected string) bool {
	return line == expected
}

// resolveConfirm returns the X-Kata-Confirm value the daemon expects:
//   - if --confirm was passed, use it as-is (the daemon validates exact match);
//   - otherwise, if stdin is a TTY, prompt with `prompt` and accept input that
//     `match` says satisfies the verb's friction rule;
//   - otherwise, exit 6 confirm_required.
func resolveConfirm(cmd *cobra.Command, flagVal, expected, prompt string,
	match confirmMatcher) (string, error) {
	if flagVal != "" {
		return flagVal, nil
	}
	if !isTTY(os.Stdin) {
		return "", &cliError{
			Message: "no TTY: pass --confirm \"" + expected + "\" to proceed noninteractively",
			Code:    "confirm_required",
			Kind:    kindConfirm, ExitCode: ExitConfirm,
		}
	}
	if _, err := fmt.Fprint(cmd.ErrOrStderr(), prompt); err != nil {
		return "", err
	}
	r := bufio.NewReader(cmd.InOrStdin())
	//nolint:errcheck // ReadString returns the data read up to EOF; an EOF here
	// just means the user closed stdin, which we treat as an empty mismatch.
	line, _ := r.ReadString('\n')
	line = strings.TrimSpace(line)
	if !match(line, expected) {
		return "", &cliError{
			Message: "confirmation input did not match",
			Code:    "confirm_mismatch",
			Kind:    kindConfirm, ExitCode: ExitConfirm,
		}
	}
	return expected, nil
}

// runDestructive POSTs to /actions/{verb} with the X-Kata-Confirm header.
// pathRef is the literal {ref} URL path component; displayRef is the
// qualified "<project>#<short_id>" rendered in human-mode success lines
// so a cross-workspace `kata delete other#abc4` prints an undo hint
// (`kata restore other#abc4`) that resolves against the right project
// regardless of the caller's workspace binding.
//
// pid and baseURL come from the caller's resolveIssueRefForCommand chain so
// a qualified ref (`other#abc4` from a workspace bound to a different
// project) targets the project the ref names rather than the workspace's
// project.
func runDestructive(cmd *cobra.Command, baseURL string, pid int64, pathRef, displayRef, verb, confirm string,
	extraBody map[string]any) error {
	ctx := cmd.Context()
	actor, _ := resolveActor(flags.As, nil)
	// Build body from extraBody first so a future caller can't overwrite the
	// resolved actor with a stray map key.
	body := map[string]any{}
	for k, v := range extraBody {
		body[k] = v
	}
	body["actor"] = actor
	client, err := httpClientFor(ctx, baseURL)
	if err != nil {
		return err
	}
	postURL := fmt.Sprintf("%s/api/v1/projects/%d/issues/%s/actions/%s", baseURL, pid, url.PathEscape(pathRef), verb)
	status, bs, err := httpDoJSONWithHeader(ctx, client, http.MethodPost, postURL,
		map[string]string{"X-Kata-Confirm": confirm}, body)
	if err != nil {
		return err
	}
	if status >= 400 {
		return apiErrFromBody(status, bs)
	}
	return printDestructive(cmd, displayRef, verb, bs)
}

// printDestructive renders the destructive-action response in the active
// output mode (JSON envelope, quiet, or one-line human).
func printDestructive(cmd *cobra.Command, ref, verb string, bs []byte) error {
	mode := currentOutputMode()
	if mode == outputJSON {
		var buf bytes.Buffer
		if err := emitJSON(&buf, json.RawMessage(bs)); err != nil {
			return err
		}
		_, err := fmt.Fprint(cmd.OutOrStdout(), buf.String())
		return err
	}
	if mode == outputAgent {
		if verb == "delete" {
			var m agentIssueMutation
			if err := json.Unmarshal(bs, &m); err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if !flags.Quiet {
				if _, err := fmt.Fprintf(out, "OK delete %s", m.Issue.ShortID); err != nil {
					return err
				}
				if !m.Changed {
					if _, err := fmt.Fprint(out, " changed=false"); err != nil {
						return err
					}
				}
				if _, err := fmt.Fprintln(out); err != nil {
					return err
				}
			}
			if m.Issue.ShortID != "" && m.Issue.Title != "" {
				if _, err := fmt.Fprintf(out, "Issue: %s %s\n", m.Issue.ShortID, agentValue(m.Issue.Title)); err != nil {
					return err
				}
			}
			if err := writeAgentField(out, "Status", "deleted"); err != nil {
				return err
			}
			if err := writeAgentField(out, "Deleted", "true"); err != nil {
				return err
			}
			return writeAgentField(out, "Undo", "kata restore "+shellQuoteArg(ref)+" --agent")
		}
		if verb == "purge" {
			var b struct {
				PurgeLog struct {
					ShortID    *string `json:"short_id"`
					IssueTitle string  `json:"issue_title"`
				} `json:"purge_log"`
			}
			if err := json.Unmarshal(bs, &b); err != nil {
				return err
			}
			shortID := ref
			if b.PurgeLog.ShortID != nil && *b.PurgeLog.ShortID != "" {
				shortID = *b.PurgeLog.ShortID
			}
			if !flags.Quiet {
				if _, err := fmt.Fprintf(cmd.OutOrStdout(), "OK purge %s\n", shortID); err != nil {
					return err
				}
			}
			if b.PurgeLog.IssueTitle != "" {
				if _, err := fmt.Fprintf(cmd.OutOrStdout(), "Issue: %s %s\n", shortID, agentValue(b.PurgeLog.IssueTitle)); err != nil {
					return err
				}
			}
			return writeAgentField(cmd.OutOrStdout(), "Status", "purged")
		}
	}
	if flags.Quiet {
		return nil
	}
	switch verb {
	case "delete":
		_, err := fmt.Fprintf(cmd.OutOrStdout(),
			"%s deleted (use `kata restore %s` to undo)\n", ref, ref)
		return err
	case "purge":
		_, err := fmt.Fprintf(cmd.OutOrStdout(), "%s purged (irreversible)\n", ref)
		return err
	}
	return nil
}

func shellQuoteArg(s string) string {
	if isShellSafeArg(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

func isShellSafeArg(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case strings.ContainsRune("@%_+=:,./-", r):
		case r == '#' && i > 0:
		default:
			return false
		}
	}
	return true
}

// httpDoJSONWithHeader mirrors httpDoJSON but lets callers attach extra
// request headers (notably X-Kata-Confirm). Defined here so delete and the
// upcoming purge command don't have to extend the helpers.go signature.
func httpDoJSONWithHeader(ctx context.Context, client *http.Client,
	method, url string, headers map[string]string, body any) (int, []byte, error) {
	var rdr io.Reader
	if body != nil {
		bs, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		rdr = bytes.NewReader(bs)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rdr)
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req) //nolint:gosec // G107: daemon-local URL controlled by ensureDaemon.
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, err
	}
	return resp.StatusCode, out, nil
}
