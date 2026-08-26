<script lang="ts">
  import { Button } from '@kenn-io/kit-ui'

  import Modal from './Modal.svelte'
  import type { KataRecurrence } from '../lib/kata/types'

  interface Props {
    open: boolean
    recurrence: KataRecurrence
    disabled?: boolean | undefined
    onConfirm: () => void
    onCancel: () => void
  }

  let { open, recurrence, disabled = false, onConfirm, onCancel }: Props = $props()

  function handleConfirm(): void {
    if (disabled) return
    onConfirm()
  }
</script>

<Modal {open} title="Delete recurrence" onClose={onCancel} width={420}>
  <p class="body">
    Stop creating new occurrences of
    <strong>{recurrence.template_title}</strong>? Existing open issues are not affected.
  </p>
  {#snippet footer()}
    <Button size="sm" label="Cancel" onclick={onCancel} />
    <Button
      size="sm"
      tone="danger"
      surface="solid"
      label="Delete"
      {disabled}
      onclick={handleConfirm}
    />
  {/snippet}
</Modal>

<style>
  .body {
    color: var(--text-primary);
    font-size: var(--font-size-md);
    line-height: 1.45;
  }
</style>
