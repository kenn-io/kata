---
title: Containers
description: Run the stock Kata image using environment settings, mounted secrets, and persistent storage.
last_edited: 2026-11-20
---

# Containers

Kata publishes container images at `ghcr.io/kenn-io/kata:<version>` for Linux
amd64 and arm64. The image runs the `kata` binary directly as UID and GID
`1000`, with `KATA_HOME=/data` and `daemon start --foreground` as the default
command. You do not need a pre-start hook, entrypoint script, config seed, or
`.env` rewriting.

Releases 0.19.0 and 0.20.0 did not publish an image. Image publication starts
with the first release after 0.20.0; until then, you can build an image locally
from a Kata checkout with
`docker buildx bake image --load --set image.tags=kata:local`. In the examples
below, replace `<version>` with a published version without the leading `v`, for
example `0.21.0`.

## Start a persistent service

Create an empty data directory writable by UID and GID `1000`. A bind mount
keeps its host ownership; set that ownership before starting the container, then
run:

```sh
docker run --name kata --restart unless-stopped \
  -p 127.0.0.1:7777:7777 \
  -v /absolute/path/kata-data:/data \
  -e KATA_LISTEN=0.0.0.0:7777 \
  -e KATA_TRUST_PRIVATE_NETWORK=1 \
  -e KATA_WEB_PUBLIC_ORIGIN=http://127.0.0.1:7777 \
  -e KATA_BACKUP_DIR=/data/backups \
  'ghcr.io/kenn-io/kata:<version>'
```

Open `http://127.0.0.1:7777`. On the first eligible non-loopback service start,
Kata creates `/data/auth-token` with mode `0600` and reuses it after a restart.
Read that file as the data-directory owner to sign in through token login, and
preserve both the volume and token when replacing the container with a newer
image.

Behind a proxy, set `KATA_WEB_PUBLIC_ORIGIN=https://daemon.example` and publish
the backend only on the trusted container network. The proxy must preserve that
exact Host and Origin. Plain HTTP beyond loopback needs the explicit
private-network trust shown above, and path-prefixed deployments are
unsupported. See [hosted mode](hosted-mode.md) for proxy, probe, and shutdown
requirements.

The shared TCP listener serves both API and browser traffic. `KATA_WEB_LISTEN`
configures a separate browser listener when the API uses a Unix socket; it does
not open a second port alongside a shared TCP API. In a host-network deployment,
`KATA_LISTEN=iface:overlay0:7777` selects an active interface with exactly one
eligible non-public IPv4 address and fails without fallback if that interface is
unavailable.

## Supply secrets and embeddings

To supply your own daemon token, mount a regular secret file owned by UID `1000`
with mode `0600` (read-only secret mounts work) and set
`KATA_AUTH_TOKEN_FILE=/run/secrets/daemon-token`. Credential precedence goes
from inline values, to files, to named environment variables, while a nonempty
`KATA_AUTH_TOKEN` preserves its legacy override. If a selected file fails to
load, startup stops rather than falling back or minting a replacement. See
[configuration](../reference/configuration.md#daemon-config) for file limits and
the auto-token opt-out.

The daemon-mounted MCP settings below require Kata 0.21.0 or later. Enable the
daemon's `/mcp` endpoint with `KATA_MCP_ENABLED=true`, set its actor with
`KATA_MCP_ACTOR=gateway-agent`, choose its scope with
`KATA_MCP_PROJECTS=spoke-project` or `KATA_MCP_ALL_PROJECTS=true`, and mount a
separate bearer file at `KATA_MCP_TOKEN_FILE=/run/secrets/mcp-token` under the
same secret-file rules. Give the gateway the proxied `/mcp` URL and this MCP
bearer, and add the gateway's Host (for example, `kata:7777`) to
`KATA_WEB_ALLOWED_HOSTS`. See the [MCP reference](../reference/mcp.md) and
[`[web].allowed_hosts`](../reference/configuration.md#daemon-config).

To publish selected typed tools in the gateway's first catalog after every
restart, set
`KATA_MCP_STARTUP_SECTIONS=issue_discovery,issue_mutation,coordination,issue_lifecycle,projects,docs`
on the daemon container. Recreate the container with the new environment, then
reconnect the gateway or refresh its catalog. You do not need a loader priming
hook, and all section loaders remain available for progressive loading. An empty
environment value preserves `[mcp].startup_sections`; with neither setting, the
first catalog contains only loaders. See
[Startup sections](../reference/mcp.md#startup-sections) for valid IDs,
capability requirements, and configuration errors.

You can configure semantic search without seeded TOML. Set
`KATA_SEARCH_EMBEDDINGS_BASE_URL=https://embedding.example/v1`,
`KATA_SEARCH_EMBEDDINGS_MODEL=example-model`, and, when needed,
`KATA_SEARCH_EMBEDDINGS_DIMS=1024` and
`KATA_SEARCH_EMBEDDINGS_API_KEY_FILE=/run/secrets/embedding-key`. For role
prompts and dimension requests, use `KATA_SEARCH_EMBEDDINGS_DOCUMENT_PREFIX`,
`_DOCUMENT_SUFFIX`, `_QUERY_PREFIX`, `_QUERY_SUFFIX`, and `_REQUEST_DIMENSIONS`;
see the
[environment reference](../reference/configuration.md#environment-variables).
The embedding file has the same owner-only requirements, and existing inline and
named-env embedding credentials keep their precedence and failure policy.

To run a separate MCP HTTP service, override the image command with
`mcp serve --all --http 0.0.0.0:8080 --trust-private-network --http-token-file /run/secrets/mcp-token`
and configure its backend daemon through `KATA_SERVER` and an explicit daemon
credential. The inbound MCP token is a separate secret; see the
[MCP reference](../reference/mcp.md).

## Preserve backups

Setting `KATA_BACKUP_DIR=/data/backups` enables immediate and daily full JSONL
snapshots. Use `KATA_BACKUP_INTERVAL` and `KATA_BACKUP_RETAIN` for positive
duration overrides. Copy snapshots off the volume to protect against volume
loss. JSONL does not preserve mounted secrets, `config.toml`, or
`/data/auth-token`; keep those separately. See
[backup and restore](backup-restore.md#scheduled-backups).

To upgrade the container, replace the image. Release publication and any app
store cutover happen after the upstream release; this repository does not
publish releases from local workflows.
