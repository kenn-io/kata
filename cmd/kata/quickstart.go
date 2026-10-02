package main

import (
	"bytes"
	"fmt"

	"github.com/spf13/cobra"
	"go.kenn.io/kata/internal/config"
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

   # Claim it (fails if already assigned to another actor)
   kata claim <ref>

   # Release ownership only if you still own it
   kata unassign <ref> --expect-owner <your actor>

   A bare unassign clears the current owner, whoever that is.
   Check kata status <ref> for the current owner and your effective actor;
   kata show's [open] by <name> line names the author, not the owner.

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

   Reached schedules and deadlines use notify.* to surface in the current
   owner's inbox, or the author's inbox when unowned. They wait behind an
   existing request for that recipient and do not introduce a reminder type.

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

   Inbox reads cover open issues in the selected project by default.
   For cross-project work, use kata inbox --for coordinator/teammate-1 --all
   without --project or --workspace. This reads the selected daemon's active
   projects and returns qualified issue refs. It requires daemon API 0.9.0 or
   newer and daemon-wide read authority.
   Closing an issue hides its requests; reopening restores uncleared requests.
   The recipient remains explicit: use --for or KATA_INBOX_USER. An external harness must map each
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

# Selected daemon recovery

Never remove a workspace override to repair a stopped daemon. Run
kata daemon diagnose --json first. A registered local profile uses
[server].daemon in .kata.local.toml and pins an existing home/instance_uid.
For stopped_local_profile, run kata daemon recover; optionally add
--expect-project-uid <uid>. Recovery never initializes storage or projects.
Explicit server URLs stay pinned; restore their server or tunnel.
Daemon start/stop/restart/reload/logs administer the current KATA_HOME and
reject --daemon. Set KATA_HOME explicitly for those operations.

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
Use kata inbox --for <actor>[/<teammate>] --all for the selected daemon's active projects; refs are qualified.
An external harness polls idle inboxes and wakes the exact mapped runtime; quickstart does not install that integration.
If work is incomplete, label needs-review and comment with what remains.
Close only verified work with substantive prose and typed evidence.
Close each verified issue promptly; valid evidence keeps sibling close bursts admissible by default.
Do not run delete or purge unless explicitly asked for that exact action and issue ref.
Poll kata events with a saved cursor; reset cached state on reset_required.
Never remove a workspace override to repair a stopped daemon.
Run kata daemon diagnose first; kata daemon recover starts only a registered existing local profile.
Use --expect-project-uid <uid> when project identity is known; never initialize a replacement project.
Explicit server URLs stay pinned; restore their server or tunnel.
`

const externalAgentOnboardingText = `
# External agent without hooks

Run kata quickstart at session start when your harness has no session hooks.
It does not install hooks, polling processes, or configuration.

Keep ordinary commands pointed at your spoke daemon. Obtain its durable UID:

   kata federation identity --json

Give that instance_uid, the intended project, and actor to the hub administrator.
Keep the selected daemon pointed at the spoke because it identifies the spoke
project. For a registered local spoke profile, pass its selector explicitly.
The hub catalog entry with a matching origin supplies credentials based on
--hub-url:

For a request-actor hub, the administrator selects the agent actor with --actor;
this example uses external-agent. Identity-mode enrollment is attributed to the
administrator's identity token, so --actor cannot override it.

   kata --daemon <spoke-profile> federation enroll hub-project \
     --hub-url https://hub.example \
     --spoke-instance <spoke-instance-uid> --actor external-agent

The administrator returns the generated kata federation join command, with its
separate enrollment token. Run it against the spoke, not the hub. For example:

   kata federation join --project spoke-project \
     --hub-url https://hub.example --hub-project-id <hub-project-id> \
     --token <enrollment-token> --capabilities claim,pull,push \
     --actor external-agent --push

Prefer the generated command and preserve its project UID, replay/baseline
cursors, actor, and capabilities. Existing standalone projects need matching
adoption permission and --adopt-existing; a new replica needs neither adoption
nor prior kata init. Do not request the hub's administration token for the agent.
Treat the generated join command as a secret.

Use the HTTPS hostname matching the hub certificate. An IP substitution may
fail certificate validation. --allow-insecure does not bypass TLS certificate
checks; it opts into plaintext transport on a trusted private network.

For request-actor spokes, set KATA_AUTHOR to the actor from the generated join
command. Identity-mode spokes use the identity token's actor; KATA_AUTHOR cannot
override it, so the token actor must match the enrollment actor:

   export KATA_AUTHOR='<actor-from-generated-join-command>'
   export KATA_INBOX_USER=external-agent
   kata whoami
   kata federation status --project spoke-project --json
   kata inbox --project spoke-project --for external-agent --json
   kata events --project spoke-project --after 0 --limit 100 --json

Push-enabled bindings attribute local-origin work to that actor. Check
pull/push status before claiming work. Poll the
exact inbox while idle. Save event cursors per daemon/project and resume with
--after <cursor>. On reset_required, discard cached state, refresh reads, and
resume from the returned reset cursor. Reads may lag the hub until sync pulls.
Clear requests only after handling, then read the inbox again:

   kata notify abc4 --project spoke-project --to external-agent --clear
   kata inbox --project spoke-project --for external-agent --json
`

const externalAgentOnboardingCompactText = `For an external agent without hooks, run quickstart at session start and poll its exact idle inbox.
On the spoke: kata federation identity --json; give instance_uid and intended actor/project to the hub administrator.
Keep the selected daemon pointed at the spoke; --hub-url selects credentials from a hub catalog entry with the matching origin. Enroll with: kata --daemon <spoke-profile> federation enroll hub-project --hub-url https://hub.example --spoke-instance <uid> --actor external-agent. Identity-mode hubs use the administrator's identity-token actor regardless of --actor.
Run the returned kata federation join command against the spoke; preserve project UID, cursors, actor, capabilities, and adoption options. Treat its enrollment token as a secret.
Use the HTTPS hostname matching the certificate; --allow-insecure does not bypass TLS certificate checks.
For request-actor spokes, set KATA_AUTHOR to the actor from the generated join command. Identity-mode spokes use an identity token whose actor matches the enrollment actor; KATA_AUTHOR cannot override it.
Check kata whoami and kata federation status before claiming; spoke inbox/events can lag the hub.
`

const embeddingsSetupText = `
Embeddings are optional; lexical search works without configuration.
This checks only local <KATA_HOME>/config.toml, without contacting any daemon
or reading key files. For a remote daemon or another profile, configure that
daemon's home and inspect its runtime with kata health --json.

For Voyage, add this block to the intended daemon's config.toml only if wanted:

   [search.embeddings]
   base_url = "https://api.voyageai.com/v1"
   model = "voyage-3-large"
   dims = 1024
   api_key_file = "~/.config/kata/embedding.key"

Have your secret manager supply the key file. Use an absolute or ~/ path; on
Unix make it owner-only (chmod 600). Never put a literal key in config.toml.
Alternatively use api_key_env = "VOYAGE_API_KEY" and supply that variable to
the daemon's launch environment; exporting it only in a later CLI shell does
not update the running daemon. Existing inline api_key takes precedence over
file/environment sources; remove it when switching to a secure source.

An OpenAI-compatible provider uses its own endpoint, model, and dimensions.
For OpenAI: base_url = "https://api.openai.com/v1", model =
"text-embedding-3-small", dims = 1536; use api_key_file or api_key_env =
"OPENAI_API_KEY". Configure one provider block, without duplicate TOML sections.
Hosted embeddings send issue titles and bodies to the configured provider.
Configure the intended search daemon's settings, not an unrelated CLI home.

Restart the intended daemon after adding/changing provider settings. An existing
provider's replaced key file needs kata daemon reload. Lifecycle commands use
the current KATA_HOME, not --daemon; a remote daemon's operator reloads it.

   kata health --json
   kata search "words from existing work" --agent
   kata search "words from existing work" --semantic --agent

Check embeddings credential status, last_success_at, and backlog in health;
ok=true alone does not establish working credentials. Default search can fall
back to lexical; explicit --semantic exposes missing/rejected keys. Verify with
existing work instead of creating a practice issue. Quickstart writes nothing.
`

const embeddingsSetupCompactText = `Embeddings are optional; lexical search works without configuration. Quickstart writes nothing and checks only local KATA_HOME/config.toml, without reading key files or contacting a daemon.
For optional Voyage: [search.embeddings] base_url="https://api.voyageai.com/v1", model="voyage-3-large", dims=1024; for OpenAI: base_url="https://api.openai.com/v1", model="text-embedding-3-small", dims=1536.
Keep keys outside config.toml: api_key_file="~/.config/kata/embedding.key" (owner-only, chmod 600), or api_key_env="VOYAGE_API_KEY"/"OPENAI_API_KEY" supplied to the daemon launch environment. Remove overriding inline api_key when switching sources.
Restart the intended search daemon for provider settings; reload existing key files with kata daemon reload using its KATA_HOME. Hosted providers receive issue titles/bodies.
Use kata health --json on the selected daemon to check embeddings credentials, last_success_at, and backlog; ok=true alone is insufficient.
Verify existing work with kata search "words from existing work" --agent and kata search "words from existing work" --semantic --agent; default search can fall back to lexical.
`

func quickstartEmbeddingsStatus() string {
	home, err := config.KataHome()
	if err != nil {
		return "could not inspect; check the intended daemon's configuration"
	}
	cfg, err := config.ReadDaemonConfigForHome(home)
	if err != nil {
		// Parser errors may contain inline credentials. Keep quickstart useful
		// with malformed configuration without printing its contents.
		return "could not inspect; check the intended daemon's configuration"
	}
	if cfg.Search.Embeddings.Enabled() {
		return "configured (credentials and running daemon not checked)"
	}
	return "not configured (lexical search is available)"
}

func newQuickstartCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "quickstart",
		Aliases: []string{"agent-instructions"},
		Short:   "print instructions for agents using kata",
		Long: `Print instructions for agents using kata.

Default: full instructions for agents using kata.
--agent: concise instructions for agent logs.
--json: instructions in a JSON response.
--format contract: the canonical managed contract, exactly what kata agent-hooks contract injects.

Run kata agent-hooks install --all to load the contract in every session on this machine.
Without hooks, run kata quickstart at session start.
Reads local embeddings settings without changing config or contacting a daemon.
The managed contract format stays static.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if currentOutputMode() == outputContract {
				_, err := fmt.Fprint(cmd.OutOrStdout(), agentContractText)
				return err
			}
			status := "Local embeddings config: " + quickstartEmbeddingsStatus() + "\n"
			full := agentQuickstartText + externalAgentOnboardingText + "\n# Optional embeddings setup\n\n" + status + embeddingsSetupText
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
				_, err := fmt.Fprint(cmd.OutOrStdout(), "OK quickstart\n"+agentQuickstartCompactText+externalAgentOnboardingCompactText+status+embeddingsSetupCompactText)
				return err
			}
			_, err := fmt.Fprint(cmd.OutOrStdout(), full)
			return err
		},
	}
}
