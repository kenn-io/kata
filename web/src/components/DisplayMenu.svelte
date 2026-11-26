<script lang="ts">
  // The Display button and its popover, shared by the list and the board so
  // both layouts keep their view options in one place.
  import SlidersHorizontalIcon from '@lucide/svelte/icons/sliders-horizontal'
  import { autoReposition, dismissable, floatingPopoverStyle } from '@kenn-io/kit-ui'
  import { tick, type Snippet } from 'svelte'

  interface Props {
    children: Snippet
  }

  let { children }: Props = $props()
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
      {@render children()}
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

  .display-menu__panel :global(.display-menu__section) {
    display: flex;
    flex-direction: column;
    gap: 8px;
  }

  .display-menu__panel :global(.display-menu__title) {
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
