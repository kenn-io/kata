# kata — agent guidance

## Agent rules

- Always Commit: Do not leave accepted repository changes uncommitted at the end of a task. Commit the completed work, or explicitly say why no commit was made.
- Never Squash or Amend: Do not squash commits, amend commits, or otherwise rewrite git history unless the user explicitly asks for that history rewrite.
- Do not commit rejected experiments. Revert them or ask before preserving them.
- Never invoke the `roborev review` CLI command in any form unless the user
  explicitly asks for it. Use all other `roborev` CLI commands normally when
  they are appropriate for interacting with roborev. Never invoke a roborev
  skill (including `roborev-fix` or `roborev-design-review-branch`) unless the
  user explicitly asks for that skill.
- Test First: Write a failing test before the implementation, then make it pass, then refactor (red, green, refactor). Don't add production code without a failing test that requires it.
- Explicit Database Change Consent: Do not add, remove, or modify a production
  database migration or persisted database schema without the user's explicit
  consent in the current conversation. This includes canonical/bootstrap DDL,
  schema-version constants, migration assets and registries, migration or
  cutover runners, and changes to tables, columns, indexes, constraints,
  triggers, functions, extensions, or other persisted database objects. A
  feature request, backend-completion request, green test suite, unreleased or
  pre-alpha status, or editable-in-place schema snapshot does not imply this
  consent. Before editing, state the exact backend and proposed persisted-state
  changes and wait for approval. Ephemeral test-local DDL that does not define
  product persisted state is exempt. After approval, follow
  `docs/development/postgres-migrations.md` for PostgreSQL migration history
  and verification rules.
- No Private Project Data: Do not use private project, workspace, customer,
  host, or repository names in tests, docs, examples, planning documents,
  design notes, issue or bug reports, review responses, PR text, or generated
  fixtures. Use neutral placeholders such as `spoke-project`, `hub-project`,
  `example-workspace`, or `daemon.example` unless the user explicitly asks
  to preserve a real name in a user-facing operational command.
- No Testing/Verification Sections in PRs: Do not include a testing plan, test plan, verification,
  or verified section in pull request descriptions.
- No Unrequested GitHub Comments: Do not comment on GitHub issues or pull
  requests unless the user explicitly instructs you to post a comment.
- No Bash Content-Assertion Tests: Do not add bash tests that assert shell
  script, workflow, or config behavior by grepping for implementation text.
  These are usually tautological: they prove only that the file contains the
  string the test searched for, not that the release, installer, or workflow
  works. Prefer exercising the command/tool behavior directly, using the tool's
  own validation command, or writing a real parser-backed test. If direct
  behavior coverage is not practical, document the manual check instead of
  adding a brittle shell content test.

## Documentation and release PRs

Update documentation as behavior lands whenever practical: CLI flags, config
keys, operator workflows, installation paths, and user-facing contracts should
not wait for a release-only sweep when they can be documented with the code.

After a release is tagged and confirmed available, do a normal post-release
documentation pass based on the release changelog. This final pass should add
or refresh release notes, check overview/navigation paths, and make sure the
public docs describe the shipped state accurately.

When opening a post-release docs PR, describe it as standard release
finalization. Do not frame the update as a surprising docs gap, a failure of
prior work, or a problem caused by the release tag. Good framing:

- "Finalizes the public docs for the 0.6.0 release using the tagged changelog."
- "Adds the 0.6.0 changelog entry and updates overview links for the shipped
  GitHub sync and Windows installer docs."

## Project management

This project tracks its own work in **kata**. Run `kata quickstart` at the
start of each session for the agent contract; the short version:

- Author defaults to `$KATA_AUTHOR > $USER > git user.name`; set
  `KATA_AUTHOR` only if you need a different actor (e.g. an agent
  handle distinct from your login).
- Issue refs use short_ids derived from each issue's ULID. In a bound
  workspace the bare form (`abc4`) is enough; cross-project references
  qualify with the project name (`kata#abc4`). Full 26-char ULIDs are
  also accepted. Legacy numeric refs (`#12`, `12`) no longer resolve.
- `kata list --json` to see open work; `kata show <ref> --json` for detail.
- Search before creating: `kata search "<keywords>" --json`.
- Update existing issues over creating duplicates (`kata comment`,
  `kata label add`, `kata edit --blocks/--blocked-by/--related/--parent`).
- Relationships live on `kata create` and `kata edit` as flags, framed
  from the operating issue's POV. Repeatable except `--parent`. Removes
  are `--remove-parent` (strict; must equal current) plus idempotent
  `--remove-blocks/--remove-blocked-by/--remove-related`.
- Close only when the work is actually complete:
  `kata close <ref> --done --message "<scope + verification>" --commit <sha>`.
  Use `--duplicate-of <ref>`, `--superseded-by <ref>`, `--audit-no-change`, or
  `--wontfix` when those reasons fit. The daemon refuses parent-close while
  children are open.
- Never `kata delete` or `kata purge` without explicit user authorization.

For long-running work, `kata events --tail` streams NDJSON.

## Closing issues

Closing an issue asserts that the work is complete. This is a stronger
claim than a comment. If the work is not actually done, DO NOT close.
Instead:

    kata label add <ref> needs-review
    kata comment <ref> --body "what was attempted, what remains"

Close each issue as soon as its work is verified, not at the end of a
batch. By default the daemon allows sibling close bursts when each close
has valid evidence and a substantive message. Operators can enable stricter
burst/prose throttling via
`[close.throttle] enabled = true` in `<KATA_HOME>/config.toml`.

When the work IS done, close with substantive prose and typed
`--evidence` so a reviewer can verify the claim later:

    kata close abc4 --done \
      --message "Fixed Safari callback double-submit; verified tests pass." \
      --commit <sha>

Other close forms:

    kata close abc4 --duplicate-of d4ex  --message "Same Safari race condition."
    kata close abc4 --superseded-by d4ex --message "Replaced by broader scope."
    kata close abc4 --wontfix --message "<>=60 chars of rationale>"
    kata close abc4 --audit-no-change \
                    --message "Reviewed schema and queries; no change needed." \
                    --evidence "no-change-audit:schema unchanged after review" \
                    --reviewed internal/db/schema.sql

The daemon refuses parent-close while open children remain. Reviewers
can replay activity with `kata audit closes` and undo specific lazy
closes with `kata reopen <ref>`.

## Federation trust/threat model (security-review triage)

kata federation is mutual trust between a spoke and each hub it joins;
there is no multi-tenant authorization model (`docs/design/federation.md`,
"Tokens And Trust Boundaries" and "No Multi-Tenant Authorization Model").
Triage security findings against that model:

- **Out of scope: a hostile federated hub.** A hub the spoke joined is
  trusted with that project's data and identity. Example: a hub "forging"
  a project UID it already knows is not a defended attack — UIDs are
  unguessable ULIDs, so UID equality means the same project. Don't fix
  these; document the boundary inline where reviews keep re-flagging it
  (see the rejoin note in `internal/daemon/handlers_federation.go`).
- **Real, fix them: credential misrouting and partial/stranded state.**
  A bearer token must only reach the origin it was configured for: the
  local daemon's global token never goes to a hub, and a catalog entry's
  admin token never goes to a different hub origin (`--hub-token` is the
  only deliberate cross-origin path). Teardown/retry paths must not
  strand state (archive-before-detach; leave's idempotent resume still
  runs daemon-side credential cleanup).

## Web UI trust/threat model

The web UI is another client of the Kata daemon, not a separate proxy or
authorization service. Review browser changes against these boundaries:

- **Direct local browser authority trusts the loopback host.** On Unix, the daemon normally
  keeps its API on the owner-only socket and binds a separate ephemeral
  loopback browser listener. A direct tab on that listener creates its own
  local-web session only when the request arrives directly from loopback, the
  exact Host and Origin match, no forwarding headers are present, and daemon
  token/identity/proxy authentication is unconfigured. This matches the
  established keyless local-web convention; processes and
  users able to connect from the same host are inside this browser boundary.
  `kata ui` and the URL advertised by `kata daemon status` both open this
  origin directly; no launch credential or CLI authorization ceremony exists.
  The local-session path grants ordinary UI authority, never token
  administration, and is disabled for non-loopback, proxied, and authenticated
  deployments.
- **A browser session is split authority.** Every browser data request requires
  both the HttpOnly instance cookie and `X-Kata-Web-Session`. Neither credential
  is sufficient alone. The cookie is ambient and shared by tabs for one daemon
  instance; the session header and matching `X-Kata-CSRF` value are tab-local.
  Issuing or logging out one tab must not rotate/delete the ambient cookie or
  invalidate another tab. Mutations additionally require the tab's CSRF value
  and same-origin request validation. Database-token-backed
  sessions must stop working when that token is revoked, and all browser
  sessions have a bounded lifetime. SSE streams revalidate that authority before
  every event, reset, and heartbeat so logout, expiry, and token revocation also
  terminate already-open streams. Authenticated responses are private and
  non-cacheable, and vary on both session credentials so a reverse proxy cannot
  replay one browser principal's data to another.
  Browser authentication and credentialed API fetches reject redirects so
  daemon tokens and session credentials cannot cross origins.
- **Capabilities follow the browser principal.** An owner-local direct session
  becomes a local-web principal that may perform ordinary attributed UI writes
  but may not administer tokens. Configured logins retain their token principal;
  in identity mode the bootstrap principal remains non-writable. A browser 401
  clears tab-local credentials, fences accepted snapshots and drafts from
  further writes. A keyless loopback tab attempts one transparent local-session
  renewal; authenticated deployments return to their explicit token-login
  mechanism. An explicit login waits for token exchange before reading
  authority. Every mutation path, including project creation, uses that
  rejection path. Mutation controls remain disabled when authority is stale or
  read-only and while another mutation is pending; project creation is not an
  exception.
- **Host validation is the DNS-rebinding boundary.** Dedicated browser
  listeners validate the exact configured browser authority before session or
  route handling. A shared TCP listener validates every request Host against
  its configured public authority, concrete backend authority, and exact
  `[web].allowed_hosts` entries before classifying the request; this includes
  ordinary API reads with no browser markers. `allowed_hosts` is for explicit
  container/proxy/backend aliases, contains authorities rather than origins,
  and is never inferred from request headers. Browser routes and
  browser-bearing mutations require the exact configured Origin even when the
  header would otherwise be absent, while ordinary CLI, health, and federation
  requests continue through daemon
  bearer/private-network authentication. A bearer header never bypasses Host
  validation. Bearer CLI event streams remain CLI traffic. Do not weaken one
  boundary to make the other client class work.
- **Plain HTTP is local or explicit private-network trust.** Loopback browser
  origins may use HTTP. A non-loopback HTTP public origin requires the same
  explicit `[auth].trust_private_network` opt-in used for plaintext private
  network daemon access and is intended only for an operator-trusted overlay.
  Otherwise use HTTPS termination. Public internet and hostile shared networks
  are out of scope, but accidental unauthenticated/public exposure is a defect.
  Configured-remote UI login applies the same bearer-target validation before
  opening a page that can submit a daemon token; plaintext non-loopback targets
  require the applicable private-network trust or `allow_insecure` opt-in.
- **Proxy authority is configured, never inferred.** `web.public_origin` is the
  exact canonical browser origin, with default HTTP/HTTPS ports removed to
  match browser `Host` and `Origin` serialization. Forwarded headers do not
  change it, and path-prefixed deployments are unsupported. Proxies must
  preserve that origin, streaming, the authenticated-response cache policy,
  and the root-relative SPA/API routes.
- **Development runs are isolated production processes.** Web tooling must
  launch the branch daemon with a temporary Kata home, workspace, and database.
  It must construct child environments from an explicit OS/toolchain allowlist
  before applying those temporary paths; daemon/database/server/config targets,
  credentials, proxies, and hosted `PORT` settings are not inherited.
  On Windows it must explicitly bind the daemon's shared TCP listener to the
  selected temporary backend port because the separate web listener is unused.
- **Anonymous mode is strictly read-only.** `--insecure-readonly` may serve the
  shell and read snapshots without a browser session, but it must not mint
  writable authority, accept mutations, or expose session/token administration.
  `kata ui` opens that validated origin directly. Cookie-only ambient state is ignored; a session header still
  opts into the cookie-plus-header authentication path.
- **CSP keeps code strict while allowing required layout state.** Production
  scripts remain same-origin-only. Dynamic component positioning and CSS custom
  properties require the narrow `style-src-attr 'unsafe-inline'` allowance;
  never extend that exception to scripts.
- **Snapshot consistency is a security and correctness boundary.** Matching
  snapshot ETags perform no projection reads, and a built snapshot captures its
  cursor inside the same consistent backend read. SQLite and PostgreSQL must
  preserve identical scoping, filtering, and project isolation. A
  `sync.reset_required` frame latches unconditional refresh until a `200`
  snapshot succeeds; failed resets retry and cannot fall through to ETag-based
  `304` acceptance or writable stale authority. Every visible mutation,
  including project merge, durably advances the cursor. Project catalog
  mutations, including config-reconciled federation replica creation and
  host-service project creation and recurrence CRUD, return their exact
  transaction event, broadcast it to live clients, and enqueue it for mutation
  hooks without post-commit rediscovery. Deliver the event immediately after
  commit, before response-only reads or credential persistence can fail. If
  project initialization fails after commit, it still delivers the retained
  event; successful orphan cleanup instead broadcasts the reset cursor reserved
  by that cleanup transaction. Recurrence deletion requires `If-Match` and
  checks the visible revision atomically in the deletion transaction.
  User-initiated project events retain the attributed actor; `system` is
  reserved for internal project creation/rename/merge. Project and recurrence
  events remain hook-eligible; exact and `*` subscriptions recognize them.
  Relationship and link projections exclude endpoints in archived projects.

- **Release publishers provision the web build.** Every GoReleaser entry point
  runs the web release check, so it must install the pinned Node and Bun
  toolchain plus frozen web dependencies before it can create or upload a
  release.

## Remote-client mode (private network)

A daemon can serve clients on other hosts over a private network:

- Server: for mutable remote access, set `[auth].token` plus
  `[auth].trust_private_network = true` in `<KATA_HOME>/config.toml`, or
  export `KATA_AUTH_TOKEN` plus `KATA_TRUST_PRIVATE_NETWORK=1`, then run
  `kata daemon start --foreground --listen 100.64.0.5:7777`. Set
  `listen = "100.64.0.5:7777"` in `<KATA_HOME>/config.toml` so every
  daemon (including the auto-started one) binds TCP.
- Client: `export KATA_SERVER=http://100.64.0.5:7777` or commit a
  gitignored `.kata.local.toml` with `[server] url = "..."` next to
  `.kata.toml`. `KATA_SERVER` env wins.
- Unauthenticated read-only private-network mode is `--insecure-readonly`,
  which permits GET requests only. Mutations and the event stream require
  bearer auth unless the server explicitly sets
  `[auth].allow_unauthenticated_private_network_writes = true` (or
  `KATA_ALLOW_UNAUTHENTICATED_PRIVATE_NETWORK_WRITES=1`) on a literal private IP
  bind. That writable tokenless mode treats the private network as the access
  boundary, uses client-supplied actors, and blocks token administration.
- Trusted plaintext bearer targets must be literal non-public IPs (loopback,
  RFC1918, CGNAT, link-local, ULA). Public IPs and DNS hostnames require HTTPS
  termination or an SSH tunnel.
- Init and resolution are both path-free whenever the client can
  derive the project name locally (existing `.kata.toml`, `--project`, or a
  git workspace): the client sends `name` and writes `.kata.toml` itself; the
  daemon never stats the client's filesystem.
  `kata init` falls back to a path-based request only when none of
  those sources are available, so the daemon (or its absence) emits
  the existing validation error.

## Hosted mode (`$PORT` convention)

When hosted on a PaaS that follows the Heroku-style `$PORT` contract
(Cloud Run, Render, Fly.io, Railway, App Engine, etc.), the daemon
binds `0.0.0.0:$PORT`. See
[`docs/operations/hosted-mode.md`](docs/operations/hosted-mode.md) for the
required env, health probes, shutdown, and persistence caveats.
