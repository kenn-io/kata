<script lang="ts">
  /* eslint-disable svelte/no-at-html-tags -- kit-ui sanitizes rendered markdown. */
  import type { TypeaheadOption } from '@kenn-io/kit-ui'
  import NetworkIcon from '@lucide/svelte/icons/network'
  import PencilIcon from '@lucide/svelte/icons/pencil'
  import { renderMarkdown, renderMarkdownSync } from '@kenn-io/kit-ui/utils/markdown'
  import {
    formatRelativeTime as timeAgo,
    formatTimestamp as localDateTimeLabel,
  } from '@kenn-io/kit-ui/utils/time'

  import type { components } from '../lib/api/schema'
  import { createKataLinkFilters, type KataLinkFilters } from '../lib/kata/linkFilters'
  import type {
    KataCreateRecurrenceInput,
    KataPatchRecurrenceInput,
    KataProjectSummary,
    KataRecurrence,
    KataTaskDetail,
    KataTaskCloseRequest,
    KataTaskEditPatch,
    KataTaskEvent,
    KataTaskSummary,
  } from '../lib/kata/types'
  import Checklist from './Checklist.svelte'
  import Comments from './Comments.svelte'
  import IssueHistory from './IssueHistory.svelte'
  import IssueLinks from './IssueLinks.svelte'
  import IssueStateDialog from './IssueStateDialog.svelte'
  import MoveIssueDialog from './MoveIssueDialog.svelte'
  import IssueProperties from './IssueProperties.svelte'
  import RecurrenceDialogs from './RecurrenceDialogs.svelte'
  import RecurrencePanel from './RecurrencePanel.svelte'

  type Reference = components['schemas']['UIIssueReference']
  interface IssueNavigationTarget {
    uid: string
  }

  interface WorkspaceAction {
    label: string
    busy?: boolean
    disabled?: boolean
    onClick?: () => void | Promise<void>
  }

  interface Props {
    issue: KataTaskDetail
    events?: readonly KataTaskEvent[] | undefined
    issueCatalog?: readonly KataTaskSummary[] | undefined
    searchReferences?: ((query: string) => Promise<Reference[]>) | undefined
    linkFilters?: KataLinkFilters | undefined
    onLinkFiltersChange?: ((next: KataLinkFilters) => void) | undefined
    projects: KataProjectSummary[]
    ownerOptions: TypeaheadOption[]
    selectedRecurrences?: KataRecurrence[] | undefined
    actionsDisabled?: boolean | undefined
    authorityBlocked?: boolean | undefined
    draftResetGeneration?: number | undefined
    movePending?: boolean | undefined
    onMoveIssue: (toProjectUID: string) => boolean | Promise<boolean>
    onPatchMetadata: (uid: string, patch: Record<string, unknown>) => boolean | Promise<boolean>
    onAddComment?: ((uid: string, body: string) => boolean | Promise<boolean>) | undefined
    onEditIssue: (uid: string, patch: KataTaskEditPatch) => boolean | Promise<boolean>
    onAssignOwner: (uid: string, owner: string) => boolean | Promise<boolean>
    onUnassignOwner: (uid: string) => boolean | Promise<boolean>
    onSetPriority: (uid: string, priority: number | null) => boolean | Promise<boolean>
    onAddLabel: (uid: string, label: string) => boolean | Promise<boolean>
    onRemoveLabel: (uid: string, label: string) => void | Promise<void>
    onCloseIssue: (request: KataTaskCloseRequest) => boolean | Promise<boolean>
    onReopenIssue: () => void | Promise<void>
    onDeleteIssue: () => boolean | Promise<boolean>
    onSelectIssue?: ((target: IssueNavigationTarget) => void | Promise<void>) | undefined
    onCreateRecurrence?:
      | ((projectID: number, input: KataCreateRecurrenceInput) => Promise<void>)
      | undefined
    onPatchRecurrence?:
      | ((id: number, input: KataPatchRecurrenceInput, etag: string) => Promise<void>)
      | undefined
    onDeleteRecurrence?: ((recurrence: KataRecurrence) => Promise<boolean>) | undefined
    onOpenGraph?: ((issue: KataTaskDetail['issue']) => void) | undefined
    workspaceAction?: WorkspaceAction | undefined
  }

  let {
    issue,
    events = [],
    issueCatalog = [],
    searchReferences = async () => [],
    linkFilters = createKataLinkFilters('all'),
    onLinkFiltersChange = () => {},
    projects,
    ownerOptions,
    selectedRecurrences = [],
    actionsDisabled = false,
    authorityBlocked = undefined,
    draftResetGeneration = 0,
    movePending = false,
    onMoveIssue,
    onPatchMetadata,
    onAddComment = async () => false,
    onEditIssue,
    onAssignOwner,
    onUnassignOwner,
    onSetPriority,
    onAddLabel,
    onRemoveLabel,
    onCloseIssue,
    onReopenIssue,
    onDeleteIssue,
    onSelectIssue = () => {},
    onCreateRecurrence = async () => {},
    onPatchRecurrence = async () => {},
    onDeleteRecurrence = async () => false,
    onOpenGraph = undefined,
    workspaceAction = undefined,
  }: Props = $props()

  let editingTitle = $state(false)
  let editingBody = $state(false)
  let savingTitle = $state(false)
  let savingBody = $state(false)
  let titleDraft = $state('')
  let bodyDraft = $state('')
  let titleInput: HTMLInputElement | null = $state(null)
  let bodyTextarea: HTMLTextAreaElement | null = $state(null)
  let cancelingTitle = $state(false)
  let lastIssueUID = $state<string | null>(null)
  let lastDraftResetGeneration = $state<number | null>(null)
  let pendingTitleResetUID = $state<string | null>(null)
  let pendingTitleResetGeneration = $state<number | null>(null)
  let pendingBodyResetUID = $state<string | null>(null)
  let pendingBodyResetGeneration = $state<number | null>(null)
  let checklistRevealed = $state(false)
  let recurrenceDialogs: {
    openCreateRecurrence: () => void
    openEditRecurrence: (recurrence: KataRecurrence) => void
    openDeleteRecurrence: (recurrence: KataRecurrence) => void
  } | null = $state(null)

  const detailInert = $derived(authorityBlocked ?? actionsDisabled)
  const canCreateRecurrence = $derived(issue.issue.recurrence_id === undefined)
  const visibleRecurrences = $derived.by(() => {
    const attachedID = issue.issue.recurrence_id
    if (attachedID !== undefined) {
      const attached = selectedRecurrences.find((recurrence) => recurrence.id === attachedID)
      return attached ? [attached] : []
    }
    return selectedRecurrences
  })

  $effect(() => {
    const uid = issue.issue.uid
    if (uid === lastIssueUID) return
    lastIssueUID = uid
    editingTitle = false
    editingBody = false
    savingTitle = false
    savingBody = false
    cancelingTitle = false
    pendingTitleResetUID = null
    pendingTitleResetGeneration = null
    pendingBodyResetUID = null
    pendingBodyResetGeneration = null
    checklistRevealed = false
  })

  $effect(() => {
    const nextGeneration = draftResetGeneration
    if (lastDraftResetGeneration === null) {
      lastDraftResetGeneration = nextGeneration
      return
    }
    if (nextGeneration === lastDraftResetGeneration) return
    lastDraftResetGeneration = nextGeneration
    const uid = issue.issue.uid
    if (pendingTitleResetUID === uid && pendingTitleResetGeneration !== nextGeneration) {
      resetTitleDraft()
    }
    if (pendingBodyResetUID === uid && pendingBodyResetGeneration !== nextGeneration) {
      resetBodyDraft()
    }
  })

  function resetTitleDraft(): void {
    editingTitle = false
    cancelingTitle = false
    titleDraft = ''
    pendingTitleResetUID = null
    pendingTitleResetGeneration = null
  }

  function resetBodyDraft(): void {
    editingBody = false
    bodyDraft = ''
    pendingBodyResetUID = null
    pendingBodyResetGeneration = null
  }

  function currentProjectName(): string {
    const fromIssue = issue.issue.project_name.trim()
    if (fromIssue) return fromIssue
    const project =
      projects.find((candidate) => candidate.uid === issue.issue.project_uid) ??
      projects.find((candidate) => candidate.id === issue.issue.project_id)
    return project?.name ?? issue.issue.project_uid
  }

  function startEditingTitle(): void {
    if (actionsDisabled) return
    cancelingTitle = false
    titleDraft = issue.issue.title
    editingTitle = true
    queueMicrotask(() => {
      titleInput?.focus()
      titleInput?.select()
    })
  }

  async function commitTitle(): Promise<void> {
    if (actionsDisabled || savingTitle) return
    if (cancelingTitle) {
      cancelingTitle = false
      editingTitle = false
      return
    }
    const next = titleDraft.trim()
    if (!next || next === issue.issue.title) {
      editingTitle = false
      return
    }
    const mutationUID = issue.issue.uid
    const resetGeneration = draftResetGeneration
    savingTitle = true
    try {
      if (await onEditIssue(mutationUID, { title: next })) {
        if (issue.issue.uid !== mutationUID) return
        if (draftResetGeneration !== resetGeneration) {
          resetTitleDraft()
        } else {
          pendingTitleResetUID = mutationUID
          pendingTitleResetGeneration = resetGeneration
        }
      }
    } finally {
      savingTitle = false
    }
  }

  function handleTitleKeydown(event: KeyboardEvent): void {
    if (event.key === 'Enter') {
      event.preventDefault()
      void commitTitle()
    } else if (event.key === 'Escape') {
      event.preventDefault()
      cancelingTitle = true
      editingTitle = false
    }
  }

  function startEditingBody(): void {
    if (actionsDisabled) return
    bodyDraft = issue.issue.body
    editingBody = true
    queueMicrotask(() => bodyTextarea?.focus())
  }

  async function commitBody(): Promise<void> {
    if (actionsDisabled || savingBody) return
    const next = bodyDraft
    if (next === issue.issue.body) {
      editingBody = false
      return
    }
    const mutationUID = issue.issue.uid
    const resetGeneration = draftResetGeneration
    savingBody = true
    try {
      if (await onEditIssue(mutationUID, { body: next })) {
        if (issue.issue.uid !== mutationUID) return
        if (draftResetGeneration !== resetGeneration) {
          resetBodyDraft()
        } else {
          pendingBodyResetUID = mutationUID
          pendingBodyResetGeneration = resetGeneration
        }
      }
    } finally {
      savingBody = false
    }
  }

  function handleBodyKeydown(event: KeyboardEvent): void {
    if (event.key === 'Escape') {
      event.preventDefault()
      editingBody = false
    } else if ((event.metaKey || event.ctrlKey) && event.key === 'Enter') {
      event.preventDefault()
      void commitBody()
    }
  }
</script>

<section
  class="kata-detail"
  aria-label="Task detail"
  aria-busy={actionsDisabled || detailInert}
  inert={detailInert}
>
  <fieldset class="mutation-controls" disabled={actionsDisabled || detailInert}>
    <div class="detail-heading">
      <div class="detail-heading-main">
        <div class="detail-kicker">
          <span class="crumb-project">{currentProjectName()}</span>
          <span class="crumb-sep">/</span>
          <span class="crumb-id">{issue.issue.short_id}</span>
          <span class="kit-sr-only">{issue.issue.qualified_id}</span>
        </div>
        {#if editingTitle}
          <input
            class="title-edit"
            aria-label="Edit title"
            bind:this={titleInput}
            bind:value={titleDraft}
            disabled={savingTitle}
            onkeydown={handleTitleKeydown}
            onblur={() => {
              void commitTitle()
            }}
          />
        {:else}
          <h2 aria-label={issue.issue.title}>
            <button
              type="button"
              class="title-button"
              aria-label="Edit title"
              onclick={startEditingTitle}
            >
              <span>{issue.issue.title}</span>
              <PencilIcon size={13} strokeWidth={1.8} />
            </button>
          </h2>
        {/if}
      </div>
      <div class="detail-actions">
        <time
          class="detail-created-at"
          datetime={issue.issue.created_at}
          title={localDateTimeLabel(issue.issue.created_at)}
        >
          {timeAgo(issue.issue.created_at)}
        </time>
        {#if workspaceAction?.onClick}
          <button
            type="button"
            class="workspace-action"
            disabled={workspaceAction.disabled || workspaceAction.busy}
            onclick={() => {
              void workspaceAction.onClick?.()
            }}
          >
            {workspaceAction.busy ? 'Working...' : workspaceAction.label}
          </button>
        {/if}
        {#if onOpenGraph}
          <button
            type="button"
            class="icon-detail-action"
            aria-label="Open reachable graph"
            title="Open reachable graph"
            onclick={() => onOpenGraph?.(issue.issue)}
          >
            <NetworkIcon size={14} strokeWidth={1.9} aria-hidden="true" />
          </button>
        {/if}
        <MoveIssueDialog
          {issue}
          {projects}
          hasChecklist={(issue.issue.metadata.checklist ?? []).length > 0 || checklistRevealed}
          hasRecurrence={!canCreateRecurrence}
          {movePending}
          {onMoveIssue}
          onAddChecklist={() => {
            checklistRevealed = true
          }}
          onCreateRecurrence={() => recurrenceDialogs?.openCreateRecurrence()}
          {onDeleteIssue}
        />
        <IssueStateDialog {issue} {onCloseIssue} {onReopenIssue} />
      </div>
    </div>

    <section class="detail-description" aria-label="Description">
      <div class="section-header">
        <h3>Description</h3>
        {#if !editingBody}
          <button
            type="button"
            class="text-button"
            aria-label="Edit description"
            onclick={startEditingBody}
          >
            <PencilIcon size={13} strokeWidth={1.8} />
            <span>Edit</span>
          </button>
        {/if}
      </div>
      {#if editingBody}
        <textarea
          class="body-edit"
          aria-label="Edit description"
          rows="8"
          bind:this={bodyTextarea}
          bind:value={bodyDraft}
          disabled={savingBody}
          onkeydown={handleBodyKeydown}
        ></textarea>
        <div class="body-edit-actions">
          <span>Cmd/Ctrl+Enter saves</span>
          <div>
            <button
              type="button"
              class="ghost-button"
              disabled={savingBody}
              onclick={() => {
                editingBody = false
              }}>Cancel</button
            >
            <button
              type="button"
              class="accent-button"
              disabled={savingBody}
              onclick={() => {
                void commitBody()
              }}
            >
              {savingBody ? 'Saving...' : 'Save'}
            </button>
          </div>
        </div>
      {:else if issue.issue.body}
        <div class="body-display markdown-body">
          {#await renderMarkdown(issue.issue.body)}
            {@html renderMarkdownSync(issue.issue.body)}
          {:then html}
            {@html html}
          {/await}
        </div>
      {:else}
        <p class="detail-body-empty">No description.</p>
      {/if}
    </section>
  </fieldset>

  <IssueProperties
    {issue}
    {ownerOptions}
    {actionsDisabled}
    {draftResetGeneration}
    {onPatchMetadata}
    {onAssignOwner}
    {onUnassignOwner}
    {onSetPriority}
    {onAddLabel}
    {onRemoveLabel}
  />

  <fieldset class="mutation-controls" disabled={actionsDisabled || detailInert}>
    <Checklist
      {issue}
      revealed={checklistRevealed}
      disabled={actionsDisabled}
      {draftResetGeneration}
      {onPatchMetadata}
      onReveal={() => {
        checklistRevealed = true
      }}
    />

    {#if visibleRecurrences.length > 0}
      <section class="recurrence-section" aria-label="Recurrence">
        <RecurrencePanel
          recurrences={visibleRecurrences}
          onCreate={() => recurrenceDialogs?.openCreateRecurrence()}
          onEdit={(recurrence) => recurrenceDialogs?.openEditRecurrence(recurrence)}
          onDelete={(recurrence) => recurrenceDialogs?.openDeleteRecurrence(recurrence)}
        />
      </section>
    {/if}
  </fieldset>

  {#key issue.issue.uid}
    <IssueLinks
      {issue}
      {issueCatalog}
      {linkFilters}
      {onLinkFiltersChange}
      {actionsDisabled}
      {draftResetGeneration}
      {onEditIssue}
      {onSelectIssue}
    />
    <Comments {issue} {searchReferences} {actionsDisabled} {draftResetGeneration} {onAddComment} />
    <IssueHistory {events} />
  {/key}
</section>

<RecurrenceDialogs
  bind:this={recurrenceDialogs}
  selectedIssue={issue}
  recurrences={selectedRecurrences}
  actor=""
  disabled={actionsDisabled}
  onCreate={onCreateRecurrence}
  onPatch={onPatchRecurrence}
  onDelete={onDeleteRecurrence}
/>

<style>
  .kata-detail {
    flex: 1 1 auto;
    min-width: 0;
    min-height: 0;
    overflow: auto;
    background: var(--bg-primary);
    padding: 18px 22px;
  }

  .mutation-controls {
    display: contents;
  }

  .detail-heading {
    display: flex;
    align-items: flex-start;
    justify-content: space-between;
    gap: var(--space-5);
    margin-bottom: 14px;
  }

  .detail-heading-main {
    min-width: 0;
    flex: 1;
  }

  .detail-kicker {
    display: flex;
    align-items: center;
    gap: 6px;
    color: var(--text-muted);
    font-size: var(--font-size-xs);
    min-width: 0;
  }

  .crumb-project {
    min-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
    color: var(--text-primary);
    font-size: var(--font-size-xs);
    font-weight: 600;
  }

  .crumb-id {
    flex: none;
    color: var(--text-secondary);
    font-family: var(--font-mono);
    font-size: var(--font-size-xs);
    font-weight: 650;
    white-space: nowrap;
  }

  .crumb-sep {
    color: var(--text-faint);
  }

  .detail-heading h2 {
    margin: 4px 0 0;
    font-size: var(--font-size-xl);
    line-height: 1.25;
  }

  .detail-actions {
    flex: 0 0 auto;
    display: inline-flex;
    align-items: center;
    gap: 6px;
    padding-top: 2px;
  }

  .detail-created-at {
    color: var(--text-muted);
    font-size: var(--font-size-xs);
    white-space: nowrap;
    padding: 6px 2px;
  }

  .workspace-action {
    min-height: 30px;
    border: 1px solid var(--border-default);
    border-radius: 6px;
    background: color-mix(in srgb, var(--accent-blue) 10%, transparent);
    color: var(--accent-blue);
    padding: 0 10px;
    font-size: var(--font-size-xs);
    font-weight: 650;
    white-space: nowrap;
    cursor: pointer;
  }

  .workspace-action:hover:not(:disabled) {
    border-color: color-mix(in srgb, var(--accent-blue) 40%, transparent);
    background: color-mix(in srgb, var(--accent-blue) 16%, transparent);
  }

  .workspace-action:disabled {
    cursor: default;
    opacity: 0.65;
  }

  .icon-detail-action {
    width: 30px;
    height: 30px;
    border: 1px solid var(--border-default);
    border-radius: 6px;
    background: var(--bg-primary);
    color: var(--text-secondary);
    display: inline-flex;
    align-items: center;
    justify-content: center;
    padding: 0;
    cursor: pointer;
  }

  .icon-detail-action:hover {
    background: var(--bg-hover);
    color: var(--accent-blue);
  }

  .title-button {
    width: 100%;
    border: 0;
    background: transparent;
    color: var(--text-primary);
    display: inline-flex;
    align-items: flex-start;
    gap: 8px;
    padding: 0;
    font: inherit;
    font-weight: inherit;
    line-height: inherit;
    text-align: left;
    cursor: pointer;
  }

  .title-button :global(svg) {
    flex: 0 0 auto;
    margin-top: 0.26em;
    color: var(--text-muted);
    opacity: 0;
  }

  .title-button:hover :global(svg),
  .title-button:focus-visible :global(svg) {
    opacity: 1;
  }

  .title-edit {
    width: 100%;
    margin: 4px 0 0;
    border: 1px solid var(--border-default);
    border-radius: 6px;
    background: var(--bg-primary);
    color: var(--text-primary);
    font: inherit;
    font-size: var(--font-size-xl);
    font-weight: 650;
    line-height: 1.25;
    padding: 6px 8px;
  }

  .detail-description {
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

  .text-button {
    min-height: 24px;
    border: 0;
    border-radius: 5px;
    background: transparent;
    color: var(--text-muted);
    display: inline-flex;
    align-items: center;
    gap: var(--space-2);
    padding: 2px 6px;
    font: inherit;
    font-size: var(--font-size-xs);
    cursor: pointer;
  }

  .text-button:hover {
    background: var(--bg-hover);
    color: var(--text-primary);
  }

  .body-display {
    color: var(--text-secondary);
    line-height: 1.5;
  }

  .body-display :global(p) {
    margin: 0;
  }

  .body-display :global(p + p) {
    margin-top: 0.8em;
  }

  .body-display :global(strong) {
    color: var(--text-primary);
    font-weight: 650;
  }

  .detail-body-empty {
    margin: 0;
    color: var(--text-muted);
    font-size: var(--font-size-sm);
  }

  .body-edit {
    width: 100%;
    resize: vertical;
    border: 1px solid var(--border-default);
    border-radius: 6px;
    background: var(--bg-primary);
    color: var(--text-primary);
    font: inherit;
    font-size: var(--font-size-sm);
    line-height: 1.45;
    padding: 8px 10px;
  }

  .body-edit-actions {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 12px;
    margin-top: 8px;
    color: var(--text-muted);
    font-size: var(--font-size-xs);
  }

  .body-edit-actions div {
    display: inline-flex;
    gap: 6px;
  }

  .ghost-button,
  .accent-button {
    min-height: 28px;
    border-radius: 6px;
    font: inherit;
    font-size: var(--font-size-sm);
    padding: 4px 10px;
    cursor: pointer;
  }

  .ghost-button {
    border: 1px solid var(--border-default);
    background: var(--bg-primary);
    color: var(--text-secondary);
  }

  .accent-button {
    border: 1px solid var(--accent-blue);
    background: var(--accent-blue);
    color: white;
  }

  .ghost-button:disabled,
  .accent-button:disabled {
    cursor: default;
    opacity: 0.62;
  }
</style>
