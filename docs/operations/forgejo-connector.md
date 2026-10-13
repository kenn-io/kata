---
title: Forgejo connector
description: "Bind a Forgejo issue to Kata using the built-in Go connector."
last_edited: 2026-10-13
---

# Forgejo connector

Bind an existing Forgejo issue to one Kata issue with the connector included in
the Kata binary. Forgejo owns its title, body, and human discussion. Kata owns
agent work, child issues, dependencies, and verified completion.

The connector uses `kata.connector.v1` and needs no Python installation. It is
separate from [GitHub bulk sync](github-sync.md): a bridge shares one selected
root and leaves its Kata children private.

## Configure the daemon

Create a Forgejo access token for the account that will read issues, publish
comments, and close issues. Give it access to the selected repository. Forgejo 11 requires `read:user`
for the connector account lookup, `read:repository` for repository identity and
catalog reads, and `write:issue` for comments, completion, and field writes.
Organization label selection also needs `read:organization`. For an inbound-only
setup, `read:issue` permits issue and comment reads, but verified completion
still needs `write:issue`. Keep the token in the
daemon's environment rather than command arguments or connector settings.

Add this to `<KATA_HOME>/config.toml`, using the absolute path of your installed
Kata binary:

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

Make `KATA_FORGEJO_TOKEN` available to the daemon process, then restart the
daemon to load the connector configuration. Use a root origin without a path,
query, fragment, or embedded credentials. Use HTTPS for ordinary hosts. HTTP
accepts only literal loopback, private (RFC 1918 or ULA), link-local, or CGNAT
(`100.64.0.0/10`) IP addresses. Provider redirects are rejected so the token
stays at the configured origin.

```sh
kata connector status forgejo
kata connector field list forgejo
```

`kata connector serve forgejo` is the connector's one-request RPC entry point.
The daemon runs it with JSON on standard input and reads one response from
standard output. It does not start or contact a Kata daemon itself.

## Bind and reconcile an issue

Create or select the Kata issue that will hold the work, then bind it:

```sh
kata bridge bind abc4 --connector forgejo --external 'example-owner/example-repo#42'
kata bridge show abc4
kata bridge reconcile abc4
```

A full issue URL on the configured origin also works. Pull requests and URLs
on another origin are rejected. The stored key includes immutable repository
and issue IDs; repository renames remain addressable, and a replacement issue
cannot silently become the bound root.

Binding imports the current title and body. Existing comments stay external;
new comments and later edits enter Kata with their external attribution.
Forgejo omits deleted comments from its API and supplies no deletion tombstone.
A previously imported Kata comment is retained after provider deletion. Title and body edits in Kata are rejected while
the binding is active.

Closing a Forgejo issue creates a verification request and adds `needs-review`
in Kata. Reopening it creates a review request too. Neither changes Kata's
lifecycle automatically. Closing Kata through its normal verified close flow
closes the Forgejo issue and checks the provider's readback.

## Publish comments

Outbound comments are off by default. Opt in when binding:

```sh
kata bridge bind abc4 --connector forgejo \
  --external 'example-owner/example-repo#42' --publish-comments
```

The connector appends an invisible delivery marker to native comments. A retry
searches every comment page and adopts only one exact matching comment from the
configured account. The marker is removed from the connector's returned body,
so imported discussion retains the original text. Imported comments and child
issue comments do not echo outward.

An ambiguous match stops publication. If the credential changes to another
Forgejo user, root identity stays stable, but publication recovery cannot adopt
a comment authored by the previous user. Restore the original credential or
inspect the pending publication with `kata bridge show` and use the explicit
[`resolve-comment` workflow](../reference/cli.md#external-root-bridges).

## Field support and current limits

The connector exposes these provider fields through the protocol:

| Field | Kind | Representation |
| --- | --- | --- |
| `due_date` | `date` or `null` | Provider calendar date; null clears it. |
| `labels` | `string_set` | Existing Forgejo label ID strings; empty clears labels. |
| `assignees` | `string_set` | Forgejo logins; empty clears assignees. |
| `milestone` | `string` or `null` | Existing milestone ID string; null clears it. |

It validates a request before writing and returns a fresh readback after
successful writes. It does not create labels, accounts, or milestones.
Multi-field requests use separate provider endpoints and can partially apply
before an error. Read the fields again before recovery.

Automatic bidirectional field mappings are currently unavailable for this
connector. Stock Forgejo 11 has no atomic compare-and-set operation for these
fields, so the connector does not advertise `conditional_fields`. Kata requires
that capability to enable mappings and refuses `kata connector field map`
rather than accepting writes that could overwrite concurrent provider edits.
Direct protocol field reads and unconditional writes remain available to
connector clients. Requests with `expected` are rejected before any mutation.

The portable `string` and `string_set` wire kinds also do not register native
Kata label, owner, or milestone mappings. Kata's registered mapping codecs
remain `scheduled_on` and `deadline_on`; Kata has one owner and no native
milestone field. See the [connector author contract](../reference/connector-protocol.md)
for field values and conditional-write guarantees.
