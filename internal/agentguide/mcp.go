package agentguide

// MCPCommentRules is CommentRules in tool terms.
const MCPCommentRules = `Comment with kata.comment and a stable idempotency_key only for findings a
later reader needs; keep live status in work.attention_msg. Correct your own
comment with kata.edit_comment. A PR that is open and not merged is unfinished
work: leave the issue open, update work.attention and work.attention_msg and
set the needs-review label.
`

// MCPClaimRules is ClaimRules in tool terms.
const MCPClaimRules = `Claim with kata.claim and ttl_seconds; renew before expiry by claiming again
with ttl_seconds, without if_unowned. Expiry while work.attention is ok sets it
to needs-human; a later claim restores ok.
`

// MCPContract is the compact bootstrap for clients that have tools and a file
// editor, but no shell. Richer guidance is fetched from kata.skills on demand.
const MCPContract = `Track changes in Kata via the connected MCP server. Do not create issues
for questions, explanations or no-change reviews.

At session start call kata.skills {"name":"workflow"}; use
kata.skills {"name":"quickstart"} for typed examples and topics.
The connection fixes actor and project scope. Read its instructions; use
project#ref for existing issues in multi-project scope. Supply project only
where the schema allows it. Never guess another connection's scope.

Call the needed kata.load_* tool, then refresh tools. Search with kata.search
before creating: reuse matching open work, otherwise kata.create with a stable
idempotency_key. Link related work or a parent. A look-alike refusal means
inspect/reuse the match; force_new is for different work. Only create work the
user requested.

` + MCPClaimRules + `Keep your own work.attention and work.attention_msg current through
kata.set_metadata. Stamp a dedicated work.branch once. Only write your own
work.*; ignore it on closed issues. Do not take over a sibling's issue or
metadata. If blocked, explain what is needed.

` + MCPCommentRules + `

Close only verified work with kata.close: reason "done", a message of 40+
characters, and typed evidence. An open child prevents parent closure. Before stopping, close completed work or update both attention fields.
Never call kata.delete or kata.purge without explicit user authorization for
that exact action and issue. Use returned web_url values when linking issues.
`
