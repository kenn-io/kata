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
token flow in the daemon itself. Use it for trusted private deployments where
single-copy state and attribution are enough.
