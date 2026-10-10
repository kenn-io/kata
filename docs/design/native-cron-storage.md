---
last_edited: 2026-10-09
---

# Dormant native cron storage

Schema 31 stores shared job and workflow definitions plus independently attributed
run observations. Saving a definition or logging a run emits ordinary audit
events. Scheduling, overlap policy, process launches and notification delivery
belong to the adapter running on each machine.

Jobs and workflows are whole version 1 documents identified by immutable ULIDs.
Updates, tombstones and restores compare the winning definition event UID and
retain its original HLC. Definitions are bounded to 256 KiB and use portable
checkout keys and secret reference names. Storage never resolves those names
into paths or credentials and never executes command or prompt text.

Each independent execution has its own retained run UID. Job/event and
workflow/event references identify historical definitions in the same project;
at least one complete pair is required. Occurrence keys and issue UIDs are
optional descriptive references, so multiple runs may share them. Identity
includes the attributed actor, optional teammate and executor label, and
server-created time. These fields remain immutable.

Reported status, bounded summary and start/end timestamps are mutable evidence.
Supported statuses are running, succeeded, failed, cancelled and unknown.
An exact canonical same-UID retry returns the current row without another
revision or event, including with a stale expected revision. Changed evidence
uses ordinary revision CAS; changed identity conflicts. Summary version 1 allows
a message and nonnegative int64 token counts, bounded to 64 KiB. The complete
observation and event are bounded to 96 KiB.

The schema has three cron tables: jobs, workflows and runs. Ordinary project,
authentication, actor-token revocation and federation enrollment boundaries
apply to them. Running or unknown observations do not block restore, reset,
adoption, merge or purge; those operations retain their ordinary guards.

Federation publishes definitions and run observations through ordinary spoke
push. Run evidence folds by original observation HLC and event UID for each
run UID. Baselines retain original provenance, so duplicate and reordered
events cannot invent a new observation or reserve an occurrence. Adoption
rewrites only the project envelope while preserving exact JSON numbers and
historical run identity. Ordinary snapshot-author enrollment policy remains.

JSONL exports contain `cron_job`, `cron_workflow` and `cron_run` records,
including definition tombstones. Import decodes these records and cron event
payloads strictly and refuses unknown fields and cron event types without a
declared wire version before clearing a target. Cron events must match their
imported project identity. New-instance restore retains ordinary instance and
event origin semantics.

SQLite schema 30 upgrades through the existing protected JSONL rebuild/cutover;
PostgreSQL uses migration 31. Fresh and upgraded physical schemas must agree.

The ordinary issue planning-date projection reads identity, revision, metadata
and recurrence timezone consistently. It returns raw scheduled/deadline values,
resolved native timezone and UTC instant without evaluating a clock. Scheduled
dates use issue timezone, recurrence fallback, then daemon default and UTC;
deadlines omit recurrence fallback. Native civil-time and DST handling and the
existing readiness/sweeper behavior stay unchanged.

See [Native cron](../reference/cron.md) for API/CLI documents,
[Backup and restore](../operations/backup-restore.md) for replacement protections,
and [PostgreSQL migrations](../development/postgres-migrations.md) for schema
history and startup validation.
