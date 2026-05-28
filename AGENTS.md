# kata — agent guidance

## Agent rules

- Always Commit: Do not leave accepted repository changes uncommitted at the end of a task. Commit the completed work, or explicitly say why no commit was made.
- Do not commit rejected experiments. Revert them or ask before preserving them.
- Test First: Write a failing test before the implementation, then make it pass, then refactor (red, green, refactor). Don't add production code without a failing test that requires it.

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
  children are open and throttles sibling-close bursts.
- Never `kata delete` or `kata purge` without explicit user authorization.

For long-running work, `kata events --tail` streams NDJSON.

## Closing issues

Closing an issue asserts that the work is complete. This is a stronger
claim than a comment. If the work is not actually done, DO NOT close.
Instead:

    kata edit <ref> --label needs-review
    kata comment <ref> --body "what was attempted, what remains"

Close each issue as soon as its work is verified, not at the end of a
batch. The daemon throttles >3 sibling closes by one actor under one
parent in 5 minutes; close eagerly and you will not see it. Operators
can disable the throttle via `[close.throttle] enabled = false` in
`<KATA_HOME>/config.toml`.

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

## Remote-client mode (private network)

A daemon can serve clients on other hosts over a private network:

- Server: for mutable remote access, set `[auth].token` plus
  `[auth].trust_private_network = true` in `<KATA_HOME>/config.toml`, or
  export `KATA_AUTH_TOKEN` plus `KATA_TRUST_PRIVATE_NETWORK=1`, then run
  `kata daemon start --listen 100.64.0.5:7777`. Set
  `listen = "100.64.0.5:7777"` in `<KATA_HOME>/config.toml` so every
  daemon (including the auto-started one) binds TCP.
- Client: `export KATA_SERVER=http://100.64.0.5:7777` or commit a
  gitignored `.kata.local.toml` with `[server] url = "..."` next to
  `.kata.toml`. `KATA_SERVER` env wins.
- Unauthenticated private-network mode is `--insecure-readonly`, which permits
  GET requests only. Mutations and the event stream require bearer auth.
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
binds `0.0.0.0:$PORT`. See [`docs/hosted-mode.md`](docs/hosted-mode.md)
for the required env, health probes, shutdown, and persistence caveats.
