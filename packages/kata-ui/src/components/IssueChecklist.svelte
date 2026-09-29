<script lang="ts">
  import type { KataChecklistItem } from '../types.js'

  interface Props {
    items: readonly KataChecklistItem[]
  }

  let { items }: Props = $props()

  const doneCount = $derived(items.filter((item) => item.done).length)
</script>

{#if items.length > 0}
  <section class="detail-section" aria-labelledby="kata-checklist-heading">
    <h3 id="kata-checklist-heading">
      Checklist <span class="count">{doneCount}/{items.length}</span>
    </h3>
    <ul>
      {#each items as item (item.id)}
        <li class:done={item.done}>
          <span class="mark" aria-hidden="true">{item.done ? '✓' : ''}</span>
          <span>{item.text}</span>
        </li>
      {/each}
    </ul>
  </section>
{/if}

<style>
  .detail-section {
    min-width: 0;
  }

  h3 {
    margin: 0 0 var(--space-4, 8px);
    color: var(--text-primary, #202124);
    font-size: var(--font-size-md, 0.8125rem);
    font-weight: var(--font-weight-semibold, 600);
  }

  .count {
    margin-left: var(--space-2, 4px);
    color: var(--text-muted, #656a73);
    font-weight: var(--font-weight-medium, 500);
  }

  ul {
    display: grid;
    gap: var(--space-3, 6px);
    margin: 0;
    padding: 0;
    list-style: none;
  }

  li {
    display: flex;
    align-items: baseline;
    gap: var(--space-4, 8px);
  }

  .mark {
    display: inline-grid;
    flex: 0 0 auto;
    place-items: center;
    width: 14px;
    height: 14px;
    border: 1px solid var(--border-default, #d8dae2);
    border-radius: var(--radius-sm, 4px);
    font-size: 10px;
    line-height: 1;
    transform: translateY(2px);
  }

  li.done .mark {
    border-color: var(--accent-blue, #2563eb);
    background: var(--accent-blue, #2563eb);
    color: var(--bg-surface, #ffffff);
  }

  li.done > span:last-child {
    color: var(--text-muted, #656a73);
    text-decoration: line-through;
  }
</style>
