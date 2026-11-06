---
title: Issue attachments
description: Attach files through Docbank and view images and videos in Kata.
last_edited: 2026-11-07
---

# Issue attachments

Attach screenshots, recordings, and other files in the web UI or with `kata docbank attachments`.
Kata stores one reference per file in issue metadata. [Docbank](https://github.com/kenn-io/docbank) stores the bytes. Only references federate. Attachments require Kata 0.21.0 or later.

## Configure the daemon

Choose a Docbank vault whose contents may be shared with every issue reader and metadata writer served by this daemon. A metadata writer can reference any content version in that vault, and a reader can fetch it through Kata. Folders organize files; use separate vaults for different audiences.

`kata docbank setup` configures the current `KATA_HOME` and restarts its running daemons. Run it on each daemon that uploads or serves files.

For a Docbank on the same machine:

```sh
kata docbank setup [--port 8080] [--folder /kata]
```

Without a fixed address, Docbank's daemon picks a new port and API key each time it starts. Setup adds a `[server]` section to `<DOCBANK_HOME>/config.toml` with `bind_addr = "127.0.0.1"`, the port (default `8080`) and a new random key, then restarts Docbank. It stores the same key in `<KATA_HOME>/docbank.key` with mode `0600` and never prints it. An existing `[server]` section with `api_port` and `api_key` is reused, and Docbank keeps running if it already serves that port. Setup finds Docbank's home with `docbank vault info`, so `docbank` must be on `PATH`.

For a Docbank that runs elsewhere:

```sh
kata docbank setup --url https://docbank.example --key-file /path/to/docbank-key
```

Both forms read Docbank's root folder with the key before adding `[docbank]` to `<KATA_HOME>/config.toml`. They ask once before changing configuration; pass `--yes` without a terminal. Running setup again prints that attachments are already set up and changes nothing. Running daemons must support restart, and a refused restart restores `config.toml`.

Setup changes nothing when:

- `KATA_DOCBANK_URL`, `KATA_DOCBANK_API_KEY_FILE` or `KATA_DOCBANK_FOLDER` is set, because it would override the new section;
- `config.toml` already has a different `[docbank]` section;
- Docbank rejects the key or can't be reached;
- the port is in use by another program, `--port` differs from Docbank's `api_port`, or Docbank's `[server]` section lacks `api_port` or `api_key`;
- `<KATA_HOME>/docbank.key` holds a different key than Docbank's `[server]` section.

If Docbank restarts on another port, for example because `DOCBANK_API_PORT` or `DOCBANK_BIND_ADDR` is set in its environment, setup stops after Docbank's changes and leaves kata's config alone.

To configure the daemon by hand, add this to `<KATA_HOME>/config.toml`:

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

## Link a file already in Docbank

Link an existing file without downloading or uploading its bytes:

```sh
kata docbank attachments link abc4 /reports/report.pdf
kata docbank attachments link abc4 42 --version 6f1c2e8a-2222-4222-8222-222222222222
```

Choose one positive node ID or an absolute Docbank path. The link pins the current content version by default; `--version UUID` selects an older version of that same node.

Linking requires daemon API 0.38.0 or newer.

Kata refuses directories, trashed files, versions belonging to another node, and file names that fail the same rules as uploads. Names must be valid UTF-8, contain 1 to 255 bytes, and contain no `/`, `\`, or control characters. The names `.` and `..` are invalid. A missing Docbank file or version returns 404 `docbank_not_found`. A trashed node returns 410 `attachment_content_missing`. Each link adds a new reference by default, even when that version is already attached. API clients on daemons that support API 0.39.0 or newer can set `reuse_existing` to return the smallest existing attachment ID for the selected version, or create one if no reference exists. Remove extra references with `rm`.

The saved name and size come from link time. Later changes in Docbank leave the reference pinned to the selected version.

## Cite a document when closing work

Use an attachment as completion evidence for a document deliverable:

```sh
kata close abc4 --done -m "Delivered the report and checked its final contents." \
  --docbank /reports/report.pdf
```

Repeat `--docbank` for several files. Each value accepts a node ID or absolute
Docbank path and pins its current version. Repeated selectors for the same
version produce one evidence item. Other done-compatible evidence flags can
be combined with it. This workflow requires daemon API 0.39.0 or newer.

Kata validates all local arguments before linking. If a link fails, the issue
stays open and the error lists every attachment ID already linked. Retry
while those versions remain current to reuse their references. A moving path
can select a newer version on retry; use the returned full ID for stable retries.

For a historical version, a dry run, a revision guard, or a keyed retry, link
first and close explicitly:

```sh
kata docbank attachments link abc4 42 --version 6f1c2e8a-2222-4222-8222-222222222222
kata close abc4 --done -m "Delivered the report and checked its final contents." \
  --evidence attachment:<full-attachment-id> --idempotency-key report-close
```

`attachment:<id-or-unique-suffix>` requires a valid reference on the issue.
The close checks it inside the same transaction and needs no Docbank connection.
A successful keyed retry accepts the original full ID even after its reference
is removed. Resolve suffixes before removal. `--docbank` cannot combine with
`--dry`, `--if-match`, or `--idempotency-key`, and is only available for `done`.

Close history freezes the name, media type, size and content version. The web
links to the attachment panel only while the current reference still names
that version. Removed or changed references retain their frozen description.
`kata show` displays these details for closed issues.

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
