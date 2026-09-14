package main

import (
	"bytes"
	"fmt"

	"github.com/spf13/cobra"
)

const agentQuickstartText = `# kata agent quickstart

Use kata as the shared issue ledger for this workspace.

1. Run from the workspace (--workspace overrides; --project picks
   another). Author = $KATA_AUTHOR > $USER > git user.name.
   If uninitialized, report that kata init is needed.
   Issue refs are short_ids derived from each issue's ULID (e.g. abc4).
   Cross-project: kata#abc4. Full 26-char ULIDs also resolve. Legacy
   numeric refs (12, kata#12) no longer work.

2. Closing an issue asserts that the work is complete. If the work is
   not done, DO NOT close. Instead:

      kata label add <ref> needs-review
      kata comment <ref> --body "what was attempted, what remains"

   When done, close with substantive prose and typed --evidence:

      kata close abc4 --done \
        --message "Fixed Safari callback double-submit; verified tests pass." \
        --commit <sha>

   Close each issue as soon as its work is verified, not in a batch or a
   single "close everything" pass at the end. By default the daemon permits
   sibling close bursts when each close has valid evidence and a substantive
   message. Operators can enable stricter burst/prose throttling via
   [close.throttle] enabled = true in <KATA_HOME>/config.toml.

   Other close forms:

      kata close abc4 --duplicate-of d4ex  --message "Same Safari race condition."
      kata close abc4 --superseded-by d4ex --message "Replaced by broader scope."
      kata close abc4 --wontfix --message "<>=60 chars of rationale>"
      kata close abc4 --audit-no-change \
                      --message "Reviewed schema and queries; no change needed." \
                      --evidence "no-change-audit:schema unchanged after review" \
                      --reviewed internal/db/schema.sql

   The daemon refuses parent-close while open children remain. Reviewers
   can replay activity with kata audit closes and undo a specific lazy
   close with kata reopen <ref>.

   Default to --agent for ordinary kata reads and mutations in agent logs.
   Use --json only when your script needs complete structured data, for
   example when piping into jq.

   Use kata for real repository work only.
   Do not create practice, tutorial, example, or scratchpad issues unless the user explicitly asks for them.
   If the work belongs to another repository or project, use that project.

3. Search before creating:

   kata search "login race" --agent
   kata search --project foo "login race" --agent

4. If no existing issue fits, create with an idempotency key:

   kata create "fix login race" \
     --body "Observed double-submit in Safari callback." \
     --idempotency-key "login-race-2026-05-02" \
     --agent

   kata create --project foo "fix login race" \
     --body "Observed double-submit in Safari callback." \
     --idempotency-key "foo-login-race-2026-05-02" \
     --agent

5. Prefer updating existing issues over creating duplicates:

   kata show abc4 --agent
   kata show --project foo abc4 --agent
   kata comment abc4 --body "Found another reproduction path." --agent
   kata label add abc4 safari --agent
   kata edit abc4 --blocks d4ex --agent

6. Find and claim available work (multi-agent environments):

   # Choose one unclaimed issue
   kata next --unowned --agent

   # Inspect the filtered queue
   kata ready --unowned --label bug --no-label blocked --agent

   # Claim it (fails if already claimed by another actor)
   kata claim <ref>

   # Release ownership
   kata unassign <ref>

7. Use native planning dates deliberately:

   # Park until a date or time; a future value excludes the issue from ready/next.
   # This is the first-class form of setting or clearing scheduled_on.
   kata schedule <ref> <date-or-time>
   kata schedule <ref> -

   # Set or clear deadline_on. A deadline does not park the issue.
   kata deadline <ref> <date-or-time>
   kata deadline <ref> -

   # Park with no date. Remove the marker to return; do not store false.
   kata meta set <ref> someday true --json-value
   kata meta unset <ref> someday

   Date and time values accept YYYY-MM-DD, local YYYY-MM-DDTHH:MM[:SS],
   or an RFC 3339 UTC instant ending in Z.

8. Use relationships deliberately. They live as flags on create + edit and
   are framed from the operating issue's POV — no argument-order traps:

   parent      = this issue is a sub-task of a larger issue
   blocks      = this issue must be resolved before the target can proceed
   blocked_by  = the target must be resolved before this issue can proceed
   related     = useful context, but not ordering

   kata create "fix auth flow" --parent abc4 --blocked-by d4ex --related j7m2 --agent
   kata edit abc4 --remove-blocks d4ex --related j7m2 --agent

   --remove-parent <ref> is strict: it must equal the current parent or
   fail loudly. Read parent before asserting a removal. The other
   --remove-* flags are idempotent (no-op when the link is already gone).

9. Attribute swarm contributions and request attention without changing the
   accountable actor or issue owner. The launcher supplies these variables to
   this child process, not globally to sibling teammates:

   export KATA_TEAMMATE=teammate-1
   export KATA_INBOX_USER=coordinator/teammate-1
   kata comment abc4 --body "Checked the retry path"
   kata create "Check retry behavior" --parent abc4 --idempotency-key retry-teammate-1
   kata --teammate=teammate-2 comment abc4 --body "Independent review"
   kata --teammate='' comment abc4 --body "Coordinator summary"
   kata notify abc4 --to coordinator --message "Please decide"
   kata notify abc4 --to coordinator/teammate-1 --message "Please check the update"
   kata inbox --for coordinator/teammate-1
   kata notify abc4 --to coordinator/teammate-1 --clear

   Comment creation stores the teammate in its dedicated field. New issue
   creation stores metadata.teammate. The author remains the accountable
   actor. --teammate overrides KATA_TEAMMATE, including an explicit empty
   value that suppresses the inherited default. KATA_INBOX_USER only selects
   an inbox; it does not set comment or create attribution.

   Inbox reads cover open issues in the selected project. Closing an issue hides
   its requests; reopening restores uncleared requests. The recipient remains
   explicit: use --for or KATA_INBOX_USER. An external harness must map each
   exact actor/teammate address to the runtime it launched and poll or watch
   while that runtime is idle. It wakes an available idle runtime, coalesces a
   request for one already running, and retains an unavailable teammate's
   request for the accountable actor. Reading or scheduling work does not clear
   the request; clear it after handling and read it back. Running quickstart
   does not install that wakeup integration.

   Requests are replaceable attention signals, not a lossless queue. One issue
   has one request per exact recipient, and a concurrent replacement and clear
   can race. A parent harness watches the actor address and each exact child
   address it allocated; inbox --for coordinator does not aggregate
   coordinator/*. Shared MCP processes use the per-call teammate on
   kata.comment and kata.create when siblings cannot have separate environments.

10. To leave context alongside a mutation, pass --comment TEXT on
   close, reopen, edit, assign, unassign, or label add/rm. The
   mutation lands first; the comment is appended in a follow-up call.
   If the comment call fails, the error names the issue so you can
   retry with kata comment <ref> --body ...

11. Do not run delete or purge unless the user explicitly asks for that exact
   destructive action and issue ref.

For long-running agents, poll events:

   kata events --after 0 --limit 100 --agent

Remember the returned cursor and resume from it. If a response says
reset_required, discard cached kata state and resume from the reset cursor.

For live streams:

   kata events --tail --agent

The agent tail stream emits one OK event line per event. Use --json only
when a consumer expects newline-delimited JSON.

# Remote daemon (optional)

When the kata daemon runs on a different host, point clients at it with
KATA_SERVER:

   export KATA_SERVER=http://100.64.0.5:7777

Or commit-free per-workspace:

   # .kata.local.toml (gitignored by 'kata init')
   version = 1

   [server]
   url = "http://100.64.0.5:7777"

KATA_SERVER wins over the file when both are set unless a command passes
--daemon <name>. If none of those are set, clients next honor active_daemon
in <KATA_HOME>/config.toml; otherwise they use the local daemon: Unix socket
on Unix platforms, loopback TCP on Windows. A configured-but-down remote
returns exit 7 (kata server not responding) — no silent fallback to spawning
a local daemon.
`

const agentQuickstartCompactText = `Use kata as the shared issue ledger for this workspace.
Do not create practice, tutorial, example, or scratchpad issues.
Search before creating or updating work.
Choose one unclaimed issue with kata next --unowned --agent.
Inspect a filtered queue with kata ready --unowned --label bug --no-label blocked --agent.
Default to --agent for ordinary kata reads and mutations in agent logs.
Use --json only when your script needs complete structured data.
Launch each child with KATA_TEAMMATE=teammate-1 and KATA_INBOX_USER=coordinator/teammate-1.
Comments store teammate; new issues store metadata.teammate while author remains accountable.
KATA_INBOX_USER selects an inbox and does not set attribution; --teammate overrides the attribution default.
Request actor or teammate attention: kata notify <ref> --to <actor>[/<teammate>] --message "<reason>".
Read exact requests with kata inbox --for <actor>[/<teammate>]; clear after handling with kata notify <ref> --to <actor>[/<teammate>] --clear.
An external harness polls idle inboxes and wakes the exact mapped runtime; quickstart does not install that integration.
If work is incomplete, label needs-review and comment with what remains.
Close only verified work with substantive prose and typed evidence.
Close each verified issue promptly; valid evidence keeps sibling close bursts admissible by default.
Do not run delete or purge unless explicitly asked for that exact action and issue ref.
Poll kata events with a saved cursor; reset cached state on reset_required.
`

func newQuickstartCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "quickstart",
		Aliases: []string{"agent-instructions"},
		Short:   "print instructions for agents using kata",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			switch currentOutputMode() {
			case outputContract:
				_, err := fmt.Fprint(cmd.OutOrStdout(), agentContractText)
				return err
			case outputJSON:
				var buf bytes.Buffer
				if err := emitJSON(&buf, map[string]string{
					"quickstart": agentQuickstartText,
				}); err != nil {
					return err
				}
				_, err := fmt.Fprint(cmd.OutOrStdout(), buf.String())
				return err
			case outputAgent:
				_, err := fmt.Fprint(cmd.OutOrStdout(), "OK quickstart\n"+agentQuickstartCompactText)
				return err
			}
			_, err := fmt.Fprint(cmd.OutOrStdout(), agentQuickstartText)
			return err
		},
	}
}
