---
title: Forgejo connector
description: "Bind a Forgejo issue to Kata using the built-in Go connector."
last_edited: 2026-11-20
---

# Forgejo connector

Use the built-in Forgejo connector to bind an existing Forgejo issue to one Kata
issue. Forgejo owns the title, body, and human discussion, while Kata owns agent
work, child issues, dependencies, and verified completion.

The connector implements `kata.connector.v1` inside the Kata binary and needs no
Python installation. It works differently from
[GitHub bulk sync](github-sync.md): a bridge shares a single selected root issue
and keeps its Kata children private.

## Configure the daemon

Create a Forgejo access token for the account that reads issues, publishes
comments, and closes issues, and grant it access to the repository you want to
bind. On Forgejo 11, the token needs these scopes:

- `read:user` to look up the connector account.
- `read:repository` to read repository identity and catalogs.
- `write:issue` to publish comments, close issues, and write fields.
- `read:organization` if you select organization labels.

For an inbound-only setup, `read:issue` allows issue and comment reads, though
verified completion still requires `write:issue`. Keep the token in the daemon's
environment rather than in command arguments or connector settings.

Add the connector to `<KATA_HOME>/config.toml`, using the absolute path to your
installed Kata binary:

```toml
[[connector]]
id = "forgejo"
command = "/usr/local/bin/kata"
args = ["connector", "serve", "forgejo"]
timeout_seconds = 30

[connector.env]
FORGEJO_TOKEN = "KATA_FORGEJO_TOKEN"

[connector.settings]
origin = "https://forge.example"
```

Expose `KATA_FORGEJO_TOKEN` in the daemon's environment, then restart the daemon
to load the connector configuration. Set `origin` to a root URL with no
path, query, fragment, or embedded credentials. Use HTTPS for ordinary hosts;
HTTP is allowed only for literal loopback, private (RFC 1918 or ULA),
link-local, or CGNAT (`100.64.0.0/10`) IP addresses. The connector rejects
provider redirects so the token stays at the configured origin.

To check the connector and see its supported fields, run:

```sh
kata connector status forgejo
kata connector field list forgejo
```

`kata connector serve forgejo` is the connector's one-request RPC entry point.
The daemon invokes it with JSON on standard input and reads one response from
standard output. The command itself does not start or contact a Kata daemon.

## Bind and reconcile an issue

Create or select the Kata issue to hold the work, then bind it:

```sh
kata bridge bind abc4 --connector forgejo --external 'example-owner/example-repo#42'
kata bridge show abc4
kata bridge reconcile abc4
```

You can also pass a full issue URL on the configured origin. The connector
rejects pull requests and URLs on any other origin. The stored key records
immutable repository and issue IDs. Repository renames stay addressable, and a
replacement issue cannot silently take over the bound root.

Binding imports the current title and body. Existing comments stay on Forgejo,
while new comments and later edits flow into Kata with their external
attribution. Forgejo omits deleted comments from its API and provides no
deletion tombstone. Kata keeps a previously imported comment after it is deleted
on Forgejo. While the binding is active, Kata rejects local title and body
edits.

Closing a Forgejo issue creates a verification request and adds `needs-review`
in Kata, and reopening it creates a review request as well. Neither action
changes Kata's lifecycle automatically. When you close the Kata issue through
the normal verified close flow, Kata closes the Forgejo issue and checks the
provider's readback.

## Publish comments

Outbound comments are off by default. To opt in when binding an issue, pass
`--publish-comments`:

```sh
kata bridge bind abc4 --connector forgejo \
  --external 'example-owner/example-repo#42' --publish-comments
```

The connector appends an invisible delivery marker to native comments. On retry,
it searches every comment page and adopts a comment only if it finds one exact
match from the configured account. The connector strips the marker from the
returned body, so imported discussion keeps the original text. Imported comments
and child issue comments do not echo back to Forgejo.

An ambiguous match stops publication. If you switch the credential to a
different Forgejo user, the root identity stays stable, but publication recovery
cannot adopt a comment authored by the previous user. To recover, restore the
original credential, or inspect the pending publication with `kata bridge show`
and follow the explicit
[`resolve-comment` workflow](../reference/cli.md#external-root-bridges).

## Field support and current limits

The connector exposes these provider fields through the protocol:

| Field | Kind | Representation |
| --- | --- | --- |
| `due_date` | `date` or `null` | Provider calendar date; null clears it. |
| `labels` | `string_set` | Existing Forgejo label ID strings; empty clears labels. |
| `assignees` | `string_set` | Forgejo logins; empty clears assignees. |
| `milestone` | `string` or `null` | Existing milestone ID string; null clears it. |

The connector validates each request before writing and returns a fresh readback
after a successful write. It does not create labels, accounts, or milestones.
Multi-field requests hit separate provider endpoints and can partially apply
before an error. Read the fields again before attempting recovery.

Automatic bidirectional field mappings are currently unavailable for this
connector. Because stock Forgejo 11 has no atomic compare-and-set operation for
these fields, the connector does not advertise `conditional_fields`. Kata
requires that capability before enabling mappings and refuses
`kata connector field map` rather than accepting writes that could overwrite
concurrent provider edits. Connector clients can still run direct protocol field
reads and unconditional writes, while the connector rejects requests with
`expected` before making any change.

The portable `string` and `string_set` wire kinds also do not register native
Kata label, owner, or milestone mappings. Kata's registered mapping codecs
remain `scheduled_on` and `deadline_on`, and Kata has one owner and no native
milestone field. See the
[connector author contract](../reference/connector-protocol.md) for field values
and conditional-write guarantees.
