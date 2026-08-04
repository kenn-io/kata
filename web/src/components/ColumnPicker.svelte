<script lang="ts">
  import Columns3Icon from '@lucide/svelte/icons/columns-3'
  import { Checkbox, autoReposition, dismissable, floatingPopoverStyle } from '@kenn-io/kit-ui'
  import { tick } from 'svelte'
  import { KATA_OPTIONAL_TASK_COLUMNS, type KataTaskColumnVisibility } from '../lib/kata/columns'

  interface Props {
    visibility: KataTaskColumnVisibility
    onchange: (visibility: KataTaskColumnVisibility) => void
    onShowAll: () => void
  }

  let { visibility, onchange, onShowAll }: Props = $props()
  let open = $state(false)
  let trigger = $state<HTMLButtonElement>()
  let panel = $state<HTMLDivElement>()
  let panelStyle = $state('')

  const allVisible = $derived(KATA_OPTIONAL_TASK_COLUMNS.every((column) => visibility[column.id]))

  $effect(() => {
    if (!open) return
    const cleanups = [
      dismissable({ owners: () => [trigger, panel], dismiss: close, escapeFocus: () => trigger }),
      autoReposition(() => panel, position),
    ]
    return () => cleanups.forEach((cleanup) => cleanup())
  })

  function close(): void {
    open = false
  }

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

  async function toggle(): Promise<void> {
    open = !open
    if (!open) return
    await tick()
    position()
    await tick()
    position()
  }
</script>

<div class="column-picker">
  <button
    bind:this={trigger}
    type="button"
    aria-label="Columns"
    title="Choose columns shown when space allows"
    aria-expanded={open}
    onclick={() => void toggle()}
  >
    <Columns3Icon size={13} strokeWidth={2} aria-hidden="true" />
    <span class="action-label">Columns</span>
  </button>
  {#if open}
    <div bind:this={panel} class="column-picker__panel kit-popover-card" style={panelStyle}>
      <div class="column-picker__title">Shown when space allows</div>
      {#each KATA_OPTIONAL_TASK_COLUMNS as column (column.id)}
        <Checkbox
          checked={visibility[column.id]}
          label={column.label}
          onchange={(checked) => onchange({ ...visibility, [column.id]: checked })}
        />
      {/each}
      <button type="button" class="column-picker__reset" disabled={allVisible} onclick={onShowAll}>
        Show all
      </button>
    </div>
  {/if}
</div>

<style>
  .column-picker {
    position: relative;
    flex-shrink: 0;
  }

  .column-picker > button {
    display: inline-flex;
    align-items: center;
    justify-content: center;
    gap: var(--space-2);
    min-height: 26px;
    padding: 0 var(--space-4);
    border: 1px solid var(--border-default);
    border-radius: var(--radius-sm);
    background: var(--bg-surface);
    color: var(--text-secondary);
    font: inherit;
    font-size: var(--font-size-xs);
    font-weight: 500;
    white-space: nowrap;
    cursor: pointer;
  }

  .column-picker > button:hover,
  .column-picker > button:focus-visible,
  .column-picker > button[aria-expanded='true'] {
    border-color: var(--border-strong);
    color: var(--text-primary);
  }

  .column-picker__panel {
    position: fixed;
    z-index: var(--z-popover);
    display: flex;
    flex-direction: column;
    gap: var(--space-4);
    min-width: 180px;
    max-width: calc(100vw - 16px);
    max-height: calc(100vh - 16px);
    overflow-y: auto;
    padding: var(--space-5);
  }

  .column-picker__title {
    color: var(--text-faint);
    font-size: var(--font-size-3xs);
    font-weight: 600;
    letter-spacing: 0.08em;
    text-transform: uppercase;
  }

  .column-picker__reset {
    margin-top: var(--space-2);
    padding: var(--space-3) var(--space-4) 0;
    border: 0;
    border-top: 1px solid var(--border-muted);
    background: transparent;
    color: var(--accent-blue);
    font: inherit;
    font-size: var(--font-size-xs);
    text-align: left;
    cursor: pointer;
  }

  .column-picker__reset:disabled {
    color: var(--text-faint);
    cursor: default;
  }
</style>
