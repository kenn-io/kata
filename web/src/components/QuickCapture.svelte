<script lang="ts">
  import { Button } from '@kenn-io/kit-ui'
  import PlusIcon from '@lucide/svelte/icons/plus'

  import Modal from './Modal.svelte'

  interface Props {
    open: boolean
    disabled?: boolean | undefined
    draftFenceGeneration?: number | undefined
    onClose: () => void
    onSubmit: (title: string) => void | Promise<void>
  }

  let { open, disabled = false, draftFenceGeneration = 0, onClose, onSubmit }: Props = $props()

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

  .capture-input:focus {
    outline: none;
    border-color: var(--accent-blue);
    box-shadow: 0 0 0 3px var(--accent-blue-soft);
  }
</style>
