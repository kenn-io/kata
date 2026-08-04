<script lang="ts">
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
    <button type="button" class="ghost-button" onclick={close} disabled={pending}>Cancel</button>
    <button
      type="button"
      class="danger-button"
      onclick={() => {
        void deleteIssue()
      }}
      disabled={pending}
    >
      {pending ? 'Deleting...' : 'Delete'}
    </button>
  {/snippet}
</Modal>

<style>
  .ghost-button,
  .danger-button {
    display: inline-flex;
    align-items: center;
    justify-content: center;
    gap: 6px;
    min-height: 28px;
    padding: 5px 11px;
    border-radius: 6px;
    font-size: var(--font-size-sm);
    font-weight: 650;
  }

  .ghost-button {
    border: 1px solid var(--border-default);
    background: var(--bg-surface);
    color: var(--text-secondary);
  }

  .danger-button {
    border: 1px solid var(--accent-red);
    background: var(--accent-red);
    color: white;
  }

  .ghost-button:disabled,
  .danger-button:disabled {
    cursor: default;
    opacity: 0.62;
  }

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
