<script lang="ts">
  import CalendarIcon from '@lucide/svelte/icons/calendar'
  import ClockIcon from '@lucide/svelte/icons/clock-3'
  import FlagIcon from '@lucide/svelte/icons/flag'
  import TimerIcon from '@lucide/svelte/icons/timer'
  import UserIcon from '@lucide/svelte/icons/user-round'
  import XIcon from '@lucide/svelte/icons/x'
  import { Typeahead, type TypeaheadOption } from '@kenn-io/kit-ui'
  import { Button, Chip, SelectDropdown } from '@kenn-io/kit-ui'
  import { formatTimestamp } from '@kenn-io/kit-ui/utils/time'
  import type { KataTaskDetail } from '../lib/kata/types'
  import DatePicker from './DatePicker.svelte'

  interface Props {
    issue: KataTaskDetail
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
  let dateError = $state('')
  let priorityDraft = $state('')
  let assignmentTTLDraft = $state('')
  let addingLabel = $state(false)
  let editingLabels = $state(false)
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
    editingLabels = false
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
    editingLabels = false
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
    const value = scheduledDate()
    return value ? formatDate(value) : 'When'
  }

  function calendarDate(projected: string | undefined, saved: string | undefined): string {
    return projected ?? saved?.match(/^\d{4}-\d{2}-\d{2}/)?.[0] ?? ''
  }

  function movedDate(value: string, displayed: string, saved: string | undefined): string | null {
    if (!value) return null
    const match = saved?.match(/^(\d{4}-\d{2}-\d{2})(.*)$/)
    if (!match || !displayed) return value
    const day = 86_400_000
    const delta = (Date.parse(`${value}T00:00:00Z`) - Date.parse(`${displayed}T00:00:00Z`)) / day
    if (saved?.endsWith('Z')) {
      const instant = new Date(saved.replace(',', '.'))
      instant.setDate(instant.getDate() + delta)
      return instant.toISOString().slice(0, 19) + (saved.match(/([.,]\d+)?Z$/)?.[0] ?? 'Z')
    }
    const savedDate = match[1]!
    const suffix = match[2]!
    const zoned = suffix.match(/^(T\d{2}:\d{2}(?::\d{2})?)\[([^\]]+)\]$/)
    if (zoned) {
      const time = zoned[1]!
      const formatter = new Intl.DateTimeFormat('en-CA', {
        timeZone: zoned[2],
        year: 'numeric',
        month: '2-digit',
        day: '2-digit',
        hour: '2-digit',
        minute: '2-digit',
        second: '2-digit',
        hourCycle: 'h23',
      })
      const wallAt = (instant: number): number => {
        const parts = Object.fromEntries(
          formatter.formatToParts(instant).map((part) => [part.type, part.value]),
        )
        return Date.parse(
          `${parts.year}-${parts.month}-${parts.day}T${parts.hour}:${parts.minute}:${parts.second}Z`,
        )
      }
      const dayMillis = Date.parse(`${value}T00:00:00Z`)
      const candidates = [-1, 0, 1].map((offset) =>
        new Date(dayMillis + offset * day).toISOString().slice(0, 10),
      )
      candidates.sort(
        (a, b) =>
          Math.abs(Date.parse(a) - Date.parse(savedDate)) -
          Math.abs(Date.parse(b) - Date.parse(savedDate)),
      )
      for (const date of candidates) {
        const wall = Date.parse(`${date}${time}${time.length === 6 ? ':00' : ''}Z`)
        const offsets = new Set(
          [wall - 2 * day, wall, wall + 2 * day].map((time) => wallAt(time) - time),
        )
        const instants = [...offsets]
          .map((offset) => wall - offset)
          .filter((instant) => wallAt(instant) === wall)
        if (instants.length === 0 && offsets.size > 1) {
          let low = wall - 2 * day
          let high = wall + 2 * day
          const initialOffset = wallAt(low) - low
          while (high - low > 1000) {
            const mid = Math.floor((low + high) / 2000) * 1000
            if (wallAt(mid) - mid === initialOffset) low = mid
            else high = mid
          }
          if (wallAt(high - 1000) < wall && wallAt(high) > wall) instants.push(high)
        }
        if (instants.length === 0) continue
        const local = new Date(Math.min(...instants))
        const viewerDate = `${local.getFullYear()}-${String(local.getMonth() + 1).padStart(2, '0')}-${String(local.getDate()).padStart(2, '0')}`
        if (viewerDate === value) return date + suffix
      }
      return ''
    }
    const moved =
      Date.parse(`${match[1]}T00:00:00Z`) +
      Date.parse(`${value}T00:00:00Z`) -
      Date.parse(`${displayed}T00:00:00Z`)
    return new Date(Math.round(moved / day) * day).toISOString().slice(0, 10) + match[2]
  }

  function scheduledDate(): string {
    return calendarDate(issue.issue.scheduled_on_date, issue.issue.metadata.scheduled_on)
  }

  function dueLabel(): string {
    const value = deadlineDate()
    return value ? formatDate(value) : 'No due date'
  }

  function deadlineDate(): string {
    return calendarDate(issue.issue.deadline_on_date, issue.issue.metadata.deadline_on)
  }

  function priorityLabel(): string {
    const priority = issue.issue.priority
    return priority === undefined || priority === null ? 'No priority' : `P${priority}`
  }

  function openProperty(property: PropertyKey): void {
    if (actionsDisabled) return
    dateError = ''
    activeProperty = property
    if (property === 'scheduled') {
      scheduledDraft = scheduledDate()
    } else if (property === 'due') {
      dueDraft = deadlineDate()
    } else {
      priorityDraft =
        issue.issue.priority === undefined || issue.issue.priority === null
          ? ''
          : String(issue.issue.priority)
    }
  }

  async function patchScheduled(value: string): Promise<void> {
    if (actionsDisabled) return
    const property = 'scheduled'
    dateError = ''
    if (value !== '' && value === scheduledDate()) {
      resetDraft(property)
      return
    }
    scheduledDraft = value
    const moved = movedDate(value, scheduledDate(), issue.issue.metadata.scheduled_on)
    if (moved === '') {
      dateError = "That day can't keep the saved time and timezone. Pick another day."
      return
    }
    const mutationUID = uid()
    const generation = draftResetGeneration
    const ok = await onPatchMetadata(mutationUID, {
      scheduled_on: moved,
    })
    if (ok) scheduleAcceptedReset(property, mutationUID, generation)
  }

  async function patchDue(value: string): Promise<void> {
    if (actionsDisabled) return
    const property = 'due'
    dateError = ''
    if (value !== '' && value === deadlineDate()) {
      resetDraft(property)
      return
    }
    dueDraft = value
    const moved = movedDate(value, deadlineDate(), issue.issue.metadata.deadline_on)
    if (moved === '') {
      dateError = "That day can't keep the saved time and timezone. Pick another day."
      return
    }
    const mutationUID = uid()
    const generation = draftResetGeneration
    const ok = await onPatchMetadata(mutationUID, {
      deadline_on: moved,
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

  function toggleLabelEditing(): void {
    if (actionsDisabled) return
    editingLabels = !editingLabels
    if (!editingLabels) {
      labelDraft = ''
      addingLabel = false
    }
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

<section class="property-pills" aria-label="Properties">
  {#if activeProperty === 'scheduled'}
    <div class="property-pill property-pill--editing" role="group" aria-label="Edit scheduled">
      <CalendarIcon size={13} strokeWidth={1.8} />
      <span>Scheduled</span>
      <DatePicker
        class="property-date-picker"
        ariaLabel="Scheduled"
        value={scheduledDraft}
        disabled={actionsDisabled}
        clearable
        onEscape={() => {
          activeProperty = null
        }}
        onchange={(value) => {
          void patchScheduled(value)
        }}
      />
    </div>
  {:else}
    <button
      type="button"
      class="property-pill"
      aria-label="Edit scheduled"
      disabled={actionsDisabled}
      onclick={() => openProperty('scheduled')}
    >
      <CalendarIcon size={13} strokeWidth={1.8} />
      <span>Scheduled</span>
      <strong>{scheduledLabel()}</strong>
    </button>
  {/if}

  {#if activeProperty === 'due'}
    <div class="property-pill property-pill--editing" role="group" aria-label="Edit due date">
      <ClockIcon size={13} strokeWidth={1.8} />
      <span>Due</span>
      <DatePicker
        class="property-date-picker"
        ariaLabel="Due"
        clearLabel="Clear due date"
        value={dueDraft}
        disabled={actionsDisabled}
        clearable
        onEscape={() => {
          activeProperty = null
        }}
        onchange={(value) => {
          void patchDue(value)
        }}
      />
    </div>
  {:else}
    <button
      type="button"
      class="property-pill"
      aria-label="Edit due date"
      disabled={actionsDisabled}
      onclick={() => openProperty('due')}
    >
      <ClockIcon size={13} strokeWidth={1.8} />
      <span>Due</span>
      <strong>{dueLabel()}</strong>
    </button>
  {/if}

  <div
    class="property-pill property-pill--typeahead"
    onfocusin={handleOwnerFocusIn}
    onfocusoutcapture={handleOwnerFocusOutCapture}
  >
    <UserIcon size={13} strokeWidth={1.8} />
    {#key ownerEditorGeneration}
      <Typeahead
        options={ownerOptions}
        value={issue.issue.owner ?? ''}
        fallbackLabel="Unassigned"
        placeholder="Owner"
        allowClear
        allowCustom
        clearLabel="Unassigned"
        triggerPrefix="Owner:"
        emptyLabel="Enter an owner"
        disabled={actionsDisabled && !ownerEditorOpen}
        onquery={trackOwnerQuery}
        onselect={updateOwner}
      />
    {/key}
  </div>

  <div class="property-pill property-pill--select" role="group" aria-label="Temporary assignment">
    <TimerIcon size={13} strokeWidth={1.8} />
    <span>Assignment</span>
    <SelectDropdown
      title="Assignment duration"
      value={assignmentTTLDraft}
      options={assignmentTTLOptions}
      disabled={actionsDisabled}
      onchange={(value) => {
        void claimAssignment(value)
      }}
    />
  </div>

  {#if activeProperty === 'priority'}
    <div
      class="property-pill property-pill--editing property-pill--select"
      role="group"
      aria-label="Edit priority"
    >
      <FlagIcon size={13} strokeWidth={1.8} />
      <span>Priority</span>
      <SelectDropdown
        title="Priority"
        value={priorityDraft}
        options={priorityOptions}
        disabled={actionsDisabled}
        onchange={(value) => {
          void updatePriority(value)
        }}
      />
    </div>
  {:else}
    <button
      type="button"
      class="property-pill"
      aria-label="Edit priority"
      disabled={actionsDisabled}
      onclick={() => openProperty('priority')}
    >
      <FlagIcon size={13} strokeWidth={1.8} />
      <span>Priority</span>
      <strong>{priorityLabel()}</strong>
    </button>
  {/if}
</section>
{#if dateError && (activeProperty === 'scheduled' || activeProperty === 'due')}
  <p class="date-error" role="alert">{dateError}</p>
{/if}

<dl class="detail-properties">
  <div>
    <dt>Project</dt>
    <dd>{issue.issue.project_name}</dd>
  </div>
  {#if issue.issue.assignment_expires_on}
    <div>
      <dt>Assignment expires</dt>
      <dd>{formatTimestamp(issue.issue.assignment_expires_on)}</dd>
    </div>
  {/if}
  {#if issue.labels.length > 0}
    <div>
      <dt>Labels</dt>
      <dd>
        <ul class="label-list" aria-label="Labels">
          {#each issue.labels as label (label.label)}
            <li class="label-token">
              {#if editingLabels}
                <Chip
                  size="xs"
                  tone="muted"
                  uppercase={false}
                  interactive
                  class="kata-label-chip"
                  ariaLabel={`Remove label ${label.label}`}
                  title={`Remove label ${label.label}`}
                  disabled={actionsDisabled}
                  onclick={() => {
                    if (!actionsDisabled) void onRemoveLabel(uid(), label.label)
                  }}
                >
                  {label.label}
                  <XIcon size={11} strokeWidth={2.2} aria-hidden="true" />
                </Chip>
              {:else}
                <Chip size="xs" tone="muted" uppercase={false} class="kata-label-chip">
                  {label.label}
                </Chip>
              {/if}
            </li>
          {/each}
        </ul>
      </dd>
    </div>
  {/if}
</dl>

<section class="label-editor" aria-label="Labels">
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
      onblur={() => {
        void submitLabel()
      }}
    />
  {:else}
    <div class="label-actions">
      <Button
        size="sm"
        surface="outline"
        label="Add label"
        disabled={actionsDisabled}
        onclick={() => {
          addingLabel = true
        }}
      />
      {#if issue.labels.length > 0}
        <Button
          size="sm"
          surface="outline"
          label={editingLabels ? 'Done' : 'Edit labels'}
          ariaLabel={editingLabels ? 'Done editing labels' : undefined}
          disabled={actionsDisabled}
          onclick={toggleLabelEditing}
        />
      {/if}
    </div>
  {/if}
</section>

<style>
  .date-error {
    color: var(--accent-red);
    font-size: 0.75rem;
    margin: 0.5rem 0 1rem;
  }
  .property-pills {
    display: flex;
    flex-wrap: wrap;
    gap: 6px;
    margin: 0 0 18px;
  }

  .property-pill {
    min-height: 28px;
    border: 1px solid transparent;
    border-radius: 6px;
    background: var(--bg-inset);
    color: var(--text-secondary);
    display: inline-flex;
    align-items: center;
    gap: 6px;
    padding: 4px 8px;
    font: inherit;
    font-size: var(--font-size-sm);
    line-height: 1;
  }

  button.property-pill {
    cursor: pointer;
  }

  button.property-pill:hover {
    border-color: var(--border-default);
    background: var(--bg-surface-hover);
    color: var(--text-primary);
  }

  .property-pill :global(svg) {
    color: var(--text-muted);
    flex: 0 0 auto;
  }

  .property-pill span {
    color: var(--text-muted);
  }

  .property-pill strong {
    color: var(--text-primary);
    font-weight: 600;
  }

  .property-pill--editing {
    border-color: var(--border-default);
    background: var(--bg-primary);
  }

  .property-pill--typeahead {
    padding: 0;
    gap: 4px;
    background: transparent;
  }

  .property-pill--typeahead > :global(svg) {
    margin-left: 8px;
  }

  .property-pill--typeahead :global(.kit-typeahead) {
    min-width: 136px;
  }

  .property-pill--typeahead :global(.kit-typeahead__trigger),
  .property-pill--typeahead :global(.kit-typeahead__input) {
    min-height: 28px;
    height: 28px;
    border-color: transparent;
    background: var(--bg-inset);
    color: var(--text-primary);
    font-size: var(--font-size-sm);
  }

  .property-pill--typeahead :global(.kit-typeahead__trigger:hover) {
    border-color: var(--border-default);
    background: var(--bg-surface-hover);
  }

  .property-pill--select :global(.kit-select-dropdown) {
    min-width: 104px;
  }

  .property-pill--select :global(.kit-select-dropdown__trigger) {
    height: 22px;
    border-color: transparent;
    background: transparent;
    color: var(--text-primary);
  }

  .property-pill--select :global(.kit-select-dropdown__trigger:hover:not(:disabled)),
  .property-pill--select :global(.kit-select-dropdown__trigger[aria-expanded='true']) {
    border-color: var(--border-default);
    background: var(--bg-inset);
  }

  .detail-properties {
    display: grid;
    gap: 8px;
    margin: 0 0 22px;
  }

  .detail-properties div {
    display: grid;
    grid-template-columns: 92px minmax(0, 1fr);
    gap: 12px;
  }

  .detail-properties dt {
    color: var(--text-muted);
    font-size: var(--font-size-xs);
    font-weight: var(--font-weight-medium);
  }

  .detail-properties dd {
    margin: 0;
    color: var(--text-secondary);
    font-size: var(--font-size-sm);
  }

  .label-list {
    display: flex;
    flex-wrap: wrap;
    gap: 6px;
    align-items: center;
    min-width: 0;
    margin: 0;
    padding: 0;
    list-style: none;
  }

  .label-token {
    display: inline-flex;
    align-items: center;
    max-width: 100%;
    min-width: 0;
  }

  .label-token :global(.kata-label-chip) {
    max-width: min(220px, 100%);
  }

  .label-editor {
    margin: 0 0 22px;
  }

  .label-actions {
    display: flex;
    flex-wrap: wrap;
    gap: 6px;
  }

  .label-input {
    width: min(220px, 100%);
    min-height: 30px;
    border: 1px solid var(--border-default);
    border-radius: 6px;
    background: var(--bg-primary);
    color: var(--text-primary);
    font: inherit;
    font-size: var(--font-size-sm);
    padding: 5px 8px;
  }
</style>
