<script lang="ts">
  import SlidersHorizontalIcon from '@lucide/svelte/icons/sliders-horizontal'
  import {
    Checkbox,
    SegmentedControl,
    autoReposition,
    dismissable,
    floatingPopoverStyle,
  } from '@kenn-io/kit-ui'
  import { tick } from 'svelte'
  import type { KataOptionalTaskColumn, KataTaskColumnVisibility } from '../lib/kata/columns'

  interface Props {
    visibility: KataTaskColumnVisibility
    columns: readonly { id: KataOptionalTaskColumn; label: string }[]
    splitDirection: 'horizontal' | 'vertical'
    onSplitDirectionChange: (direction: 'horizontal' | 'vertical') => void
    onchange: (visibility: KataTaskColumnVisibility) => void
  }

  let { visibility, columns, splitDirection, onSplitDirectionChange, onchange }: Props = $props()
  let open = $state(false)
  let trigger = $state<HTMLButtonElement>()
  let panel = $state<HTMLDivElement>()
  let panelStyle = $state('')

  $effect(() => {
    if (!open) return
    const cleanups = [
      dismissable({ owners: () => [trigger, panel], dismiss: close, escapeFocus: () => trigger }),
      autoReposition(() => [panel], position),
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

<div class="display-menu">
  <button
    bind:this={trigger}
    type="button"
    aria-label="Display"
    title="Display"
    aria-expanded={open}
    onclick={() => void toggle()}
  >
    <SlidersHorizontalIcon size={14} strokeWidth={2} aria-hidden="true" />
    <span class="action-label">Display</span>
  </button>
  {#if open}
    <div bind:this={panel} class="display-menu__panel kit-popover-card" style={panelStyle}>
      <div class="display-menu__section">
        <div class="display-menu__title">Task opens</div>
        <SegmentedControl
          ariaLabel="Task opens"
          options={[
            { value: 'horizontal', label: 'Beside list' },
            { value: 'vertical', label: 'Below list' },
          ]}
          value={splitDirection}
          onchange={(value) => onSplitDirectionChange(value as 'horizontal' | 'vertical')}
          block
        />
      </div>
      <div class="display-menu__section">
        <div class="display-menu__title">Columns, shown when space allows</div>
        {#each columns as column (column.id)}
          <Checkbox
            checked={visibility[column.id]}
            label={column.label}
            onchange={(checked) => onchange({ ...visibility, [column.id]: checked })}
          />
        {/each}
      </div>
    </div>
  {/if}
</div>

<style>
  .display-menu {
    position: relative;
    flex-shrink: 0;
  }

  .display-menu > button {
    display: inline-flex;
    align-items: center;
    justify-content: center;
    gap: var(--space-2);
    height: 24px;
    padding: 0 9px;
    border: 1px solid var(--border-default);
    border-radius: var(--radius-md);
    background: var(--bg-surface);
    color: var(--text-secondary);
    font: inherit;
    font-size: var(--font-size-sm);
    font-weight: var(--font-weight-medium);
    white-space: nowrap;
    cursor: pointer;
  }

  .display-menu > button:hover,
  .display-menu > button:focus-visible,
  .display-menu > button[aria-expanded='true'] {
    border-color: var(--text-muted);
    color: var(--text-primary);
  }

  .display-menu__panel {
    position: fixed;
    z-index: var(--z-popover);
    display: flex;
    flex-direction: column;
    gap: 16px;
    min-width: 240px;
    max-width: calc(100vw - 16px);
    max-height: calc(100vh - 16px);
    overflow-y: auto;
    padding: var(--space-5);
  }

  .display-menu__section {
    display: flex;
    flex-direction: column;
    gap: 8px;
  }

  .display-menu__title {
    color: var(--text-muted);
    font-size: var(--font-size-xs);
    font-weight: var(--font-weight-medium);
  }

  @container list (max-width: 420px) {
    .action-label {
      display: none;
    }
  }
</style>
