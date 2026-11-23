<script lang="ts">
  import {
    autoReposition,
    Chip,
    dismissable,
    floatingPopoverStyle,
    type TypeaheadOption,
  } from '@kenn-io/kit-ui'
  import { tick } from 'svelte'
  import { listRecurrences } from '../lib/api/generated/default/default'
  import { formatDay } from '../lib/kata/dates'
  import type {
    KataCreateRecurrenceInput,
    KataPatchRecurrenceInput,
    KataRecurrence,
  } from '../lib/kata/types'
  import { formatRRule } from '../lib/recurrence/rrule'
  import RecurrenceDialogs from './RecurrenceDialogs.svelte'

  interface Props {
    projectID: number
    actor: string
    ownerOptions?: TypeaheadOption[]
    disabled?: boolean
    refreshGeneration?: number
    onCreate: (projectID: number, input: KataCreateRecurrenceInput) => Promise<void>
    onPatch: (
      recurrence: KataRecurrence,
      input: KataPatchRecurrenceInput,
      etag: string,
    ) => Promise<void>
    onDelete: (recurrence: KataRecurrence) => Promise<boolean>
  }
  let {
    projectID,
    actor,
    ownerOptions = [],
    disabled = false,
    refreshGeneration = 0,
    onCreate,
    onPatch,
    onDelete,
  }: Props = $props()
  let recurrences = $state<KataRecurrence[]>([])
  let open = $state(false)
  let root: HTMLDivElement | undefined = $state()
  let panel: HTMLDivElement | undefined = $state()
  let panelStyle = $state('')
  let dialogs:
    | {
        openEditRecurrence: (r: KataRecurrence) => void
        openDeleteRecurrence: (r: KataRecurrence) => void
      }
    | undefined = $state()
  let generation = 0
  let lastProjectID: number | undefined

  async function refresh(id: number): Promise<KataRecurrence[]> {
    const current = ++generation
    try {
      const result = await listRecurrences({ projectId: id })
      const loaded = result.status === 200 ? (result.data.recurrences as KataRecurrence[]) : []
      if (current === generation) recurrences = loaded
      return loaded
    } catch {
      if (current === generation) recurrences = []
      return []
    }
  }
  $effect(() => {
    const id = projectID
    void refreshGeneration
    if (lastProjectID !== id) {
      lastProjectID = id
      recurrences = []
      open = false
    }
    void refresh(id)
    return () => {
      generation++
    }
  })
  $effect(() => {
    if (!open) return
    const trigger = root?.querySelector('button') ?? null
    return [
      dismissable({
        owners: () => [root, panel],
        dismiss: () => {
          open = false
        },
        escapeFocus: () => trigger,
      }),
      autoReposition(() => [panel], position),
    ].reduce((a, b) => () => {
      a()
      b()
    })
  })
  function position(): void {
    const trigger = root?.querySelector('button')
    if (!trigger || !panel) return
    panelStyle = floatingPopoverStyle({
      trigger: trigger.getBoundingClientRect(),
      viewportWidth: window.innerWidth,
      viewportHeight: window.innerHeight,
      popoverWidth: panel.offsetWidth,
      popoverHeight: panel.offsetHeight,
      align: 'start',
    })
  }
  async function toggle(): Promise<void> {
    open = !open
    if (!open) return
    await tick()
    position()
  }
  async function patch(
    recurrence: KataRecurrence,
    input: KataPatchRecurrenceInput,
    etag: string,
  ): Promise<void> {
    try {
      await onPatch(recurrence, input, etag)
      await refresh(projectID)
    } catch (error) {
      const fresh = await refresh(projectID)
      const replacement = fresh.find((item) => item.id === recurrence.id)
      if (replacement && error instanceof Object && 'status' in error && error.status === 412) {
        throw Object.assign(error, {
          response: { recurrence: replacement, etag: `"rev-${replacement.revision}"` },
        })
      }
      throw error
    }
  }
  async function remove(recurrence: KataRecurrence): Promise<boolean> {
    try {
      return await onDelete(recurrence)
    } finally {
      await refresh(projectID)
    }
  }
</script>

{#if recurrences.length}
  <div class="host" bind:this={root}>
    <Chip interactive expanded={open} onclick={() => void toggle()}
      >↻ {recurrences.length} repeating {recurrences.length === 1 ? 'task' : 'tasks'}</Chip
    >
    {#if open}
      <div class="list kit-popover-card" bind:this={panel} style={panelStyle}>
        {#each recurrences as recurrence (recurrence.uid)}
          <div class="row">
            <strong>{recurrence.template_title}</strong>
            <span
              >{formatRRule(recurrence.rrule)} · {recurrence.next_occurrence_key
                ? `Next ${formatDay(recurrence.next_occurrence_key)}`
                : 'Ended'}</span
            >
            {#if !disabled}<div>
                <button onclick={() => dialogs?.openEditRecurrence(recurrence)}>Edit</button>
                <button onclick={() => dialogs?.openDeleteRecurrence(recurrence)}
                  >Stop repeating…</button
                >
              </div>{/if}
          </div>
        {/each}
      </div>
    {/if}
  </div>
{/if}
<RecurrenceDialogs
  bind:this={dialogs}
  selectedIssue={null}
  {recurrences}
  {actor}
  {ownerOptions}
  {disabled}
  {onCreate}
  onPatch={patch}
  onDelete={remove}
/>

<style>
  .host {
    display: inline-flex;
  }
  .list {
    position: fixed;
    z-index: 35;
    display: grid;
    gap: 12px;
    width: min(340px, calc(100vw - 24px));
    padding: 16px;
  }
  .row {
    display: grid;
    gap: 4px;
  }
  .row span {
    color: var(--text-muted);
    font-size: var(--font-size-xs);
  }
  .row button {
    color: var(--accent-blue);
  }
</style>
