---
title: Issue attachments
description: Attach files through Docbank and view images and videos in Kata.
last_edited: 2026-10-21
---

# Issue attachments

Attach screenshots, recordings, and other files to issues in the web UI or with `kata docbank attachments`.
Kata stores an immutable reference in issue metadata. A separately hosted
[Docbank](https://github.com/kenn-io/docbank) stores the bytes.
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
metadata and attachment lists. Upload and content requests report
`docbank_not_configured`. A reference to a different vault reports `foreign_vault`
and is never fetched.

## Add, read, and remove files

```sh
kata docbank attachments add abc4 screenshot.png recording.mp4
kata docbank attachments ls abc4
kata docbank attachments get abc4 <attachment-id-or-suffix> -o screenshot.png
kata docbank attachments rm abc4 <attachment-id-or-suffix>
```

Each file gets its own id and outcome. Failed uploads do not add metadata;
retries reuse the same id. `get` verifies the pinned hash and size before
publishing the destination file. An existing destination requires `--force`.

Removal drops only the reference. Docbank retains the file under its own
retention rules. Do not edit reference entries in place; remove and add instead.

## Limits and display

The default upload limit is 256 MiB per file; operators may raise it to 2 GiB.
A daemon admits two concurrent uploads and at most 100 valid references per issue.
References received through federation may exceed that count; removal remains
available. Each map key must be a ULID and each entry must be an object. Entries
with invalid or unknown fields, including empty entries left by older peers, are omitted from lists and the count limit. Readers treat unknown fields as an unsupported entry format. These entries remain removable by id, and other metadata writes preserve them.
A peer map with a non-ULID key or a non-object entry blocks attachment commands.
Repair that map with the [generic metadata commands](../reference/metadata.md),
preserving valid entries when replacing its value.

## Use the web UI

On a daemon with Docbank configured, click **Attach files** on an issue and select one or more files. Each file shows its upload progress or error. Upload controls require a writable session and stay disabled while the connection is stale, an upload is running, or another mutation is pending. New entries appear through the normal issue refresh.

The web UI fetches attachments using the current browser session. Click **Preview** for PNG, JPEG, GIF, and WebP images up to 20 MiB, or **Play** for MP4, QuickTime, WebM, and Ogg videos up to 100 MiB. Videos play in an inline player when the browser supports their codecs. Downloads are available up to 100 MiB. Larger files show a `kata docbank attachments get` hint. SVG and HTML files download.
