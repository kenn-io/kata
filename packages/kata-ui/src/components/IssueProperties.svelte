<script lang="ts">
  import type { KataIssueDetailModel } from '../types.js'

  interface Props {
    issue: KataIssueDetailModel['issue']
  }

  let { issue }: Props = $props()
</script>

<div class="properties">
  <div class="summary" aria-label="Issue properties">
    <span class="status" class:closed={issue.status === 'closed'}>{issue.status}</span>
    {#if issue.priority !== undefined}<span>P{issue.priority}</span>{/if}
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

<style>
  .properties,
  .summary,
  .labels {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: 8px;
    min-width: 0;
  }

  .summary > span,
  .labels > span {
    border: 1px solid var(--border-muted, #e2e4e8);
    border-radius: 999px;
    padding: 3px 8px;
    font-size: var(--font-size-xs, 0.75rem);
  }

  .summary > span.status {
    color: color-mix(in srgb, var(--accent-green, #227a41) 72%, var(--text-primary, #202124));
  }

  .summary > span.closed {
    color: var(--text-muted, #656a73);
  }
</style>
