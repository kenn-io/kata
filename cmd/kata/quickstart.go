package main

import (
	"bytes"
	"fmt"

	"github.com/spf13/cobra"
)

const agentQuickstartText = `# kata agent quickstart

Use kata as the shared issue ledger for this workspace.

1. Run from the workspace (--workspace overrides; --project picks
   another). Author = $KATA_AUTHOR > $USER > git user.name. --json for
   parsing. If uninitialized, report that kata init is needed.
   Issue refs are short_ids derived from each issue's ULID (e.g. abc4).
   Cross-project: kata#abc4. Full 26-char ULIDs also resolve. Legacy
   numeric refs (12, kata#12) no longer work.

2. Closing an issue asserts that the work is complete. If the work is
   not done, DO NOT close. Instead:

      kata edit <ref> --label needs-review
      kata comment <ref> --body "what was attempted, what remains"

   When done, close with substantive prose and typed --evidence:

      kata close abc4 --done \
        --message "Fixed Safari callback double-submit; verified tests pass." \
        --commit <sha>

   Close each issue as soon as its work is verified, not in a single
   "close everything" pass at the end. The daemon throttles >3 sibling
   closes by one actor under one parent in 5 minutes; close eagerly
   and you will not see the throttle. Operators can disable it via
   [close.throttle] enabled = false in <KATA_HOME>/config.toml.

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

3. Search before creating:

   kata search "login race" --json
   kata search --project foo "login race" --json

4. If no existing issue fits, create with an idempotency key:

   kata create "fix login race" \
     --body "Observed double-submit in Safari callback." \
     --idempotency-key "login-race-2026-05-02" \
     --json

   kata create --project foo "fix login race" \
     --body "Observed double-submit in Safari callback." \
     --idempotency-key "foo-login-race-2026-05-02" \
     --json

5. Prefer updating existing issues over creating duplicates:

   kata show abc4 --json
   kata show --project foo abc4 --json
   kata comment abc4 --body "Found another reproduction path." --json
   kata label add abc4 safari --json
   kata edit abc4 --blocks d4ex --json

6. Find and claim available work (multi-agent environments):

   # Find unclaimed work
   kata ready --unowned --json
   kata ready --unowned --label bug --no-label blocked --json

   # Claim it (fails if already claimed by another actor)
   kata claim <ref>

   # Release ownership
   kata unassign <ref>

7. Use relationships deliberately. They live as flags on create + edit and
   are framed from the operating issue's POV — no argument-order traps:

   parent      = this issue is a sub-task of a larger issue
   blocks      = this issue must be resolved before the target can proceed
   blocked_by  = the target must be resolved before this issue can proceed
   related     = useful context, but not ordering

   kata create "fix auth flow" --parent abc4 --blocked-by d4ex --related j7m2 --json
   kata edit abc4 --remove-blocks d4ex --related j7m2 --json

   --remove-parent <ref> is strict: it must equal the current parent or
   fail loudly. Read parent before asserting a removal. The other
   --remove-* flags are idempotent (no-op when the link is already gone).

8. To leave context alongside a mutation, pass --comment TEXT on
   close, reopen, edit, assign, unassign, or label add/rm. The
   mutation lands first; the comment is appended in a follow-up call.
   If the comment call fails, the error names the issue so you can
   retry with kata comment <ref> --body ...

9. Do not run delete or purge unless the user explicitly asks for that exact
   destructive action and issue ref.

For long-running agents, poll events:

   kata events --after 0 --limit 100 --json

Remember the returned cursor and resume from it. If a response says
reset_required, discard cached kata state and resume from the reset cursor.

For live streams:

   kata events --tail

The tail stream emits newline-delimited JSON.

# Remote daemon (optional)

When the kata daemon runs on a different host, point clients at it with
KATA_SERVER:

   export KATA_SERVER=http://100.64.0.5:7777

Or commit-free per-workspace:

   # .kata.local.toml (gitignored by 'kata init')
   version = 1

   [server]
   url = "http://100.64.0.5:7777"

KATA_SERVER wins over the file when both are set. A configured-but-down
remote returns exit 7 (kata server not responding) — no silent fallback to
spawning a local daemon. The default (no env, no .kata.local.toml) is
unchanged: a local Unix-socket daemon is auto-started on demand.
`

func newQuickstartCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "quickstart",
		Aliases: []string{"agent-instructions"},
		Short:   "print instructions for agents using kata",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if flags.JSON {
				var buf bytes.Buffer
				if err := emitJSON(&buf, map[string]string{
					"quickstart": agentQuickstartText,
				}); err != nil {
					return err
				}
				_, err := fmt.Fprint(cmd.OutOrStdout(), buf.String())
				return err
			}
			_, err := fmt.Fprint(cmd.OutOrStdout(), agentQuickstartText)
			return err
		},
	}
}
