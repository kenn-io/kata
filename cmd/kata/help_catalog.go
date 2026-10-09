package main

import "github.com/spf13/cobra"

// helpDoc supplements constructor help while nearby command work lands.
// A Long description has one owner: the constructor or this catalog.
type helpDoc struct{ Long, Example string }

var helpCatalog = map[string]helpDoc{
	"kata search": {
		Long: `Find issues whose title, body, or comments match the words. Run this before
kata create and reuse an open match.
Searches the bound project (or --project), open and closed unless --status.
Mode is auto by default: hybrid when embeddings are configured, else
lexical. --agent prints "OK search count=N" then one "- issue=<ref> ..." row
per match.`,
		Example: `  kata search "login race" --agent
  kata search login safari --status open --label bug --agent
  kata search --project example-project "token refresh" --limit 5 --json`,
	},
	"kata create": {
		Long: `Create an issue in the bound project (or --project). Search first.
--idempotency-key makes retries safe for seven days: the same key returns
the original issue; the same key with different fields fails with conflict (exit 5). Use a
key that is stable for this piece of work, such as "<task>-<date>".
A title and body that look like an open or closed issue may be refused with
the matching refs (conflict, exit 5); pass --force-new only after reading them.
Relationship flags take refs. --parent groups work and does not gate
readiness; --blocked-by does. --meta key=value stores a string.
--agent prints "OK create <ref>"; --json has .issue.short_id.`,
		Example: `  kata create "fix login race" --body "Safari double-submits the callback." --idempotency-key login-race-2026-10-09 --agent
  kata create "write retry test" --parent abc4 --blocked-by d4ex --idempotency-key retry-test-abc4 --agent
  kata create "audit tokens" --label security --priority 1 --meta work.branch=audit-tokens --agent`,
	},
	"kata show": {
		Example: `  kata show abc4 --agent
  kata show example-project#abc4 --json`,
	},
	"kata status": {
		Long: `Print the issue's identity, current owner, your effective actor, and any
hold (schedule, someday, blockers). Use it before claim or unassign; kata
show's "by <name>" line is the author, not the owner.`,
		Example: `  kata status abc4 --agent`,
	},
	"kata list": {
		Long: `List issues in the bound project (or --project). Defaults to open issues,
most recently updated first, at most 200 rows. --all lists every active
project, newest created first, with no default row limit.
--meta key or key=value filters metadata, for example work.attention=stuck.
For what to work on next, use kata ready or kata next instead.`,
		Example: `  kata list --agent
  kata list --status all --label bug --owner alice --agent
  kata list --meta work.attention=stuck --all --agent`,
	},
	"kata ready": {
		Long: `List open issues that nothing open blocks (--blocked-by), that are not
scheduled for a future date, and not marked someday. Parent links do not
block. Most recently updated first; kata next selects by priority (0 = highest).
Use --unowned to see unclaimed work; kata next returns just the top one.`,
		Example: `  kata ready --unowned --agent
  kata ready --project example-project --label bug --no-label blocked --agent`,
	},
	"kata next": {
		Long: `Print the single highest-priority issue from kata ready, with the same
filters. An empty queue succeeds (exit 0): --agent prints "OK next found=false",
--json returns a null issue. Claim the selected issue next.`,
		Example: `  kata next --unowned --agent
  kata next --label bug --full --json`,
	},
	"kata claim": {
		Long: `Claim an issue. The daemon's authenticated actor takes precedence over
the client-selected actor when present. Use kata status <ref> to check the
effective actor and current owner; kata whoami shows only the client-selected actor.
Refuses when another actor owns it unless --force.
--ttl releases the claim after 1m-24h. After claiming, mark the work tracked:
  kata meta set <ref> work.attention ok
kata assign sets a different owner; kata unassign --expect-owner releases.`,
		Example: `  kata claim abc4 --agent
  kata claim abc4 --if-unowned --ttl 2h --comment "starting on the Safari path" --agent`,
	},
	"kata assign": {
		Long:    `Set another actor as owner. To take an issue yourself, use kata claim.`,
		Example: `  kata assign abc4 alice --agent`,
	},
	"kata unassign": {
		Long: `Clear the owner. A bare unassign clears whoever owns it; pass
--expect-owner <you> to release only your own claim.`,
		Example: `  kata unassign abc4 --expect-owner alice --agent`,
	},
	"kata edit": {
		Long: `Change title, body, owner, or priority, and add or remove relationships.
Relationships read from this issue's point of view:
  --blocks X      this issue must finish before X (gates X in kata ready)
  --blocked-by X  X must finish before this issue
  --parent X      this issue is a sub-task of X (does not gate readiness)
  --related X     context only
--remove-parent must name the current parent; other --remove-* flags are
idempotent. Labels use kata label; metadata uses kata meta.`,
		Example: `  kata edit abc4 --blocked-by d4ex --agent
  kata edit abc4 --title "fix login race in Safari" --priority 1 --agent
  kata edit abc4 --remove-blocks d4ex --related j7m2 --agent`,
	},
	"kata comment": {
		Long: `Append a comment. Body comes from -m/--body, --body-file, or --body-stdin.
KATA_TEAMMATE (or --teammate) records which teammate wrote it.`,
		Example: `  kata comment abc4 -m "Reproduced on Safari 18; fix in progress." --agent
  kata comment abc4 --body-file notes.md --agent`,
	},
	"kata comment edit": {
		Example: `  kata comment edit abc4 <comment-uid> -m "corrected repro steps" --agent`,
		Long:    `Replace the body of one comment. Find its comment UID with kata show; use -m/--body, --body-file, or --body-stdin.`,
	},
	"kata label": {
		Long: `Attach or detach one label. kata labels lists counts.`,
	},
	"kata label add": {
		Example: `  kata label add abc4 bug --agent
  kata label add abc4 needs-review --comment "Fix drafted; focused test still fails." --agent`,
		Long: `Attach one label to an issue. Use needs-review with --comment to hand off unfinished work.`,
	},
	"kata label rm": {
		Example: `  kata label rm abc4 needs-review --agent`,
		Long:    `Detach one label from an issue. kata label remove is an alias.`,
	},
	"kata labels": {
		Example: `  kata labels --agent`,
		Long:    `List label counts in the bound project (or --project). Use kata label add or kata label rm to change a label.`,
	},
	"kata meta": {
		Long: `Per-issue key/value metadata. Kata stores it; these keys carry meaning:
  work.attention      ok | needs-human | stuck  (agent's live signal)
  work.attention_msg  one-line reason shown with work.attention
  work.branch         git branch doing the work
  someday             true (with --json-value) parks the issue without a date
Write only your own work.* keys and never on closed issues. Planning dates
use kata schedule and kata deadline.`,
	},
	"kata meta set": {
		Long: `Set <key> to <value>. Values are strings unless --json-value. --if-absent,
--if-value, and --if-match make the write conditional.
See kata meta --help for the keys agents use.`,
		Example: `  kata meta set abc4 work.attention stuck --agent
  kata meta set abc4 work.attention_msg "waiting on schema review" --agent
  kata meta set abc4 someday true --json-value --agent`,
	},
	"kata meta get": {
		Example: `  kata meta get abc4 work.attention --agent`,
		Long:    `Read one metadata key, or omit the key to read all metadata. See kata meta --help for the keys agents use.`,
	},
	"kata meta unset": {
		Example: `  kata meta unset abc4 someday --agent`,
		Long:    `Remove one metadata key. Removing someday returns an issue to the ready queue when no other hold applies.`,
	},
	"kata schedule": {
		Long: `Set scheduled_on. Until that time the issue is left out of kata ready and
kata next. "-" clears it. For no date at all: kata meta set <ref> someday
true --json-value.
Formats: YYYY-MM-DD, local YYYY-MM-DDTHH:MM[:SS], or RFC 3339 UTC ending
in Z. Relative words like "monday" are rejected; compute the date.
When it is reached, the owner (or author if unowned) gets an inbox request.`,
		Example: `  kata schedule abc4 2026-11-02 --agent
  kata schedule abc4 2026-11-02T09:00 --agent
  kata schedule abc4 - --agent`,
	},
	"kata deadline": {
		Example: `  kata deadline abc4 2026-11-15 --agent
  kata deadline abc4 - --agent`,
		Long: `Set deadline_on. A deadline does not park the issue; use kata schedule for that.
"-" clears it. Formats: YYYY-MM-DD, local YYYY-MM-DDTHH:MM[:SS], or RFC 3339
UTC ending in Z. Relative words like "monday" are rejected; compute the date.
When it is reached, the owner (or author if unowned) gets an inbox request.`,
	},
	"kata notify": {
		Long: `Put a request in the recipient's inbox. --to takes an address: <actor> or
<actor>/<teammate>. --message is required unless --clear. One request per
issue and recipient; a new one replaces the old. Clear it after handling.`,
		Example: `  kata notify abc4 --to coordinator --message "Need a decision on the schema" --agent
  kata notify abc4 --to coordinator/teammate-1 --message "Please re-run the tests" --agent
  kata notify abc4 --to coordinator/teammate-1 --clear --agent`,
	},
	"kata inbox": {
		Long: `List open requests addressed exactly to --for (or KATA_INBOX_USER):
<actor> or <actor>/<teammate>. "coordinator" does not include
"coordinator/*". Bound project by default; --all reads every active project
and prints qualified refs. --context emits bounded context for a harness.
--context conflicts with --agent, --json, or --format; omit output selectors.
Reading does not clear a request; clear it with kata notify --clear.`,
		Example: `  kata inbox --for coordinator/teammate-1 --agent
  kata inbox --for coordinator --all --agent
  kata inbox --for coordinator/teammate-1 --context`,
	},
	"kata wait": {
		Example: `  kata wait abc4 d4ex j7m2 --until attention --any --timeout 30m --agent
  kata wait abc4 --until closed --timeout 2h --json`,
	},
	"kata close": {
		Example: `  kata close abc4 --done -m "Fixed Safari double-submit; go test ./auth passes." --commit <sha> --agent
  kata close abc4 --done -m "Docs updated and link check passes for the guide." --pr <url> --test "make docs-check" --agent
  kata close abc4 --duplicate-of d4ex -m "Same Safari race as d4ex." --agent`,
	},
	"kata reopen": {
		Example: `  kata reopen abc4 --comment "Regressed in 0.15; repro attached." --agent`,
		Long:    `Reopen a closed issue when work remains or a regression appears. Add --comment to explain why.`,
	},
	"kata move": {
		Example: `  kata move abc4 example-project --dry --agent`,
	},
	"kata whoami": {
		Long: `Print the client-resolved actor and source (--as, KATA_AUTHOR, USER,
or git). It does not query daemon authentication. For the effective actor
and current issue owner on an authenticated daemon, use kata status <ref>.`,
		Example: `  kata whoami --agent`,
	},
	"kata init": {
		Example: `  kata init --project example-project --agent
  kata init --with-agents --agent`,
	},
	"kata quickstart": {
		Example: `  kata quickstart --format contract
  kata quickstart --agent`,
	},
	"kata events": {
		Example: `  kata events --after 0 --limit 100 --agent
  kata events --tail --agent`,
	},
	"kata delete": {
		Long: `Soft-delete an issue; kata restore undoes it. Requires --force and, for
non-interactive calls, --confirm "DELETE <qualified-id>". Agents: only when
the user asked for this exact issue.`,
		Example: `  kata delete example-project#abc4 --force --confirm "DELETE example-project#abc4" --agent`,
	},
	"kata restore": {
		Example: `  kata restore abc4 --agent`,
		Long:    `Restore a soft-deleted issue. Permanently purged issues cannot be restored.`,
	},
	"kata purge": {
		Long:    `Permanently remove an issue and all its rows. Cannot be undone. Agents: never run without explicit user authorization for this exact ref.`,
		Example: `  kata purge example-project#abc4 --force --confirm "PURGE example-project#abc4" --reason "user asked to remove leaked secret"`,
	},
	"kata doctor": {
		Long:    `Diagnose configuration and connectivity without starting the daemon or changing state. Use kata health for a running daemon.`,
		Example: `  kata doctor --json`,
	},
	"kata health": {
		Long:    `Read the selected daemon’s health. Use kata doctor to diagnose configuration without starting the daemon.`,
		Example: `  kata health --agent`,
	},
	"kata version": {
		Long:    `Print the CLI build identity. --json and --agent provide structured output; kata --version is the root shortcut.`,
		Example: `  kata version --json`,
	},
	"kata sync github enable": {
		Long: `Bind the Kata project (bound or --project) to one GitHub repository (--repo
owner/repo; --host for GitHub Enterprise) and start daemon-side polling.
--since limits the import to issues updated after a date; omit it to keep
the saved cutoff, or pass --since= to clear it. Re-enable keeps every option
you omit. --status-sync two-way also sends close and reopen back to GitHub.`,
		Example: `  kata sync github enable --repo example-org/example-repo --agent
  kata sync github enable --since 2026-09-01 --status-sync two-way --agent`,
	},
	"kata sync github once": {
		Long:    `Run one GitHub sync pass now instead of waiting for the poll interval.`,
		Example: `  kata sync github once --agent`,
	},
	"kata sync github status": {
		Long:    `Show the bound GitHub source and sync progress for this project.`,
		Example: `  kata sync github status --agent`,
	},
	"kata sync github disable": {
		Long:    `Stop GitHub polling for this project. Imported issues stay.`,
		Example: `  kata sync github disable --agent`,
	},
	"kata sync linear enable": {
		Long: `Bind the Kata project (bound or --project) to one Linear team and start
daemon-side polling. The first enable needs --linear-workspace and
--linear-team (UUIDs); --linear-project narrows it to one Linear project.
Re-enable keeps every option you omit. --status-sync two-way also writes
open and closed states back; --open-state and --closed-state pick the target
states.`,
		Example: `  kata sync linear enable --linear-workspace <workspace-uuid> --linear-team <team-uuid> --agent
  kata sync linear enable --status-sync two-way --interval 10m --agent`,
	},
	"kata sync linear once": {
		Long:    `Run one Linear sync pass now instead of waiting for the poll interval.`,
		Example: `  kata sync linear once --agent`,
	},
	"kata sync linear status": {
		Long:    `Show the bound Linear source and sync progress for this project.`,
		Example: `  kata sync linear status --agent`,
	},
	"kata sync linear disable": {
		Long:    `Stop Linear polling for this project. Imported issues stay.`,
		Example: `  kata sync linear disable --agent`,
	},
}

func applyHelpCatalog(cmd *cobra.Command) {
	if doc, ok := helpCatalog[cmd.CommandPath()]; ok {
		if cmd.Long == "" {
			cmd.Long = doc.Long
		}
		if doc.Example != "" {
			cmd.Example = doc.Example
		}
	}
	for _, child := range cmd.Commands() {
		applyHelpCatalog(child)
	}
}
