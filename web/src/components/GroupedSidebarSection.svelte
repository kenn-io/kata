<script lang="ts">
  import ChevronDownIcon from '@lucide/svelte/icons/chevron-down'
  import type { Snippet } from 'svelte'

  interface Props {
    label: string
    count: number
    collapsed: boolean
    onclick: () => void
    leading?: Snippet
    children: Snippet
  }

  const { label, count, collapsed, onclick, leading, children }: Props = $props()
</script>

<section class="sidebar-list-group">
  <button type="button" class="sidebar-group-header" aria-expanded={!collapsed} {onclick}>
    <span
      class="sidebar-group-header__chevron"
      class:sidebar-group-header__chevron--collapsed={collapsed}
    >
      <ChevronDownIcon size={10} strokeWidth={1.8} aria-hidden="true" />
    </span>
    {#if leading}
      <span class="sidebar-group-header__leading">{@render leading()}</span>
    {/if}
    <span class="sidebar-group-header__name">{label}</span>
    <span class="sidebar-group-header__count">{count}</span>
  </button>
  {#if !collapsed}
    {@render children()}
  {/if}
</section>

<style>
  .sidebar-list-group {
    display: grid;
    gap: var(--space-1);
    padding: var(--space-2) 12px var(--space-4);
  }

  .sidebar-group-header {
    display: flex;
    align-items: center;
    gap: var(--space-3);
    width: 100%;
    min-height: 26px;
    padding: 0 10px;
    border: 0;
    border-radius: var(--radius-md);
    background: transparent;
    color: var(--text-muted);
    cursor: pointer;
    font-family: inherit;
    font-size: var(--font-size-xs);
    font-weight: var(--font-weight-medium);
    text-align: left;
  }

  .sidebar-group-header:hover {
    color: var(--text-primary);
  }

  .sidebar-group-header__chevron {
    display: inline-flex;
    flex-shrink: 0;
    transition: transform 120ms ease;
  }

  .sidebar-group-header__chevron--collapsed {
    transform: rotate(-90deg);
  }

  .sidebar-group-header__leading {
    display: inline-flex;
    flex-shrink: 0;
  }

  .sidebar-group-header__leading:empty {
    display: none;
  }

  .sidebar-group-header__name {
    flex: 1;
    min-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .sidebar-group-header__count {
    flex-shrink: 0;
    color: var(--text-muted);
    font-size: var(--font-size-xs);
    font-variant-numeric: tabular-nums;
  }

  @media (prefers-reduced-motion: reduce) {
    .sidebar-group-header__chevron {
      transition: none;
    }
  }
</style>
