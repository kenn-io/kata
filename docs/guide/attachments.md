---
title: Issue attachments
description: Attach files through Docbank and view images and videos in Kata.
last_edited: 2026-11-08
---

# Issue attachments

Attach screenshots, recordings, and other files in the web UI or with `kata docbank attachments`.
Kata stores one reference per file in issue metadata. [Docbank](https://github.com/kenn-io/docbank) stores the bytes. Only references federate. Attachments require Kata 0.21.0 or later.

## Configure the daemon

Choose a Docbank vault whose contents may be shared with every issue reader and metadata writer served by this daemon. A metadata writer can reference any content version in that vault, and a reader can fetch it through Kata. Folders organize files; use separate vaults for different audiences.

`kata docbank setup` turns attachments on for the current `KATA_HOME` and restarts its running daemons. Run it on each daemon that uploads or serves files.

For a Docbank on the same machine:

```sh
kata docbank setup [--folder /kata]
```

Setup then:

1. Prints the shared-vault warning with the vault path and asks once. Without a terminal, pass `--yes` to skip the question.
2. Checks that the Docbank in `DOCBANK_HOME` (default `~/.docbank`) answers with its API key. If Docbank is not running, setup starts it with `docbank daemon start`, so `docbank` must be on `PATH`. Nothing starts before you approve.
3. Adds `[docbank]` with `local = true` and `folder` to `<KATA_HOME>/config.toml` and restarts running daemons.

Setup does not edit Docbank's config or store a key.

Setup reads `DOCBANK_HOME` from its own shell, but the daemon reads it from its own environment, so the two must match. A daemon restart keeps the daemon's environment.

For a Docbank on another host, or a local one whose port and key you already fixed in its config (loopback `http://127.0.0.1:<port>` is allowed):

```sh
kata docbank setup --url https://docbank.example --key-file /path/to/docbank-key
```

Setup reads the key from `--key-file` (allowed only with `--url`), checks it with an authenticated read of the Docbank's root folder, and adds `[docbank]` with `url`, `api_key_file`, and `folder`.

Setup changes nothing while `KATA_DOCBANK_URL`, `KATA_DOCBANK_API_KEY_FILE`, or `KATA_DOCBANK_FOLDER` is set, when `config.toml` already has a different `[docbank]` section, or when the Docbank check fails.

Running setup again with the same settings reports that attachments are already set up, writes nothing and does not ask; it still checks Docbank, starting it when it is not running. See the [CLI reference](../reference/cli.md#issue-attachments) for flags and output.

To configure the daemon by hand for a Docbank on the same machine, add this to `<KATA_HOME>/config.toml` on each daemon that uploads or serves files, then restart the daemon:

```toml
[docbank]
local = true
```

When `local = true`, the daemon reads the running Docbank's current address and API key on every request, so a Docbank restart, reboot, or self-update needs no Kata change. Kata uses the Docbank in `DOCBANK_HOME` from the daemon's environment, or `~/.docbank` when unset. That is the vault the `docbank` CLI uses for the same user. When no Docbank is running, the daemon runs `docbank daemon start`, so `docbank` must be on the daemon's `PATH`. `local` needs Docbank 0.16.0 or later, which can prove it holds its key before Kata sends it.

See [Docbank attachments](../reference/configuration.md#docbank-attachments) for how `local` combines with `url`, key settings, and `folder`.

To configure a Docbank on another host by hand, set a URL and key instead:

```toml
[docbank]
url = "https://docbank.example"
api_key_file = "/path/to/docbank-key"
folder = "/kata"
```

Make the key file private to the daemon user, following the [key file rules](../reference/configuration.md#credentials-and-batching). Restart after changing the section. Keys resolve on each request, so rotating the key needs no restart. HTTPS is required except on loopback. See the [configuration reference](../reference/configuration.md#docbank-attachments) for credential precedence and environment overrides.

If the daemon cannot read its key, upload, content, and link requests return `docbank_key_unavailable` (daemon API 0.44.0 or newer) without contacting Docbank. Run `kata doctor` on the daemon host, or with a daemon-owner token, to see which key source failed and why.

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
