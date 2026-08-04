<script lang="ts">
  import { autoReposition, Checkbox, dismissable, floatingPopoverStyle } from '@kenn-io/kit-ui'
  import FunnelIcon from '@lucide/svelte/icons/funnel'
  import { tick } from 'svelte'
  import {
    KATA_LINK_RELATIONS,
    type KataLinkFilters,
    type KataLinkRelation,
  } from '../lib/kata/linkFilters'

  interface Props {
    filters: KataLinkFilters
    onChange: (next: KataLinkFilters) => void
  }

  let { filters, onChange }: Props = $props()
  let open = $state(false)
  let trigger = $state<HTMLButtonElement>()
  let panel = $state<HTMLDivElement>()
  let panelStyle = $state('')

  const relationLabels: Record<KataLinkRelation, string> = {
    parent: 'Parent',
    child: 'Child',
    blocks: 'Blocks',
    blocked_by: 'Blocked by',
    related: 'Related',
  }

  $effect(() => {
    if (!open) return
    const cleanups = [
      dismissable({
        owners: () => [panel, trigger],
        dismiss: close,
        escapeFocus: () => trigger,
      }),
      autoReposition(() => panel, position),
    ]
    return () => cleanups.forEach((cleanup) => cleanup())
  })

  function position(): void {
    if (!trigger || !panel) return
    panelStyle = floatingPopoverStyle({
      trigger: trigger.getBoundingClientRect(),
      viewportWidth: window.innerWidth,
      viewportHeight: window.innerHeight,
      popoverWidth: panel.offsetWidth,
      popoverHeight: panel.offsetHeight,
      align: 'end',
    })
  }

  function close(): void {
    open = false
  }

  async function toggle(): Promise<void> {
    open = !open
    if (!open) return
    await tick()
    position()
    await tick()
    position()
  }

  function changeStatus(status: 'open' | 'closed', checked: boolean): void {
    onChange({ ...filters, statuses: { ...filters.statuses, [status]: checked } })
  }

  function changeRelation(relation: KataLinkRelation, checked: boolean): void {
    onChange({ ...filters, relations: { ...filters.relations, [relation]: checked } })
  }
</script>

<div class="link-filter-menu">
  <button
    bind:this={trigger}
    type="button"
    class="link-filter-trigger"
    aria-label="Filter links"
    aria-expanded={open}
    onclick={toggle}
  >
    <FunnelIcon size={12} strokeWidth={2} aria-hidden="true" />
    <span>Filters</span>
  </button>

  {#if open}
    <div
      bind:this={panel}
      class="link-filter-panel kit-popover-card"
      style={panelStyle}
      role="group"
      aria-label="Link filters"
    >
      <fieldset>
        <legend>Task state</legend>
        <Checkbox
          label="Open"
          checked={filters.statuses.open}
          onchange={(checked) => changeStatus('open', checked)}
        />
        <Checkbox
          label="Closed"
          checked={filters.statuses.closed}
          onchange={(checked) => changeStatus('closed', checked)}
        />
      </fieldset>
      <fieldset>
        <legend>Relationship</legend>
        {#each KATA_LINK_RELATIONS as relation (relation)}
          <Checkbox
            label={relationLabels[relation]}
            checked={filters.relations[relation]}
            onchange={(checked) => changeRelation(relation, checked)}
          />
        {/each}
      </fieldset>
    </div>
  {/if}
</div>

<style>
  .link-filter-menu {
    position: relative;
  }

  .link-filter-trigger {
    min-height: 26px;
    border: 1px solid var(--border-default);
    border-radius: var(--radius-sm);
    background: var(--bg-surface);
    color: var(--text-muted);
    display: inline-flex;
    align-items: center;
    gap: var(--space-2);
    padding: 3px 8px;
    font: inherit;
    font-size: var(--font-size-xs);
    cursor: pointer;
  }

  .link-filter-trigger:hover,
  .link-filter-trigger[aria-expanded='true'] {
    border-color: var(--accent-blue);
    color: var(--text-primary);
  }

  .link-filter-trigger:focus-visible {
    outline: var(--focus-ring);
    outline-offset: 2px;
  }

  .link-filter-panel {
    position: fixed;
    z-index: var(--z-popover);
    width: 220px;
    max-width: calc(100vw - 16px);
    max-height: calc(100vh - 16px);
    overflow-y: auto;
    padding: 10px;
    display: grid;
    gap: var(--space-4);
  }

  fieldset {
    min-width: 0;
    border: 0;
    margin: 0;
    padding: 0;
    display: grid;
    gap: var(--space-3);
  }

  legend {
    margin: 0 0 3px;
    color: var(--text-muted);
    font-size: var(--font-size-xs);
    font-weight: 650;
    text-transform: uppercase;
  }
</style>
