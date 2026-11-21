<script lang="ts">
  import ChevronDownIcon from '@lucide/svelte/icons/chevron-down'
  import XIcon from '@lucide/svelte/icons/x'
  import {
    SearchInput,
    SelectDropdown,
    Typeahead,
    autoReposition,
    dismissable,
    floatingPopoverStyle,
    type TypeaheadOption,
  } from '@kenn-io/kit-ui'
  import { tick } from 'svelte'

  import type { KataProjectSummary, KataTaskSearchFilters } from '../lib/kata/types'
  import { projectMarkColor } from '../lib/kata/projectColor'

  interface Props {
    onSubmitSearch?: ((query: string) => void) | undefined
    filters: KataTaskSearchFilters
    projects: readonly KataProjectSummary[]
    projectView?: boolean
    showStatus?: boolean
    onChange: (
      filters: KataTaskSearchFilters,
      changed: keyof KataTaskSearchFilters,
    ) => void | Promise<void>
  }

  let {
    filters,
    projects,
    projectView = false,
    showStatus = true,
    onChange,
    onSubmitSearch,
  }: Props = $props()
  let draftOverride = $state<KataTaskSearchFilters | null>(null)
  let draft = $derived(draftOverride ?? filters)
  let open = $state(false)
  let trigger = $state<HTMLButtonElement>()
  let panel = $state<HTMLDivElement>()
  let panelStyle = $state('')
  let lastFilters: KataTaskSearchFilters | null = null

  $effect(() => {
    if (filters !== lastFilters) {
      lastFilters = filters
      draftOverride = draftOverride && {
        ...filters,
        query: draftOverride.query.trim() === filters.query ? draftOverride.query : filters.query,
        owner: draftOverride.owner.trim() === filters.owner ? draftOverride.owner : filters.owner,
        label: draftOverride.label.trim() === filters.label ? draftOverride.label : filters.label,
      }
    }
  })

  const statusOptions = [
    { value: 'open', label: 'Open' },
    { value: 'ready', label: 'Ready' },
    { value: 'closed', label: 'Closed' },
    { value: 'all', label: 'All' },
  ]
  const relationshipOptions = [
    { value: '', label: 'Any' },
    { value: 'parent', label: 'Has parent' },
    { value: 'child', label: 'Has child' },
    { value: 'blocks', label: 'Blocks' },
    { value: 'blocked_by', label: 'Blocked by' },
    { value: 'related', label: 'Related' },
  ]
  const projectOptions = $derived.by<TypeaheadOption[]>(() =>
    projects
      .map((project) => ({
        name: project.uid,
        label: project.name,
        meta: String(project.open_count),
      }))
      .sort((a, b) => a.label.localeCompare(b.label, undefined, { sensitivity: 'base' })),
  )
  const selectedProject = $derived.by(() => {
    const scope = draft.scope
    return scope.kind === 'project'
      ? projects.find((project) => project.uid === scope.project_uid)
      : undefined
  })
  const activeFilterCount = $derived(
    (draft.scope.kind === 'project' && !projectView ? 1 : 0) +
      (draft.owner.trim() ? 1 : 0) +
      (draft.label.trim() ? 1 : 0) +
      (draft.relationships?.length ? 1 : 0),
  )

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

  async function focusFilter(field: keyof KataTaskSearchFilters): Promise<void> {
    if (!open) open = true
    await tick()
    position()
    if (field === 'scope') {
      panel?.querySelector<HTMLButtonElement>('.filter-control-project button')?.click()
      await tick()
    }
    const selector =
      field === 'scope'
        ? '.filter-control-project input'
        : field === 'relationships'
          ? '.filter-control-relationship button'
          : `[data-filter-field="${field}"]`
    panel?.querySelector<HTMLElement>(selector)?.focus()
  }

  function emit(changed: keyof KataTaskSearchFilters, next: Partial<KataTaskSearchFilters>): void {
    const nextFilters = {
      ...draft,
      ...next,
      scope: next.scope ?? draft.scope,
    }
    draftOverride = nextFilters
    void onChange(nextFilters, changed)
  }

  function inputValue(event: Event): string {
    const target = event.currentTarget
    if (target instanceof HTMLInputElement || target instanceof HTMLSelectElement)
      return target.value
    return ''
  }

  function clear(changed: keyof KataTaskSearchFilters): void {
    if (changed === 'scope') emit('scope', { scope: { kind: 'all' } })
    else if (changed === 'relationships') emit('relationships', { relationships: [] })
    else if (changed === 'owner') emit('owner', { owner: '' })
    else if (changed === 'label') emit('label', { label: '' })
  }

  function relationshipLabel(value: string | undefined): string | undefined {
    return relationshipOptions.find((option) => option.value === value)?.label
  }
</script>

<section class="kata-search-panel" aria-label="Search and filters">
  <div class="kata-search-toolbar">
    <div class="query-field">
      <SearchInput
        value={draft.query}
        block
        placeholder="Search tasks..."
        ariaLabel="Search tasks"
        oninput={(query) => emit('query', { query })}
        onkeydown={(event) => {
          if (event.key === 'Enter' && !event.isComposing) onSubmitSearch?.(draft.query.trim())
        }}
      />
    </div>

    {#if showStatus}
      <div class="filter-control filter-control-status">
        <span class="kit-sr-only">Status</span>
        <SelectDropdown
          title="Status"
          value={draft.status}
          options={statusOptions}
          onchange={(value) => emit('status', { status: value as KataTaskSearchFilters['status'] })}
        />
      </div>
    {/if}

    <div class="filter-menu">
      <button
        bind:this={trigger}
        class="filter-trigger"
        type="button"
        aria-label={activeFilterCount ? `Filter, ${activeFilterCount} active` : 'Filter'}
        aria-expanded={open}
        onclick={() => void toggle()}
      >
        <span>Filter</span>
        {#if activeFilterCount}<span class="filter-count">{activeFilterCount}</span>{/if}
        <ChevronDownIcon size={13} strokeWidth={2} aria-hidden="true" />
      </button>
      {#if open}
        <div bind:this={panel} class="filter-popover kit-popover-card" style={panelStyle}>
          <div class="filter-popover__title">Filter tasks</div>
          {#if !projectView}
            <div class="filter-row filter-control-project">
              <span>Project</span>
              <Typeahead
                options={projectOptions}
                value={draft.scope.kind === 'project' ? draft.scope.project_uid : ''}
                fallbackLabel="All projects"
                placeholder="Project"
                triggerPrefix="Project:"
                allowClear
                clearLabel="All projects"
                emptyLabel="No matching projects"
                onselect={(value) => {
                  emit('scope', {
                    scope: value === '' ? { kind: 'all' } : { kind: 'project', project_uid: value },
                  })
                }}
              />
            </div>
          {/if}
          <div class="filter-row">
            <label for="task-owner-filter">Owner</label>
            <input
              id="task-owner-filter"
              data-filter-field="owner"
              aria-label="Owner"
              value={draft.owner}
              placeholder="Any owner"
              oninput={(event) => emit('owner', { owner: inputValue(event) })}
              onchange={(event) => emit('owner', { owner: inputValue(event) })}
            />
          </div>
          <div class="filter-row">
            <label for="task-label-filter">Label</label>
            <input
              id="task-label-filter"
              data-filter-field="label"
              aria-label="Label"
              value={draft.label}
              placeholder="Any label"
              oninput={(event) => emit('label', { label: inputValue(event) })}
              onchange={(event) => emit('label', { label: inputValue(event) })}
            />
          </div>
          <div class="filter-row filter-control-relationship">
            <span>Relationship</span>
            <SelectDropdown
              title="Relationship"
              value={draft.relationships?.[0] ?? ''}
              options={relationshipOptions}
              onchange={(value) => emit('relationships', { relationships: value ? [value] : [] })}
            />
          </div>
        </div>
      {/if}
    </div>
  </div>

  {#if activeFilterCount}
    <div class="active-filter-chips" aria-label="Active filters">
      {#if draft.scope.kind === 'project' && !projectView}
        <div class="filter-chip">
          <span
            class="project-mark"
            style:background={projectMarkColor(selectedProject?.name ?? draft.scope.project_uid)}
          ></span>
          <button type="button" onclick={() => void focusFilter('scope')}>
            {selectedProject?.name ?? draft.scope.project_uid}
          </button>
          <button type="button" aria-label="Clear Project filter" onclick={() => clear('scope')}>
            <XIcon size={12} aria-hidden="true" />
          </button>
        </div>
      {/if}
      {#if draft.owner.trim()}
        <div class="filter-chip">
          <button type="button" onclick={() => void focusFilter('owner')}
            >Owner {draft.owner}</button
          >
          <button type="button" aria-label="Clear Owner filter" onclick={() => clear('owner')}>
            <XIcon size={12} aria-hidden="true" />
          </button>
        </div>
      {/if}
      {#if draft.label.trim()}
        <div class="filter-chip">
          <button type="button" onclick={() => void focusFilter('label')}
            >Label {draft.label}</button
          >
          <button type="button" aria-label="Clear Label filter" onclick={() => clear('label')}>
            <XIcon size={12} aria-hidden="true" />
          </button>
        </div>
      {/if}
      {#if draft.relationships?.[0]}
        <div class="filter-chip">
          <button type="button" onclick={() => void focusFilter('relationships')}>
            {relationshipLabel(draft.relationships[0])}
          </button>
          <button
            type="button"
            aria-label="Clear Relationship filter"
            onclick={() => clear('relationships')}
          >
            <XIcon size={12} aria-hidden="true" />
          </button>
        </div>
      {/if}
    </div>
  {/if}
</section>

<style>
  .kata-search-panel {
    box-sizing: border-box;
    display: flex;
    flex-direction: column;
    gap: 6px;
    flex-shrink: 0;
    padding: 4px 0 8px;
    border-bottom: 1px solid var(--border-muted);
    background: var(--bg-surface);
  }

  .kata-search-toolbar {
    width: 100%;
    display: flex;
    align-items: center;
    gap: var(--space-3);
    min-width: 0;
  }

  .query-field {
    flex: 1 1 160px;
    min-width: 160px;
  }

  .filter-control {
    display: flex;
    align-items: center;
    min-width: 0;
  }

  input {
    box-sizing: border-box;
    min-width: 0;
    height: 30px;
    border: 1px solid var(--border-default);
    border-radius: var(--radius-md);
    background: var(--bg-surface);
    color: var(--text-primary);
    font: inherit;
    font-size: var(--font-size-sm);
    padding: 0 var(--space-4);
  }

  input:focus {
    border-color: var(--accent-blue);
  }

  .filter-control-status :global(.kit-select-dropdown) {
    width: 140px;
  }

  .filter-control-relationship :global(.kit-select-dropdown) {
    width: 100%;
  }

  .filter-control-project :global(.kit-typeahead) {
    width: 190px;
  }

  .filter-control-project :global(.kit-typeahead__input) {
    height: 30px;
    font-size: var(--font-size-sm);
  }

  .filter-menu {
    position: relative;
    flex-shrink: 0;
  }

  .filter-trigger {
    display: inline-flex;
    align-items: center;
    justify-content: center;
    gap: 6px;
    min-height: 30px;
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

  .filter-trigger:hover,
  .filter-trigger:focus-visible,
  .filter-trigger[aria-expanded='true'] {
    border-color: var(--text-muted);
    color: var(--text-primary);
  }

  .filter-count {
    min-width: 16px;
    height: 16px;
    padding: 0 4px;
    border-radius: 8px;
    background: var(--bg-inset);
    font-size: var(--font-size-2xs);
    line-height: 16px;
    text-align: center;
  }

  .filter-popover {
    position: fixed;
    z-index: var(--z-popover);
    display: flex;
    flex-direction: column;
    gap: 8px;
    min-width: 280px;
    max-width: calc(100vw - 16px);
    max-height: calc(100vh - 16px);
    overflow-y: auto;
    padding: var(--space-5);
  }

  .filter-popover__title {
    color: var(--text-primary);
    font-size: var(--font-size-sm);
    font-weight: var(--font-weight-semibold);
  }

  .filter-row {
    display: grid;
    grid-template-columns: 86px minmax(0, 1fr);
    align-items: center;
    gap: 8px;
    color: var(--text-secondary);
    font-size: var(--font-size-sm);
  }

  .filter-row input {
    width: 100%;
  }

  .active-filter-chips {
    display: flex;
    flex-wrap: wrap;
    gap: 6px;
  }

  .filter-chip {
    display: inline-flex;
    align-items: center;
    min-height: 24px;
    gap: 2px;
    padding: 0 5px;
    border: 1px solid var(--border-muted);
    border-radius: 12px;
    background: var(--bg-inset);
    color: var(--text-secondary);
    font-size: var(--font-size-xs);
    white-space: nowrap;
  }

  .filter-chip button {
    display: inline-flex;
    align-items: center;
    min-height: 22px;
    padding: 0 3px;
    border: 0;
    background: transparent;
    color: inherit;
    font: inherit;
    cursor: pointer;
  }

  .filter-chip button:hover {
    color: var(--text-primary);
  }

  .filter-chip .project-mark {
    width: 8px;
    height: 8px;
    flex-basis: 8px;
  }

  @container list (max-width: 760px) {
    .kata-search-toolbar {
      flex-wrap: wrap;
    }

    .query-field {
      flex: 1 0 160px;
    }
  }
</style>
