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
    account = 'Local, no sign-in',
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
  onopenchange={(open) => {
    if (open) onOpen()
  }}
>
  <MenuTrigger ariaLabel={`Switch Kata daemon: ${displayId ?? 'default'}`}>
    {#snippet child({ attachment })}
      <button class="daemon-chip" data-testid="daemon-chip" {@attach attachment}>
        <Avatar name={displayId ?? 'Kata'} size={24} shape="rounded" decorative />
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
    <div class="account"><span>Account</span><strong>{account}</strong></div>
    {#if credential}<div class="account">
        <span>Credential</span><strong>{credential}</strong>
      </div>{/if}
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
            ><Avatar name={daemon.id} size={24} shape="rounded" decorative /><span class="row-name"
              >{daemon.id}</span
            >{#if status(daemon)}<span class="row-status">{status(daemon)}</span>{/if}</span
          >
          {#if daemon.hint}<span class="row-hint">{daemon.hint}</span>{/if}
        </MenuRadioItem>
      {/each}
    </MenuRadioGroup>
  </MenuContent>
</Menu>

<style>
  .daemon-chip {
    display: inline-flex;
    align-items: center;
    gap: 6px;
    min-width: 0;
    border-radius: var(--radius-sm);
  }
  .chip-label,
  .row-name {
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  :global(.daemon-menu) {
    width: min(280px, calc(100vw - 16px));
  }
  .account {
    display: flex;
    justify-content: space-between;
    gap: 12px;
    padding: 8px;
    color: var(--text-muted);
    font-size: var(--font-size-xs);
  }
  .account strong {
    color: var(--text-secondary);
    font-weight: normal;
  }
  .daemon-row {
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
  .row-status {
    margin-left: auto;
    white-space: nowrap;
  }
  .row-hint {
    display: block;
  }
</style>
