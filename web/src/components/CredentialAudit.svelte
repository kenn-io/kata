<script lang="ts">
  import RefreshCwIcon from '@lucide/svelte/icons/refresh-cw'
  import {
    Avatar,
    Button,
    SearchInput,
    SelectDropdown,
    SettingsSection,
    type SelectDropdownOption,
  } from '@kenn-io/kit-ui'

  import type { TokenOut } from '../lib/api/generated'
  import { relativeTime, shortDate } from '../lib/kata/dates'
  import SettingsAlert from './SettingsAlert.svelte'

  type StateFilter = 'all' | TokenOut['state']
  type ScopeFilter = 'all' | 'scoped' | 'unscoped'

  interface Props {
    tokens: readonly TokenOut[]
    loading: boolean
    error?: string | undefined
    onRefresh: () => void | Promise<void>
  }

  let { tokens, loading, error = undefined, onRefresh }: Props = $props()

  // A short list reads at a glance; filters show only once it gets long.
  const filterThreshold = 8
  const stateOptions: SelectDropdownOption[] = [
    { value: 'all', label: 'Any state' },
    { value: 'live', label: 'Live' },
    { value: 'expired', label: 'Expired' },
    { value: 'revoked', label: 'Revoked' },
  ]
  const scopeOptions: SelectDropdownOption[] = [
    { value: 'all', label: 'Any access' },
    { value: 'unscoped', label: 'Full access' },
    { value: 'scoped', label: 'One issue subtree' },
  ]

  let stateFilter = $state<StateFilter>('all')
  let scopeFilter = $state<ScopeFilter>('all')
  let search = $state('')
  let filtering = $derived(stateFilter !== 'all' || scopeFilter !== 'all' || search.trim() !== '')
  let showFilters = $derived(tokens.length > filterThreshold || filtering)
  let visibleTokens = $derived.by(() => {
    const query = search.trim().toLocaleLowerCase()
    return tokens.filter((token) => {
      if (stateFilter !== 'all' && token.state !== stateFilter) return false
      if (scopeFilter === 'scoped' && !token.scope) return false
      if (scopeFilter === 'unscoped' && token.scope) return false
      return (
        !query ||
        token.actor.toLocaleLowerCase().includes(query) ||
        (token.name ?? '').toLocaleLowerCase().includes(query)
      )
    })
  })
  function ago(value: string): string {
    const label = relativeTime(value)
    if (label === 'now') return 'just now'
    return /^\d+[mhd]$/.test(label) ? `${label} ago` : label
  }

  function exact(value: string): string {
    const parsed = new Date(value)
    if (Number.isNaN(parsed.getTime())) return value
    return new Intl.DateTimeFormat(undefined, { dateStyle: 'medium', timeStyle: 'short' }).format(
      parsed,
    )
  }
</script>

<div class="credential-audit">
  <SettingsSection title="Credentials">
    <div class="title-action">
      <Button
        size="sm"
        ariaLabel="Refresh credentials"
        disabled={loading}
        onclick={() => void onRefresh()}
      >
        <RefreshCwIcon size={14} aria-hidden="true" />
        <span>Refresh</span>
      </Button>
    </div>

    {#if showFilters && !error}
      <div class="filters" role="group" aria-label="Credential filters">
        <SelectDropdown
          title="Credential state"
          value={stateFilter}
          options={stateOptions}
          onchange={(value) => {
            stateFilter = value as StateFilter
          }}
        />
        <SelectDropdown
          title="Credential scope"
          value={scopeFilter}
          options={scopeOptions}
          onchange={(value) => {
            scopeFilter = value as ScopeFilter
          }}
        />
        <SearchInput
          value={search}
          size="sm"
          block
          ariaLabel="Search credentials"
          placeholder="Actor or name"
          oninput={(value) => {
            search = value
          }}
        />
      </div>
    {/if}

    {#if error}
      <SettingsAlert>{error}</SettingsAlert>
    {:else if loading && tokens.length === 0}
      <p class="audit-message" role="status">Loading credentials…</p>
    {:else if tokens.length === 0}
      <p class="audit-message" role="status">This server has no credentials.</p>
    {:else if visibleTokens.length === 0}
      <p class="audit-message" role="status">No matching credentials</p>
    {:else}
      {#if loading}
        <p class="audit-message" role="status">Refreshing credentials…</p>
      {/if}
      <ul class="credential-list" aria-label="Credentials">
        {#each visibleTokens as token (token.id)}
          <li class="credential">
            <div class="credential-line">
              <strong class="credential-name" class:unnamed={!token.name}
                >{token.name ?? 'Unnamed'}</strong
              >
              <span class="credential-actor">
                <Avatar name={token.actor} decorative size={16} />
                <span>{token.actor}</span>
              </span>
              {#if token.state !== 'live'}
                <span class="state-badge">{token.state === 'expired' ? 'Expired' : 'Revoked'}</span>
              {/if}
              <span class="credential-id">ID {token.id}</span>
            </div>
            <div class="credential-meta">
              <span
                >Created <time datetime={token.created_at} title={exact(token.created_at)}
                  >{ago(token.created_at)}</time
                ></span
              >
              {#if token.last_used_at}
                <span
                  >Last used <time datetime={token.last_used_at} title={exact(token.last_used_at)}
                    >{ago(token.last_used_at)}</time
                  ></span
                >
              {:else}
                <span>Never used</span>
              {/if}
              {#if token.expires_at}
                <span
                  >{token.state === 'expired' ? 'Expired' : 'Expires'}
                  <time datetime={token.expires_at} title={exact(token.expires_at)}
                    >{shortDate(token.expires_at)}</time
                  ></span
                >
              {/if}
              {#if token.revoked_at}
                <span
                  >Revoked <time datetime={token.revoked_at} title={exact(token.revoked_at)}
                    >{ago(token.revoked_at)}</time
                  ></span
                >
              {/if}
            </div>
            {#if token.scope}
              <div class="credential-meta credential-scope">
                <span>Scoped to issue subtree</span>
                <span class="uid" title="Project">{token.scope.project_uid}</span>
                <span class="uid" title="Root issue">{token.scope.root_issue_uid}</span>
              </div>
            {/if}
          </li>
        {/each}
      </ul>
    {/if}
  </SettingsSection>
</div>

<style>
  .credential-audit {
    min-width: 0;
  }

  /* Refresh sits at the right end of the section's title row. */
  .title-action {
    position: absolute;
    top: 0;
    right: 0;
    display: flex;
    align-items: center;
    height: var(--layout-header-h);
  }

  .title-action :global(.kit-button) {
    gap: var(--space-2);
  }

  .filters {
    display: grid;
    grid-template-columns: 1fr 1fr 1fr;
    gap: var(--space-4);
  }

  .filters :global(.kit-select-dropdown) {
    width: 100%;
  }

  .audit-message {
    margin: 0;
    color: var(--text-secondary);
    font-size: var(--font-size-sm);
  }

  .credential-list {
    display: grid;
    gap: var(--space-5);
    margin: 0;
    padding: 0;
    list-style: none;
  }

  .credential {
    display: grid;
    gap: var(--space-1);
    min-width: 0;
  }

  .credential-line,
  .credential-meta {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    column-gap: var(--space-3);
    row-gap: var(--space-1);
    min-width: 0;
  }

  .credential-name {
    color: var(--text-primary);
    font-weight: var(--font-weight-semibold);
    overflow-wrap: anywhere;
  }

  .credential-name.unnamed {
    color: var(--text-secondary);
    font-weight: 400;
  }

  .credential-actor {
    display: inline-flex;
    align-items: center;
    gap: var(--space-2);
    min-width: 0;
    color: var(--text-secondary);
    overflow-wrap: anywhere;
  }

  .credential-meta {
    color: var(--text-secondary);
    font-size: var(--font-size-sm);
  }

  /* Dots between the times, so the line reads "Created … · Last used …". */
  .credential-meta:not(.credential-scope) > span + span::before {
    margin-right: var(--space-3);
    content: '·';
  }

  .credential-id,
  .uid {
    color: var(--text-muted);
    font-family: var(--font-mono);
    font-size: var(--font-size-xs);
    overflow-wrap: anywhere;
  }

  .state-badge {
    padding: 0 6px;
    border-radius: var(--radius-sm);
    background: color-mix(in srgb, var(--accent-red) 12%, transparent);
    color: var(--accent-red);
    font-size: var(--font-size-xs);
    font-weight: var(--font-weight-semibold);
    line-height: 18px;
  }

  @media (max-width: 760px) {
    .filters {
      grid-template-columns: 1fr 1fr;
    }

    .filters :global(.kit-search-input) {
      grid-column: 1 / -1;
    }
  }
</style>
