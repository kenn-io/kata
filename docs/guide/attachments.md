---
title: Issue attachments
description: Attach files through Docbank and view images and videos in Kata.
last_edited: 2026-10-24
---

# Issue attachments

Attach screenshots, recordings, and other files in the web UI or with `kata docbank attachments`.
Kata stores one reference per file in issue metadata. [Docbank](https://github.com/kenn-io/docbank) stores the bytes. Only references federate. This feature is available on main after 0.20.0.

## Configure the daemon

Choose a Docbank vault whose contents may be shared with every issue reader and metadata writer served by this daemon. A metadata writer can reference any content version in that vault, and a reader can fetch it through Kata. Folders organize files; use separate vaults for different audiences.

Add this to `<KATA_HOME>/config.toml` on each daemon that uploads or serves files:

```toml
[docbank]
url = "https://docbank.example"
api_key_file = "/path/to/docbank-key"
folder = "/kata"
```

Protect the key file with mode `0600`. Restart after changing the section. Keys resolve on each request, so rotating the key needs no restart. HTTPS is required except on loopback. See the [configuration reference](../reference/configuration.md#docbank-attachments) for credential precedence and environment overrides.

Without Docbank configured, references still appear in lists, and the web UI shows their names and details. Upload and content requests return `docbank_not_configured`. If a referenced version is absent from the configured Docbank, reads return `attachment_content_missing`.

## Add, read, and remove files

```sh
kata docbank attachments add abc4 screenshot.png recording.mp4
kata docbank attachments ls abc4
kata docbank attachments get abc4 <attachment-id-or-suffix> -o screenshot.png
kata docbank attachments rm abc4 <attachment-id-or-suffix>
```

Each upload gets a new ID. Failed uploads add no reference. Retrying after a lost response may add another reference to the same Docbank file; remove the extra with `rm`. Downloads check the bytes against Docbank's `X-Docbank-Blob-Hash` header and the byte count against the reference before publishing the destination. Replacing an existing file requires `--force`.

Removal drops the reference. Docbank retains the bytes under its own retention rules.

## Read references as metadata

Each file uses an `attachment.<ULID>` key with `name`, `media_type`, `size`, and `docbank_version` fields. These are ordinary opaque metadata keys. The browser checks field types for display; the daemon validates the full reference when serving content.

```sh
kata meta get abc4
kata meta get abc4 attachment.<ULID>
kata meta unset abc4 attachment.<ULID>
```

## Limits and display

Uploads are limited to 256 MiB per file. Kata determines media types from filename extensions. Other types download as `application/octet-stream`.

On an issue, click **Attach files** and select one or more files. Each file shows progress or its error. Upload controls require a writable, current session and stay disabled while a mutation is pending. New entries arrive through the normal issue refresh.

Click **Preview** for PNG, JPEG, GIF, and WebP images up to 20 MiB, or **Play** for MP4, M4V, QuickTime, WebM, and Ogg videos up to 100 MiB. Playback depends on browser codec support. Downloads are available up to 100 MiB. Larger files show a `kata docbank attachments get` hint. SVG and unknown types download.
