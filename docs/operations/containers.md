---
title: Containers
description: Run the stock Kata image using environment settings, mounted secrets, and persistent storage.
last_edited: 2026-10-08
---

# Containers

Kata's release packaging builds `ghcr.io/kenn-io/kata:<version>` for Linux
amd64 and arm64. The image runs the `kata` binary directly as UID/GID `1000`,
with `KATA_HOME=/data` and `daemon start --foreground` as its default command.
It needs no pre-start hook, entrypoint script, config seed, or `.env` rewriting.

These deployment features are unreleased. The external release publisher must
publish a matching version before you use the image commands below. Replace
`<version>` with that published version; older images do not gain these settings.

## Start a persistent service

Create an empty data directory writable by UID/GID `1000`. A bind mount keeps
its host ownership: arrange that ownership before starting the container.
Then run:

```sh
docker run --name kata --restart unless-stopped \
  -p 127.0.0.1:7777:7777 \
  -v /absolute/path/kata-data:/data \
  -e KATA_LISTEN=0.0.0.0:7777 \
  -e KATA_TRUST_PRIVATE_NETWORK=1 \
  -e KATA_WEB_PUBLIC_ORIGIN=http://127.0.0.1:7777 \
  -e KATA_BACKUP_DIR=/data/backups \
  -e KATA_TELEMETRY_ENABLED=off \
  'ghcr.io/kenn-io/kata:<version>'
```

Open `http://127.0.0.1:7777`. On the first eligible non-loopback service start,
Kata creates `/data/auth-token` with mode `0600`; read it as the data-directory
owner and use it for token login. Kata reuses it after a restart. Preserve the
volume and token when replacing the container with a newer image.

For a proxy deployment, set `KATA_WEB_PUBLIC_ORIGIN=https://daemon.example` and
publish the backend only on the trusted container network. The proxy must
preserve that exact Host and Origin. Plain HTTP beyond loopback needs the
explicit private-network trust shown above. Path-prefixed deployments are
unsupported. See [hosted mode](hosted-mode.md) for proxy, probe, and shutdown
requirements.

The shared TCP listener serves both API and browser traffic.
`KATA_WEB_LISTEN` configures a separate browser listener when the API uses a
Unix socket; it does not open a second port alongside a shared TCP API.
On a host-network deployment, `KATA_LISTEN=iface:overlay0:7777` selects an up
interface with exactly one eligible non-public IPv4 address and fails without
fallback if that interface is unavailable.

## Supply secrets and embeddings

Mount a regular secret file owned by UID `1000` with mode `0600`, then set
`KATA_AUTH_TOKEN_FILE=/run/secrets/daemon-token`. Read-only secret mounts work.
The selected credential order is inline > file > named environment variable;
a nonempty `KATA_AUTH_TOKEN` preserves its legacy override. A selected file
failure stops startup rather than falling back or minting a replacement.
See [configuration](../reference/configuration.md#daemon-config) for file limits
and the auto-token opt-out.

Semantic search needs no seeded TOML. Set
`KATA_SEARCH_EMBEDDINGS_BASE_URL=https://embedding.example/v1`,
`KATA_SEARCH_EMBEDDINGS_MODEL=example-model`, and, when needed,
`KATA_SEARCH_EMBEDDINGS_DIMS=1024` and
`KATA_SEARCH_EMBEDDINGS_API_KEY_FILE=/run/secrets/embedding-key`.
Role prompts and dimension requests use
`KATA_SEARCH_EMBEDDINGS_DOCUMENT_PREFIX`, `_DOCUMENT_SUFFIX`, `_QUERY_PREFIX`,
`_QUERY_SUFFIX`, and `_REQUEST_DIMENSIONS`; see the
[environment reference](../reference/configuration.md#environment-variables).
The embedding file has the same owner-only requirements. Existing inline and
named-env embedding credentials retain their precedence and failure policy.

For a separate MCP HTTP service, override the image command with
`mcp serve --all --http 0.0.0.0:8080 --trust-private-network
--http-token-file /run/secrets/mcp-token` and configure its backend daemon
through `KATA_SERVER` and an explicit daemon credential. The inbound MCP token
is a separate secret; see the [MCP reference](../reference/mcp.md).

## Preserve backups

`KATA_BACKUP_DIR=/data/backups` enables immediate and daily full JSONL snapshots.
Use `KATA_BACKUP_INTERVAL` and `KATA_BACKUP_RETAIN` for positive duration
overrides. Copy snapshots off the volume for protection against volume loss.
JSONL does not preserve mounted secrets, `config.toml`, or `/data/auth-token`;
keep those separately. See [backup and restore](backup-restore.md#scheduled-backups).

Replace the image to upgrade the container. Release publication and any app
store cutover happen after the upstream release; this repository does not
publish releases from local workflows.
