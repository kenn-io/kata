<script lang="ts">
  import { Button, SelectDropdown } from '@kenn-io/kit-ui'
  import PlusIcon from '@lucide/svelte/icons/plus'

  import Modal from './Modal.svelte'

  interface Props {
    open: boolean
    disabled?: boolean | undefined
    draftFenceGeneration?: number | undefined
    projects?: { uid: string; name: string }[] | undefined
    inboxUID?: string | undefined
    onClose: () => void
    onSubmit: (title: string, projectUID?: string) => void | Promise<void>
  }

  let {
    open,
    disabled = false,
    draftFenceGeneration = 0,
    projects = undefined,
    inboxUID = undefined,
    onClose,
    onSubmit,
  }: Props = $props()

  let input = $state<HTMLInputElement>()
  let title = $state('')
  let pending = $state(false)
  let error = $state<string | null>(null)
  let lastDraftFenceGeneration = $state<number | null>(null)
  let selectedProjectUID = $state<string | undefined>(undefined)
  const effectiveProjectUID = $derived(
    projects?.some((project) => project.uid === selectedProjectUID)
      ? selectedProjectUID
      : projects?.some((project) => project.uid === inboxUID)
        ? inboxUID
        : '',
  )
  const projectReady = $derived(!projects || !!effectiveProjectUID)

  $effect(() => {
    if (open && !disabled) input?.focus()
  })

  $effect(() => {
    if (!open) {
      title = ''
      pending = false
      error = null
      selectedProjectUID = undefined
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
    if (!value || pending || disabled || !projectReady) return
    pending = true
    error = null
    try {
      if (projects) await onSubmit(value, effectiveProjectUID)
      else await onSubmit(value)
      title = ''
      onClose()
    } catch (err) {
      error = err instanceof Error ? err.message : 'Could not create task.'
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

<Modal {open} title="New task" {onClose} width={560}>
  <form
    class="capture"
    onsubmit={(event) => {
      event.preventDefault()
      void submit()
    }}
  >
    <label
      >Title
      <input
        bind:this={input}
        class="capture-input"
        type="text"
        aria-label="Quick capture"
        placeholder="Task title"
        bind:value={title}
        onkeydown={handleKeydown}
        disabled={pending || disabled}
      />
    </label>
    {#if error}<p class="capture-error" role="alert">{error}</p>{/if}
    {#if projects}
      <div class="project-row">
        <span>Project</span>
        <SelectDropdown
          title="Project"
          value={effectiveProjectUID ?? ''}
          options={[
            { value: '', label: 'Choose a project', disabled: true },
            ...projects.map((project) => ({ value: project.uid, label: project.name })),
          ]}
          onchange={(uid) => (selectedProjectUID = uid)}
          disabled={pending || disabled}
        />
      </div>
    {/if}
  </form>
  {#snippet footer()}
    <Button size="sm" label="Cancel" onclick={onClose} disabled={pending} />
    <Button
      size="sm"
      tone="info"
      surface="solid"
      label="Add"
      onclick={() => void submit()}
      disabled={disabled || pending || title.trim().length === 0 || !projectReady}
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

  .capture-error {
    margin: 0;
    padding: 8px 10px;
    border-radius: var(--radius-sm);
    background: color-mix(in srgb, var(--accent-red) 12%, transparent);
    color: var(--accent-red);
    font-size: var(--font-size-sm);
  }

  .capture label,
  .project-row {
    display: grid;
    grid-template-columns: 96px 1fr;
    align-items: baseline;
  }

  @media (max-width: 640px) {
    .capture label,
    .project-row {
      grid-template-columns: 1fr;
      gap: 6px;
    }
  }

  .capture-input:focus {
    outline: none;
    border-color: var(--accent-blue);
    box-shadow: 0 0 0 3px color-mix(in srgb, var(--accent-blue) 16%, transparent);
  }
</style>
