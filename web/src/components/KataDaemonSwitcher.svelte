<script lang="ts">
  import CheckIcon from '@lucide/svelte/icons/check'
  import ChevronDownIcon from '@lucide/svelte/icons/chevron-down'
  import ServerIcon from '@lucide/svelte/icons/server'

  import type { WebDaemonInfo } from '../lib/daemons/client'

  interface Props {
    daemons: WebDaemonInfo[]
    activeId: string | undefined
    activeStatusLabel?: string | undefined
    activeStatusTone?: 'error' | undefined
    disabled?: boolean | undefined
    onSelect: (id: string) => void
  }

  let {
    daemons,
    activeId,
    activeStatusLabel = undefined,
    activeStatusTone = undefined,
    disabled = false,
    onSelect,
  }: Props = $props()

  let open = $state(false)
  let displayId = $derived(
    activeId ?? daemons.find((daemon) => daemon.default)?.id ?? daemons[0]?.id,
  )

  function choose(id: string): void {
    if (disabled) return
    open = false
    if (id !== activeId) onSelect(id)
  }

  function daemonStatusLabel(daemon: WebDaemonInfo): string {
    if (daemon.id === displayId && activeStatusLabel) return activeStatusLabel
    if (daemon.health === 'connected') return 'connected'
    if (daemon.health === 'auth_required') return 'needs auth'
    return 'unreachable'
  }

  function daemonStatusTone(daemon: WebDaemonInfo): string {
    if (daemon.id === displayId && activeStatusTone) return activeStatusTone
    return daemon.health
  }
</script>

<div class="daemon-switcher">
  <button
    type="button"
    class="daemon-chip"
    data-testid="daemon-chip"
    aria-label={`Switch Kata daemon: ${displayId ?? 'default'}`}
    title="Switch Kata daemon"
    aria-haspopup="menu"
    aria-expanded={open}
    onclick={() => {
      open = !open
    }}
  >
    <ServerIcon class="chip-icon" size={13} strokeWidth={1.9} aria-hidden="true" />
    <span class="chip-label">{displayId ?? 'kata'}</span>
    <ChevronDownIcon size={12} strokeWidth={2} aria-hidden="true" />
  </button>
  {#if activeStatusLabel}
    <span
      class="daemon-status"
      class:error={activeStatusTone === 'error'}
      role="status"
      aria-label="Kata daemon status"
    >
      {activeStatusLabel}
    </span>
  {/if}

  {#if open}
    <div class="daemon-menu" role="menu" aria-label="Configured Kata daemons">
      {#each daemons as daemon (daemon.id)}
        <button
          type="button"
          class="daemon-row"
          class:selected={daemon.id === activeId}
          data-testid={`daemon-row-${daemon.id}`}
          role="menuitemradio"
          aria-checked={daemon.id === activeId}
          {disabled}
          onclick={() => choose(daemon.id)}
        >
          <span class={`dot dot--${daemonStatusTone(daemon)}`} aria-hidden="true"></span>
          <span class="row-name">{daemon.id}</span>
          <span class="row-meta" title={daemonStatusLabel(daemon)}>{daemonStatusLabel(daemon)}</span
          >
          {#if daemon.id === activeId}
            <CheckIcon class="check" size={13} strokeWidth={2} aria-hidden="true" />
          {/if}
          {#if daemon.hint}
            <span class="row-hint">{daemon.hint}</span>
          {/if}
        </button>
      {/each}
    </div>
  {/if}
</div>

<style>
  .daemon-switcher {
    position: relative;
    display: inline-flex;
    align-items: center;
    gap: var(--space-3);
  }

  .daemon-chip {
    height: 28px;
    border: 1px solid var(--border-default);
    border-radius: var(--radius-sm);
    background: var(--bg-surface);
    color: var(--text-primary);
    display: inline-flex;
    align-items: center;
    gap: 6px;
    padding: 0 8px;
    font-size: var(--font-size-sm);
    line-height: 1;
    cursor: pointer;
  }

  .daemon-chip:hover {
    background: var(--bg-surface-hover);
  }

  .chip-label {
    min-width: 0;
    max-width: 120px;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .chip-icon {
    color: var(--text-muted);
    flex: none;
  }

  .daemon-status {
    color: var(--text-muted);
    font-size: var(--font-size-xs);
    white-space: nowrap;
  }

  .daemon-status.error {
    color: var(--accent-red);
  }

  .dot {
    width: 8px;
    height: 8px;
    border-radius: var(--radius-pill);
    flex: none;
  }

  .dot--connected {
    background: var(--accent-green);
  }

  .dot--auth_required {
    background: var(--accent-amber);
  }

  .dot--down {
    background: var(--text-faint);
  }

  .dot--error {
    background: var(--accent-red);
  }

  .daemon-menu {
    position: absolute;
    top: calc(100% + 6px);
    left: 0;
    z-index: 30;
    width: min(280px, calc(100vw - 16px));
    border: 1px solid var(--border-default);
    border-radius: var(--radius-md);
    background: var(--bg-surface);
    box-shadow: var(--shadow-popover, 0 8px 24px rgb(15 23 42 / 16%));
    padding: 5px;
  }

  .daemon-row {
    width: 100%;
    border: 0;
    border-radius: var(--radius-sm);
    background: transparent;
    color: var(--text-primary);
    display: grid;
    grid-template-columns: 14px minmax(0, 1fr) auto auto;
    align-items: center;
    gap: var(--space-4);
    padding: 7px 8px;
    text-align: left;
    font-size: var(--font-size-sm);
    cursor: pointer;
  }

  .daemon-row:hover:not(:disabled) {
    background: var(--bg-surface-hover);
  }

  .daemon-row:disabled {
    color: var(--text-muted);
    cursor: not-allowed;
    opacity: 0.6;
  }

  .daemon-row.selected {
    background: var(--bg-inset);
  }

  .row-name {
    min-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .row-meta {
    color: var(--text-muted);
    font-size: var(--font-size-xs);
    max-width: 132px;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .row-hint {
    grid-column: 2 / -1;
    color: var(--text-muted);
    font-size: var(--font-size-xs);
    line-height: 1.3;
  }

  .check {
    color: var(--accent-blue);
  }
</style>
