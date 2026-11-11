package agentguide

// MCPGuide returns fixed, native tool guidance. It is independent of local
// files and the server's actor/project binding; that authority stays on the
// connection and is never copied into persistent instructions.
func MCPGuide(name string) (string, bool) {
	call := func(tool, args string) string {
		return "\n```json\n{\"tool\":\"" + tool + "\",\"arguments\":" + args + "}\n```\n"
	}
	const scope = "The connection fixes actor and project scope. Read its server instructions for the actual identity and allowed projects. In multi-project scope, use project#ref for existing issues; create requires project. Pass project only when the tool schema permits it. Call the section loader, then refresh the tool list for its typed schemas. Examples use placeholder projects and refs; replace them with returned values.\n"
	const refresh = "\nRefresh the tool list before calling the newly loaded tools.\n"
	switch name {
	case "workflow":
		return "# Kata agent workflow\n\n" + MCPContract, true
	case "setup":
		block, _ := ManagedBlock("mcp")
		return `# Set up MCP-only repository instructions

No files were read or written. The managed_block structured field is the exact
text to install with your own local file editor. This server cannot edit files
on your machine. If you lack a file tool, present the text as pending setup.
This installs instructions only; it does not bind or initialize a Kata project.
The connection must already authorize the intended project.

1. Inspect local AGENTS.md and CLAUDE.md metadata without following symlinks.
   Refuse a symlinked AGENTS.md; skip a symlinked CLAUDE.md. Refuse non-regular
   files. If AGENTS.md exists, update it. If regular CLAUDE.md exists, update it
   too. If neither exists, create AGENTS.md.
2. Read each selected regular file and retain its preimage and permissions.
   With no Kata markers, append a blank-line separator and the managed block.
   With exactly one begin marker followed by one end marker, replace only that
   inclusive span. Preserve all unrelated bytes, including the final newline.
   Refuse a file with orphaned, reversed, duplicate or malformed markers; do not
   guess. Continue with the other selected file and report the refusal.
   Both instruction modes use these same markers, so switching replaces one span.
3. If a complete Beads integration block exists, leave the original untouched.
   Its begin marker starts with <!-- BEGIN BEADS INTEGRATION and its end is
   <!-- END BEADS INTEGRATION -->.
   Propose a sibling <filename>.kata-proposed with that block removed and Kata's
   block installed. Never overwrite an existing sidecar. If its boundaries are
   unclear, stop and ask the user to resolve the conflict.
4. Before writing, compare file content and metadata with the preimage; stop if
   either changed. Recheck symlink status. Use an atomic replacement preserving
   permissions, or an exclusive create for a new file/sidecar. Skip identical
   content. If your file tool cannot enforce these rules, return a proposal.
5. Verify the readback and unchanged outside text. Report which files you
   actually wrote, which were unchanged and which remain pending. Never claim
   installation merely because this server returned content.

Exact managed text (the historical markers are identifiers, not commands):

` + block + "\n", true
	case "quickstart":
		return "# Kata MCP quickstart\n\n" + scope + `
Use Kata for real changes the user requested. Fetch the workflow guide at session
start and the setup guide when you need managed repository instructions. Other
guides cover issues, claiming, closing, teammates, scheduling and events.

Search before creating; every query word must match. Start with one or two
distinctive words. Reuse matching open work; create only when no issue covers it.
` + call("kata.load_issue_discovery", `{}`) + refresh +
			call("kata.search", `{"query":"example task","project":"spoke-project"}`) +
			call("kata.load_issue_mutation", `{}`) + refresh + `
If creation is needed, use a stable key for this operation. Retry with the same
key and arguments. A look-alike refusal means inspect/reuse the returned match;
force_new is only for genuinely different work. Capture the returned issue ref
and web_url. Replace spoke-project#abc4 below with that actual qualified ref.
` + call("kata.create", `{"project":"spoke-project","title":"Complete example task","body":"Make the requested change and review the result.","idempotency_key":"example-task-create-1"}`) +
			call("kata.claim", `{"ref":"spoke-project#abc4","if_unowned":true}`) +
			call("kata.set_metadata", `{"ref":"spoke-project#abc4","patch":{"work.attention":"ok","work.attention_msg":"Implementing the requested change."}}`) + `
Do the requested work. Record findings useful to the next reader with a new
stable comment key; keep your own attention fields current. Read delegated
work.* without writing it. Stamp work.branch once when using a dedicated branch.
` + call("kata.comment", `{"ref":"spoke-project#abc4","body":"Completed the requested change and reviewed the result.","idempotency_key":"example-task-progress-1"}`) +
			call("kata.show", `{"ref":"spoke-project#abc4"}`) +
			call("kata.load_issue_lifecycle", `{}`) + refresh + `
Close only after verification, with a message of at least 40 characters and
typed evidence describing actual completed work. The example's external account
must be replaced with your own evidence. Open children prevent parent closure.
` + call("kata.close", `{"ref":"spoke-project#abc4","reason":"done","message":"Completed the example task and reviewed the resulting changes.","evidence":[{"type":"external","account":"Checked the fixture issue and its progress through the MCP client."}]}`) + `
If unfinished, leave it open, record what remains, set needs-review with
kata.set_label and update both attention fields. Never delete or purge without
the user's explicit authorization for that exact action and issue.
`, true
	case "issues":
		return "# Find, create and update issues\n\n" + scope + `
Load kata.load_issue_discovery for search/show; load kata.load_issue_mutation
for create/edit/comment/labels/metadata. Refresh the tool list after each load.
Search matches every word; use fewer words after an empty result. Reuse matching
open work. Create only real requested changes with a stable idempotency_key.
` + call("kata.search", `{"query":"export","project":"spoke-project"}`) +
			call("kata.create", `{"project":"spoke-project","title":"Fix export retry","body":"Retry the requested export safely.","idempotency_key":"export-retry-create-1"}`) + `
Link related work with add_related; use parent for grouped work. Blocks and
blocked_by are readiness prerequisites, not grouping. Remove parent strictly
with its current ref; relationship removals otherwise are idempotent.
` + call("kata.edit", `{"ref":"spoke-project#abc4","add_related":["spoke-project#d4ex"]}`) +
			call("kata.comment", `{"ref":"spoke-project#abc4","body":"The retry path preserves the original export identity.","idempotency_key":"export-retry-finding-1"}`) + `
Use reply_to and kind (reply, confirm, refute, supersede) for threaded responses.
Confirm asserts verification; confirm/refute require at least 40 evidence
characters. Field/relationship edits and metadata patches are separate calls.
Use returned web_url values for links rather than constructing URLs.
`, true
	case "claiming":
		return "# Pick up and own work\n\n" + scope + `
Load kata.load_issue_discovery and refresh for next/ready/show. Inspect an issue
before claiming. Load kata.load_issue_mutation and refresh for claim.
` + call("kata.next", `{"project":"spoke-project","unowned":true}`) +
			call("kata.claim", `{"ref":"spoke-project#abc4","if_unowned":true}`) + `
A current owner conflict is not permission to force takeover. if_unowned is an
atomic claim guard. ttl_seconds may bound a claim from 60 to 86400 seconds.
Load kata.load_coordination and refresh to release only the expected owner.
` + call("kata.unassign", `{"ref":"spoke-project#abc4","expected_owner":"example-agent"}`) + `
Maintain your own work.attention (ok, stuck, needs-human) and work.attention_msg.
Read sibling work.* without writing it; ignore work.* on closed issues.
`, true
	case "closing":
		return "# Close with evidence\n\n" + scope + `
Load kata.load_issue_lifecycle and refresh. done requires a substantive message
of at least 40 characters and typed evidence. Use actual commit SHA, PR URL,
executed test command, reviewed paths or an external completion account.
` + call("kata.close", `{"ref":"spoke-project#abc4","reason":"done","message":"Implemented the export retry and reviewed the completed changes.","evidence":[{"type":"reviewed-paths","paths":["export.go"]}]}`) + `
Never close unfinished work. Load kata.load_issue_mutation and refresh, record
what remains with kata.comment and a stable idempotency_key, then label it:
` + call("kata.set_label", `{"ref":"spoke-project#abc4","label":"needs-review","present":true}`) + `
Update both attention fields before handoff. Open children prevent parent close.
duplicate/superseded require at least 20 message characters and exactly one
matching duplicate-of/superseded-by evidence with issue_ref. wontfix requires
60 message characters and no evidence. audit-no-change needs a 40-character
message plus no-change-audit rationale; reviewed-paths may supplement it.
kata.reopen reverses an incorrect close. Deletion and purge require explicit
user authorization for that exact issue and action, plus exact confirmation.
`, true
	case "teammates":
		return "# Teammates, inbox and attention\n\n" + scope + `
The actor comes from server startup. A startup teammate is the default; create
and comment accept teammate overrides when agents share a connection. Empty
teammate suppresses the default. Keep the actor and assign distinct teammates.
Load kata.load_issue_mutation and refresh for attributed progress and attention.
` + call("kata.comment", `{"ref":"spoke-project#abc4","teammate":"worker-1","body":"The export fixture is ready for review.","idempotency_key":"worker-1-ready-1"}`) +
			call("kata.set_metadata", `{"ref":"spoke-project#abc4","patch":{"work.attention":"needs-human","work.attention_msg":"Ready for the assigned reviewer."}}`) + `
Only write your own work.* keys. Delegated issues may have a parent and branch
metadata; use real blocking relationships only for prerequisites.
Load kata.load_coordination and refresh to read pending notify.* requests:
` + call("kata.inbox", `{"for":"example-agent/worker-1","project":"spoke-project"}`) + `
Inbox requests are separate from work.attention. Omitting project aggregates
within the server's startup scope. This version has no notification send/clear
tool; an operator must handle those requests. Do not invent a tool name.
kata.wait currently waits on status only. For attention, use bounded kata.show
polling with your own deadline: closure completes first, then needs-human or
stuck. Report timeout distinctly. Read delegated attention; never overwrite it.
`, true
	case "scheduling":
		return "# Park, schedule and set deadlines\n\n" + scope + `
Load kata.load_issue_mutation and refresh. Schedule gates readiness; a deadline
does not. Date-only/local values use the issue's timezone metadata first, then
the daemon timezone, then UTC. Explicit UTC instants end in Z.
` + call("kata.set_schedule", `{"ref":"spoke-project#abc4","schedule":"2027-01-10"}`) +
			call("kata.set_deadline", `{"ref":"spoke-project#abc4","deadline":"2027-01-15"}`) +
			call("kata.set_metadata", `{"ref":"spoke-project#abc4","patch":{"someday":true}}`) + `
Use someday=true when the start date is unknown; remove it with JSON null.
Clear dates with clear_schedule or clear_deadline, not an invented date string.
` + call("kata.set_schedule", `{"ref":"spoke-project#abc4","clear_schedule":true}`) + `
Reached schedules/deadlines generate notify.* requests for the current owner,
or author when unowned. Read them through kata.inbox; ask an operator to clear
them on versions without a notification write tool.
`, true
	case "events":
		return "# Follow activity with events\n\n" + scope + `
Load kata.load_activity and refresh. Poll from a cursor or request a bounded
wait. Resume with after set to next_after_id from the last successful response.
` + call("kata.events", `{"after":0,"limit":50}`) + `
If reset_required is true, rebuild your view through fresh scoped reads before
resuming. Do not treat an incomplete/reset stream as proof that nothing changed.
Event reads and waits do not grant authority beyond the connection's scope.
`, true
	default:
		return "", false
	}
}
