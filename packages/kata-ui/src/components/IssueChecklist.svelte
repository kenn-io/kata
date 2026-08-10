<script lang="ts">
  import type { KataChecklistItem } from '../types.js'

  interface Props {
    items: readonly KataChecklistItem[]
  }

  let { items }: Props = $props()
</script>

{#if items.length > 0}
  <section class="detail-section" aria-labelledby="kata-checklist-heading">
    <h3 id="kata-checklist-heading">Checklist</h3>
    <ul>
      {#each items as item (item.id)}
        <li class:done={item.done}>
          <span aria-hidden="true">{item.done ? '✓' : '○'}</span>
          <span>{item.text}</span>
        </li>
      {/each}
    </ul>
  </section>
{/if}

<style>
  .detail-section {
    min-width: 0;
    border-top: 1px solid var(--border-muted, #e2e4e8);
    padding-top: 16px;
  }

  h3 {
    margin: 0 0 9px;
    color: var(--text-muted, #656a73);
    font-size: var(--font-size-xs, 0.75rem);
    font-weight: 650;
    letter-spacing: 0.04em;
    text-transform: uppercase;
  }

  ul {
    display: grid;
    gap: 8px;
    margin: 0;
    padding: 0;
    list-style: none;
  }

  li {
    display: flex;
    align-items: baseline;
    gap: 8px;
  }

  li.done > span:last-child {
    color: var(--text-muted, #656a73);
    text-decoration: line-through;
  }
</style>
