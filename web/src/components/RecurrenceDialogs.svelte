<script lang="ts">
  import RecurrenceDeleteDialog from './RecurrenceDeleteDialog.svelte'
  import RecurrenceEditorDialog from './RecurrenceEditorDialog.svelte'
  import type {
    KataCreateRecurrenceInput,
    KataPatchRecurrenceInput,
    KataRecurrence,
    KataTaskDetail,
  } from '../lib/kata/types'

  interface Props {
    selectedIssue: KataTaskDetail | null
    recurrences: readonly KataRecurrence[]
    actor: string
    disabled?: boolean | undefined
    draftFenceGeneration?: number | undefined
    onCreate: (projectID: number, input: KataCreateRecurrenceInput) => Promise<void>
    // Edit and stop are offered only where a caller passes these.
    onPatch?:
      | ((
          recurrence: KataRecurrence,
          input: KataPatchRecurrenceInput,
          etag: string,
        ) => Promise<void>)
      | undefined
    onDelete?: ((recurrence: KataRecurrence) => Promise<void>) | undefined
  }

  let {
    selectedIssue,
    recurrences,
    actor,
    disabled = false,
    draftFenceGeneration = 0,
    onCreate,
    onPatch,
    onDelete,
  }: Props = $props()

  let recurrenceDialog = $state<
    | { open: false; mode: 'create'; recurrence: null; etag: '' }
    | { open: true; mode: 'create'; recurrence: null; etag: '' }
    | { open: true; mode: 'edit'; recurrence: KataRecurrence; etag: string }
  >({ open: false, mode: 'create', recurrence: null, etag: '' })
  let recurrenceDelete = $state<{ open: boolean; recurrence: KataRecurrence | null }>({
    open: false,
    recurrence: null,
  })
  let deletingRecurrence = $state(false)
  let lastDraftFenceGeneration = $state<number | null>(null)

  $effect(() => {
    reconcileRecurrences(recurrences)
  })

  $effect(() => {
    const nextGeneration = draftFenceGeneration
    if (lastDraftFenceGeneration === null) {
      lastDraftFenceGeneration = nextGeneration
      return
    }
    if (nextGeneration === lastDraftFenceGeneration) return
    lastDraftFenceGeneration = nextGeneration
    recurrenceDialog = { open: false, mode: 'create', recurrence: null, etag: '' }
    recurrenceDelete = { open: false, recurrence: null }
    deletingRecurrence = false
  })

  export function openCreateRecurrence(): void {
    if (disabled) return
    recurrenceDialog = { open: true, mode: 'create', recurrence: null, etag: '' }
  }

  export function openEditRecurrence(recurrence: KataRecurrence): void {
    if (disabled || !onPatch) return
    recurrenceDialog = {
      open: true,
      mode: 'edit',
      recurrence,
      etag: `"rev-${recurrence.revision}"`,
    }
  }

  export function openDeleteRecurrence(recurrence: KataRecurrence): void {
    if (disabled || !onDelete) return
    recurrenceDelete = { open: true, recurrence }
  }

  export function closeAll(): void {
    closeRecurrenceDialog()
    closeDeleteRecurrence()
  }

  // A delete conflict (412) reloads the recurrence list; without reconciling,
  // the open delete dialog would retry with the stale revision forever.
  export function reconcileRecurrences(recurrences: readonly KataRecurrence[]): void {
    const target = recurrenceDelete.recurrence
    if (!recurrenceDelete.open || !target) return
    const fresh = recurrences.find((item) => item.uid === target.uid)
    if (!fresh) {
      recurrenceDelete = { open: false, recurrence: null }
      return
    }
    if (fresh.revision !== target.revision) {
      recurrenceDelete = { open: true, recurrence: fresh }
    }
  }

  function closeRecurrenceDialog(): void {
    recurrenceDialog = { open: false, mode: 'create', recurrence: null, etag: '' }
  }

  function closeDeleteRecurrence(): void {
    if (deletingRecurrence) return
    recurrenceDelete = { open: false, recurrence: null }
  }

  async function confirmDeleteRecurrence(): Promise<void> {
    const recurrence = recurrenceDelete.recurrence
    if (disabled || !onDelete || !recurrence || deletingRecurrence) return
    deletingRecurrence = true
    try {
      await onDelete(recurrence)
      recurrenceDelete = { open: false, recurrence: null }
    } catch {
      // The refreshed list keeps the dialog ready for another attempt.
    } finally {
      deletingRecurrence = false
    }
  }
</script>

{#if recurrenceDialog.open && (selectedIssue || recurrenceDialog.mode === 'edit')}
  <RecurrenceEditorDialog
    open={recurrenceDialog.open}
    mode={recurrenceDialog.mode === 'create' && selectedIssue
      ? {
          kind: 'create',
          projectID: selectedIssue.issue.project_id,
          initialIssueRef: selectedIssue.issue.short_id,
        }
      : { kind: 'edit', recurrence: recurrenceDialog.recurrence!, etag: recurrenceDialog.etag }}
    {actor}
    {disabled}
    onClose={closeRecurrenceDialog}
    {onCreate}
    onPatch={onPatch ?? (async () => {})}
  />
{/if}

{#if recurrenceDelete.open && recurrenceDelete.recurrence}
  <RecurrenceDeleteDialog
    open={recurrenceDelete.open}
    recurrence={recurrenceDelete.recurrence}
    disabled={disabled || deletingRecurrence}
    onConfirm={() => {
      void confirmDeleteRecurrence()
    }}
    onCancel={closeDeleteRecurrence}
  />
{/if}
