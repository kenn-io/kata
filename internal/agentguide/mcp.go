package agentguide

// MCPContract is the compact bootstrap for clients that have tools and a file
// editor, but no shell. Richer guidance is fetched from kata.skills on demand.
const MCPContract = `This workspace tracks real changes in Kata through the connected Kata MCP
server. Do not create issues for questions, explanations or no-change reviews.

At session start call kata.skills {"name":"workflow"}; use
kata.skills {"name":"quickstart"} for typed examples and topic guides.
The connection fixes actor and project scope. Read its server instructions;
use project#ref for existing issues in multi-project scope. Supply project
only where the tool schema allows it. Never guess another connection's scope.

Call the applicable kata.load_* tool, then refresh the tool list before using
its tools. Search with kata.search before creating: reuse matching open work,
otherwise kata.create with a stable idempotency_key. Link related work or a
parent. A look-alike refusal means inspect/reuse the match; force_new is only
for genuinely different work. Only create real work the user requested.

Claim your issue with kata.claim. Keep your own work.attention and
work.attention_msg current through kata.set_metadata; record progress with
kata.comment and a stable idempotency_key. Stamp a dedicated work.branch once.
Only write your own work.*; ignore it on closed issues. Do not take over a
sibling's issue or metadata. If blocked, explain what is needed.

Close only verified work with kata.close: reason "done", a substantive message
of at least 40 characters, and typed evidence. An open child prevents parent
closure. Leave unfinished work open, explain what remains and mark needs-review.
Before stopping, close completed work or update both attention fields.
Never call kata.delete or kata.purge without explicit user authorization for
that exact action and issue. Use returned web_url values when linking issues.
`
