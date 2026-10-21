---
title: Issue attachments
description: Attach files through Docbank and read their pinned content or text from Kata.
last_edited: 2026-10-20
---

# Issue attachments

Attach screenshots, recordings, and other files to issues with `kata docbank attachments`.
Kata stores an immutable reference in issue metadata. A separately hosted
[Docbank](https://github.com/kenn-io/docbank) stores the bytes and text renditions.
Only the reference federates; file bytes never enter Kata's database or events.
This feature is available on main after 0.19.0.

## Configure the daemon

Choose a Docbank vault whose contents may be shared with **every issue reader
and metadata writer** served by the Kata daemon. A metadata writer can reference
any file in that vault, and a reader can fetch it through Kata. Folder placement
does not restrict access. Use separate vaults for different audiences.

Add this to `<KATA_HOME>/config.toml` on each daemon that uploads or serves files:

```toml
[docbank]
url = "https://docbank.example"
api_key_file = "/path/to/docbank-key"
folder = "/kata"
max_upload_bytes = 268435456
```

Protect the key file with mode `0600`. Restart the daemon after changing the
section. Key file contents are read on each request, so key rotation needs no
restart. HTTPS is required except on loopback. See the
[configuration reference](../reference/configuration.md#docbank-attachments)
for credential precedence and environment overrides.

Without this section, references received from peers still appear in issue
metadata and attachment lists. Upload, content, and text requests report
`docbank_not_configured`. A reference to a different vault reports `foreign_vault`
and is never fetched.

## Add, read, and remove files

```sh
kata docbank attachments add abc4 screenshot.png recording.mp4
kata docbank attachments ls abc4
kata docbank attachments get abc4 <attachment-id-or-suffix> -o screenshot.png
kata docbank attachments text abc4 <attachment-id-or-suffix> --max-bytes 65536
kata docbank attachments rm abc4 <attachment-id-or-suffix>
```

Each file gets its own id and outcome. Failed uploads do not add metadata;
retries reuse the same id. `get` verifies the pinned hash and size before
publishing the destination file. An existing destination requires `--force`.
`text` returns a successful state such as `unprocessed` when Docbank has not
built a rendition. Kata does not start OCR or transcription jobs. Returned text
defaults to 64 KiB and is capped at 16 MiB, truncated at a UTF-8 boundary.
Kata verifies the complete artifact before returning that prefix.

Removal drops only the reference. Docbank retains the file under its own
retention rules. Do not edit reference entries in place; remove and add instead.

## Limits and display

The default upload limit is 256 MiB per file; operators may raise it to 2 GiB.
A daemon admits two concurrent uploads and at most 100 references per issue.
References received through federation may exceed that count; removal remains
available. Each map key must be a ULID and each entry must be an object. Entries
with invalid reference fields stay visible as `invalid` and remain removable.
A peer map with a non-ULID key or a non-object entry blocks attachment commands.
Repair that map with the [generic metadata commands](../reference/metadata.md),
preserving valid entries when replacing its value.

The web UI fetches attachments using the current browser session. It previews
PNG, JPEG, GIF, and WebP images up to 20 MiB and offers downloads up to 100 MiB.
Larger files show a `kata docbank attachments get` hint. SVG, HTML, and video are
always downloads. Video playback and seeking are outside this version.

Agents can load `kata.load_attachments`, then call `kata.attachment_text` with
an issue reference and full attachment id. MCP caps text at 1 MiB to fit its
response limit after JSON escaping. See the [MCP reference](../reference/mcp.md).
