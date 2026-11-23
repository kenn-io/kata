---
title: Gated workflows
description: "Coordinate agent steps and human approval with issue dependencies."
last_edited: 2026-11-20
---

# Gated workflows

You can model a gated workflow with a parent issue for the run and child issues
for each stage. Link the stages in order with `--blocked-by`, then have
automated workers pick up unowned ready stages. A person owns the approval stage
and closes it after reviewing the output.

Because this is a team convention built from ordinary issues, Kata does not
enforce the gate itself:

- Dependencies filter the ready queue, but they do not stop anyone from
  claiming, editing, or closing a blocked issue.
- Ownership records who should approve a stage, without restricting approval to
  a human.
- Any closed blocker releases the next stage, including `wontfix` and
  `superseded` closes. If a reviewer rejects the output, park or cancel
  downstream work before closing the gate.

## Create a three-stage run

Run these commands in the [workspace and project](../guide/workspaces-projects.md)
you want to use. The example uses `jq` to capture issue refs. Pick a new run key
for each run; if a response is lost, retry the same create request with its
original key. Kata retains creation keys for seven days.

```sh
run=$(kata create 'Review run' --idempotency-key run-1 --json | jq -r .issue.short_id)
draft=$(kata create 'Draft brief' --parent "$run" --label stage --idempotency-key run-1-draft --json | jq -r .issue.short_id)
approval=$(kata create 'Approve brief' --parent "$run" --blocked-by "$draft" --owner reviewer --label stage --idempotency-key run-1-approval --json | jq -r .issue.short_id)
publish=$(kata create 'Publish summary' --parent "$run" --blocked-by "$approval" --label stage --idempotency-key run-1-publish --json | jq -r .issue.short_id)
kata ready --label stage --agent
```

The `ready --label stage` commands on this page inspect all ready stages with
that label, including the reviewer-owned approval. Here is sample output from a
disposable project (your refs differ):

```text
OK ready count=1
- issue=bvpb title="Draft brief" labels=stage
```

`--parent` groups the stages together without ordering them, while
`--blocked-by` sets the sequence (you can express the same dependency with
`kata edit "$draft" --blocks "$approval"`). The `stage` label keeps the parent
issue out of this queue; in a shared project, add a label unique to the run to
keep other runs' issues out of the queue too. Future schedules and `someday`
also keep issues out of the ready queue, while deadlines do not.

Automated workers pull only unowned stages, which leaves the assigned approval
stage for the reviewer:

```sh
kata ready --unowned --label stage --agent
```

When claiming a stage, follow [Claim work](agents.md#claim-work).

## Work, review, then continue

1. Have the first worker write and check the brief, then close its stage with
   evidence. In this example, the output is a small text file:

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

2. Ask the reviewer to read `brief.txt`. Leave the approval issue open while you
   wait, and do not attach an execute job to it. Assignment records who is
   responsible, and workers must honor the agreed human gate themselves.

   Record why the stage is waiting and send a request to the reviewer's inbox:

   ```sh
   kata meta set "$approval" work.attention needs-human
   kata meta set "$approval" work.attention_msg 'Please review brief.txt before publication.'
   kata notify "$approval" --to reviewer --message 'Please review brief.txt before publication.'
   ```

   If a worker needs the approval before continuing, it can wait until the
   reviewer closes the issue:

   ```sh
   kata wait "$approval" --until closed --timeout 30m
   ```

   Once the reviewer approves the brief, they close the approval issue under
   their configured identity:

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

3. Have the next worker write and check the summary, then close its stage:

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
`commit:`, `pr:`, `test:`, `reviewed-paths:`, `external:`, or `attachment:`. You
can use shortcuts like `--commit`, `--pr`, `--test`, and `--reviewed`. Kata
checks the structure of your evidence, but it does not run the recorded test or
inspect the artifact's contents, so record what you actually verified. See
[closing issues](../reference/cli.md#issue-lifecycle).

Once all child stages are closed, close the parent run issue:

```sh
kata close "$run" --done \
  --message 'Completed all three stages and checked the brief and final summary.' \
  --reviewed brief.txt --reviewed summary.txt --agent
```

Kata refuses to close a parent issue while any child is still open. To close an
unused branch, use `--wontfix` with a rationale of at least 60 characters, or
`--superseded-by <ref>` with a message of at least 20 characters. Because those
closures also release dependent stages, park downstream issues first with
`kata meta set <ref> someday true --json-value` if needed.

## Attention and reminders

Setting `work.attention` does not change whether an issue is ready. Use
`needs-human` when a stage is waiting on a person, and `stuck` when work stalls.
After you handle a request, clear it with
`kata notify <ref> --to reviewer --clear` and set `work.attention` back to `ok`.

To watch several stages for a new attention request, run
`kata wait <ref>... --until attention --any`. It returns as soon as any watched
issue has a nonempty value other than `ok` or closes, so pass only open stages
that do not already need attention.

You can also set a reminder with `kata deadline "$approval" <date-or-time>`.
When a deadline is reached, Kata uses `notify.*` to request the current owner's
attention (or the author's if the issue is unowned), waiting behind any existing
request. See [planning dates](../reference/cli.md#issue-metadata).

## Repeat or automate a run

Kata does not generate a graph of stage issues from a template. Either you or an
initial setup step creates each run's issues and links. Native recurring jobs
create one child issue per occurrence rather than a full stage graph.

To automate stages, use an adapter that runs execute jobs against an existing
issue (issue policy `existing`) and have its workers honor the ready queue.
Shared `kata cron workflow` definitions describe command or prompt steps rather
than this issue graph. See the [Cron reference](../reference/cron.md) for the
adapter contract.

Put run-level decisions in comments on the parent issue and stage details in
each child's comments (`kata show <ref>`). You can inspect project activity with
`kata events`; the parent issue does not automatically roll up its children's
history.
