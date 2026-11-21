<script lang="ts">
  import ChevronDownIcon from '@lucide/svelte/icons/chevron-down'
  import {
    Avatar,
    Menu,
    MenuContent,
    MenuRadioGroup,
    MenuRadioItem,
    MenuTrigger,
    StatusDot,
  } from '@kenn-io/kit-ui'
  import type { WebDaemonInfo } from '../lib/daemons/client'

  interface Props {
    daemons: WebDaemonInfo[]
    activeId: string | undefined
    activeStatusLabel?: string | undefined
    activeStatusTone?: 'error' | undefined
    disabled?: boolean | undefined
    account?: string | undefined
    credential?: string | undefined
    onSelect: (id: string) => void
    onOpen?: () => void
  }
  let {
    daemons,
    activeId,
    activeStatusLabel,
    activeStatusTone,
    disabled = false,
    account,
    credential,
    onSelect,
    onOpen = () => {},
  }: Props = $props()
  let displayId = $derived(
    activeId ?? daemons.find((daemon) => daemon.default)?.id ?? daemons[0]?.id,
  )
  function status(daemon: WebDaemonInfo): string {
    if (daemon.id === displayId && activeStatusLabel) return activeStatusLabel
    if (daemon.health === 'auth_required') return 'needs auth'
    if (daemon.health === 'upgrade_required') return 'update required'
    if (daemon.health !== 'connected') return 'unreachable'
    return ''
  }
</script>

<Menu
  class="daemon-menu-root"
  onopenchange={(open) => {
    if (open) onOpen()
  }}
>
  <MenuTrigger ariaLabel={`Switch Kata daemon: ${displayId ?? 'default'}`}>
    {#snippet child({ attachment })}
      <button class="daemon-chip" data-testid="daemon-chip" {@attach attachment}>
        <Avatar name={displayId ?? 'Kata'} size={16} shape="rounded" decorative />
        <span class="chip-label">{displayId ?? 'kata'}</span>
        {#if activeStatusLabel}<span
            class="kit-sr-only"
            role="status"
            aria-label="Kata daemon status">{activeStatusLabel}</span
          ><StatusDot
            status={activeStatusTone === 'error' ? 'unclean' : 'stale'}
            label={activeStatusLabel}
          />{/if}
        <ChevronDownIcon size={12} aria-hidden="true" />
      </button>
    {/snippet}
  </MenuTrigger>
  <MenuContent ariaLabel="Configured Kata daemons" class="daemon-menu">
    {#if account}
      <dl class="account">
        <dt>Account</dt>
        <dd>{account}</dd>
        {#if credential}<dt>Credential</dt>
          <dd>{credential}</dd>{/if}
      </dl>
    {/if}
    <MenuRadioGroup
      value={activeId ?? ''}
      onchange={(id) => {
        if (id !== activeId) onSelect(id)
      }}
    >
      {#each daemons as daemon (daemon.id)}
        <MenuRadioItem
          value={daemon.id}
          disabled={disabled || daemon.health === 'upgrade_required'}
          closeOnSelect
          textValue={daemon.id}
        >
          <span class="daemon-row" data-testid={`daemon-row-${daemon.id}`}
            ><Avatar name={daemon.id} size={16} shape="rounded" decorative /><span class="row-name"
              >{daemon.id}</span
            ></span
          >
          {#if status(daemon)}<span class="row-hint"
              ><span class="row-status">{status(daemon)}</span
              >{#if daemon.hint}&nbsp;{daemon.hint}{/if}</span
            >{/if}
        </MenuRadioItem>
      {/each}
    </MenuRadioGroup>
  </MenuContent>
</Menu>

<style>
  :global(.daemon-menu-root) {
    min-width: 0;
    width: 100%;
  }
  /* The negative margin keeps the tile on the header's 16px inset. */
  .daemon-chip {
    display: inline-flex;
    align-items: center;
    gap: 6px;
    min-width: 0;
    max-width: 100%;
    margin-left: -8px;
    padding: 4px 8px;
    border-radius: var(--radius-md);
  }
  .daemon-chip:hover,
  .daemon-chip:global([aria-expanded='true']) {
    background: var(--bg-surface-hover);
  }
  .daemon-chip :global(.kit-avatar),
  .daemon-chip :global(.kit-status-dot),
  .daemon-chip :global(svg) {
    flex: none;
  }
  .chip-label,
  .row-name {
    min-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  /* Outranks kit-ui's scoped width: max-content so the menu holds 280px. */
  /* Its surface sits near the sidebar's; kit-ui derives the menu's hover
     and selected fills from --bg-popover, so they follow. */
  :global(.kit-menu__content.kit-popover-card.daemon-menu) {
    --bg-popover: var(--bg-surface);
    width: min(280px, calc(100vw - 16px));
  }
  /* The active server sits in a well darker than the menu, its check in
     primary ink at the row's right end. Every server name is primary ink. */
  :global(.daemon-menu .kit-menu__item.kit-menu__radio-item:not([aria-disabled='true'])) {
    color: var(--text-primary);
  }
  :global(.daemon-menu .kit-menu__radio-item[aria-checked='true']) {
    background: var(--bg-inset);
  }
  :global(.daemon-menu .kit-menu__radio-item) {
    box-sizing: border-box;
    min-height: 28px;
    padding-block: 4px;
    font-size: 14px;
  }
  :global(.daemon-menu .kit-menu__radio-content) {
    flex: 1;
    flex-direction: column;
    align-items: stretch;
    gap: 0;
  }
  :global(.daemon-menu .kit-menu__radio-item .kit-menu__indicator) {
    order: 1;
    color: var(--text-primary);
  }
  .account {
    display: grid;
    grid-template-columns: 72px minmax(0, 1fr);
    gap: var(--space-2) var(--space-4);
    margin: 0 0 var(--space-2);
    padding: 8px;
    color: var(--text-muted);
    font-size: var(--font-size-xs);
  }
  .account dd {
    margin: 0;
    color: var(--text-secondary);
  }
  .daemon-row {
    flex: 1;
    display: flex;
    align-items: center;
    gap: 8px;
    min-width: 0;
  }
  .row-status,
  .row-hint {
    color: var(--text-muted);
    font-size: var(--font-size-xs);
  }
  .row-name {
    flex: 1;
  }
  .row-hint {
    display: block;
    padding-left: 24px;
    white-space: normal;
    overflow-wrap: anywhere;
  }
</style>
