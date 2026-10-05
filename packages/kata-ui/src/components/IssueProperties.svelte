<script lang="ts">
  import type { KataIssueDetailModel } from '../types.js'
  import CreationAttribution from './CreationAttribution.svelte'

  interface Props {
    issue: KataIssueDetailModel['issue']
  }

  let { issue }: Props = $props()
</script>

<div class="properties">
  <div class="summary" aria-label="Issue properties">
    <span class="status" class:closed={issue.status === 'closed'}>{issue.status}</span>
    {#if issue.priority !== undefined}
      <span class="priority" data-priority={issue.priority}>P{issue.priority}</span>
    {/if}
    {#if issue.owner}<span>Owner: {issue.owner}</span>{/if}
    {#if issue.scheduledOn}<span>Scheduled: {issue.scheduledOn}</span>{/if}
    {#if issue.deadlineOn}<span>Deadline: {issue.deadlineOn}</span>{/if}
  </div>

  {#if issue.labels.length > 0}
    <div class="labels" aria-label="Labels">
      {#each issue.labels as label (label)}<span>{label}</span>{/each}
    </div>
  {/if}
</div>
<CreationAttribution creation={issue.creation} />

<style>
  .properties,
  .summary,
  .labels {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    min-width: 0;
  }

  .properties {
    gap: var(--space-4, 8px) var(--space-5, 12px);
  }

  .summary {
    gap: var(--space-2, 4px) var(--space-5, 12px);
    color: var(--text-secondary, #555b6e);
    font-size: var(--font-size-sm, 0.75rem);
  }

  .labels {
    gap: var(--space-2, 4px);
  }

  .status {
    display: inline-flex;
    align-items: center;
    gap: var(--space-2, 4px);
    color: var(--text-primary, #202124);
    font-weight: var(--font-weight-medium, 500);
    text-transform: capitalize;
  }

  .status::before {
    width: 8px;
    height: 8px;
    border-radius: 50%;
    background: var(--accent-green, #227a41);
    content: '';
  }

  .status.closed::before {
    background: var(--accent-purple, #7c3aed);
  }

  .priority {
    font-weight: var(--font-weight-semibold, 600);
  }

  .priority[data-priority='0'] {
    color: var(--accent-red, #dc2626);
  }

  .priority[data-priority='1'] {
    color: var(--accent-amber, #d97706);
  }

  .labels > span {
    border: 1px solid var(--border-default, #d8dae2);
    border-radius: 999px;
    padding: 0 8px;
    color: var(--text-secondary, #555b6e);
    font-size: var(--font-size-xs, 0.6875rem);
    line-height: 18px;
  }
</style>
