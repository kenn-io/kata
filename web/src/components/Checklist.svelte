<script lang="ts">
  import { Checkbox, IconButton } from '@kenn-io/kit-ui'
  import PlusIcon from '@lucide/svelte/icons/plus'
  import XIcon from '@lucide/svelte/icons/x'

  import type { KataTaskChecklistItem, KataTaskDetail } from '../lib/kata/types'
  import { createULID } from '../lib/ulid'

  interface Props {
    issue: KataTaskDetail
    revealed: boolean
    disabled?: boolean
    draftResetGeneration?: number
    onPatchMetadata: (uid: string, patch: Record<string, unknown>) => boolean | Promise<boolean>
    onReveal: () => void
  }

  let {
    issue,
    revealed,
    disabled = false,
    draftResetGeneration = 0,
    onPatchMetadata,
    onReveal,
  }: Props = $props()

  let checklistPending = $state(false)
  let checklistDraft = $state('')
  let checklistInput: HTMLInputElement | null = $state(null)
  let trackedUID = $state<string | null>(null)
  let lastDraftResetGeneration = $state<number | null>(null)
  let pendingDraftResetUID = $state<string | null>(null)
  let pendingDraftResetGeneration = $state<number | null>(null)

  const visible = $derived(checklistItems().length > 0 || revealed)

  $effect(() => {
    const uid = issue.issue.uid
    if (trackedUID === null) {
      trackedUID = uid
      return
    }
    if (uid === trackedUID) return
    trackedUID = uid
    checklistDraft = ''
    checklistPending = false
    pendingDraftResetUID = null
    pendingDraftResetGeneration = null
  })

  $effect(() => {
    const nextGeneration = draftResetGeneration
    if (lastDraftResetGeneration === null) {
      lastDraftResetGeneration = nextGeneration
      return
    }
    if (nextGeneration === lastDraftResetGeneration) return
    lastDraftResetGeneration = nextGeneration
    if (
      pendingDraftResetUID === issue.issue.uid &&
      pendingDraftResetGeneration !== nextGeneration
    ) {
      resetChecklistDraft()
    }
  })

  function checklistItems(): KataTaskChecklistItem[] {
    return issue.issue.metadata.checklist ?? []
  }

  function resetChecklistDraft(): void {
    checklistDraft = ''
    pendingDraftResetUID = null
    pendingDraftResetGeneration = null
    queueMicrotask(() => checklistInput?.focus())
  }

  async function replaceChecklist(uid: string, next: KataTaskChecklistItem[]): Promise<boolean> {
    const changed = await onPatchMetadata(uid, { checklist: next })
    if (changed && next.length === 0) {
      onReveal()
    }
    return changed
  }

  async function guarded(work: () => Promise<boolean>): Promise<boolean> {
    if (disabled || checklistPending) return false
    checklistPending = true
    try {
      return await work()
    } finally {
      checklistPending = false
    }
  }

  async function toggleChecklistItem(id: string, done: boolean): Promise<void> {
    const uid = issue.issue.uid
    await guarded(() =>
      replaceChecklist(
        uid,
        checklistItems().map((item) => (item.id === id ? { ...item, done } : item)),
      ),
    )
  }

  async function removeChecklistItem(id: string): Promise<void> {
    const uid = issue.issue.uid
    await guarded(() =>
      replaceChecklist(
        uid,
        checklistItems().filter((item) => item.id !== id),
      ),
    )
  }

  async function addChecklistItem(): Promise<void> {
    const text = checklistDraft.trim()
    if (!text) return
    const mutationUID = issue.issue.uid
    const resetGeneration = draftResetGeneration
    const changed = await guarded(() =>
      replaceChecklist(mutationUID, [...checklistItems(), { id: createULID(), text, done: false }]),
    )
    if (!changed || issue.issue.uid !== mutationUID) return
    if (draftResetGeneration !== resetGeneration) {
      resetChecklistDraft()
    } else {
      pendingDraftResetUID = mutationUID
      pendingDraftResetGeneration = resetGeneration
    }
  }

  function handleChecklistKeydown(event: KeyboardEvent): void {
    if (event.key === 'Enter') {
      event.preventDefault()
      void addChecklistItem()
    }
  }
</script>

{#if visible}
  <section class="checklist" aria-label="Checklist">
    <div class="section-header">
      <h3>Checklist</h3>
    </div>
    {#if checklistItems().length > 0}
      <div class="checklist-items">
        {#each checklistItems() as item (item.id)}
          <div class="checklist-row" class:done={item.done}>
            <Checkbox
              class="checklist-item"
              checked={item.done}
              disabled={disabled || checklistPending}
              label={item.text}
              onchange={(done) => {
                void toggleChecklistItem(item.id, done)
              }}
            />
            <IconButton
              size="sm"
              tone="danger"
              class="row-remove"
              ariaLabel={`Remove ${item.text}`}
              disabled={disabled || checklistPending}
              onclick={() => {
                void removeChecklistItem(item.id)
              }}
            >
              <XIcon size={13} strokeWidth={1.9} />
            </IconButton>
          </div>
        {/each}
      </div>
    {/if}
    <div class="checklist-add">
      <PlusIcon size={13} strokeWidth={1.9} aria-hidden="true" />
      <input
        bind:this={checklistInput}
        aria-label="New checklist item"
        placeholder="Add subtask..."
        bind:value={checklistDraft}
        disabled={disabled || checklistPending}
        onkeydown={handleChecklistKeydown}
      />
      <button
        type="button"
        class="add-checklist-button"
        disabled={disabled || checklistPending || checklistDraft.trim() === ''}
        onclick={() => {
          void addChecklistItem()
        }}
      >
        Add
      </button>
    </div>
  </section>
{/if}

<style>
  .checklist {
    display: grid;
    gap: 6px;
    margin: 0 0 18px;
  }

  .section-header {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 12px;
    margin-bottom: 8px;
  }

  .section-header h3 {
    margin: 0;
    color: var(--text-muted);
    font-size: var(--font-size-xs);
    font-weight: 650;
    text-transform: uppercase;
  }

  .checklist-items {
    display: grid;
    gap: 2px;
  }

  .checklist-row {
    min-height: 28px;
    border-radius: 6px;
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 8px;
    padding: 2px 4px;
  }

  .checklist-row:hover {
    background: var(--bg-hover);
  }

  :global(.checklist-item) {
    flex: 1;
    min-width: 0;
  }

  :global(.checklist-item .kit-checkbox__label) {
    color: var(--text-secondary);
  }

  .checklist-row.done :global(.checklist-item .kit-checkbox__label) {
    color: var(--text-muted);
    text-decoration: line-through;
  }

  /* The remove affordance stays hidden until its row is hovered or it
     receives keyboard focus. */
  .checklist-row :global(.row-remove) {
    opacity: 0;
  }

  .checklist-row:hover :global(.row-remove),
  .checklist-row :global(.row-remove:focus-visible) {
    opacity: 1;
  }

  .checklist-add {
    min-height: 30px;
    border-radius: 6px;
    display: flex;
    align-items: center;
    gap: var(--space-3);
    padding: 2px 4px;
    color: var(--text-muted);
  }

  .checklist-add:focus-within {
    background: var(--bg-hover);
  }

  .checklist-add input {
    flex: 1;
    min-width: 0;
    border: 0;
    background: transparent;
    color: var(--text-primary);
    font: inherit;
    font-size: var(--font-size-sm);
    padding: 4px 2px;
  }

  .checklist-add input:focus {
    outline: none;
  }

  .add-checklist-button {
    min-height: 24px;
    border: 1px solid var(--border-default);
    border-radius: 6px;
    background: var(--bg-primary);
    color: var(--text-secondary);
    font: inherit;
    font-size: var(--font-size-xs);
    padding: 2px 8px;
    cursor: pointer;
  }

  .add-checklist-button:disabled {
    cursor: default;
    opacity: 0.62;
  }

  .add-checklist-button:not(:disabled):hover {
    background: var(--bg-surface-hover);
    color: var(--text-primary);
  }
</style>
