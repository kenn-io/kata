<script lang="ts">
  import type { KataIssueDetailModel } from '../types.js'

  interface Props {
    issue: KataIssueDetailModel['issue']
  }

  let { issue }: Props = $props()
</script>

<div class="summary" aria-label="Issue properties">
  <span class:closed={issue.status === 'closed'}>{issue.status}</span>
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

<style>
  .summary,
  .labels {
    display: flex;
    flex-wrap: wrap;
    gap: 8px;
  }

  .summary > span,
  .labels > span {
    border: 1px solid var(--border-muted, #e2e4e8);
    border-radius: 999px;
    padding: 3px 8px;
    font-size: var(--font-size-xs, 0.75rem);
  }

  .summary > span:first-child {
    color: var(--accent-green, #227a41);
  }

  .summary > span.closed {
    color: var(--text-muted, #656a73);
  }
</style>
