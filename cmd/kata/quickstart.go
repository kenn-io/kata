package main

import (
	"bytes"
	"fmt"

	"github.com/spf13/cobra"

	"go.kenn.io/kata/internal/agentguide"
)

const hooklessQuickstartText = `
# Hookless consumer Muse

Preview setup: kata agent-hook instructions install muse --home <path> --actor <actor> --dry.
This generates standing instructions, a skill and a scheduled poll spec. It relies on
instruction-following, not enforcement, and provides no native attention support.
Choose authentication explicitly; create the scheduled task through Muse itself.
`

var agentQuickstartText = agentguide.Quickstart

const agentQuickstartCompactText = `Use kata as the shared issue ledger for this workspace.
Do not create practice, tutorial, example, or scratchpad issues.
Search before creating or updating work.
If create refuses a look-alike, reuse it; --force-new only if the work truly differs.
No bound workspace: use <project>#<id> refs or --project <project>.
Choose one unclaimed issue with kata next --unowned --agent.
Inspect a filtered queue with kata ready --unowned --label bug --no-label blocked --agent.
Claim it with kata claim <ref>.
Default to --agent for ordinary kata reads and mutations in agent logs.
Use --json only when your script needs complete structured data.
Launch each child with KATA_TEAMMATE=teammate-1 and KATA_INBOX_USER=coordinator/teammate-1.
Comments store teammate; new issues store metadata.teammate while author remains accountable.
Record progress: kata comment <ref> -m "<what changed>".
When a comment-sized task needs ownership or tracked completion, make it a child issue: kata create "fix retry handling" --parent <ref>, then kata claim <child>.
KATA_INBOX_USER selects an inbox and does not set attribution; --teammate overrides the attribution default.
Request actor or teammate attention: kata notify <ref> --to <actor>[/<teammate>] --message "<reason>".
Read exact requests with kata inbox --for <actor>[/<teammate>]; clear after handling with kata notify <ref> --to <actor>[/<teammate>] --clear.
Use kata inbox --for <actor>[/<teammate>] --all for the selected daemon's active projects; refs are qualified.
An external harness polls idle inboxes and wakes the exact mapped runtime; quickstart does not install that integration.
If work is incomplete, use kata label add <ref> needs-review --comment "<what remains>".
Close verified work: kata close <ref> --done --message "<scope and verification, 40+ characters>" --evidence commit:<sha> | pr:<url> | test:<cmd> | reviewed-paths:<path>.
Evidence shortcuts: --commit, --pr, --test, --reviewed.
Retire duplicate or superseded work with --duplicate-of <ref> or --superseded-by <ref> (20+ characters); dropped work with --wontfix (60+ characters).
Close each verified issue promptly; valid evidence keeps sibling close bursts admissible by default.
Do not run delete or purge unless explicitly asked for that exact action and issue ref.
Poll kata events with a saved cursor; reset cached state on reset_required.
Never remove a workspace override to repair a stopped daemon.
Run kata daemon diagnose first; kata daemon recover starts only a registered existing local profile.
Use --expect-project-uid <uid> when project identity is known; never initialize a replacement project.
Explicit server URLs stay pinned; restore their server or tunnel.
`

const optionalSetupText = `
Optional setup:
For federation, run kata federation identity on the spoke and give its instance UID
and intended actor/project to the hub administrator. Keep the selected daemon pointed at the spoke.
Enrollment, joining, and polling: https://katatracker.com/docs/operations/federation/
Embeddings are optional; lexical search works without them. Setup and credentials:
https://katatracker.com/docs/guide/semantic-search/
`

func newQuickstartCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "quickstart",
		Aliases: []string{"agent-instructions"},
		Short:   "print instructions for agents using kata",
		Long: `Print instructions for agents using kata.

Default: full instructions for agents using kata.
--agent: concise instructions for agent logs.
--json: instructions in a JSON response.
--format contract: the canonical managed contract, exactly what kata agent-hook contract injects.

Run kata agent-hook install --all to load the contract in every session on this machine.
Without hooks, run kata quickstart at session start.
Setup guides cover federation enrollment and optional embeddings.
The managed contract format stays static.
` + hooklessQuickstartText,
		Example: `  kata quickstart --format contract
  kata quickstart --agent`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if currentOutputMode() == outputContract {
				_, err := fmt.Fprint(cmd.OutOrStdout(), agentContractText)
				return err
			}
			full := agentQuickstartText + optionalSetupText
			switch currentOutputMode() {
			case outputJSON:
				var buf bytes.Buffer
				if err := emitJSON(&buf, map[string]string{
					"quickstart": full,
				}); err != nil {
					return err
				}
				_, err := fmt.Fprint(cmd.OutOrStdout(), buf.String())
				return err
			case outputAgent:
				_, err := fmt.Fprint(cmd.OutOrStdout(), "OK quickstart\n"+agentQuickstartCompactText+optionalSetupText)
				return err
			}
			_, err := fmt.Fprint(cmd.OutOrStdout(), full)
			return err
		},
	}
}
