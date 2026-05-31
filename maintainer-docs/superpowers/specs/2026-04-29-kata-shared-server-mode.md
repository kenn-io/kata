# kata Shared Server Mode - Future Design Notes

**Status:** Future design note, not v1 scope
**Date:** 2026-04-29
**Topic:** How kata can grow from local daemon mode into a collaborative shared service without forcing a painful HTTP API or database rewrite.

## 1. Purpose

kata v1 is intentionally local-first: a daemon owned by one OS user, a SQLite database under `KATA_HOME`, a Unix socket or loopback TCP endpoint, and no authentication. That is the right initial implementation.

There is a likely future collaborative mode: a kata daemon runs on a VPS, a company network host, or a Tailscale/private-network machine. Developers, agents, CI, and merge automation all talk to that shared daemon. This would let a team share one project issue namespace, see live changes in the TUI, and close issues from trusted automation when work merges.

The risk is not that v1 omits shared mode. The risk is that v1 bakes local-only assumptions into the HTTP API and database schema so deeply that shared mode requires a rewrite. This note captures the guardrails that should influence v1 implementation while keeping shared mode out of v1.

## 2. Core Position

Shared mode should be a distinct deployment mode, not "expose the local daemon on a public interface."

Local mode trust boundary:

- Same OS user.
- Unix socket or loopback TCP.
- No auth.
- CLI/TUI can assume they are talking to the user's daemon.

Shared mode trust boundary:

- Multiple users and agents.
- Network endpoint.
- Authentication required.
- Server owns actor identity.
- Server runs hooks.
- Backups and operational discipline matter.

The same database model and resource API should survive both modes, but transport, authentication, and project resolution need different contracts.

## 3. Deployment Shape

Future shared mode should look roughly like:

```text
kata CLI/TUI/agents --HTTPS/SSH tunnel--> shared kata server --> SQLite on local disk
                                                       |
                                                       +--> SSE
                                                       +--> server-side hooks
                                                       +--> backups
```

Acceptable network patterns:

- HTTPS behind a reverse proxy on an internal network.
- Tailscale/WireGuard/private-network HTTPS.
- SSH tunnel to a loopback-bound server endpoint.

Avoid:

- Binding the v1 auto-start local daemon to `0.0.0.0`.
- SQLite on a network filesystem.
- Treating LAN access as authentication.
- Running developer-local hooks from a shared server event.

## 4. HTTP API Guardrails

### 4.1 Keep All Real Operations API-First

The v1 decision that all reads and writes go through the daemon is important. Do not add direct SQLite reads to the CLI as an optimization unless there is a strong measured reason and a clean fallback. Direct DB access would create an immediate split between local mode and shared mode.

Project operations, issue CRUD, event polling, SSE, search, and ready-list behavior should all be expressed through the HTTP API and generated client.

### 4.2 Separate Workspace Discovery From Project Resolution

This is the biggest local-only trap.

In local mode, `POST /api/v1/projects/resolve` can accept a start path because the daemon runs on the same machine and can read `.kata.toml`, inspect `.git`, and compute aliases.

In shared mode, the server cannot read the developer's checkout path. The client must read `.kata.toml` locally and send a project binding or alias identity to the server.

The implementation should therefore keep these as separate concepts:

```go
type WorkspaceBinding struct {
    ProjectIdentity string
    ProjectName     string
    AliasIdentity   string
    AliasKind       string
    WorkspacePath   string // local hint only; not identity
}
```

Local daemon mode may derive `WorkspaceBinding` from `start_path` inside the daemon. Shared mode can derive the same binding in the CLI using the same library code, then send it to the server.

The important boundary: downstream issue APIs should operate on `project_id` or project identity after resolution. They should not depend on a server-readable filesystem path.

### 4.3 Design Resolve So It Can Grow

V1 can keep:

```http
POST /api/v1/projects/resolve
{ "start_path": "/Users/wesm/code/kata" }
```

But handlers should be structured so a future request can support:

```json
{
  "project_identity": "github.com/company/product",
  "project_name": "product",
  "alias_identity": "github.com/company/product-cli",
  "alias_kind": "git"
}
```

Do not scatter path-walking across API handlers. Keep it in a workspace-resolution package that can run either daemon-side in local mode or client-side in shared mode.

### 4.4 Server-Derived Actor Identity

V1 accepts an `actor` field on mutation bodies because the local daemon trusts the local user. Shared mode must not blindly trust that field.

Future shared behavior:

- `Authorization: Bearer <token>` identifies the caller.
- Server derives the canonical actor from the token.
- If `--as` exists at all, it should be restricted to admin/service tokens or recorded separately as requested impersonation.
- Events store the server-derived actor.

The current `events.actor TEXT NOT NULL` column is still valid. It can store the authenticated actor string later. Avoid adding client-only assumptions around actor provenance.

### 4.5 Error Envelope And Versioning

The existing error envelope and `/api/v1/` prefix are compatible with shared mode. Keep them strict:

- Stable error codes.
- Generated client.
- No Huma default error leakage.
- No local-only text parsing in the CLI.

Auth can be added later with new error codes such as `auth_required`, `token_invalid`, and `project_access_denied`.

## 5. Database Schema Guardrails

### 5.1 Projects Are The Correct Namespace

The project model is the right shared-mode foundation:

- `projects` owns issue numbering.
- `project_aliases` maps one or more git remotes or workspaces to a project.
- `issues.project_id`, `events.project_id`, `links.project_id`, and `purge_log.project_id` make project-scoped behavior explicit.

This avoids a future migration from "repo as namespace" to "project as namespace."

### 5.2 Workspace Paths Are Metadata, Not Identity

Do not make local filesystem paths part of the durable identity model.

For shared mode, developer paths are often private, unstable, and not visible to the server. `project_aliases.root_path` should be treated as last-seen diagnostic metadata only.

Recommended schema wording before v1 migration hardens:

```sql
last_seen_path TEXT, -- nullable local hint, never identity
```

rather than:

```sql
root_path TEXT NOT NULL
```

The stable fields are `projects.identity` and `project_aliases.alias_identity`.

### 5.3 Local Aliases Are Not Portable

`local://<absolute_path>` aliases are useful for local-only projects and repositories without remotes. They are not meaningful across multiple machines.

Shared mode should prefer committed `.kata.toml` project identity and git-remote aliases. A shared server may still accept local aliases for single-user projects, but should not rely on them for team collaboration.

### 5.4 Keep Event Payloads Flexible

The `events.payload` JSON column is the right place for integration metadata. It should remain valid JSON with event-specific structure.

For merge-driven closure, an `issue.closed` event can carry:

```json
{
  "reason": "done",
  "source": {
    "kind": "ci.merge",
    "provider": "github",
    "repo": "github.com/company/product",
    "pull_request": 123,
    "commit": "abc123",
    "branch": "main"
  }
}
```

No git-log scanner is required. CI or merge automation performs an explicit API mutation.

### 5.5 Future Auth Tables Can Be Additive

Do not add auth tables in v1. Shared mode can add them later without changing issue/event semantics:

```sql
users(id, actor, display_name, created_at, disabled_at)
api_tokens(id, user_id, token_hash, name, created_at, last_used_at, revoked_at)
project_memberships(project_id, user_id, role)
```

Start shared mode with coarse tokens if needed; add project-scoped roles only when required. The event log should continue storing actor text snapshots so historical audit remains readable after users are renamed or disabled.

## 6. Merge And Close Workflow

kata should not infer issue closure from git history. That recreates the beads gravity well: commit-message conventions, orphan checks, branch heuristics, and workflow linting.

Preferred model:

1. Developer or agent references an issue in the normal development workflow however the team wants.
2. CI or merge automation knows which issue(s) to close, either from PR metadata, a manually supplied list, or a small project-specific script.
3. Automation calls the shared kata API:

```http
POST /api/v1/projects/{project_id}/issues/{number}/actions/close
Authorization: Bearer <ci-token>
Content-Type: application/json

{
  "reason": "done",
  "source": {
    "kind": "ci.merge",
    "provider": "github",
    "repo": "github.com/company/product",
    "pull_request": 123,
    "commit": "abc123",
    "branch": "main"
  }
}
```

4. Server records actor as the authenticated CI/service identity.
5. TUI and agents see the `issue.closed` event through SSE/polling.

This keeps kata explicit. It supports merge-driven closure without making kata a git workflow engine.

## 7. `.kata.toml` In Shared Mode

V1 `.kata.toml` should stay secret-free:

```toml
version = 1

[project]
identity = "github.com/company/product"
name = "product"
```

Future shared mode may add:

```toml
[server]
url = "https://kata.internal"
```

Credentials must remain outside the repository:

```toml
# ~/.kata/credentials.toml
[[server]]
url = "https://kata.internal"
token = "..."
```

Do not put tokens, user identities, or per-developer state in `.kata.toml`.

## 8. Hooks In Shared Mode

Shared mode hooks run on the server because the server commits the event and owns the hook runner.

Implications:

- Hook commands execute with the server's OS user.
- Hook config is server configuration, not developer checkout configuration.
- A developer's local `.kata.toml` should not cause server-side code execution.
- If a team wants per-project hook policy later, it should be configured server-side or through a controlled admin API.

This matches the v1 decision to defer workspace-local hooks.

## 9. Migration Path

### Phase 1: v1 Local Mode

Build the current local daemon:

- Project-based schema.
- `.kata.toml` project binding.
- All reads and writes through HTTP API.
- Generated client used by CLI and TUI.
- No auth.
- Loopback/Unix socket only.
- Global hooks only.

### Phase 2: Shared-Mode Readiness Pass

Before exposing anything over the network:

- Add auth middleware.
- Add token storage and hashing.
- Add credentials file support in the CLI.
- Add a path-free project resolve request that accepts `WorkspaceBinding`.
- Make the CLI able to parse `.kata.toml` locally using the same resolver library.
- Add server config for explicit listen address.
- Add backup/restore commands or documented backup procedure.

### Phase 3: Shared Server Beta

Run on a VPS or internal host:

- HTTPS or SSH tunnel only.
- SQLite on local disk.
- Server-side hooks.
- CI/service token for merge closure.
- TUI uses SSE against the shared endpoint.

### Phase 4: Access Control If Needed

Only add project memberships, roles, and fine-grained scopes after real use shows they are needed.

## 10. Things To Avoid In V1

- Do not expose the auto-start local daemon on non-loopback addresses.
- Do not add direct CLI reads from SQLite.
- Do not make `root_path` a required durable identity field.
- Do not make git commit scanning part of kata's truth model.
- Do not add auth tables before there is a server mode.
- Do not store secrets in `.kata.toml`.
- Do not make hooks run from developer checkout config on a shared server.

## 11. Immediate Spec Follow-Ups

These are small enough to consider before the initial implementation sprint goes too far:

1. Rename `project_aliases.root_path` to `last_seen_path` and make it nullable.
2. Keep workspace discovery behind a single package/interface that can run daemon-side or client-side.
3. Structure `POST /api/v1/projects/resolve` so it can later accept a path-free `WorkspaceBinding`.
4. Keep mutation handlers independent of filesystem paths after project resolution.
5. Keep event payloads flexible enough to store explicit merge/CI source metadata.

These changes do not implement shared mode. They keep the door open.
