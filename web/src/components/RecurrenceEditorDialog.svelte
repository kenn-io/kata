<script lang="ts">
  import type { TypeaheadOption } from '@kenn-io/kit-ui'
  import { Button } from '@kenn-io/kit-ui'

  import Modal from './Modal.svelte'
  import RecurrenceEditor from './RecurrenceEditor.svelte'
  import type {
    KataCreateRecurrenceInput,
    KataPatchRecurrenceInput,
    KataRecurrence,
    KataTaskDetail,
  } from '../lib/kata/types'

  type Mode =
    | { kind: 'create'; projectID: number; initialIssueRef: string; issue: KataTaskDetail }
    | { kind: 'edit'; recurrence: KataRecurrence; etag: string }

  interface Props {
    open: boolean
    mode: Mode
    actor: string
    ownerOptions?: TypeaheadOption[] | undefined
    disabled?: boolean | undefined
    onClose: () => void
    onCreate: (projectID: number, input: KataCreateRecurrenceInput) => Promise<void>
    onPatch: (
      recurrence: KataRecurrence,
      input: KataPatchRecurrenceInput,
      etag: string,
    ) => Promise<void>
  }

  let {
    open,
    mode,
    actor,
    ownerOptions = [],
    disabled = false,
    onClose,
    onCreate,
    onPatch,
  }: Props = $props()

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
  title={mode.kind === 'create' ? 'Repeat' : 'Edit repeat'}
  onClose={busy ? () => {} : onClose}
  width={560}
>
  <RecurrenceEditor
    bind:this={editorRef}
    {mode}
    {actor}
    {ownerOptions}
    {onCreate}
    {onPatch}
    onSaved={onClose}
  />
  {#snippet footer()}
    <Button size="sm" label="Cancel" disabled={busy} onclick={onClose} />
    <Button
      size="sm"
      tone="info"
      surface="solid"
      label={busy ? 'Saving...' : 'Save'}
      disabled={disabled || busy || !editorRef?.canSave()}
      onclick={() => void handleSave()}
    />
  {/snippet}
</Modal>
