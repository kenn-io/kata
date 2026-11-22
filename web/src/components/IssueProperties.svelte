<script lang="ts">
  import type { Snippet } from 'svelte'
  import XIcon from '@lucide/svelte/icons/x'
  import ChevronDownIcon from '@lucide/svelte/icons/chevron-down'
  import { Avatar, Typeahead, type TypeaheadOption } from '@kenn-io/kit-ui'
  import { Chip, SelectDropdown } from '@kenn-io/kit-ui'
  import { formatTimestamp } from '@kenn-io/kit-ui/utils/time'
  import type { KataTaskDetail } from '../lib/kata/types'
  import { deadlineDate, scheduledDate } from '../lib/kata/view'
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

  function formatDate(value: string): string {
    const parts = value.split('-')
    if (parts.length !== 3) return value
    const [year, month, day] = parts
    const date = new Date(Number(year), Number(month) - 1, Number(day))
    if (Number.isNaN(date.getTime())) return value
    return date.toLocaleDateString(undefined, {
      month: 'short',
      day: 'numeric',
      year: new Date().getFullYear() === date.getFullYear() ? undefined : 'numeric',
    })
  }

  function scheduledLabel(): string {
    const saved = issue.issue.metadata.scheduled_on
    const value = dateZone(saved) ? scheduledDate(issue.issue) : saved
    return value ? formatDate(value) : 'When'
  }

  function dueLabel(): string {
    const value = deadlineDate(issue.issue)
    return value ? formatDate(value) : 'No due date'
  }

  function priorityLabel(): string {
    const priority = issue.issue.priority
    return priority === undefined || priority === null ? 'No priority' : `P${priority}`
  }

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

<section class="properties" aria-label="Properties">
  <div class="property-row">
    <span>Status</span><strong
      >{issue.issue.status === 'closed'
        ? 'Closed'
        : issue.issue.open_blocked_by?.length
          ? 'Blocked'
          : 'Open'}</strong
    >
  </div>
  <div class="property-row">
    <span>Project</span>
    {#if administrativeActionsAllowed}
      <button type="button" class="value" disabled={actionsDisabled} onclick={onMove}
        >{issue.issue.project_name}</button
      >
    {:else}<strong>{issue.issue.project_name}</strong>{/if}
  </div>
  {#if issue.issue.priority !== undefined || activeProperty === 'priority'}
    <div class="property-row">
      <span>Priority</span>
      {#if activeProperty === 'priority'}
        <SelectDropdown
          title="Priority"
          value={priorityDraft}
          options={priorityOptions}
          disabled={actionsDisabled}
          onchange={(value) => void updatePriority(value)}
        />
      {:else}<button
          type="button"
          class="value"
          aria-label="Edit priority"
          disabled={actionsDisabled}
          onclick={() => openProperty('priority')}>{priorityLabel()}</button
        >{/if}
    </div>
  {/if}
  <div class="property-row">
    <span>Owner</span>
    <div
      class="owner-controls"
      onfocusin={handleOwnerFocusIn}
      onfocusoutcapture={handleOwnerFocusOutCapture}
    >
      {#key ownerEditorGeneration}
        <Typeahead
          options={ownerOptions}
          value={issue.issue.owner ?? ''}
          fallbackLabel="Unassigned"
          placeholder="Owner"
          allowClear
          allowCustom
          clearLabel="Unassigned"
          emptyLabel="Enter an owner"
          disabled={actionsDisabled && !ownerEditorOpen}
          onquery={trackOwnerQuery}
          onselect={updateOwner}
        >
          {#snippet trigger({ attachment })}
            <button
              type="button"
              class="owner-trigger"
              aria-label={`Owner: ${issue.issue.owner ?? 'Unassigned'}`}
              disabled={actionsDisabled && !ownerEditorOpen}
              {@attach attachment}
            >
              {#if issue.issue.owner}<Avatar name={issue.issue.owner} size={20} decorative />{/if}
              {issue.issue.owner ?? 'Unassigned'}
              <ChevronDownIcon size={14} aria-hidden="true" />
            </button>
          {/snippet}
        </Typeahead>
      {/key}
      {#if issue.issue.assignment_expires_on}<span
          >until {formatTimestamp(issue.issue.assignment_expires_on)}</span
        >{/if}
      <SelectDropdown
        title="Assignment duration"
        value={assignmentTTLDraft}
        options={assignmentTTLOptions}
        disabled={actionsDisabled}
        onchange={(value) => void claimAssignment(value)}
      />
    </div>
  </div>
  {#if scheduledDate(issue.issue) || activeProperty === 'scheduled'}
    <div class="property-row">
      <span>Scheduled</span>
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
          class="value"
          aria-label="Edit scheduled"
          disabled={actionsDisabled}
          onclick={() => openProperty('scheduled')}>{scheduledLabel()}</button
        >{/if}
    </div>
  {/if}
  {#if deadlineDate(issue.issue) || activeProperty === 'due'}
    <div class="property-row">
      <span>Due</span>
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
          class="value"
          aria-label="Edit due date"
          disabled={actionsDisabled}
          onclick={() => openProperty('due')}>{dueLabel()}</button
        >{/if}
    </div>
  {/if}
  <div class="property-row">
    <span>Labels</span>
    <div class="labels">
      {#each issue.labels as label (label.label)}
        <Chip
          size="xs"
          tone="muted"
          uppercase={false}
          interactive
          ariaLabel={`Remove label ${label.label}`}
          disabled={actionsDisabled}
          onclick={() => {
            if (!actionsDisabled) void onRemoveLabel(uid(), label.label)
          }}>{label.label}<XIcon size={11} aria-hidden="true" /></Chip
        >
      {/each}
      {#if addingLabel}
        <input
          aria-label="New label"
          class="label-input"
          value={labelDraft}
          disabled={actionsDisabled}
          oninput={(event) => {
            labelDraft = event.currentTarget.value
          }}
          onkeydown={handleLabelKeydown}
          onblur={() => void submitLabel()}
        />
      {:else}<button
          class="value"
          type="button"
          aria-label="Add label"
          disabled={actionsDisabled}
          onclick={() => {
            addingLabel = true
          }}>+</button
        >{/if}
    </div>
  </div>
  {#if repeats}<div class="property-row"><span>Repeats</span>{@render repeats()}</div>{/if}
  {#if issue.issue.status === 'open'}
    <div class="property-row">
      <span>Add</span>
      <div class="add-actions">
        {#if issue.issue.priority === undefined && activeProperty !== 'priority'}<button
            type="button"
            disabled={actionsDisabled}
            onclick={() => openProperty('priority')}>+ Priority</button
          >{/if}
        {#if !scheduledDate(issue.issue) && activeProperty !== 'scheduled'}<button
            type="button"
            disabled={actionsDisabled}
            onclick={() => openProperty('scheduled')}>+ Scheduled</button
          >{/if}
        {#if !deadlineDate(issue.issue) && activeProperty !== 'due'}<button
            type="button"
            disabled={actionsDisabled}
            onclick={() => openProperty('due')}>+ Due</button
          >{/if}
        {#if canCreateRecurrence}<button
            type="button"
            disabled={actionsDisabled}
            onclick={onCreateRecurrence}>+ Repeats…</button
          >{/if}
      </div>
    </div>
  {/if}
</section>

<style>
  .properties {
    display: grid;
    gap: 0;
    margin: 0 0 24px;
  }
  .property-row {
    display: grid;
    grid-template-columns: 96px minmax(0, 1fr);
    gap: 12px;
    min-height: 32px;
    align-items: center;
    font-size: var(--font-size-sm);
  }
  .property-row > span {
    color: var(--text-muted);
  }
  .property-row strong {
    font-weight: 500;
  }

  .owner-controls {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: 6px;
  }
  .owner-trigger {
    display: inline-flex;
    align-items: center;
    gap: 6px;
    border: 0;
    background: transparent;
    color: var(--text-primary);
    font: inherit;
    cursor: pointer;
    padding: 4px;
  }
  .value,
  .add-actions button {
    border: 0;
    background: transparent;
    color: var(--text-primary);
    font: inherit;
    text-align: left;
    padding: 4px;
    cursor: pointer;
  }
  .value:hover,
  .add-actions button:hover {
    background: var(--bg-surface-hover);
  }
  .labels,
  .add-actions {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: 6px;
  }
  .add-actions button {
    color: var(--text-muted);
  }
  .label-input {
    min-height: 28px;
    font: inherit;
  }
</style>
