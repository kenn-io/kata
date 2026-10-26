<script lang="ts">
  import { CommentCard, StatusDot } from '@kenn-io/kit-ui'
  import type { CronPeek } from '../../lib/cron/controller'
  import type { CronRun } from '../../lib/cron/types'
  import { compareInstants } from '../../lib/cron/time'
  import { statusDot } from '../../lib/cron/format'
  let {
    run,
    peek,
    onOpenIssue,
    onOpenJob,
  }: {
    run: CronRun
    peek: CronPeek
    onOpenIssue: (uid: string) => void
    onOpenJob: (uid: string) => void
  } = $props()
  const comment = $derived(
    peek.issue
      ? [...peek.issue.comments].sort(
          (a, b) => compareInstants(b.created_at, a.created_at) || b.uid.localeCompare(a.uid),
        )[0]
      : undefined,
  )
  const age = (value: string) => {
    const minutes = Math.max(0, Math.floor((Date.now() - Date.parse(value)) / 60000))
    return minutes < 60
      ? `${minutes}m ago`
      : minutes < 1440
        ? `${Math.floor(minutes / 60)}h ago`
        : `${Math.floor(minutes / 1440)}d ago`
  }
</script>

<div class="run-detail">
  <p class="summary">{run.summary.message ?? 'No run summary'}</p>
  <p class="status">
    <StatusDot status={statusDot(run.status)} label={run.status} /><strong>{run.status}</strong>
  </p>
  <dl>
    <dt>Started</dt>
    <dd>{run.started_at ?? '—'}</dd>
    <dt>Ended</dt>
    <dd>{run.ended_at ?? '—'}</dd>
    <dt>Created</dt>
    <dd>{run.created_at}</dd>
    <dt>Job</dt>
    <dd>
      {#if run.job_uid}<button type="button" onclick={() => onOpenJob(run.job_uid!)}
          >{run.job_uid}</button
        >{:else}—{/if}
    </dd>
    <dt>Issue</dt>
    <dd>
      {#if run.issue_uid}<button type="button" onclick={() => onOpenIssue(run.issue_uid!)}
          >{run.issue_uid}</button
        >{:else}No issue{/if}
    </dd>
  </dl>
  <section class="peek" aria-label="Latest on the issue">
    <h3>Latest on the issue</h3>
    {#if peek.status === 'loading'}<p>
        Loading latest on the issue…
      </p>{:else if peek.status === 'no-issue'}<p>
        No issue
      </p>{:else if peek.status === 'unreadable'}<p>Issue not readable</p>{:else}
      {#if peek.issue}<button type="button" onclick={() => onOpenIssue(run.issue_uid!)}
          >{peek.issue.issue.short_id ?? run.issue_uid} · {peek.issue.issue.title}</button
        >{/if}
      {#if comment}<CommentCard author={comment.author} time={age(comment.created_at)}
          ><p class="comment-body">{comment.body}</p></CommentCard
        >{:else}<p>No comments</p>{/if}
    {/if}
  </section>
</div>

<style>
  .run-detail {
    padding: var(--space-4);
    overflow-wrap: anywhere;
  }
  .summary {
    white-space: pre-wrap;
    font-size: var(--font-size-md);
  }
  dl {
    display: grid;
    grid-template-columns: auto minmax(0, 1fr);
    gap: var(--space-2) var(--space-4);
  }
  .status {
    display: flex;
    align-items: center;
    gap: var(--space-2);
  }
  dt {
    color: var(--text-muted);
  }
  dd {
    margin: 0;
  }
  button {
    background: transparent;
    border: 0;
    padding: 0;
    color: var(--accent-blue);
    font: inherit;
    cursor: pointer;
    text-align: left;
  }
  h3 {
    margin-top: var(--space-6);
  }
  .peek {
    min-height: 160px;
  }
  .comment-body {
    white-space: pre-wrap;
  }
  p {
    font-size: var(--font-size-sm);
  }
</style>
