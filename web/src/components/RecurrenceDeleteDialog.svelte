<script lang="ts">
  import { Button, Notice } from '@kenn-io/kit-ui'

  import Modal from './Modal.svelte'
  import { formatDay } from '../lib/kata/dates'
  import type { KataRecurrence } from '../lib/kata/types'
  import { formatRRule } from '../lib/recurrence/rrule'

  interface Props {
    open: boolean
    recurrence: KataRecurrence
    disabled?: boolean | undefined
    error?: string | undefined
    // Set when the rule moved to a newer revision while this dialog was open.
    // Stop stays off until the user acknowledges the rule shown here.
    onReviewChange?: (() => void) | undefined
    onConfirm: () => void
    onCancel: () => void
  }

  let {
    open,
    recurrence,
    disabled = false,
    error = '',
    onReviewChange,
    onConfirm,
    onCancel,
  }: Props = $props()

  let template = $derived(
    [
      recurrence.template_owner,
      recurrence.template_priority === undefined ? '' : `P${recurrence.template_priority}`,
      ...recurrence.template_labels,
    ]
      .filter(Boolean)
      .join(' · '),
  )

  function handleConfirm(): void {
    if (disabled) return
    onConfirm()
  }
</script>

<Modal {open} title="Stop repeating" onClose={onCancel} width={420}>
  <div class="rule" class:changed={!!onReviewChange}>
    <strong>{recurrence.template_title}</strong>
    <span
      >{formatRRule(recurrence.rrule)} · {recurrence.next_occurrence_key
        ? `Next ${formatDay(recurrence.next_occurrence_key)}`
        : 'Ended'}</span
    >
    {#if template}<span>{template}</span>{/if}
    {#if recurrence.last_materialized_uid}<span
        >Latest task <code>{recurrence.last_materialized_uid}</code></span
      >{/if}
  </div>
  <p class="body">Stop creating new occurrences? Existing open issues are not affected.</p>
  {#if error}<Notice tone="error">{error}</Notice>{/if}
  {#if onReviewChange}<div class="review">
      <Button size="sm" label="Review updated rule" onclick={onReviewChange} />
    </div>{/if}
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
  .rule {
    display: grid;
    gap: 2px;
    margin-bottom: 12px;
  }
  .rule.changed {
    border-left: 2px solid var(--accent-blue);
    padding-left: 8px;
  }
  .review {
    margin-top: 8px;
  }
  .rule span {
    color: var(--text-muted);
    font-size: var(--font-size-sm);
    overflow-wrap: anywhere;
  }
  .body {
    color: var(--text-primary);
    font-size: var(--font-size-md);
    line-height: 1.45;
  }
</style>
