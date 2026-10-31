---
title: Gated workflows
description: "Coordinate agent steps and human approval with issue dependencies."
last_edited: 2026-10-30
---

# Gated workflows

Use a parent issue for one run and child issues for its stages. Connect the
stages with `--blocked-by`, then have automated workers take unowned ready
stages. A person owns the approval stage and closes it after reviewing the output.

This is a team convention built from ordinary issues. Dependencies filter the
ready queue; they do not prevent someone from claiming, editing, or closing a
blocked issue. Ownership does not restrict approval to a human. Any closed
blocker releases the next stage, including `wontfix` or `superseded` closes.
If approval is refused, park or cancel downstream work before closing the gate.

## Create a three-stage run

Run these commands in the intended [workspace and project](../guide/workspaces-projects.md).
The example uses `jq` to capture issue refs. Choose a new run key for each run;
retry the same create request with its original key if a response is lost.
Creation keys are retained for seven days.

```sh
run=$(kata create 'Review run' --idempotency-key run-1 --json | jq -r .issue.short_id)
draft=$(kata create 'Draft brief' --parent "$run" --label stage --idempotency-key run-1-draft --json | jq -r .issue.short_id)
approval=$(kata create 'Approve brief' --parent "$run" --blocked-by "$draft" --owner reviewer --label stage --idempotency-key run-1-approval --json | jq -r .issue.short_id)
publish=$(kata create 'Publish summary' --parent "$run" --blocked-by "$approval" --label stage --idempotency-key run-1-publish --json | jq -r .issue.short_id)
kata ready --label stage --agent
```

The `ready --label stage` commands here inspect all ready stages with that label,
including the reviewer-owned approval. Output from a disposable project (your refs
will differ):

```text
OK ready count=1
- issue=bvpb title="Draft brief" labels=stage
```

`--parent` groups the stages; it does not order them. `--blocked-by` orders
them; `kata edit "$draft" --blocks "$approval"` expresses the same dependency.
The `stage` label keeps the parent out of this queue. In a shared project,
use a label unique to the run to keep other runs out too. Future schedules and
`someday` also exclude issues from readiness; a deadline does not.

Automated workers select only unowned stages, leaving the assigned approval
with the reviewer:

```sh
kata ready --unowned --label stage --agent
```

Follow [Claim work](agents.md#claim-work) when taking a stage.

## Work, review, then continue

1. The first worker writes and checks the brief, then closes its stage with
   evidence. Here the output is a small text file:

   ```sh
   printf 'Brief: publish a summary of the reviewed example.\n' > brief.txt
   kata close "$draft" --done \
     --message 'Drafted the example brief and checked its scope for review.' \
     --reviewed brief.txt --agent
   kata ready --label stage --agent
   ```

   ```text
   OK ready count=1
   - issue=5ybd owner=reviewer title="Approve brief" labels=stage
   ```

2. The reviewer reads `brief.txt`.

   Leave the approval issue open while waiting. Do not attach an execute job
   to this stage. Assignment records responsibility; workers must honor the
   agreed human gate themselves.

   After approving, the reviewer closes the approval issue using their
   configured identity:

   ```sh
   kata close "$approval" --done \
     --message 'Reviewed the example brief and approved its scope for publication.' \
     --reviewed brief.txt --agent
   kata ready --label stage --agent
   ```

   ```text
   OK ready count=1
   - issue=vg9q title="Publish summary" labels=stage
   ```

3. The next worker writes and checks the summary, then closes its stage:

   ```sh
   printf 'Summary: the reviewed example is ready to share.\n' > summary.txt
   kata close "$publish" --done \
     --message 'Created the summary from the approved brief and checked its contents.' \
     --reviewed summary.txt --agent
   kata ready --label stage --agent
   ```

   ```text
   OK ready count=0
   ```

A `--done` close requires a message of at least 40 characters and typed evidence:
`commit:`, `pr:`, `test:`, `reviewed-paths:`, or `external:`. Shortcuts include
`--commit`, `--pr`, `--test`, and `--reviewed`. Kata validates evidence structure;
it does not run the recorded test or verify the artifact's contents. Record
what you actually checked. See [closing issues](../reference/cli.md#issue-lifecycle).

After all children close, finish the run parent:

```sh
kata close "$run" --done \
  --message 'Completed all three stages and checked the brief and final summary.' \
  --reviewed brief.txt --reviewed summary.txt --agent
```

Kata refuses to close a parent with open children. Close an unused branch with
`--wontfix` and a rationale of at least 60 characters, or `--superseded-by <ref>`
and a message of at least 20 characters. These closures also release dependents;
park them first with `kata meta set <ref> someday true --json-value` if needed.

## Request attention and wait

While approval is still open, before closing it in step 2, record the reason
and request the reviewer's attention:

```sh
kata meta set "$approval" work.attention needs-human
kata meta set "$approval" work.attention_msg 'Please review brief.txt before publication.'
kata notify "$approval" --to reviewer --message 'Please review brief.txt before publication.'
kata wait "$approval" "$publish" --until attention --any --timeout 30m
```

`work.attention stuck` marks stalled work. These metadata values do not change
readiness. `--until attention` returns for any nonempty value other than `ok`,
or when an issue closes; `--any` returns after the first match. The example
returns immediately because `needs-human` is already set. Already-closed stages
also match immediately, so pass only the stages you still need to watch.
After handling a request, clear it with `kata notify <ref> --to reviewer --clear`
and set `work.attention` back to `ok`.

To wait specifically for the reviewer to finish the approval stage, use:

```sh
kata wait "$approval" --until closed --timeout 30m
```

Optionally use `kata deadline "$approval" <date-or-time>` for a reminder.
Reached deadlines use `notify.*` to request the current owner's attention,
or the author's when unowned, and wait behind an existing request. See
[planning dates](../reference/cli.md#issue-metadata).

## Repeat or automate a run

Kata does not instantiate a graph of stage issues from a template. The author
or a first step creates each run's issues and links. Native recurring jobs
create one child per occurrence, rather than this whole graph.

For automation, use an adapter that runs execute jobs against an existing issue
(issue policy `existing`), and make its workers honor the ready queue. Shared
`kata cron workflow` definitions describe command or prompt steps, rather than
this issue graph. See the [Native cron reference](../reference/cron.md) for the
adapter contract.

Keep run-level decisions in parent comments and stage details in each child's
comments (`kata show <ref>`). `kata events` provides project activity; the
parent does not automatically collect its children's history.
