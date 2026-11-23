<script lang="ts">
  import { Button } from '@kenn-io/kit-ui'

  import Modal from './Modal.svelte'
  import { Card } from '@kenn-io/kit-ui'
  import type { KataRecurrence } from '../lib/kata/types'

  interface Props {
    open: boolean
    recurrence: KataRecurrence
    disabled?: boolean | undefined
    error?: string | undefined
    retry?: boolean | undefined
    onConfirm: () => void
    onCancel: () => void
  }

  let {
    open,
    recurrence,
    disabled = false,
    error = undefined,
    retry = false,
    onConfirm,
    onCancel,
  }: Props = $props()

  function handleConfirm(): void {
    if (disabled) return
    onConfirm()
  }
</script>

<Modal {open} title="Stop repeating" onClose={onCancel} width={440}>
  <p class="body">
    <strong>{recurrence.template_title}</strong>
  </p>
  {#if error}<p class="error" role="alert">{error}</p>{/if}
  <Card padding="none" class="dialog-preview"
    >Tasks it already made stay as they are, open or done.</Card
  >
  {#snippet footer()}
    <Button size="sm" label="Cancel" onclick={onCancel} />
    <Button
      size="sm"
      tone="danger"
      surface="solid"
      label={retry ? 'Retry' : 'Stop repeating'}
      {disabled}
      onclick={handleConfirm}
    />
  {/snippet}
</Modal>

<style>
  .error {
    color: var(--accent-red);
  }
  .body {
    color: var(--text-primary);
    font-size: var(--font-size-md);
    line-height: 1.45;
  }
</style>
