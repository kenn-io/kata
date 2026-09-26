<script lang="ts">
  import { Button } from '@kenn-io/kit-ui'
  import PlusIcon from '@lucide/svelte/icons/plus'

  import Modal from './Modal.svelte'

  interface Props {
    open: boolean
    disabled?: boolean | undefined
    draftFenceGeneration?: number | undefined
    inboxName?: string | undefined
    onClose: () => void
    onSubmit: (title: string) => void | Promise<void>
    onChangeInbox?: (() => void) | undefined
  }

  let {
    open,
    disabled = false,
    draftFenceGeneration = 0,
    inboxName = undefined,
    onClose,
    onSubmit,
    onChangeInbox = undefined,
  }: Props = $props()

  let title = $state('')
  let pending = $state(false)
  let lastDraftFenceGeneration = $state<number | null>(null)

  $effect(() => {
    if (!open) {
      title = ''
      pending = false
    }
  })

  $effect(() => {
    const nextGeneration = draftFenceGeneration
    if (lastDraftFenceGeneration === null) {
      lastDraftFenceGeneration = nextGeneration
      return
    }
    if (nextGeneration === lastDraftFenceGeneration) return
    lastDraftFenceGeneration = nextGeneration
    title = ''
    pending = false
  })

  async function submit(): Promise<void> {
    const value = title.trim()
    if (!value || pending || disabled) return
    pending = true
    try {
      await onSubmit(value)
      title = ''
      onClose()
    } finally {
      pending = false
    }
  }

  function handleKeydown(event: KeyboardEvent): void {
    if (event.key === 'Enter' && !event.shiftKey) {
      event.preventDefault()
      void submit()
    }
  }
</script>

<Modal {open} title="New task" {onClose} width={440}>
  <form
    class="capture"
    onsubmit={(event) => {
      event.preventDefault()
      void submit()
    }}
  >
    <input
      class="capture-input"
      type="text"
      aria-label="Quick capture"
      placeholder="Task title"
      bind:value={title}
      onkeydown={handleKeydown}
      disabled={pending || disabled}
    />
    {#if inboxName}
      <p class="capture-destination">
        Captured in <strong>{inboxName}</strong>
        {#if onChangeInbox}
          <button
            type="button"
            class="change-inbox"
            aria-label="Change Inbox project"
            disabled={pending}
            onclick={onChangeInbox}>Change</button
          >
        {/if}
      </p>
    {/if}
  </form>
  {#snippet footer()}
    <Button size="sm" label="Cancel" onclick={onClose} disabled={pending} />
    <Button
      size="sm"
      tone="info"
      surface="solid"
      label="Capture"
      onclick={() => void submit()}
      disabled={disabled || pending || title.trim().length === 0}
    >
      <PlusIcon size={12} strokeWidth={2} />
    </Button>
  {/snippet}
</Modal>

<style>
  .capture {
    display: flex;
    flex-direction: column;
    gap: 8px;
  }

  .capture-input {
    width: 100%;
    height: 36px;
    padding: 0 12px;
    border-radius: var(--radius-sm);
    border: 1px solid var(--border-default);
    background: var(--bg-primary);
    color: var(--text-primary);
    font: inherit;
    font-size: var(--font-size-md);
  }

  .capture-destination {
    display: flex;
    align-items: baseline;
    gap: 6px;
    margin: 0;
    color: var(--text-secondary);
    font-size: var(--font-size-sm);
  }

  .change-inbox {
    border: 0;
    padding: 0;
    background: none;
    color: var(--accent-blue);
    font: inherit;
    cursor: pointer;
  }

  .change-inbox:disabled {
    cursor: default;
    opacity: 0.55;
  }

  .capture-input:focus {
    outline: none;
    border-color: var(--accent-blue);
    box-shadow: 0 0 0 3px var(--accent-blue-soft);
  }
</style>
