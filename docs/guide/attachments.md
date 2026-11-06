---
title: Issue attachments
description: Attach files through Docbank and view images and videos in Kata.
last_edited: 2026-11-06
---

# Issue attachments

Attach screenshots, recordings, and other files in the web UI or with `kata docbank attachments`.
Kata stores one reference per file in issue metadata. [Docbank](https://github.com/kenn-io/docbank) stores the bytes. Only references federate. Attachments require Kata 0.21.0 or later.

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

Without Docbank configured, references still appear in lists, and the web UI shows their names and details. Local upload and content requests return `docbank_not_configured`. A relayed project uses its origin's configuration instead. If a referenced version is absent from the selected Docbank, reads return `attachment_content_missing`.

## Use attachments through project relays

New attachments for a shared project use the root hub's Docbank. Add and link
commands stay the same on a leaf or intermediate relay. Only the root needs
the Docbank key; every connection uses its own project enrollment credential.
Upgrade the CLI and every daemon on the path before using this feature.
An unsupported hop returns an explicit upgrade error.

The root exposes its configured `folder` subtree to each enrolled project's
readers and metadata writers. This includes upload folders for other projects
under that same subtree. Use a separate Docbank instance or folder audience
when those files must not be shared. Forwarded links accept only files whose
live canonical path is inside the exposed folder, including links selected by
node ID or historical version. Moving a file outside that folder makes its
origin references unreadable. Membership checks and content reads are separate
Docbank operations; they do not provide an atomic folder ACL during moves.

An origin service uploads or resolves the file and returns a pinned descriptor.
The requesting daemon saves the issue reference locally, so it can be cited in
an immediate close before metadata synchronization reaches the root. A new
issue that has not reached the root returns `attachment_issue_not_synced`;
sync the project and retry. The issue is not created at the root by uploading.

New references carry the serving Kata instance UID in `origin`. Every replica
stores the same value. References without this marker, including history from
before relay adoption, keep using each serving daemon's local Docbank. A marked
reference never falls back to a different local vault: an unreachable upstream
returns `federation_offline`; a missing or changed route is an explicit origin
error; a missing version at a reachable origin remains
`attachment_content_missing`.

Signed uploads use bounded temporary disk files for digest verification at
each hop. The 256 MiB file limit remains. Transfer failures or a denied final
metadata write can leave an unreferenced Docbank version; Docbank retention
owns those bytes. A lost upload response can create another reference on retry.

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

Each file uses an `attachment.<ULID>` key with `name`, `media_type`, `size`, and `docbank_version` fields, plus optional `origin` for origin-routed references. An absent marker or explicit `local` keeps local resolution. These are ordinary opaque metadata keys. The browser checks field types for display; the daemon validates the full reference when serving content. Version reuse compares both origin and content version.

```sh
kata meta get abc4
kata meta get abc4 attachment.<ULID>
kata meta unset abc4 attachment.<ULID>
```

## Limits and display

Uploads are limited to 256 MiB per file. Kata determines media types from filename extensions. Other types download as `application/octet-stream`.

On an issue, click **Attach files** and select one or more files. Each file shows progress or its error. Upload controls require a writable, current session and stay disabled while a mutation is pending. New entries arrive through the normal issue refresh.

Click **Preview** for PNG, JPEG, GIF, and WebP images up to 20 MiB, or **Play** for MP4, M4V, QuickTime, WebM, and Ogg videos up to 100 MiB. Playback depends on browser codec support. Downloads are available up to 100 MiB. Larger files show a `kata docbank attachments get` hint. SVG and unknown types download.
