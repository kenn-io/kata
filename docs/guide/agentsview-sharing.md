---
title: Share a frozen AgentsView snapshot
description: "Prepare explicit project-scoped snapshot consent and recover the same retained bytes."
last_edited: 2026-11-08
---

# Share a frozen AgentsView snapshot

This draft adds the CLI workflow for explicitly sharing a closing-session
snapshot through the selected project's relay. The live verification adapter
is unavailable in this main-based slice, so these commands currently refuse
before closing or uploading. This is not a delivered end-to-end relay feature.

Integration requires [root discovery](https://github.com/kenn-io/kata/pull/1009),
[the association and upload slice](https://github.com/kenn-io/kata/pull/1011), [binary relay transport](https://github.com/kenn-io/kata/pull/983),
[direct-close compatibility](https://github.com/kenn-io/kata/pull/982), and the
[export companion](https://github.com/kenn-io/agentsview/pull/2847) using [AgentsView's session-push codec](https://github.com/kenn-io/agentsview/pull/2811).
Those prerequisites are independently owned and remain unmerged dependencies.
The [design note](https://github.com/kenn-io/kata/issues/1007) describes the full outcome.

## Select and review one snapshot

The intended integrated workflow is:

```sh
agentsview export-push pi:example-session --agent pi --snapshot-prefix --output snapshot.json
kata agentsview prepare example-project#abcd --snapshot snapshot.json --output consent.json
# Review snapshot.json before confirming the digest printed by preparation.
kata close example-project#abcd --done \
  --message "Completed the selected work and reviewed its change." \
  --reviewed README.md --idempotency-key example-close \
  --share-transcript consent.json --snapshot snapshot.json \
  --confirm-transcript-sha256 <reviewed-sha256>
```

`agentsview push <session-id>` is the existing direct upload command. It needs
a receiver token. `export-push` is a separate export-only companion and must
not call `push` or read a receiver token. Its real codec adapter is also a draft
integration prerequisite; no successful export is claimed here.

A snapshot ends at its captured point and may omit later running-session turns.
It does not prove the session finished. Known truncated, malformed, summary-only,
empty or uncaptured content must be refused by the export companion. Sparse
message ordinals alone do not prove missing content. Separately stored parent,
child and sibling transcripts are excluded. Selected messages may already
contain mixed-purpose tool results; review those bytes explicitly.

AgentsView retains the first accepted snapshot immutably. Another close in the
same growing session cannot update that receiver's first snapshot. A locator
URL is not upload acceptance. Root-confirmed acceptance is not indexing proof.

## Retain the exact files

Keep both private files until sharing and any explicit retries are finished.
Kata creates private files from the outset: mode 0600 on Unix and a protected
user DACL on Windows. Draft consent files lacking `root_key_id` are refused;
prepare a fresh consent before closing.
Every close or share attempt requires an explicit snapshot path and fresh digest
confirmation. Paths resolve from the invoking directory, not from the consent
file. The portable close association contains identity and digest information,
never paths, receiver tokens or transcript bytes.

Kata freezes the opened file into one bounded private spool per attempt and
uses those bytes through close and upload. Cleanup removes the spool while
retaining the user's files. Abrupt process termination can leave a private
temporary spool for manual cleanup; graceful exits remove it. Missing or replaced bytes refuse; retries never
scan or re-export an archive. Do not reopen and close again to replace consent.

## Recover after an uncertain response

Only a confirmed new close with its exact matching event can upload once
automatically. Dry-run, no-op, replay and uncertain close responses never upload.
Use the existing keyed close receipt or issue history to recover the original
event. A replay can recover proof but cannot trigger an automatic upload.

```sh
kata agentsview share consent.json --issue example-project#abcd \
  --snapshot snapshot.json --close-event <original-close-event-uid> \
  --confirm-transcript-sha256 <reviewed-sha256>
```

Manual share uses that original event's session identity even from another
terminal. It refreshes live discovery only for the same daemon, issue, project,
root, root key, service and entire route generation. A rename with unchanged
identity is acceptable; moving the issue, rotating the root key, rebinding or
changing an epoch invalidates selection. Missing/purged original close proof,
a reopened or superseded close, unsupported capability or revoked authority
refuses. There is no direct/personal fallback or automatic retry queue.

If the root has not received the close yet, retain the files and retry explicitly
after ordinary replication. If upload times out, bytes may already be accepted;
retry only the same frozen snapshot. An ordinary close without an association
cannot acquire one later in this version.

The explicit sharing path excludes the optional direct-close runner before
local transcript configuration or native identity discovery can fail. Omitting
all sharing flags preserves the existing close path. This draft refuses
`--docbank` preparation or follow-up `--comment` together with sharing; create
the attachment or post the comment separately.

## Remaining integration evidence

Synthetic unit, property and process fixture source covers file retention,
identity substitution, recovery decisions and fail-closed destination counters.
It has not run locally. The process fixture exercises real CLI rejection;
it does not stand in for a real root/relay/leaf with two opted-in SQLite
AgentsView receivers. That combined process proof, PostgreSQL parity, live
revoke/epoch races, direct-runner compatibility and signed receipt integration
remain pending. See the [developer integration notes](../development/agentsview-relay-integration.md)
for the exact inspected prerequisite heads and adapter boundaries.

No compile, test, lint, typecheck or generated API refresh ran
locally for this draft.
