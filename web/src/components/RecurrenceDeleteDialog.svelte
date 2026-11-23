<script lang="ts">
  import { Button } from '@kenn-io/kit-ui'

  import Modal from './Modal.svelte'
  import type { KataRecurrence } from '../lib/kata/types'

  interface Props {
    open: boolean
    recurrence: KataRecurrence
    disabled?: boolean | undefined
    error?: string | null | undefined
    onConfirm: () => void
    onCancel: () => void
  }

  let { open, recurrence, disabled = false, error = null, onConfirm, onCancel }: Props = $props()

  function handleConfirm(): void {
    if (disabled) return
    onConfirm()
  }
</script>

<Modal {open} title="Stop repeating" onClose={onCancel} width={420}>
  <p class="body">
    Stop creating new occurrences of
    <strong>{recurrence.template_title}</strong>? Existing open issues are not affected.
  </p>
  {#if error}<p class="error" role="alert">{error}</p>{/if}
  {#snippet footer()}
    <Button size="sm" label="Cancel" onclick={onCancel} />
    <Button
      size="sm"
      tone="danger"
      surface="solid"
      label="Stop repeating"
      {disabled}
      onclick={handleConfirm}
    />
  {/snippet}
</Modal>

<style>
  .error {
    color: var(--accent-red);
    font-size: var(--font-size-sm);
  }
  .body {
    color: var(--text-primary);
    font-size: var(--font-size-md);
    line-height: 1.45;
  }
</style>
