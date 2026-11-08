// Package agentguide holds canonical agent guidance shared by the CLI and MCP server.
package agentguide

import "strings"

// Contract is the marker-free agent workflow contract: the short core every
// coding agent sees. Coordination detail lives in the quickstart fragments.
const Contract = `This workspace tracks its work in kata. Use kata for the changes the user asks
you to make here, not for questions, explanations, or reviews that change
nothing.

Before starting a change, look for an existing issue:

    kata search "<one or two distinctive words>" --agent

Search returns issues that contain every word, so add words only to narrow the
results. If an open issue covers the work, use it. Otherwise create one:

    kata create "<title>" --body "<what and why>" --idempotency-key <key> --agent

If create reports a look-alike, use that issue unless the work truly differs
(--force-new). Link related but different work with --related <ref>, or
--parent <ref> when the new issue is part of a larger one.

Claim the issue before you work on it: kata claim <ref>. When the user only
asks you to record something, create the issue and stop there.

Comment when you learn something a later reader needs:

    kata comment <ref> -m "<finding>" --agent

Close the issue as soon as the work is verified. The message needs 40 or more
characters and the close needs evidence:

    kata close <ref> --done --message "<what changed and how you verified it>" --test "<command>" --agent

Use --commit <sha> or --pr <url> instead of --test when that is the evidence.
If you stop before the work is done, leave the issue open and say what remains:

    kata label add <ref> needs-review --comment "<what remains>"

Never run kata delete or kata purge unless the user asks for that exact action.

For subagents, inboxes, attention signals, scheduling, and relationship rules,
run kata quickstart.
`

// Quickstart is the full CLI agent briefing: every fragment, in order.
var Quickstart = joinFragments()

func joinFragments() string {
	var b strings.Builder
	for _, f := range Fragments {
		b.WriteString(f.Text)
	}
	return b.String()
}

// Fragment is one named, verbatim part of Quickstart.
type Fragment struct{ ID, Text string }

// Fragments lists the quickstart fragments in presentation order.
var Fragments = []Fragment{
	{"intro", fragIntro},
	{"closing", fragClosing},
	{"output-flags", fragOutputFlags},
	{"real-work", fragRealWork},
	{"search-create-update", fragSearchCreateUpdate},
	{"claiming", fragClaiming},
	{"scheduling", fragScheduling},
	{"relationships", fragRelationships},
	{"teammates", fragTeammates},
	{"coordination", fragCoordination},
	{"mutation-comments", fragMutationComments},
	{"destructive", fragDestructive},
	{"events", fragEvents},
	{"daemon-recovery", fragDaemonRecovery},
	{"remote-daemon", fragRemoteDaemon},
}

const fragIntro = `# kata agent quickstart

Use kata as the shared issue ledger for this workspace.

1. Find the right command by describing the task:

      kata cli search "<task>"

   Run from the workspace (--workspace overrides; --project picks
   another). Author = $KATA_AUTHOR > $USER > git user.name.
   No bound workspace: use <project>#<id> refs or --project <project>.
   Issue refs are short_ids derived from each issue's ULID (e.g. abc4).
   Cross-project: kata#abc4. Full 26-char ULIDs also resolve. Legacy
   numeric refs (12, kata#12) no longer work.

`

const fragClosing = `2. Closing an issue asserts that the work is complete. If the work is
   not done, DO NOT close. Instead:

      kata label add <ref> needs-review --comment "<what remains>"

   When done, use a message of 40+ characters and typed --evidence:

      kata close abc4 --done \
        --message "Fixed Safari callback double-submit; verified tests pass." \
        --commit <sha>

   Typed --evidence accepts commit:<sha>, pr:<url>, test:<cmd>,
   reviewed-paths:<path>, or external:<account>. Shortcuts are --commit,
   --pr, --test, and --reviewed.

   Close each issue as soon as its work is verified, not in a batch or a
   single "close everything" pass at the end. By default the daemon permits
   sibling close bursts when each close has valid evidence and a substantive
   message. Operators can enable stricter burst/prose throttling via
   [close.throttle] enabled = true in <KATA_HOME>/config.toml.

   Other close forms (duplicate/superseded: 20+ characters;
   wontfix: 60+ characters):

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

`

const fragOutputFlags = `   Default to --agent for ordinary kata reads and mutations in agent logs.
   Use --json only when your script needs complete structured data, for
   example when piping into jq.

`

const fragRealWork = `   Use kata for real repository work only.
   Do not create practice, tutorial, example, or scratchpad issues unless the user explicitly asks for them.
   If the work belongs to another repository or project, use that project.

`

const fragSearchCreateUpdate = `3. Search before creating:

   kata search "login race" --agent
   kata search --project foo "login race" --agent

   If create refuses a look-alike, reuse the match. Pass --force-new only
   if the work truly differs.

   A hit that is related but not the same work still matters: create the
   new issue and link every such hit, --parent <ref> when the new work is a
   slice of it, --related <ref> for context.

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

   Answer a specific comment with a typed reply, using the handle shown
   by kata show. --confirm and --refute take evidence in the body:

   kata comment abc4 --reply c:xbzxrq -m "This builds on the finding." --agent
   kata comment abc4 --confirm c:xbzxrq -m "Reproduced with the documented retry steps." --agent
   kata comment abc4 --refute c:xbzxrq -m "Still fails when the connection drops twice." --agent
   kata comment abc4 --supersede c:xbzxrq -m "The updated finding replaces this one." --agent

`

const fragClaiming = `6. Find and claim available work (multi-agent environments):

   # Choose one unclaimed issue
   kata next --unowned --agent

   # Inspect the filtered queue
   kata ready --unowned --label bug --no-label blocked --agent

   # Claim it (fails if already assigned to another actor)
   kata claim <ref>

   # Release ownership only if you still own it
   kata unassign <ref> --expect-owner <your-actor>

   A bare unassign clears the current owner, whoever that is.
   Check kata status <ref> for the current owner and your effective actor;
   the "[open] by <name>" line in kata show names the author, not the owner.

`

const fragScheduling = `7. Use native planning dates deliberately:

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

`

const fragRelationships = `8. Use relationships deliberately. They live as flags on create + edit and
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

`

const fragTeammates = `9. Attribute swarm contributions and request attention without changing the
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

   If a comment-sized task needs ownership or tracked completion, make it a
   child issue and claim it:

      kata create "fix retry handling" --parent <ref> --idempotency-key retry-child
      kata claim <child>

   Comments record findings; child issues track owned work.

`

const fragCoordination = `   Coordinate tracked work through work.* metadata when a coordinator or
   dashboard watches it. Each agent writes only its own work.* keys, and
   never on closed issues:

   kata meta set <ref> work.attention ok           # actively working
   kata meta set <ref> work.attention stuck        # blocked
   kata meta set <ref> work.attention needs-human  # wants input or review
   kata meta set <ref> work.attention_msg "<why>"
   kata meta set <ref> work.branch <branch>        # once, for branch work

   Before stopping on watched work, close it or update both
   work.attention and work.attention_msg for the hand-off.

   A coordinator creates delegated children with --parent <ref>,
   --meta work.branch=<branch>, and an idempotency key. It reads their
   work.* but never writes it, and joins with:

   kata wait <refs> --until attention --any --timeout 30m

   needs-human or stuck completes the wait; so does a close, and the
   reported reason says which.

`

const fragMutationComments = `10. To leave context alongside a mutation, pass --comment TEXT on
   close, reopen, edit, assign, unassign, or label add/rm. The
   mutation lands first; the comment is appended in a follow-up call.
   If the comment call fails, the error names the issue so you can
   retry with kata comment <ref> --body ...

`

const fragDestructive = `11. Do not run delete or purge unless the user explicitly asks for that exact
   destructive action and issue ref.

`

const fragEvents = `For long-running agents, poll events:

   kata events --after 0 --limit 100 --agent

Remember the returned cursor and resume from it. If a response says
reset_required, discard cached kata state and resume from the reset cursor.

For live streams:

   kata events --tail --agent

The agent tail stream emits one OK event line per event. Use --json only
when a consumer expects newline-delimited JSON.

`

const fragDaemonRecovery = `# Selected daemon recovery

Never remove a workspace override to repair a stopped daemon. Start
with kata daemon diagnose --json. A registered local profile uses
[server].daemon in .kata.local.toml and pins an existing home/instance_uid.
For stopped_local_profile, run kata daemon recover; optionally add
--expect-project-uid <uid>. Recovery never initializes storage or projects.
Explicit server URLs stay pinned; restore their server or tunnel.
Daemon start/stop/restart/reload/logs administer the current KATA_HOME and
reject --daemon. Set KATA_HOME explicitly for those operations.

`

const fragRemoteDaemon = `# Remote daemon (optional)

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
