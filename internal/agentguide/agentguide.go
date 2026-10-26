// Package agentguide holds canonical agent guidance shared by the CLI and MCP server.
package agentguide

import "strings"

// Contract is the marker-free agent workflow contract.
const Contract = "Kata is the system of record for intent.\n\n" +
	"- Never `kata delete` or `kata purge` without explicit user authorization.\n\n" +
	`~~~dot
digraph kata {
  rankdir=TB; node [shape=box];

  arrive   [shape=diamond label="Work arrives"];
  search   [label="Search first:\nkata search \"<terms>\" --agent\nReuse an open issue, else create one.\nIf create refuses a look-alike, reuse it;\n--force-new only if the work truly differs.\nNo bound workspace: refs are <project>#<id>,\nor pass --project <project>."];
  route    [shape=diamond label="Work it, or delegate it?"];

  subgraph cluster_work {
    label="";
    claim  [label="Claim and mark it tracked:\nkata claim <ref>\nkata meta set <ref> work.attention ok"];
    branch [label="If the work happens on a dedicated branch, stamp it once:\nkata meta set <ref> work.branch <branch>\nor bind at creation:\nkata create ... --meta work.branch=<branch> --idempotency-key <key>"];
    live   [label="Keep state current:\nkata meta set <ref> work.attention stuck|needs-human|ok\nkata meta set <ref> work.attention_msg \"<why>\"\nstuck = blocked; needs-human = input/review; ok = unblocked.\nRecord progress:\nkata comment <ref> -m \"<what changed>\"\nRequest attention:\nkata notify <ref> --to <actor>[/<teammate>] --message <reason>"];
    claim -> branch -> live;
  }

  subgraph cluster_delegate {
    label="";
    fanout [label="Tracked children: --parent <ref>, --meta work.branch=<branch>,\n--idempotency-key <key>, --json; capture .issue.short_id.\nSubagents: distinct KATA_TEAMMATE and\nKATA_INBOX_USER=<actor>/<teammate>; keep the actor.\nRead requests: kata inbox --for <actor>[/<teammate>].\nAfter handling: kata notify <ref> --to <actor>[/<teammate>] --clear."];
    join   [label="Join with kata wait <refs> --until attention --any\nMatches needs-human or stuck; a close also completes the wait,\nand the reported reason distinguishes which. Use --timeout so a\nwrapper can tell timeout from satisfaction."];
    coord  [label="Read delegated work.*; never write it."];
    fanout -> join -> coord;
  }

  done     [shape=diamond label="Verified complete?"];
  close    [label="kata close <ref> --done --message \"<scope and verification, 40+ chars>\"\n--evidence commit:<sha> | pr:<url> | test:<cmd> | reviewed-paths:<path>\n(or --commit, --pr, --test, --reviewed)"];
  retire   [label="kata close <ref> --message \"<why>\"\n--duplicate-of <ref> | --superseded-by <ref> (20+ chars)\n| --wontfix (60+ chars)"];
  review   [label="kata label add <ref> needs-review --comment \"<what remains>\""];
  park     [shape=diamond label="Park it?"];
  schedule [label="kata schedule <ref> <date-or-time>\nsets scheduled_on; clear with -"];
  someday  [label="kata meta set <ref> someday true --json-value\nclear with kata meta unset <ref> someday"];

  arrive -> search -> route;
  route -> claim   [label="work it"];
  route -> fanout  [label="delegate it"];
  route -> park    [label="record only"];
  live  -> done;
  coord -> done;
  done -> close    [label="yes"];
  done -> retire   [label="duplicate, superseded or dropped"];
  done -> park     [label="no"];
  park -> schedule [label="start date known"];
  park -> someday  [label="start date unknown"];
  park -> review   [label="needs review"];
}
~~~

Inbox reads stay in the selected project. For cross-project work, use kata inbox --for <actor>[/<teammate>] --all without --project or --workspace.

Parent links group work; they do not gate readiness, but a parent cannot close with open children.
Use --blocks <dependent> / --blocked-by <prerequisite> only for real prerequisites; they gate kata ready.
Use --related <ref> for context only. kata wait observes state without requiring a dependency edge.

One writer per key: only write your own work.*. Ignore work.* on closed issues; never write it there.
Before stopping, close completed work or update both work.attention and work.attention_msg for the handoff.

Schedule/someday defer work; deadlines don’t. kata deadline <ref> <date-or-time> sets deadline_on without changing readiness.
Reached schedules and deadlines use notify.* for the current owner, or the author when unowned; clear with kata notify <ref> --to <recipient> --clear.
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
	{"mutation-comments", fragMutationComments},
	{"destructive", fragDestructive},
	{"events", fragEvents},
	{"daemon-recovery", fragDaemonRecovery},
	{"remote-daemon", fragRemoteDaemon},
}

const fragIntro = `# kata agent quickstart

Use kata as the shared issue ledger for this workspace.

1. Run from the workspace (--workspace overrides; --project picks
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
