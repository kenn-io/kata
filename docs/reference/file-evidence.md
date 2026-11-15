---
title: File evidence
description: Cite configured local or remote files, capture raw-byte SHA-256, and retrieve or compare retained references.
last_edited: 2026-11-15
---

# File evidence

Close an issue with a portable file reference, then retrieve or compare its bytes
later. The daemon records the source alias, relative path, its origin instance
UID, byte count, optional SHA-256, and an optional source revision in the close
event. File content stays with the source. A digest detects changes; it does not
retain old bytes or replace the independent confirmation required by
`verify.required=true`.

## Configure a source on its owning daemon

Add a source to `<KATA_HOME>/config.toml` and restart that daemon:

```toml
[file_evidence.sources.reports]
root = "/srv/evidence"
sha256 = true

[file_evidence.sources.notes]
command = ["/usr/local/bin/markdown-evidence-adapter"]
sha256 = true
env = ["EXAMPLE_ADAPTER_TOKEN_FILE"]
```

Choose exactly one absolute `root` or command argv per alias. There is no shell
expansion. Aliases are stable dataset identities: never repoint an existing
alias to another dataset. An origin instance UID and alias together distinguish
sources. The root belongs to the daemon, including when the client runs on
another machine; client mount paths have no meaning here.

Configuring a source is a daemon-wide publication grant to eligible closers.
They can cite any allowed file in that source. Authorized readers of that issue
can then read current and future contents at that path. Use roots containing
only publishable material. References also expose relative paths and hashes to
issue history, exports, and federated peers. There is no per-project source ACL.
Existing read-only/browser/issue permissions still apply to the retrieval API.

Portable paths use `/`, are relative and canonical, and cannot contain `..`,
empty components, backslashes, drive/stream colons, or NUL. Local reads reject
observed descendant symlinks and nonregular files. The configured root itself
is trusted and may resolve a symlink. Go's root handle confines reads even
through descendant replacement; it is not a hostile-writer snapshot protocol.
Inode, size and modification checks refuse ordinary concurrent edits. Keep files
stable during capture. Files over 16 MiB are refused rather than truncated.

## Cite and read files

```sh
kata close abc4 --done \
  --message "Completed the requested change and retained its full report." \
  --evidence file:reports:results/report.md

# Request SHA-256 even when the source default is off.
kata close abc4 --done \
  --message "Completed the requested change and retained its full report." \
  --evidence file-sha256:notes:results/report.md

kata show abc4 --json
kata evidence abc4 --index 0 --json
kata evidence abc4 --index 0 --content --json
kata evidence abc4 --event <close-event-uid> --index 0 --historical --json
```

`file` uses the source's `sha256` setting. `file-no-sha256:source:path` explicitly disables capture. API and MCP callers can set
`capture_sha256` to true or false to override it. Only source, path, and that
capture preference are close inputs; origin, SHA-256, size and revision are
outputs. Requested capture failure leaves the issue open. Dry runs resolve
sources but do not mutate issues, so adapters must be read-only. At most 16 file
entries are accepted per close. Idempotent retries reuse the original receipt
without reading the source again.

File evidence is accepted for `done`. It can also accompany `audit-no-change`,
which still requires its rationale. The existing Docbank `--docbank` and
attachment evidence workflow retains its exact-version descriptors.

SHA-256 covers every raw byte, including BOMs, line endings, NUL, invalid UTF-8
and final newlines. A byte change changes the digest, subject to cryptographic
collisions; metadata-only changes do not. SHA-256 is distinct from Git object
identifiers and needs no repository or commit.

The default read selects the effective current close, preserving reopen and
imported-status semantics. An explicit event UID selects visible historical
close evidence. `index` is zero-based among **file entries only**, in retained
order; removing a private verdict citation never shifts file indexes. Responses
include the selected event UID, index, original reference and status. Content is
optional complete base64, never raw bytes printed into a terminal. Show and
audit expose retained metadata; ordinary history/export/federation retain it in
the canonical close payload without reading any source.

| Status | Meaning |
|---|---|
| `unchanged` | Retrieved bytes match the captured SHA-256 and count. |
| `changed` | Retrieved bytes differ from captured SHA-256 or count. |
| `unverified` | Read succeeded; the reference has no digest to compare. |
| `missing` | Source reports the path is absent. |
| `unreachable` | Source/config/adapter cannot currently be reached. |
| `history-unavailable` | The source cannot supply the requested historical bytes. Local roots do not retain history. |
| `foreign-origin` | Contact the original instance UID through its existing authorized route. No local alias is invoked and no credentials are forwarded. |
| `invalid` / `invalid-response` | Retained metadata or the adapter protocol is inconsistent. |

Historical reads supply the recorded revision and/or digest. With neither,
Kata returns `history-unavailable`. A revision is an opaque locator, not content
identity. Revision-only history remains `unverified`; digest-bound history must
match before it is reported unchanged. Adapters must never silently substitute
current content for unavailable historical content.

## Resolver protocol version 1

An operator-owned adapter can use an existing MCP/Executor connection. Kata adds
no provider SDK or plugin runtime. The command receives exactly one JSON request
on stdin and returns exactly one JSON response on stdout; trailing JSON and
unknown response fields are rejected. Errors/stderr are discarded. There is a
15-second timeout with process-tree cleanup. Decoded content is limited to
16 MiB; the encoded envelope allows its base64 expansion plus 16 KiB metadata.
Source/path/revision strings are bounded. The child receives only basic OS
runtime variables plus the configured `env` names. Explicitly configured names
must exist; missing ones fail the read before the command starts. Put credential file variable
names in that allowlist; never put credentials in argv, references or stdout.
Caller bearer credentials are not forwarded.

Current read:

```json
{"version":1,"source":"notes","path":"results/report.md","mode":"current"}
```

Historical read (only recorded locators are supplied):

```json
{"version":1,"source":"notes","path":"results/report.md","mode":"historical","revision":"opaque-version","sha256":"<64 lowercase hex characters>"}
```

Success requires matching source/path and complete base64 content, even for an
empty file. A revision is optional; when historical mode requests one it must
match the returned revision. Kata computes byte count and SHA-256 itself.

```json
{"version":1,"source":"notes","path":"results/report.md","status":"ok","revision":"opaque-version","content_base64":"cmVwb3J0Cg=="}
```

Failure has no content or revision, and uses one of `missing`, `unreachable` or
`history-unavailable`:

```json
{"version":1,"source":"notes","path":"results/report.md","status":"history-unavailable"}
```

### Markdown Vault through MCP/Executor

Use the existing authenticated connection inside the adapter. For a complete
current Markdown `read(path)` response, UTF-8 encode the returned full content
and compute SHA-256. Accept and base64-encode it **only if that digest exactly
matches the response's raw-byte etag**. Refuse sections, truncation, missing
etags, or mismatches. Do not repair line endings or strip a BOM to force a match.
Return no revision unless the source binds it to those same returned bytes.

The upstream contract inspected at
`31eb2c1c702724e70deaba5bf19399ae8dc456f5` normalizes Markdown text (BOM/newlines)
and reads text/etag separately. Thus LF UTF-8 notes can pass this byte-bound
check, while BOM, CRLF or invalid UTF-8 notes may fail. Historical Markdown reads
without byte-bound provenance must return `history-unavailable`. Attachments
with complete base64 bytes can be adapted when their response binds path and
requested version to those bytes. The adapter must verify that contract.

[pvliesdonk/markdown-vault-mcp#2124](https://github.com/pvliesdonk/markdown-vault-mcp/issues/2124)
requests portable metadata-only SHA-256 references. Kata's protocol does not
depend on that unshipped feature. An adapter that only has normalized text must
fail explicitly for unsupported bytes. Never hash a text excerpt and label it
the original file. Synthetic adapters can implement the protocol independently
of a live service.

## HTTP and MCP

`GET /api/v1/projects/{project_id}/issues/{ref}/file-evidence` accepts
`event_uid`, `index`, `historical` and `content` query parameters. Current issue
and selected close-event authority are required, with scope revalidated after
source I/O. The route never accepts an executable, root or free-standing file
reference. Old clients that reject unknown evidence tags need upgrading; opaque
stored event payloads remain portable without a database migration.

MCP's closing section provides `kata.file_evidence` with the same selectors.
`kata.close` accepts `{"type":"file","file":{"source":"notes","path":"results/report.md","capture_sha256":true}}`.
MCP show/audit include the retained file reference. Existing startup scope still
limits which issues and events can be read.
