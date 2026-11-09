---
last_edited: 2026-11-09
---

# Persist explicit tracker account identity

Native storage callers can retain the exact tracker owner, connection UID and
generation with an issue-sync binding. Persisting that identity grants no
provider access. A named binding requires a live account router before any
content or status adapter can run; a missing router fails unavailable.

`UpsertIssueSyncBindingParams.Connection` supplies the complete selection. The
store writes it into the existing binding configuration and writes its connection
UID into `issue_sync_bindings.connection_uid`. Decoders, scans, import/export and
runners require both representations to agree. Omission preserves an existing
selection; it never chooses a default account or changes an existing selection.
Provider and source identity remain immutable, and one project has one binding.

The nullable column is a downgrade boundary as well as a consistency marker.
Older GitHub decoders ignore unknown configuration fields and could otherwise
use default credentials. Schema version 51 makes older binaries refuse the
new database before interpreting a named binding. PostgreSQL migration `000051`
adds the column; SQLite uses its version-aware JSONL cutover. See
[PostgreSQL migrations](postgres-migrations.md) for the source migration contract.

Execution evidence reuses native metadata. Root reservations, exact completed
receipts, semantic replay history, owner checkpoints, retired generations and
restore barriers are separate typed records. No new journal table or index is
introduced. Semantic identity excludes caller, run and connection generation,
so replacing those cannot replay an old intent. Unknown provider effects never
expire into permission to resend. Retained evidence is not a portable project
credential or execution grant.

Full backups preserve that evidence with restore protection. Project transfer
and new-instance imports omit local operational authority. Supported restore
retains a missing-history barrier even when the backup predates the first named
binding; ordinary activation cannot prove that omitted external effects stopped.
Recovery may acknowledge an exact retained result without clearing that barrier.

The ordinary enable endpoint refuses named configuration before legacy provider
preflight. Integrators must use an explicitly authorized account workflow rather
than entering the daemon-default credential path. The operator workflow is
separate from this native storage contract.
