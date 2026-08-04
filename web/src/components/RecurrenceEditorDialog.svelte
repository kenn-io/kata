<script lang="ts">
  import Modal from './Modal.svelte'
  import RecurrenceEditor from './RecurrenceEditor.svelte'
  import type {
    KataCreateRecurrenceInput,
    KataPatchRecurrenceInput,
    KataRecurrence,
  } from '../lib/kata/types'

  type Mode =
    | { kind: 'create'; projectID: number }
    | { kind: 'edit'; recurrence: KataRecurrence; etag: string }

  interface Props {
    open: boolean
    mode: Mode
    actor: string
    disabled?: boolean | undefined
    onClose: () => void
    onCreate: (projectID: number, input: KataCreateRecurrenceInput) => Promise<void>
    onPatch: (id: number, input: KataPatchRecurrenceInput, etag: string) => Promise<void>
  }

  let { open, mode, actor, disabled = false, onClose, onCreate, onPatch }: Props = $props()

  let busy = $state(false)
  let editorRef: { trySave: () => Promise<void>; canSave: () => boolean } | null = $state(null)

  async function handleSave() {
    if (disabled || !editorRef) return
    if (!editorRef.canSave()) return
    busy = true
    try {
      await editorRef.trySave()
    } finally {
      busy = false
    }
  }
</script>

<Modal
  {open}
  title={mode.kind === 'create' ? 'New recurrence' : 'Edit recurrence'}
  onClose={busy ? () => {} : onClose}
  width={560}
>
  <RecurrenceEditor bind:this={editorRef} {mode} {actor} {onCreate} {onPatch} onSaved={onClose} />
  {#snippet footer()}
    <button type="button" class="btn-secondary" disabled={busy} onclick={onClose}>Cancel</button>
    <button
      type="button"
      class="btn-primary"
      disabled={disabled || busy || !editorRef?.canSave()}
      onclick={handleSave}>{busy ? 'Saving...' : 'Save'}</button
    >
  {/snippet}
</Modal>

<style>
  .btn-secondary,
  .btn-primary {
    padding: 6px 12px;
    border-radius: var(--radius-sm);
    border: 1px solid var(--border-default);
    background: var(--bg-surface);
    color: var(--text-primary);
  }

  .btn-primary {
    background: var(--accent-primary);
    border-color: transparent;
    color: white;
  }

  .btn-primary:disabled,
  .btn-secondary:disabled {
    opacity: 0.55;
    pointer-events: none;
  }
</style>
