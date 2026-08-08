# Model Context Protocol server

Kata includes a native stdio server for coding agents and other MCP clients:

```sh
kata --workspace /path/to/repository mcp serve
```

The process binds one Kata project and one actor when it starts. It then serves
project-scoped issue tools until the client closes stdin. JSON-RPC uses stdin
and stdout. Kata writes no status text or logs to stdout.

## Client configuration

Configure the MCP client to start the Kata binary in the target workspace. A
typical command configuration has this shape:

```json
{
  "command": "kata",
  "args": ["--workspace", "/path/to/repository", "mcp", "serve"]
}
```

You can replace `--workspace` with `--project <name>` when the client should
bind an explicit project. Use `--daemon <name>` to select a configured daemon.
The normal Kata client settings also apply, including `KATA_SERVER`, bearer
authentication, private-network checks, and Unix socket discovery.

The actor uses the normal precedence:

1. `--as`
2. `KATA_AUTHOR`
3. `USER`
4. `git config user.name`
5. `anonymous`

The actor and project cannot be changed by a tool call. Start another server
process to use a different identity or project.

## Protocol contract

Kata delegates protocol negotiation to its bundled official Go MCP SDK.
Current clients can use the stateless `server/discover` flow and per-request
metadata; handshake-based clients can use `initialize` followed by
`notifications/initialized`. Both paths expose the same project-bound tools.

Unsupported revisions receive the standard MCP negotiation error with the
revisions supported by the bundled SDK. JSON-RPC batches are rejected. Each
stdio message must be compact JSON on one line and is limited to 8 MiB.

Kata advertises tools only. It does not advertise prompts, resources, roots,
logging, sampling, server-to-client requests, subscriptions, or tool-list
change notifications. Discovery and tool catalogs use a five-minute private
cache hint when the negotiated revision supports it. Tool execution is limited
to 20 starts per second with a burst of 20 and at most eight concurrent daemon
calls per process.

## Tools

| Tool | Behavior |
| --- | --- |
| `kata.search` | Search issue titles, bodies, and comments. |
| `kata.list` | List compact issue summaries with project filters. |
| `kata.show` | Read one issue with bounded comments and relationships. |
| `kata.ready` | List open issues without an open blocking predecessor. |
| `kata.labels` | Discover labels already used in the project. |
| `kata.create` | Create an issue with initial labels, metadata, and relationships. |
| `kata.edit` | Atomically change issue fields, ownership, priority, and relationship deltas. |
| `kata.comment` | Append a progress or context comment. |
| `kata.claim` | Claim an unowned issue for the bound actor. There is no force mode. |
| `kata.set_label` | Ensure that a label is present or absent. |
| `kata.set_metadata` | Patch metadata. A JSON `null` value removes a key. |
| `kata.close` | Close completed work with a reason, message, and typed evidence. |
| `kata.reopen` | Reopen work that needs another change. |

`kata.create` and `kata.comment` require `idempotency_key` so a client can retry
without duplicating work. Other writes are idempotent through the daemon's
natural no-op behavior. Tool results use structured content and include the
bound project identity. Mutations report whether the state changed. They report
`reused` only when the daemon confirms an idempotent reuse. Results also include
the actual event actor when an event exists.
`kata.edit` includes all ordered events and the relationship changes that the
daemon applied. Compact issue results omit labels and blocked state when the
daemon response does not contain those fields; they do not invent empty or
false values.

List, search, and ready results omit issue bodies and comments to keep agent
context small. Their limits default to 20 and cannot exceed 100. `kata.show`
returns at most 100 comments and defaults to the newest 20.

`kata.search` is annotated as open-world because `auto`, `hybrid`, and
`semantic` modes can use the daemon's configured third-party embedding
provider. Select `lexical` when a search must stay inside the Kata daemon.

## Scope and safety

Tool inputs do not accept an actor, workspace, project ID, or project name.
Subject and relationship references must stay inside the bound project. Use a
separate MCP server process for cross-project work. Relationship inputs reject
unscoped full UIDs because they do not prove project membership. They also
reject 26-character short IDs because the daemon wire form cannot distinguish
them from globally resolved ULIDs after safe project-name canonicalization.
Use a bare short ID or a shorter short ID qualified with the bound project
name. Use the Kata CLI for the rare full-length relationship target.

The baseline server deliberately excludes deletion, purge, restore, project
administration, token administration, federation, synchronization, import,
export, recurrence, event streaming, digest, force-create, and force-claim
operations. It also excludes the CLI's priority-aware `kata next` selection;
use `kata.ready` to list ready candidates or the Kata CLI when exact `next`
selection is required. Use the Kata CLI with explicit operator intent for the
excluded workflows.

The daemon remains the authority for authentication, actor policy, claim
conflicts, revisions, relationship rules, close evidence, and all mutations.
MCP validation is an additional client boundary; it does not bypass daemon
policy.
