<script lang="ts">
  import type { Snippet } from 'svelte'
  import ChevronRightIcon from '@lucide/svelte/icons/chevron-right'

  // One section of the task view: heading, count, actions on the right, then
  // the body. An empty section is one line, its `empty` text beside the
  // heading. `folded` starts the body closed under its count.
  interface Props {
    title: string
    count?: string | number | undefined
    empty?: string | undefined
    folded?: boolean | undefined
    hideBody?: boolean | undefined
    actions?: Snippet | undefined
    children?: Snippet | undefined
  }

  let {
    title,
    count = undefined,
    empty = undefined,
    folded = undefined,
    hideBody = false,
    actions = undefined,
    children = undefined,
  }: Props = $props()

  let open = $state(true)
  $effect.pre(() => {
    if (folded !== undefined) open = !folded
  })
  const headingID = $props.id()
</script>

<section class="detail-section" aria-labelledby={headingID}>
  <header class="detail-section-header">
    <h3 id={headingID}>{title}</h3>
    {#if empty}
      <span class="detail-section-note">{empty}</span>
    {:else if folded !== undefined}
      <button
        type="button"
        class="detail-section-fold"
        aria-expanded={open}
        aria-label={`${open ? 'Hide' : 'Show'} ${title.toLowerCase()}`}
        onclick={() => (open = !open)}
      >
        <ChevronRightIcon size={13} strokeWidth={2} aria-hidden="true" />
        <span>{count ?? (open ? 'Hide' : 'Show')}</span>
      </button>
    {:else if count !== undefined}
      <span class="detail-section-note">{count}</span>
    {/if}
    {#if actions}
      <div class="detail-section-actions">{@render actions()}</div>
    {/if}
  </header>
  {#if open && !empty && !hideBody && children}
    <div class="detail-section-body">{@render children()}</div>
  {/if}
</section>

<style>
  .detail-section {
    display: grid;
    gap: 8px;
    min-width: 0;
  }

  /* One 28px heading line: the count or empty text on the heading's
     baseline, actions as 24px controls at the right inset. */
  .detail-section-header {
    display: flex;
    align-items: baseline;
    gap: 8px;
    min-height: 28px;
    padding-top: 3px;
  }

  h3 {
    margin: 0;
    font-size: var(--font-size-lg);
    font-weight: var(--font-weight-semibold);
    line-height: 21px;
  }

  .detail-section-note {
    color: var(--text-secondary);
    font-size: var(--font-size-sm);
    font-variant-numeric: tabular-nums;
  }

  .detail-section-fold {
    display: inline-flex;
    align-items: center;
    align-self: center;
    gap: 4px;
    height: 24px;
    margin-left: -8px;
    padding: 0 8px;
    border: 0;
    border-radius: var(--radius-md);
    background: transparent;
    color: var(--text-secondary);
    font: inherit;
    font-size: var(--font-size-sm);
    font-variant-numeric: tabular-nums;
    cursor: pointer;
  }

  .detail-section-fold:hover {
    background: var(--bg-surface-hover);
    color: var(--text-primary);
  }

  .detail-section-fold :global(svg) {
    transition: transform 120ms ease;
  }

  .detail-section-fold[aria-expanded='true'] :global(svg) {
    transform: rotate(90deg);
  }

  .detail-section-actions {
    display: flex;
    align-items: center;
    align-self: center;
    gap: 4px;
    margin-left: auto;
  }

  .detail-section-actions :global(.section-action) {
    height: 24px;
    border: 0;
    border-radius: var(--radius-md);
    background: transparent;
    color: var(--text-secondary);
    font: inherit;
    font-size: var(--font-size-md);
    padding: 0 8px;
    cursor: pointer;
  }

  .detail-section-actions :global(.section-action:hover:not(:disabled)),
  .detail-section-actions :global(.section-action[aria-expanded='true']) {
    background: var(--bg-surface-hover);
    color: var(--text-primary);
  }

  .detail-section-body {
    min-width: 0;
  }

  /* A fold inside a section, such as finished checklist items or closed
     links, reads as a quiet "> 2 done" line. */
  .detail-section-body :global(.kit-disclosure--plain > .kit-disclosure__summary) {
    width: fit-content;
    min-height: 24px;
    padding: 0;
    gap: 4px;
    color: var(--text-secondary);
    font-weight: var(--font-weight-normal);
  }
  .detail-section-body :global(.kit-disclosure--plain > .kit-disclosure__summary:hover) {
    color: var(--text-primary);
  }
</style>
