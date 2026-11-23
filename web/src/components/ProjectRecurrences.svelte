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
    mutationMessage?: string | undefined
    draftFenceGeneration?: number
    snapshotRecurrences?: readonly KataRecurrence[] | undefined
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
    mutationMessage = undefined,
    draftFenceGeneration = 0,
    snapshotRecurrences = undefined,
    refreshGeneration = 0,
    onCreate,
    onPatch,
    onDelete,
  }: Props = $props()
  let recurrences = $state<KataRecurrence[]>([])
  let open = $state(false)
  let readError = $state(false)
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
  let reloadNeeded = false

  async function refresh(id: number): Promise<KataRecurrence[] | null> {
    const current = ++generation
    try {
      const result = await listRecurrences({ projectId: id })
      if (result.status !== 200 || !Array.isArray(result.data.recurrences))
        throw new Error('Could not load repeating tasks')
      const loaded = result.data.recurrences as KataRecurrence[]
      if (current === generation) {
        recurrences = loaded
        readError = false
      }
      return loaded
    } catch {
      if (current === generation) readError = true
      return null
    }
  }
  $effect(() => {
    const id = projectID
    void refreshGeneration
    if (snapshotRecurrences !== undefined) {
      recurrences = [...snapshotRecurrences]
      readError = false
    } else void refresh(id)
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
    } catch (error) {
      const fresh = await refresh(projectID)
      if (error instanceof Object && 'status' in error && error.status === 412) {
        const replacement = fresh?.find((item) => item.id === recurrence.id)
        throw Object.assign(error, {
          response: replacement
            ? { recurrence: replacement, etag: `"rev-${replacement.revision}"` }
            : undefined,
        })
      }
      throw error
    }
  }
  async function stop(recurrence: KataRecurrence): Promise<boolean> {
    if (reloadNeeded) {
      const fresh = await refresh(projectID)
      if (!fresh) return false
      reloadNeeded = false
      const replacement = fresh.find((item) => item.uid === recurrence.uid)
      if (!replacement) return true
      recurrence = replacement
    }
    let stopped = false
    try {
      stopped = await onDelete(recurrence)
    } catch {
      /* reload after any refusal */
    }
    if (!stopped) {
      reloadNeeded = true
      if (await refresh(projectID)) reloadNeeded = false
    }
    return stopped
  }
</script>

{#if readError}
  <span role="alert"
    >Could not load repeating tasks. <button onclick={() => void refresh(projectID)}>Retry</button
    ></span
  >
{/if}
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
  {mutationMessage}
  {draftFenceGeneration}
  {onCreate}
  onPatch={patch}
  onDelete={stop}
  reloadFailed={readError}
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
    max-height: calc(100vh - 24px);
    overflow-y: auto;
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
