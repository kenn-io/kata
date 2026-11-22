<script lang="ts">
  import { Checkbox, Disclosure, IconButton } from '@kenn-io/kit-ui'
  import DetailSection from './DetailSection.svelte'
  import PlusIcon from '@lucide/svelte/icons/plus'
  import PencilIcon from '@lucide/svelte/icons/pencil'
  import XIcon from '@lucide/svelte/icons/x'

  import type { KataTaskChecklistItem, KataTaskDetail } from '../lib/kata/types'
  import { createULID } from '../lib/ulid'

  interface Props {
    issue: KataTaskDetail
    disabled?: boolean
    draftResetGeneration?: number
    draftFenceGeneration?: number
    onPatchMetadata: (uid: string, patch: Record<string, unknown>) => boolean | Promise<boolean>
  }

  let {
    issue,
    disabled = false,
    draftResetGeneration = 0,
    draftFenceGeneration = 0,
    onPatchMetadata,
  }: Props = $props()

  let checklistPending = $state(false)
  let checklistDraft = $state('')
  let addingItem = $state(false)
  let renamingID = $state<string | null>(null)
  let renameDraft = $state('')
  let checklistInput: HTMLInputElement | null = $state(null)
  let trackedUID = $state<string | null>(null)
  let lastDraftResetGeneration = $state<number | null>(null)
  let lastDraftFenceGeneration = $state<number | null>(null)
  let pendingDraftResetUID = $state<string | null>(null)
  let pendingDraftResetGeneration = $state<number | null>(null)

  const doneItems = $derived(checklistItems().filter((item) => item.done))
  const openItems = $derived(checklistItems().filter((item) => !item.done))

  $effect(() => {
    const uid = issue.issue.uid
    if (trackedUID === null) {
      trackedUID = uid
      return
    }
    if (uid === trackedUID) return
    trackedUID = uid
    checklistDraft = ''
    renamingID = null
    renameDraft = ''
    checklistPending = false
    pendingDraftResetUID = null
    pendingDraftResetGeneration = null
  })

  $effect(() => {
    const nextGeneration = draftFenceGeneration
    if (lastDraftFenceGeneration === null) {
      lastDraftFenceGeneration = nextGeneration
      return
    }
    if (nextGeneration === lastDraftFenceGeneration) return
    lastDraftFenceGeneration = nextGeneration
    resetChecklistDraft()
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
    renamingID = null
    renameDraft = ''
    pendingDraftResetUID = null
    pendingDraftResetGeneration = null
    queueMicrotask(() => checklistInput?.focus())
  }

  async function replaceChecklist(uid: string, next: KataTaskChecklistItem[]): Promise<boolean> {
    const changed = await onPatchMetadata(uid, { checklist: next })
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

  async function renameChecklistItem(id: string): Promise<void> {
    const text = renameDraft.trim()
    if (!text) return
    const uid = issue.issue.uid
    const changed = await guarded(() =>
      replaceChecklist(
        uid,
        checklistItems().map((item) => (item.id === id ? { ...item, text } : item)),
      ),
    )
    if (changed && uid === issue.issue.uid && renamingID === id) renamingID = null
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

{#snippet checklistRow(item: KataTaskChecklistItem)}
  <div class="checklist-row" class:done={item.done}>
    {#if renamingID === item.id}
      <input
        aria-label="Rename checklist item"
        bind:value={renameDraft}
        disabled={disabled || checklistPending}
        onkeydown={(event) => {
          if (event.key === 'Enter') void renameChecklistItem(item.id)
          else if (event.key === 'Escape') renamingID = null
        }}
      />
    {:else}
      <Checkbox
        class="checklist-item"
        checked={item.done}
        disabled={disabled || checklistPending}
        label={item.text}
        onchange={(done) => void toggleChecklistItem(item.id, done)}
      />
      <IconButton
        size="sm"
        class="row-remove"
        ariaLabel={`Rename ${item.text}`}
        disabled={disabled || checklistPending}
        onclick={() => {
          renamingID = item.id
          renameDraft = item.text
        }}><PencilIcon size={13} aria-hidden="true" /></IconButton
      >
    {/if}
    <IconButton
      size="sm"
      tone="danger"
      class="row-remove"
      ariaLabel={`Remove ${item.text}`}
      disabled={disabled || checklistPending}
      onclick={() => void removeChecklistItem(item.id)}
      ><XIcon size={13} strokeWidth={1.9} /></IconButton
    >
  </div>
{/snippet}
<DetailSection
  title="Checklist"
  count={checklistItems().length > 0
    ? `${doneItems.length} of ${checklistItems().length} done`
    : undefined}
>
  {#if openItems.length}<div class="checklist-items">
      {#each openItems as item (item.id)}{@render checklistRow(item)}{/each}
    </div>{/if}
  {#if addingItem}<div class="checklist-add">
      <PlusIcon size={14} aria-hidden="true" />
      <input
        bind:this={checklistInput}
        aria-label="New checklist item"
        placeholder="Add item"
        bind:value={checklistDraft}
        disabled={disabled || checklistPending}
        onkeydown={handleChecklistKeydown}
      />
      <button
        type="button"
        class="add-checklist-button"
        disabled={disabled || checklistPending || checklistDraft.trim() === ''}
        onclick={() => void addChecklistItem()}>Add</button
      >
    </div>{:else}
    <button
      type="button"
      class="checklist-add add-item"
      disabled={disabled || checklistPending}
      onclick={() => {
        addingItem = true
        queueMicrotask(() => checklistInput?.focus())
      }}><PlusIcon size={14} aria-hidden="true" />Add item</button
    >
  {/if}
  {#if doneItems.length}
    <Disclosure variant="plain" title={`${doneItems.length} done`}>
      <div class="checklist-items">
        {#each doneItems as item (item.id)}{@render checklistRow(item)}{/each}
      </div>
    </Disclosure>
  {/if}
</DetailSection>

<style>
  .checklist-items {
    display: grid;
  }

  /* A record row: the box starts at the pane inset and the highlight runs
     to the pane's edges. */
  .checklist-row,
  .checklist-add {
    min-height: 32px;
    margin-inline: -16px;
    display: flex;
    align-items: center;
    gap: 8px;
    padding: 2px 16px;
  }

  .checklist-row:hover,
  .checklist-add:hover:not(:disabled),
  .checklist-add:focus-within {
    background: var(--bg-surface-hover);
  }

  :global(.checklist-item) {
    flex: 1;
    min-width: 0;
  }

  :global(.checklist-item .kit-checkbox__label) {
    color: var(--text-primary);
  }

  .checklist-row.done :global(.checklist-item .kit-checkbox__label) {
    color: var(--text-muted);
    text-decoration: line-through;
  }

  .checklist-row input {
    flex: 1;
    min-width: 0;
    height: 24px;
    font: inherit;
  }

  /* Row actions stay hidden until the row is hovered or they receive
     keyboard focus; a screen that can't hover shows them always. */
  .checklist-row :global(.row-remove) {
    color: var(--text-secondary);
    opacity: 0;
  }

  .checklist-row:hover :global(.row-remove),
  .checklist-row :global(.row-remove:focus-visible) {
    opacity: 1;
  }

  @media (hover: none) {
    .checklist-row :global(.row-remove) {
      opacity: 1;
    }
  }

  .checklist-add {
    width: calc(100% + 32px);
    color: var(--text-secondary);
  }

  .add-item {
    border: 0;
    background: transparent;
    font: inherit;
    font-size: var(--font-size-sm);
    text-align: left;
    cursor: pointer;
  }

  .checklist-add :global(svg) {
    flex: none;
    color: var(--text-muted);
  }

  .checklist-add input {
    flex: 1;
    min-width: 0;
    height: 24px;
    border: 0;
    background: transparent;
    color: var(--text-primary);
    font: inherit;
    font-size: var(--font-size-sm);
    padding: 0;
  }

  .checklist-add input:focus {
    outline: none;
  }

  .add-checklist-button {
    min-height: 24px;
    border: 1px solid var(--border-default);
    border-radius: var(--radius-md);
    background: var(--bg-primary);
    color: var(--text-secondary);
    font: inherit;
    font-size: var(--font-size-sm);
    padding: 0 8px;
    cursor: pointer;
  }

  .add-checklist-button:disabled {
    cursor: default;
    opacity: var(--opacity-disabled);
  }

  .add-checklist-button:not(:disabled):hover {
    background: var(--bg-surface-hover);
    color: var(--text-primary);
  }
</style>
