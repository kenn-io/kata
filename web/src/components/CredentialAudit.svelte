<script lang="ts">
  import ArrowLeftIcon from '@lucide/svelte/icons/arrow-left'
  import RefreshCwIcon from '@lucide/svelte/icons/refresh-cw'
  import { SearchInput, SelectDropdown, type SelectDropdownOption } from '@kenn-io/kit-ui'

  import type { TokenOut } from '../lib/api/generated'

  type StateFilter = 'all' | TokenOut['state']
  type ScopeFilter = 'all' | 'scoped' | 'unscoped'

  interface Props {
    tokens: readonly TokenOut[]
    loading: boolean
    error?: string | undefined
    onRefresh: () => void | Promise<void>
    onBack: () => void | Promise<void>
  }

  let { tokens, loading, error = undefined, onRefresh, onBack }: Props = $props()

  const stateOptions: SelectDropdownOption[] = [
    { value: 'all', label: 'All' },
    { value: 'live', label: 'Live' },
    { value: 'expired', label: 'Expired' },
    { value: 'revoked', label: 'Revoked' },
  ]
  const scopeOptions: SelectDropdownOption[] = [
    { value: 'all', label: 'All' },
    { value: 'scoped', label: 'Scoped' },
    { value: 'unscoped', label: 'Unscoped' },
  ]

  let stateFilter = $state<StateFilter>('all')
  let scopeFilter = $state<ScopeFilter>('all')
  let search = $state('')
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

  function timestamp(value: string | null | undefined): string {
    if (!value) return '—'
    const parsed = new Date(value)
    if (Number.isNaN(parsed.getTime())) return value
    return new Intl.DateTimeFormat(undefined, {
      dateStyle: 'medium',
      timeStyle: 'short',
    }).format(parsed)
  }
</script>

<section class="credential-audit" aria-labelledby="credential-audit-heading">
  <header class="audit-header">
    <div class="audit-heading">
      <button type="button" class="back-button" aria-label="Back to issues" onclick={onBack}>
        <ArrowLeftIcon size={16} strokeWidth={1.8} aria-hidden="true" />
      </button>
      <div>
        <h2 id="credential-audit-heading">Credentials</h2>
        <p>Redacted credential inventory for this daemon.</p>
      </div>
    </div>
    <button
      type="button"
      class="refresh-button"
      aria-label="Refresh credentials"
      disabled={loading}
      onclick={onRefresh}
    >
      <RefreshCwIcon size={14} strokeWidth={1.8} aria-hidden="true" />
      <span>Refresh</span>
    </button>
  </header>

  <div class="filters" role="group" aria-label="Credential filters">
    <div class="filter-field">
      <span aria-hidden="true">State</span>
      <SelectDropdown
        title="Credential state"
        value={stateFilter}
        options={stateOptions}
        onchange={(value) => {
          stateFilter = value as StateFilter
        }}
      />
    </div>
    <div class="filter-field">
      <span aria-hidden="true">Scope</span>
      <SelectDropdown
        title="Credential scope"
        value={scopeFilter}
        options={scopeOptions}
        onchange={(value) => {
          scopeFilter = value as ScopeFilter
        }}
      />
    </div>
    <div class="search-filter">
      <span>Actor or name</span>
      <SearchInput
        value={search}
        size="sm"
        block
        ariaLabel="Search credentials"
        placeholder="Search actor or name"
        oninput={(value) => {
          search = value
        }}
      />
    </div>
  </div>

  {#if error}
    <p class="audit-message error" role="alert">{error}</p>
  {:else if loading && tokens.length === 0}
    <p class="audit-message" role="status">Refreshing credentials…</p>
  {:else if visibleTokens.length === 0}
    <p class="audit-message" role="status">
      {tokens.length === 0
        ? 'No credentials are retained by this daemon.'
        : 'No credentials match these filters.'}
    </p>
  {:else}
    {#if loading}
      <p class="refresh-status" role="status">Refreshing credentials…</p>
    {/if}
    <div class="table-scroll">
      <table>
        <thead>
          <tr>
            <th scope="col">ID</th>
            <th scope="col">Name</th>
            <th scope="col">Actor</th>
            <th scope="col">State</th>
            <th scope="col">Scope</th>
            <th scope="col">Created</th>
            <th scope="col">Expires</th>
            <th scope="col">Last observed use</th>
            <th scope="col">Revoked</th>
          </tr>
        </thead>
        <tbody>
          {#each visibleTokens as token (token.id)}
            <tr>
              <td data-label="ID" class="credential-id">{token.id}</td>
              <td data-label="Name">{token.name ?? '—'}</td>
              <td data-label="Actor">{token.actor}</td>
              <td data-label="State">
                <span class="state-badge" class:live={token.state === 'live'}>{token.state}</span>
              </td>
              <td data-label="Scope">
                {#if token.scope}
                  <div class="scope-value">
                    <span>issue subtree</span>
                    <span class="uid">{token.scope.project_uid}</span>
                    <span class="uid">{token.scope.root_issue_uid}</span>
                  </div>
                {:else}
                  <span>unscoped</span>
                {/if}
              </td>
              <td data-label="Created">
                <time datetime={token.created_at}>{timestamp(token.created_at)}</time>
              </td>
              <td data-label="Expires">
                {#if token.expires_at}
                  <time datetime={token.expires_at}>{timestamp(token.expires_at)}</time>
                {:else}
                  <span>—</span>
                {/if}
              </td>
              <td data-label="Last observed use">
                {#if token.last_used_at}
                  <time datetime={token.last_used_at}>{timestamp(token.last_used_at)}</time>
                {:else}
                  <span>—</span>
                {/if}
              </td>
              <td data-label="Revoked">
                {#if token.revoked_at}
                  <time datetime={token.revoked_at}>{timestamp(token.revoked_at)}</time>
                {:else}
                  <span>—</span>
                {/if}
              </td>
            </tr>
          {/each}
        </tbody>
      </table>
    </div>
  {/if}
</section>

<style>
  .credential-audit {
    width: 100%;
    min-width: 0;
    min-height: 0;
    overflow: auto;
    padding: var(--space-5);
    background: var(--bg-primary);
  }

  .audit-header,
  .audit-heading,
  .refresh-button {
    display: flex;
    align-items: center;
  }

  .audit-header {
    justify-content: space-between;
    gap: var(--space-4);
  }

  .audit-heading {
    min-width: 0;
    gap: var(--space-3);
  }

  h2 {
    font-size: var(--font-size-xl);
    line-height: 1.25;
  }

  .audit-heading p,
  .refresh-status {
    color: var(--text-muted);
    font-size: var(--font-size-sm);
  }

  .back-button,
  .refresh-button {
    border: 1px solid var(--border-default);
    border-radius: var(--radius-sm);
    background: var(--bg-surface);
  }

  .back-button {
    display: grid;
    flex: 0 0 auto;
    width: 30px;
    height: 30px;
    place-items: center;
  }

  .refresh-button {
    gap: var(--space-2);
    padding: 5px 10px;
    white-space: nowrap;
  }

  .back-button:hover,
  .refresh-button:hover:not(:disabled) {
    background: var(--bg-hover);
  }

  .refresh-button:disabled {
    cursor: wait;
    opacity: 0.55;
  }

  .filters {
    display: grid;
    grid-template-columns: minmax(110px, 160px) minmax(110px, 160px) minmax(180px, 1fr);
    gap: var(--space-3);
    margin: var(--space-4) 0;
  }

  .filter-field :global(.kit-select-dropdown) {
    width: 100%;
  }

  .filter-field,
  .search-filter {
    display: grid;
    gap: 4px;
    color: var(--text-muted);
    font-size: var(--font-size-xs);
    font-weight: 600;
  }

  .audit-message {
    margin-top: var(--space-6);
    color: var(--text-muted);
    text-align: center;
  }

  .audit-message.error {
    color: var(--accent-red);
  }

  .refresh-status {
    margin-bottom: var(--space-2);
  }

  .table-scroll {
    overflow-x: auto;
    border: 1px solid var(--border-default);
    border-radius: var(--radius-sm);
    background: var(--bg-surface);
  }

  table {
    width: 100%;
    min-width: 1060px;
    border-collapse: collapse;
    font-size: var(--font-size-sm);
  }

  th,
  td {
    padding: var(--space-3);
    border-bottom: 1px solid var(--border-default);
    text-align: left;
    vertical-align: top;
  }

  th {
    color: var(--text-muted);
    font-size: var(--font-size-xs);
    font-weight: 650;
    white-space: nowrap;
  }

  tbody tr:last-child td {
    border-bottom: 0;
  }

  .credential-id,
  .uid {
    font-family: var(--font-mono, ui-monospace, monospace);
  }

  .scope-value {
    display: grid;
    gap: 2px;
  }

  .uid {
    max-width: 28ch;
    overflow-wrap: anywhere;
    color: var(--text-muted);
    font-size: var(--font-size-xs);
  }

  .state-badge {
    display: inline-block;
    border-radius: 999px;
    background: var(--accent-red-soft);
    color: var(--accent-red);
    padding: 1px 7px;
    font-size: var(--font-size-xs);
    font-weight: 650;
  }

  .state-badge.live {
    background: color-mix(in srgb, var(--accent-green) 14%, transparent);
    color: var(--accent-green);
  }

  @media (max-width: 760px) {
    .credential-audit {
      padding: var(--space-4);
    }

    .audit-header {
      align-items: flex-start;
    }

    .filters {
      grid-template-columns: 1fr 1fr;
    }

    .search-filter {
      grid-column: 1 / -1;
    }

    .table-scroll {
      overflow: visible;
      border: 0;
      background: transparent;
    }

    table,
    thead,
    tbody,
    tr,
    th,
    td {
      display: block;
      min-width: 0;
    }

    thead {
      display: none;
    }

    tbody {
      display: grid;
      gap: var(--space-3);
    }

    tr {
      border: 1px solid var(--border-default);
      border-radius: var(--radius-sm);
      background: var(--bg-surface);
      padding: var(--space-3);
    }

    td {
      display: grid;
      grid-template-columns: minmax(108px, 0.42fr) minmax(0, 1fr);
      gap: var(--space-3);
      padding: 5px 0;
      border: 0;
      overflow-wrap: anywhere;
    }

    td::before {
      content: attr(data-label);
      color: var(--text-muted);
      font-size: var(--font-size-xs);
      font-weight: 650;
    }
  }

  @media (max-width: 640px) {
    .refresh-button span {
      display: none;
    }

    .filters {
      grid-template-columns: 1fr;
    }

    .search-filter {
      grid-column: auto;
    }

    td {
      grid-template-columns: 96px minmax(0, 1fr);
    }
  }
</style>
