<script lang="ts">
  import { tick, type Snippet } from 'svelte'
  import XIcon from '@lucide/svelte/icons/x'
  import PlusIcon from '@lucide/svelte/icons/plus'
  import CircleIcon from '@lucide/svelte/icons/circle'
  import CircleCheckIcon from '@lucide/svelte/icons/circle-check'
  import ChevronDownIcon from '@lucide/svelte/icons/chevron-down'
  import {
    Avatar,
    Menu,
    MenuContent,
    MenuRadioGroup,
    MenuRadioItem,
    MenuTrigger,
    SelectDropdown,
    Typeahead,
    type TypeaheadOption,
  } from '@kenn-io/kit-ui'
  import { formatTimestamp } from '@kenn-io/kit-ui/utils/time'
  import { formatDay } from '../lib/kata/dates'
  import { markColor } from '../lib/marks'
  import type { KataTaskDetail } from '../lib/kata/types'
  import { deadlineDate, httpURL, scheduledDate } from '../lib/kata/view'
  import DatePicker from './DatePicker.svelte'

  interface Props {
    issue: KataTaskDetail
    administrativeActionsAllowed?: boolean | undefined
    onMove?: (() => void) | undefined
    onCreateRecurrence?: (() => void) | undefined
    canCreateRecurrence?: boolean | undefined
    repeats?: Snippet | undefined
    ownerOptions: TypeaheadOption[]
    actionsDisabled?: boolean | undefined
    draftResetGeneration?: number | undefined
    draftFenceGeneration?: number | undefined
    onPatchMetadata: (uid: string, patch: Record<string, unknown>) => boolean | Promise<boolean>
    onAssignOwner: (uid: string, owner: string) => boolean | Promise<boolean>
    onClaimAssignment?:
      | ((uid: string, ttlSeconds: number) => boolean | Promise<boolean>)
      | undefined
    onUnassignOwner: (uid: string) => boolean | Promise<boolean>
    onSetPriority: (uid: string, priority: number | null) => boolean | Promise<boolean>
    onAddLabel: (uid: string, label: string) => boolean | Promise<boolean>
    onRemoveLabel: (uid: string, label: string) => void | Promise<void>
  }

  let {
    issue,
    administrativeActionsAllowed = true,
    onMove = () => {},
    onCreateRecurrence = () => {},
    canCreateRecurrence = false,
    repeats = undefined,
    ownerOptions,
    actionsDisabled = false,
    draftResetGeneration = 0,
    draftFenceGeneration = 0,
    onPatchMetadata,
    onAssignOwner,
    onClaimAssignment = async () => false,
    onUnassignOwner,
    onSetPriority,
    onAddLabel,
    onRemoveLabel,
  }: Props = $props()

  type PropertyKey = 'scheduled' | 'due' | 'priority'
  type PropertyDraftKey = PropertyKey | 'owner' | 'label'

  interface PendingDraftReset {
    key: PropertyDraftKey
    uid: string
    generation: number
    ownerEditVersion?: number
  }

  const priorityOptions = [
    { value: '', label: 'No priority' },
    { value: '0', label: 'P0' },
    { value: '1', label: 'P1' },
    { value: '2', label: 'P2' },
    { value: '3', label: 'P3' },
    { value: '4', label: 'P4' },
  ]
  const assignmentTTLOptions = $derived([
    {
      value: '',
      label: issue.issue.assignment_expires_on ? 'Renew assignment…' : 'Assign temporarily…',
    },
    { value: '300', label: '5 minutes' },
    { value: '900', label: '15 minutes' },
    { value: '3600', label: '1 hour' },
    { value: '28800', label: '8 hours' },
    { value: '86400', label: '1 day' },
  ])

  let activeProperty = $state<PropertyKey | null>(null)
  let scheduledDraft = $state('')
  let dueDraft = $state('')
  let priorityDraft = $state('')
  let assignmentTTLDraft = $state('')
  let addingLabel = $state(false)
  let labelDraft = $state('')
  let labelInput: HTMLInputElement | null = $state(null)
  let priorityMenuOpen = $state(false)
  let ownerEditVersion = $state(0)
  let ownerDraft = $state('')
  let ownerEditorOpen = $state(false)
  let ownerEditorGeneration = $state(0)
  let trackedUID = $state<string | null>(null)
  let lastDraftResetGeneration = $state<number | null>(null)
  let lastDraftFenceGeneration = $state<number | null>(null)
  let pendingDraftReset = $state.raw<PendingDraftReset | null>(null)

  $effect(() => {
    if (issue.issue.uid === trackedUID) return
    trackedUID = issue.issue.uid
    activeProperty = null
    scheduledDraft = ''
    dueDraft = ''
    priorityDraft = ''
    assignmentTTLDraft = ''
    addingLabel = false
    labelDraft = ''
    ownerEditVersion = 0
    ownerDraft = ''
    ownerEditorOpen = false
    ownerEditorGeneration += 1
    pendingDraftReset = null
    lastDraftResetGeneration = draftResetGeneration
  })

  $effect(() => {
    const nextGeneration = draftFenceGeneration
    if (lastDraftFenceGeneration === null) {
      lastDraftFenceGeneration = nextGeneration
      return
    }
    if (nextGeneration === lastDraftFenceGeneration) return
    lastDraftFenceGeneration = nextGeneration
    for (const key of ['scheduled', 'due', 'priority', 'owner', 'label'] as const) resetDraft(key)
    pendingDraftReset = null
  })

  $effect(() => {
    const nextGeneration = draftResetGeneration
    if (lastDraftResetGeneration === null) {
      lastDraftResetGeneration = nextGeneration
      return
    }
    if (nextGeneration === lastDraftResetGeneration) return
    lastDraftResetGeneration = nextGeneration
    const pending = pendingDraftReset
    pendingDraftReset = null
    if (
      !pending ||
      pending.uid !== issue.issue.uid ||
      pending.generation === nextGeneration ||
      (pending.key === 'owner' && pending.ownerEditVersion !== ownerEditVersion)
    )
      return
    resetDraft(pending.key)
  })

  function resetDraft(key: PropertyDraftKey): void {
    if (key === 'scheduled') {
      if (activeProperty === key) activeProperty = null
      scheduledDraft = ''
    } else if (key === 'due') {
      if (activeProperty === key) activeProperty = null
      dueDraft = ''
    } else if (key === 'priority') {
      if (activeProperty === key) activeProperty = null
      priorityDraft = ''
    } else if (key === 'owner') {
      ownerDraft = ''
      ownerEditorOpen = false
      ownerEditorGeneration += 1
    } else {
      labelDraft = ''
      addingLabel = false
    }
  }

  function scheduleAcceptedReset(
    key: PropertyDraftKey,
    mutationUID: string,
    generation: number,
    ownerVersion: number | undefined = undefined,
  ): boolean {
    if (issue.issue.uid !== mutationUID) return false
    if (draftResetGeneration !== generation) {
      if (key === 'owner' && ownerVersion !== ownerEditVersion) return false
      resetDraft(key)
      return true
    }
    pendingDraftReset = {
      key,
      uid: mutationUID,
      generation,
      ...(ownerVersion === undefined ? {} : { ownerEditVersion: ownerVersion }),
    }
    return false
  }

  function uid(): string {
    return issue.issue.uid
  }

  function scheduledLabel(): string {
    const saved = issue.issue.metadata.scheduled_on
    const value = dateZone(saved) ? scheduledDate(issue.issue) : saved
    return value ? (value.includes('T') ? value : formatDay(value)) : ''
  }

  function dueLabel(): string {
    const value = deadlineDate(issue.issue)
    return value ? formatDay(value) : ''
  }

  const hasPriority = $derived(issue.issue.priority !== undefined && issue.issue.priority !== null)
  const prURL = $derived(httpURL(issue.issue.metadata['work.pr_url']))
  const shownPriority = $derived(
    activeProperty === 'priority' ? priorityDraft : hasPriority ? String(issue.issue.priority) : '',
  )

  // The current owner leads the menu, then Unassigned, then everyone else.
  const ownerMenu = $derived.by((): TypeaheadOption[] => {
    const current = issue.issue.owner ?? ''
    const unassigned = { name: '', label: 'Unassigned' }
    const rest = ownerOptions.filter((option) => option.name !== current)
    if (!current) return [unassigned, ...rest]
    const currentOption = ownerOptions.find((option) => option.name === current) ?? {
      name: current,
      label: current,
    }
    return [currentOption, unassigned, ...rest]
  })

  // An assignment ending today names its time; a later one, its day.
  function expiryLabel(value: string): string {
    const date = new Date(value)
    if (Number.isNaN(date.getTime())) return value
    if (date.toDateString() === new Date().toDateString())
      return date.toLocaleTimeString(undefined, { hour: 'numeric', minute: '2-digit' })
    const pad = (part: number) => String(part).padStart(2, '0')
    return formatDay(`${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())}`)
  }

  async function addPriority(): Promise<void> {
    openProperty('priority')
    await tick()
    priorityMenuOpen = true
  }

  function priorityMenuChanged(open: boolean): void {
    // An unset priority leaves its row again when its menu closes unchosen.
    if (!open && activeProperty === 'priority' && priorityDraft === '' && !hasPriority)
      activeProperty = null
  }

  function startLabel(): void {
    addingLabel = true
    void tick().then(() => labelInput?.focus())
  }

  const addItems = $derived(
    actionsDisabled
      ? []
      : [
          ...(hasPriority || activeProperty === 'priority'
            ? []
            : [{ label: 'Priority', onclick: () => void addPriority() }]),
          ...(scheduledDate(issue.issue) || activeProperty === 'scheduled'
            ? []
            : [{ label: 'Scheduled', onclick: () => openProperty('scheduled') }]),
          ...(deadlineDate(issue.issue) || activeProperty === 'due'
            ? []
            : [{ label: 'Due', onclick: () => openProperty('due') }]),
          ...(issue.labels.length > 0 || addingLabel
            ? []
            : [{ label: 'Labels', onclick: startLabel }]),
          ...(canCreateRecurrence ? [{ label: 'Repeats…', onclick: onCreateRecurrence }] : []),
        ],
  )

  function openProperty(property: PropertyKey): void {
    if (actionsDisabled) return
    activeProperty = property
    if (property === 'scheduled') {
      const saved = issue.issue.metadata.scheduled_on
      scheduledDraft = (dateZone(saved) ? scheduledDate(issue.issue) : saved?.slice(0, 10)) ?? ''
    } else if (property === 'due') {
      dueDraft = deadlineDate(issue.issue) ?? ''
    } else {
      priorityDraft =
        issue.issue.priority === undefined || issue.issue.priority === null
          ? ''
          : String(issue.issue.priority)
    }
  }

  function dateZone(saved: unknown): string {
    return typeof saved === 'string' ? (saved.match(/\[[^\]]+\]$/)?.[0] ?? '') : ''
  }

  async function patchScheduled(value: string): Promise<void> {
    if (actionsDisabled) return
    const property = 'scheduled'
    scheduledDraft = value
    if (
      dateZone(issue.issue.metadata.scheduled_on) &&
      value !== '' &&
      value === (scheduledDate(issue.issue) ?? '')
    ) {
      resetDraft(property)
      return
    }
    const mutationUID = uid()
    const generation = draftResetGeneration
    const ok = await onPatchMetadata(mutationUID, {
      scheduled_on: value === '' ? null : value + dateZone(issue.issue.metadata?.scheduled_on),
    })
    if (ok) scheduleAcceptedReset(property, mutationUID, generation)
  }

  async function patchDue(value: string): Promise<void> {
    if (actionsDisabled) return
    const property = 'due'
    dueDraft = value
    if (value !== '' && value === (deadlineDate(issue.issue) ?? '')) {
      resetDraft(property)
      return
    }
    const mutationUID = uid()
    const generation = draftResetGeneration
    const ok = await onPatchMetadata(mutationUID, {
      deadline_on: value === '' ? null : value + dateZone(issue.issue.metadata?.deadline_on),
    })
    if (ok) scheduleAcceptedReset(property, mutationUID, generation)
  }

  async function updateOwner(value: string): Promise<boolean> {
    if (actionsDisabled) return false
    const owner = value.trim()
    const mutationUID = uid()
    const generation = draftResetGeneration
    const editVersion = ownerEditVersion
    const ok = owner ? await onAssignOwner(mutationUID, owner) : await onUnassignOwner(mutationUID)
    if (!ok) return false
    return scheduleAcceptedReset('owner', mutationUID, generation, editVersion)
  }

  async function claimAssignment(value: string): Promise<void> {
    assignmentTTLDraft = value
    const ttlSeconds = Number(value)
    if (!Number.isInteger(ttlSeconds) || ttlSeconds < 60) {
      assignmentTTLDraft = ''
      return
    }
    await onClaimAssignment(uid(), ttlSeconds)
    assignmentTTLDraft = ''
  }

  async function updatePriority(value: string): Promise<void> {
    if (actionsDisabled) return
    const property = 'priority'
    const priority = value === '' ? null : Number(value)
    priorityDraft = value
    const mutationUID = uid()
    const generation = draftResetGeneration
    const ok = await onSetPriority(mutationUID, priority)
    if (ok) scheduleAcceptedReset(property, mutationUID, generation)
  }

  async function submitLabel(): Promise<void> {
    if (actionsDisabled) return
    const label = labelDraft.trim()
    if (!label) {
      addingLabel = false
      return
    }
    const mutationUID = uid()
    const generation = draftResetGeneration
    const ok = await onAddLabel(mutationUID, label)
    if (ok) scheduleAcceptedReset('label', mutationUID, generation)
  }

  function handleLabelKeydown(event: KeyboardEvent): void {
    if (event.key === 'Enter') {
      event.preventDefault()
      void submitLabel()
    } else if (event.key === 'Escape') {
      event.preventDefault()
      labelDraft = ''
      addingLabel = false
    }
  }

  function trackOwnerQuery(query: string): void {
    if (query === ownerDraft) return
    ownerDraft = query
    ownerEditVersion += 1
  }

  function handleOwnerFocusIn(event: FocusEvent): void {
    ownerEditorOpen =
      event.target instanceof HTMLInputElement && event.target.getAttribute('role') === 'combobox'
  }

  function handleOwnerFocusOutCapture(event: FocusEvent): void {
    if (actionsDisabled && ownerEditorOpen) {
      event.stopPropagation()
      return
    }
    const related = event.relatedTarget as Node | null
    if (!(event.currentTarget as HTMLElement).contains(related)) ownerEditorOpen = false
  }
</script>

<dl class="properties" aria-label="Properties">
  <dt>Status</dt>
  <dd>
    <span class="value-text status">
      {#if issue.issue.status === 'closed'}<CircleCheckIcon
          size={14}
          aria-hidden="true"
          class="status-closed"
        />Closed{:else}<CircleIcon size={14} aria-hidden="true" />Open{/if}
    </span>
  </dd>

  <dt>Project</dt>
  <dd>
    {#if administrativeActionsAllowed}
      <button
        type="button"
        class="value-button"
        aria-label={`Project: ${issue.issue.project_name}`}
        title="Move to another project…"
        disabled={actionsDisabled}
        onclick={onMove}
        ><span class="project-mark" style:background={markColor(issue.issue.project_name)}
        ></span>{issue.issue.project_name}</button
      >
    {:else}
      <span class="value-text"
        ><span class="project-mark" style:background={markColor(issue.issue.project_name)}
        ></span>{issue.issue.project_name}</span
      >
    {/if}
  </dd>

  {#if hasPriority || activeProperty === 'priority'}
    <dt>Priority</dt>
    <dd>
      <Menu bind:open={priorityMenuOpen} onopenchange={priorityMenuChanged}>
        <MenuTrigger
          class="value-button priority-trigger"
          ariaLabel={`Priority: ${shownPriority === '' ? 'No priority' : `P${shownPriority}`}`}
          disabled={actionsDisabled}
        >
          {#if shownPriority === ''}<span class="muted">No priority</span>{:else}<span
              class="priority-badge"
              data-priority={shownPriority}>P{shownPriority}</span
            >{/if}
          <ChevronDownIcon size={14} aria-hidden="true" />
        </MenuTrigger>
        <MenuContent ariaLabel="Priority" class="priority-menu">
          <MenuRadioGroup
            value={shownPriority}
            onchange={(value) => {
              activeProperty = 'priority'
              void updatePriority(value)
            }}
          >
            {#each priorityOptions as option (option.value)}
              <MenuRadioItem value={option.value} textValue={option.label} closeOnSelect>
                {#if option.value === ''}{option.label}{:else}<span
                    class="priority-badge"
                    data-priority={option.value}>{option.label}</span
                  >{/if}
              </MenuRadioItem>
            {/each}
          </MenuRadioGroup>
        </MenuContent>
      </Menu>
    </dd>
  {/if}

  <dt>Owner</dt>
  <dd>
    <div
      class="owner-value"
      onfocusin={handleOwnerFocusIn}
      onfocusoutcapture={handleOwnerFocusOutCapture}
    >
      {#key ownerEditorGeneration}
        <Typeahead
          options={ownerMenu}
          value={issue.issue.owner ?? ''}
          fallbackLabel="Unassigned"
          placeholder="Owner"
          allowCustom
          emptyLabel="Enter an owner"
          disabled={actionsDisabled && !ownerEditorOpen}
          onquery={trackOwnerQuery}
          onselect={updateOwner}
        >
          {#snippet icon(option)}
            {#if option.name}<Avatar name={option.name} size={16} decorative />{/if}
          {/snippet}
          {#snippet trigger({ attachment })}
            <button
              type="button"
              class="value-button owner-trigger"
              class:muted={!issue.issue.owner}
              aria-label={`Owner: ${issue.issue.owner ?? 'Unassigned'}`}
              disabled={actionsDisabled && !ownerEditorOpen}
              {@attach attachment}
            >
              {#if issue.issue.owner}<Avatar name={issue.issue.owner} size={16} decorative />{/if}
              {issue.issue.owner ?? 'Unassigned'}
              <ChevronDownIcon size={14} aria-hidden="true" />
            </button>
          {/snippet}
        </Typeahead>
      {/key}
      {#if issue.issue.assignment_expires_on}<span
          class="value-note"
          title={formatTimestamp(issue.issue.assignment_expires_on)}
          >until {expiryLabel(issue.issue.assignment_expires_on)}</span
        >{/if}
      {#if issue.issue.status === 'open'}
        <!-- Without a temporary assignment, its choice shows on hover and focus. -->
        <div class="assignment" class:assignment--reveal={!issue.issue.assignment_expires_on}>
          <SelectDropdown
            title="Assignment duration"
            value={assignmentTTLDraft}
            options={assignmentTTLOptions}
            disabled={actionsDisabled}
            onchange={(value) => void claimAssignment(value)}
          />
        </div>
      {/if}
    </div>
  </dd>

  {#if scheduledDate(issue.issue) || activeProperty === 'scheduled'}
    <dt>Scheduled</dt>
    <dd>
      {#if activeProperty === 'scheduled'}<DatePicker
          ariaLabel="Scheduled"
          value={scheduledDraft}
          disabled={actionsDisabled}
          clearable
          initiallyOpen
          onEscape={() => {
            activeProperty = null
          }}
          onchange={(value) => void patchScheduled(value)}
        />
      {:else}<button
          type="button"
          class="value-button"
          aria-label="Edit scheduled"
          disabled={actionsDisabled}
          onclick={() => openProperty('scheduled')}>{scheduledLabel()}</button
        >{/if}
    </dd>
  {/if}

  {#if deadlineDate(issue.issue) || activeProperty === 'due'}
    <dt>Due</dt>
    <dd>
      {#if activeProperty === 'due'}<DatePicker
          ariaLabel="Due"
          clearLabel="Clear due date"
          value={dueDraft}
          disabled={actionsDisabled}
          clearable
          initiallyOpen
          onEscape={() => {
            activeProperty = null
          }}
          onchange={(value) => void patchDue(value)}
        />
      {:else}<button
          type="button"
          class="value-button"
          aria-label="Edit due date"
          disabled={actionsDisabled}
          onclick={() => openProperty('due')}>{dueLabel()}</button
        >{/if}
    </dd>
  {/if}

  {#if prURL}
    <dt>PR</dt>
    <dd>
      <a
        class="value-text pr-link"
        href={prURL}
        target="_blank"
        rel="noopener noreferrer"
        title={prURL}>{prURL.replace(/^https?:\/\//, '')}</a
      >
    </dd>
  {/if}

  {#if issue.labels.length > 0 || addingLabel}
    <dt>Labels</dt>
    <dd>
      {#each issue.labels as label (label.label)}
        <span class="label-chip" title={label.label}
          ><span class="label-text">{label.label}</span><button
            type="button"
            class="label-remove"
            aria-label={`Remove label ${label.label}`}
            title={`Remove label ${label.label}`}
            disabled={actionsDisabled}
            onclick={() => {
              if (!actionsDisabled) void onRemoveLabel(uid(), label.label)
            }}><XIcon size={14} aria-hidden="true" /></button
          ></span
        >
      {/each}
      {#if addingLabel}
        <input
          aria-label="New label"
          class="label-input"
          bind:this={labelInput}
          value={labelDraft}
          disabled={actionsDisabled}
          oninput={(event) => {
            labelDraft = event.currentTarget.value
          }}
          onkeydown={handleLabelKeydown}
          onblur={() => void submitLabel()}
        />
      {:else}<button
          class="value-button add-label"
          type="button"
          aria-label="Add label"
          title="Add label"
          disabled={actionsDisabled}
          onclick={startLabel}><PlusIcon size={14} aria-hidden="true" /></button
        >{/if}
    </dd>
  {/if}

  {#if repeats}<dt>Repeats</dt>
    <dd>{@render repeats()}</dd>{/if}

  {#if addItems.length > 0}
    <dt>Add</dt>
    <dd class="add-actions">
      {#each addItems as item (item.label)}
        <button type="button" onclick={item.onclick}
          ><PlusIcon size={14} aria-hidden="true" />{item.label}</button
        >
      {/each}
    </dd>
  {/if}
</dl>

<style>
  /* Labels in a 96px column, values 12px after; every row is at least 32px
     and each value's 24px control sits centered on its label's line. */
  .properties {
    display: grid;
    grid-template-columns: 96px minmax(0, 1fr);
    align-items: start;
    column-gap: 12px;
    margin: 0;
    font-size: var(--font-size-sm);
    line-height: 18px;
  }
  dt {
    min-height: 32px;
    padding-top: 7px;
    color: var(--text-secondary);
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  dd {
    min-height: 32px;
    min-width: 0;
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    align-content: flex-start;
    gap: 4px;
    padding-block: 4px;
    margin: 0;
  }
  .value-text,
  .value-note {
    display: inline-flex;
    align-items: center;
    gap: 4px;
    min-height: 24px;
  }
  .pr-link {
    display: block;
    max-width: 100%;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
    line-height: 24px;
    color: var(--text-primary);
  }
  .value-note {
    color: var(--text-secondary);
  }
  .status :global(svg) {
    color: var(--text-muted);
  }
  .status :global(.status-closed) {
    color: var(--accent-green);
  }
  .project-mark {
    flex: 0 0 10px;
    width: 10px;
    height: 10px;
    border-radius: 3px;
  }
  .muted {
    color: var(--text-secondary);
  }

  /* A value reads as text until hovered; its hover box hangs into the gap so
     the text starts on the value column. */
  .value-button,
  dd :global(.value-button) {
    min-height: 24px;
    border: 1px solid transparent;
    border-radius: var(--radius-md);
    background: transparent;
    color: var(--text-primary);
    display: inline-flex;
    align-items: center;
    gap: 4px;
    margin-left: -9px;
    padding: 0 8px;
    font: inherit;
    cursor: pointer;
  }
  dd :global(.value-button.muted) {
    color: var(--text-secondary);
  }
  dd :global(.value-button svg) {
    color: var(--text-muted);
  }
  dd :global(.value-button:hover:not(:disabled)),
  dd :global(.value-button[aria-expanded='true']) {
    background: var(--bg-surface-hover);
  }
  .value-button.add-label {
    justify-content: center;
    width: 24px;
    margin-left: 0;
    padding: 0;
    color: var(--text-secondary);
  }

  .priority-badge {
    display: inline-flex;
    align-items: center;
    justify-content: center;
    min-width: 24px;
    height: 18px;
    padding: 0 4px;
    border-radius: var(--radius-sm);
    color: var(--text-secondary);
    font-variant-numeric: tabular-nums;
    line-height: 1;
  }
  .priority-badge[data-priority='0'] {
    background: color-mix(in srgb, var(--accent-red) 16%, transparent);
    color: color-mix(in srgb, var(--accent-red) 72%, var(--text-primary));
    font-weight: var(--font-weight-semibold);
  }
  .priority-badge[data-priority='1'] {
    background: color-mix(in srgb, var(--accent-amber) 16%, transparent);
    color: color-mix(in srgb, var(--accent-amber) 72%, var(--text-primary));
    font-weight: var(--font-weight-semibold);
  }
  .priority-badge[data-priority='2'] {
    background: var(--bg-inset);
  }
  :global(.priority-menu .kit-menu__item) {
    min-width: 120px;
  }

  /* The owner and its assignment choice share one line; the choice shrinks
     rather than wrapping under the owner. */
  .owner-value {
    display: inline-flex;
    flex-wrap: nowrap;
    max-width: 100%;
    align-items: center;
    gap: 4px;
    min-width: 0;
  }
  .owner-value :global(.kit-typeahead) {
    min-width: 0;
    --typeahead-panel-min-width: 180px;
  }
  /* The assignment choice reads as quiet text until hovered. */
  .assignment {
    min-width: 0;
  }
  .assignment :global(.kit-select-dropdown),
  :global(#app) .assignment :global(.kit-select-dropdown__trigger) {
    min-width: 0;
    max-width: 100%;
  }
  .owner-value :global(.kit-typeahead),
  .value-note {
    flex: none;
  }
  :global(#app) .assignment :global(.kit-select-dropdown__trigger) {
    min-height: 24px;
    height: 24px;
    padding-inline: 8px;
    border-color: transparent;
    background: transparent;
    color: var(--text-secondary);
    font-size: var(--font-size-sm);
    font-weight: var(--font-weight-normal);
  }
  :global(#app) .assignment :global(.kit-select-dropdown__trigger:hover:not(:disabled)),
  :global(#app) .assignment :global(.kit-select-dropdown__trigger[aria-expanded='true']) {
    background: var(--bg-surface-hover);
  }
  /* It keeps its place while hidden so the row doesn't jump. */
  .assignment--reveal {
    opacity: 0;
  }
  .owner-value:hover .assignment--reveal,
  .owner-value:focus-within .assignment--reveal {
    opacity: 1;
  }
  @media (hover: none) {
    .assignment--reveal {
      opacity: 1;
    }
  }

  /* A label chip; its remove button opens inside the chip on hover and focus. */
  .label-chip {
    display: inline-flex;
    align-items: center;
    gap: 2px;
    max-width: min(220px, 100%);
    height: 24px;
    padding: 0 8px;
    border-radius: var(--radius-md);
    background: var(--bg-inset);
    color: var(--text-secondary);
    white-space: nowrap;
  }
  .label-text {
    min-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
  }
  .label-remove {
    width: 0;
    height: 16px;
    overflow: hidden;
    border: 0;
    border-radius: var(--radius-sm);
    background: transparent;
    color: var(--text-secondary);
    display: inline-grid;
    flex: none;
    place-items: center;
    margin-right: -4px;
    padding: 0;
    cursor: pointer;
  }
  .label-chip:hover .label-remove,
  .label-remove:focus-visible {
    width: 16px;
  }
  @media (hover: none) {
    .label-remove {
      width: 16px;
    }
  }
  .label-remove:hover {
    background: var(--bg-surface-hover);
    color: var(--text-primary);
  }
  .label-input {
    width: min(180px, 100%);
    height: 24px;
    border: 1px solid var(--border-default);
    border-radius: var(--radius-md);
    background: var(--bg-primary);
    color: var(--text-primary);
    font: inherit;
    padding: 0 8px;
  }

  /* Quiet buttons with no fill until hover; the first icon starts on the
     value column. */
  .add-actions {
    margin-left: -8px;
  }
  .add-actions button {
    display: inline-flex;
    align-items: center;
    gap: 4px;
    height: 24px;
    padding: 0 8px;
    border: 0;
    border-radius: var(--radius-md);
    background: transparent;
    color: var(--text-secondary);
    font: inherit;
    cursor: pointer;
  }
  .add-actions button:hover,
  .add-actions button:focus-visible {
    background: var(--bg-surface-hover);
    color: var(--text-primary);
  }
  .add-actions :global(svg) {
    color: var(--text-muted);
  }
</style>
