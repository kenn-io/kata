# Remote daemon

A daemon can serve clients on other hosts over a private network. This is an
opt-in mode for trusted environments. It is not the same thing as a public
multi-tenant server.

## Server setup

Start the daemon on a literal non-public IP address:

```sh
KATA_AUTH_TOKEN=change-me KATA_TRUST_PRIVATE_NETWORK=1 \
  kata daemon start --listen 100.64.0.5:7777
```

Or configure it persistently in `<KATA_HOME>/config.toml`:

```toml
listen = "100.64.0.5:7777"

[auth]
token = "change-me"
trust_private_network = true
```

The CLI flag wins over config. Auto-started daemons also read the config-file
listener, so a host that should always use one TCP address only needs the config
set once.

Run the daemon under a process manager such as launchd, systemd, or a container
runtime on the host that owns the SQLite database.

## Client setup

Point clients at the daemon:

```sh
export KATA_SERVER=http://100.64.0.5:7777
export KATA_AUTH_TOKEN=change-me
kata list
```

For a per-workspace, gitignored setting:

```toml
version = 1

[server]
url = "http://100.64.0.5:7777"
```

`KATA_SERVER` wins over `.kata.local.toml`.

## Plain HTTP guardrails

Mutable non-loopback HTTP requires both:

- a daemon API token;
- explicit trusted private-network opt-in.

Plain HTTP bearer-token targets must be literal non-public IPs: loopback,
RFC1918, CGNAT, link-local, or ULA. Public IPs and DNS hostnames are rejected
for plaintext bearer auth. Use HTTPS through a reverse proxy or an SSH tunnel
for those shapes.

Unix sockets, loopback HTTP, and HTTPS do not require the same private-network
trust opt-in.

## Identity tokens

For stable per-user attribution, mint DB-backed tokens and enable identity
mode:

```sh
export KATA_AUTH_TOKEN=bootstrap-admin-token
kata tokens create --actor wesm --name laptop
kata tokens list
kata tokens revoke 1
```

Then configure:

```toml
[auth]
token = "bootstrap-admin-token"
trust_private_network = true
require_token_identity = true
```

In identity mode:

- the bootstrap token can create, list, and revoke user tokens;
- the bootstrap token can perform reads;
- attributed writes require a DB-backed token;
- the daemon derives the actor from the token and ignores body-provided actor
  strings for mutations.

Token lifecycle events are stored in the event log and preserved by backup,
restore, and JSONL cutover. Hidden system-project token events are excluded
from ordinary project lists, stats, event feeds, and federation.

## Web or OAuth frontends

A web app can sit in front of kata without daemon-side OAuth. The web app
should authenticate the browser user, map that identity to a kata actor handle,
and call the daemon with a server-side kata bearer token. The browser should
hold only the web app session, never the daemon token.

Because the daemon stores only token hashes, a web app cannot retrieve an
existing plaintext kata token later. It must vault plaintext tokens server-side
or mint and revoke tokens as part of its own session lifecycle.

## Trusted-proxy actor header

When a reverse proxy already authenticates users — through SSO, OAuth, or mutual
TLS — it can assert the kata actor on each request through a configured header,
and the daemon credits that actor instead of any client-supplied value. This is a
third attribution pattern alongside identity tokens (where the daemon is the auth
boundary, for direct clients holding personal tokens) and the web-frontend
pattern above: here the proxy is the auth boundary and the daemon owns only audit
attribution.

The mode is off by default and changes nothing until configured. Enable it by
naming the header and the listeners on which it is honored:

```toml
[auth.proxy]
trusted_actor_header = "X-Kata-Actor"
trusted_proxy_listeners = ["unix:///run/kata/proxy.sock"]
```

The environment overrides are `KATA_TRUSTED_ACTOR_HEADER` and
`KATA_TRUSTED_PROXY_LISTENERS` (comma-separated). An empty or unset env var means
"no override," so a mode enabled in `config.toml` cannot be silently turned off
by an empty environment variable; disabling it requires editing the config.

After merging environment and config:

- Header empty or absent — mode is off; any header is ignored everywhere.
- Header set but `trusted_proxy_listeners` empty — **rejected at config load**. A
  silent no-op here would let an operator believe proxy attribution is on while
  body-supplied actors keep flowing.
- Header and at least one listener set — on for those listeners; every other
  listener passes through unchanged.

### Listener addresses

Listener entries must be **literal** bind addresses that match the daemon's
`listen` value — a Unix socket path (`unix:///run/kata/proxy.sock`) or a specific
`host:port` (`100.64.0.5:7777`). Wildcard binds (`0.0.0.0:7777`, `:7777`) are
never valid entries: an accepted connection reports the specific interface it
arrived on, so a wildcard would never match. A trusted listener should be a Unix
socket or a private IP that only the proxy can reach.

### Security model

The header is trustworthy only because of where it is honored, so the deployment
must hold up its end:

- **Trust is bound to the listed listeners.** A client on any other path cannot
  set the header to spoof an actor. The header is meaningful exactly because
  nothing but the proxy can reach that listener.
- **The proxy must strip any client-supplied copy of the header** before
  forwarding, and must send a single value. The daemon uses the first value of
  the configured header and does not police duplicates; sanitizing inbound
  headers at the proxy boundary is the operator's responsibility.
- **Terminate each attribution mode on its own listener.** On a trusted listener
  the proxy-asserted actor overwrites a token identity, so running direct
  token-holding clients and the proxy on the same listener would discard their
  token identity. Give the proxy a Unix socket and direct clients a separate TCP
  port.
- **A proxy front-end cannot mint or revoke tokens.** Token-admin endpoints stay
  restricted to the bootstrap or loopback path regardless of the header; the
  proxy asserts identity, never token administration.

### Behavior

On a trusted listener the header value becomes the actor and any body or query
actor is ignored. A mutation that arrives on a trusted listener with the header
missing or empty is rejected with `400 actor_header_required` rather than falling
back to a client-supplied actor — otherwise a non-proxied client could omit the
header and claim any identity. Reads carry no actor, never reach actor
resolution, and are never blocked by this mode. The header asserts **identity,
not permission**: it does not add per-operation or per-actor authorization.

## Read-only private-network experiments

For unauthenticated experiments:

```sh
kata daemon start --listen 100.64.0.5:7777 --insecure-readonly
```

This permits GET requests only. Mutations and the event stream still require
authentication. Network ACLs, VPNs, or tailnet policy are the access boundary in
this development mode.

`require_token_identity = true` cannot be combined with
`--insecure-readonly`.

## What this mode does not provide

Remote daemon mode is not a full authorization system. There is no project ACL
model, role model, impersonation scope, OAuth provider, or browser-safe daemon
token flow in the daemon itself. A reverse proxy can still own user
authentication and assert the actor (see
[Trusted-proxy actor header](#trusted-proxy-actor-header)), but kata models
identity, not per-actor permissions. Use it for trusted private deployments where
single-copy state and attribution are enough.
