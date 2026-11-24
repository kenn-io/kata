package agentguide

// MCPCommentRules is CommentRules in tool terms.
const MCPCommentRules = `Comment with kata.comment and a stable idempotency_key only for findings a
later reader needs; keep live status in work.attention_msg. Correct your own
comment with kata.edit_comment. A PR that is open and not merged is unfinished
work: leave the issue open, update work.attention and work.attention_msg and
set the needs-review label.
`

// MCPClaimRules is ClaimRules in tool terms.
const MCPClaimRules = `Claim with kata.claim and ttl_seconds; renew before expiry using ttl_seconds,
without if_unowned.
`

// MCPContract is the compact bootstrap for clients that have tools and a file
// editor, but no shell. Richer guidance is fetched from kata.skills on demand.
const MCPContract = `Track changes in Kata through the connected MCP server. Do not create issues
for questions, explanations or no-change reviews.

At session start call kata.skills {"name":"workflow"}; use
kata.skills {"name":"quickstart"} for typed examples and topic guides.
Read actor and project scope in the server instructions;
use project#ref for existing issues in multi-project scope. Supply project
only where the tool schema allows it. Never guess another connection's scope.

Call the applicable kata.load_* tool, then refresh the tool list before using
its tools. Search with kata.search before creating: reuse matching open work,
otherwise kata.create with a stable idempotency_key. Link related work or a
parent. A look-alike refusal means inspect/reuse the match; force_new is only
for genuinely different work. Only create real work the user requested.
Never add p0-p4 or priority: labels; set priority.

` + MCPClaimRules + `Keep your own work.attention and work.attention_msg current through
kata.set_metadata. Stamp a dedicated work.branch once. Only write your own
work.*; ignore it on closed issues. Do not take over a sibling's issue or
metadata. If blocked, explain what is needed.

` + MCPCommentRules + `

Close only verified work with kata.close: reason "done", a substantive message
of at least 40 characters, and typed evidence. An open child prevents parent
closure. Before stopping, close completed work or update both attention fields.
Never call kata.delete or kata.purge without explicit user authorization for
that exact action and issue. Use returned web_url values when linking issues.
`
