<script lang="ts">
  import { Button } from '@kenn-io/kit-ui'

  import type { KataTaskDetail } from '../lib/kata/types'
  import Modal from './Modal.svelte'

  interface Props {
    open: boolean
    issue: KataTaskDetail
    onClose: () => void
    onDeleteIssue: () => boolean | Promise<boolean>
  }

  let { open, issue, onClose, onDeleteIssue }: Props = $props()
  let pending = $state(false)
  let trackedUID = $state<string | null>(null)

  $effect(() => {
    if (issue.issue.uid === trackedUID) return
    trackedUID = issue.issue.uid
    pending = false
  })

  function close(): void {
    if (!pending) onClose()
  }

  async function deleteIssue(): Promise<void> {
    if (pending) return
    pending = true
    try {
      if (await onDeleteIssue()) onClose()
    } finally {
      pending = false
    }
  }
</script>

<Modal {open} title="Delete issue" onClose={close} width={420}>
  <div class="delete-dialog">
    <p>
      Delete <strong>{issue.issue.title}</strong>?
    </p>
    <p class="delete-hint">
      The task moves to closed / won't-do state. Reopen it if you change your mind.
    </p>
  </div>

  {#snippet footer()}
    <Button size="sm" label="Cancel" onclick={close} disabled={pending} />
    <Button
      size="sm"
      tone="danger"
      surface="solid"
      label={pending ? 'Deleting...' : 'Delete'}
      onclick={() => {
        void deleteIssue()
      }}
      disabled={pending}
    />
  {/snippet}
</Modal>

<style>
  .delete-dialog {
    display: grid;
    gap: 8px;
  }

  .delete-dialog p {
    margin: 0;
    color: var(--text-primary);
    font-size: var(--font-size-md);
    line-height: 1.45;
  }

  .delete-hint {
    color: var(--text-muted) !important;
    font-size: var(--font-size-sm) !important;
  }
</style>
