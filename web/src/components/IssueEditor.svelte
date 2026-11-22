<script lang="ts">
  import { tick } from 'svelte'
  import { Avatar, CopyButton, Markdown, Notice, type TypeaheadOption } from '@kenn-io/kit-ui'
  import { IssueAttachments, projectIssueDetail } from '@kenn-io/kata-ui'
  import NetworkIcon from '@lucide/svelte/icons/network'
  import TriangleAlertIcon from '@lucide/svelte/icons/triangle-alert'
  import PencilIcon from '@lucide/svelte/icons/pencil'
  import {
    formatRelativeTime as timeAgo,
    formatTimestamp as localDateTimeLabel,
  } from '@kenn-io/kit-ui/utils/time'

  import type { UIIssueReference } from '../lib/api/generated'
  import { fetchAttachmentBlob } from '../lib/api/client'
  import { deadlineDate, closedReasonLabel } from '../lib/kata/view'
  import { localDateString } from '../lib/kata/dates'
  import { describeResolution } from '../lib/history/format'
  import { closingAttachments } from '../lib/history/attachments'
  import { createKataLinkFilters, type KataLinkFilters } from '../lib/kata/linkFilters'
  import type {
    KataCreateRecurrenceInput,
    KataPatchRecurrenceInput,
    KataProjectSummary,
    KataRecurrence,
    KataTaskDetail,
    KataCommentReplyIntent,
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

  type Reference = UIIssueReference
  interface IssueNavigationTarget {
    commentUID?: string | undefined
    returnComment?: { issueUID: string; commentUID: string } | undefined
    uid: string
  }

  interface Props {
    selectedCommentUID?: string | undefined
    selfActor?: string | undefined
    attachmentsEnabled?: boolean | undefined
    onUploadAttachment?: ((uid: string, file: File) => Promise<void>) | undefined
    // True while the task shows the list's copy and its comments load.
    pending?: boolean | undefined
    navigationActive?: boolean | undefined
    returnComment?: { issueUID: string; commentUID: string } | undefined
    commentError?: string | undefined
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
    administrativeActionsAllowed?: boolean | undefined
    draftResetGeneration?: number | undefined
    draftFenceGeneration?: number | undefined
    movePending?: boolean | undefined
    onMoveIssue: (toProjectUID: string) => boolean | Promise<boolean>
    onPatchMetadata: (uid: string, patch: Record<string, unknown>) => boolean | Promise<boolean>
    onAddComment?:
      | ((uid: string, body: string, reply?: KataCommentReplyIntent) => boolean | Promise<boolean>)
      | undefined
    onEditIssue: (uid: string, patch: KataTaskEditPatch) => boolean | Promise<boolean>
    onAssignOwner: (uid: string, owner: string) => boolean | Promise<boolean>
    onClaimAssignment?:
      | ((uid: string, ttlSeconds: number) => boolean | Promise<boolean>)
      | undefined
    onUnassignOwner: (uid: string) => boolean | Promise<boolean>
    onSetPriority: (uid: string, priority: number | null) => boolean | Promise<boolean>
    onAddLabel: (uid: string, label: string) => boolean | Promise<boolean>
    onRemoveLabel: (uid: string, label: string) => void | Promise<void>
    onCloseIssue: (request: KataTaskCloseRequest) => boolean | Promise<boolean>
    onReopenIssue: () => void | Promise<void>
    onSelectIssue?: ((target: IssueNavigationTarget) => void | Promise<void>) | undefined
    onCreateRecurrence?:
      | ((projectID: number, input: KataCreateRecurrenceInput) => Promise<void>)
      | undefined
    onPatchRecurrence?:
      | ((id: number, input: KataPatchRecurrenceInput, etag: string) => Promise<void>)
      | undefined
    onDeleteRecurrence?: ((recurrence: KataRecurrence) => Promise<boolean>) | undefined
    onOpenGraph?: ((issue: KataTaskDetail['issue']) => void) | undefined
    graphOpen?: boolean | undefined
    onCloseGraph?: (() => void) | undefined
    statusRequest?: { uid: string; generation: number } | null | undefined
    onStatusRequestConsumed?: (() => void) | undefined
  }

  let {
    issue,
    selfActor = undefined,
    attachmentsEnabled = false,
    onUploadAttachment,
    selectedCommentUID,
    pending = false,
    navigationActive = true,
    commentError = undefined,
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
    administrativeActionsAllowed = true,
    draftResetGeneration = 0,
    draftFenceGeneration = 0,
    movePending = false,
    onMoveIssue,
    onPatchMetadata,
    onAddComment = async () => false,
    onEditIssue,
    onAssignOwner,
    onClaimAssignment = async () => false,
    onUnassignOwner,
    onSetPriority,
    onAddLabel,
    onRemoveLabel,
    onCloseIssue,
    onReopenIssue,
    onSelectIssue = () => {},
    onCreateRecurrence = async () => {},
    onPatchRecurrence = async () => {},
    onDeleteRecurrence = async () => false,
    onOpenGraph = undefined,
    graphOpen = false,
    onCloseGraph = undefined,
    statusRequest = null,
    onStatusRequestConsumed = undefined,
  }: Props = $props()

  let editingTitle = $state(false)
  let editingBody = $state(false)
  let savingTitle = $state(false)
  let savingBody = $state(false)
  let titleDraft = $state('')
  let bodyDraft = $state('')
  let titleRevision = $state<number | undefined>()
  let bodyRevision = $state<number | undefined>()
  let titleConflict = $state(false)
  let bodyConflict = $state(false)
  let titleStart = ''
  let bodyStart = ''
  let bodyTitleStart = ''
  let titleBodyStart = ''
  let titleInput: HTMLInputElement | null = $state(null)
  let bodyTextarea: HTMLTextAreaElement | null = $state(null)
  let cancelingTitle = $state(false)
  let lastIssueUID = $state<string | null>(null)
  let lastDraftResetGeneration = $state<number | null>(null)
  let lastDraftFenceGeneration = $state<number | null>(null)
  let pendingTitleResetUID = $state<string | null>(null)
  let pendingTitleResetGeneration = $state<number | null>(null)
  let pendingBodyResetUID = $state<string | null>(null)
  let pendingBodyResetGeneration = $state<number | null>(null)
  let moveDialog: { open: () => Promise<void> } | null = $state(null)
  let stateDialog: { activate: () => void } | null = $state(null)
  let consumedStatusGeneration = 0
  let recurrenceDialogs: {
    openCreateRecurrence: () => void
    openEditRecurrence: (recurrence: KataRecurrence) => void
    openDeleteRecurrence: (recurrence: KataRecurrence) => void
    closeAll: () => void
  } | null = $state(null)

  const detailInert = $derived(authorityBlocked ?? actionsDisabled)
  const attachments = $derived(projectIssueDetail(issue).issue.attachments)
  $effect(() => {
    if (
      !statusRequest ||
      statusRequest.generation === consumedStatusGeneration ||
      pending ||
      detailInert ||
      actionsDisabled ||
      statusRequest.uid !== issue.issue.uid ||
      !stateDialog
    )
      return
    consumedStatusGeneration = statusRequest.generation
    stateDialog.activate()
    onStatusRequestConsumed?.()
  })
  const canCreateRecurrence = $derived(
    issue.issue.status === 'open' && issue.issue.recurrence_id === undefined,
  )
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
    titleConflict = false
    editingBody = false
    savingTitle = false
    savingBody = false
    cancelingTitle = false
    pendingTitleResetUID = null
    pendingTitleResetGeneration = null
    pendingBodyResetUID = null
    pendingBodyResetGeneration = null
  })

  $effect(() => {
    const nextGeneration = draftFenceGeneration
    if (lastDraftFenceGeneration === null) {
      lastDraftFenceGeneration = nextGeneration
      return
    }
    if (nextGeneration === lastDraftFenceGeneration) return
    lastDraftFenceGeneration = nextGeneration
    resetTitleDraft()
    resetBodyDraft()
    recurrenceDialogs?.closeAll()
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
    titleConflict = false
    pendingTitleResetUID = null
    pendingTitleResetGeneration = null
  }

  function resetBodyDraft(): void {
    editingBody = false
    bodyDraft = ''
    pendingBodyResetUID = null
    pendingBodyResetGeneration = null
  }

  const blocker = $derived(issue.issue.open_blocked_by?.[0])
  const blockerTitle = $derived(issueCatalog.find((task) => task.uid === blocker?.uid)?.title ?? '')
  const due = $derived(deadlineDate(issue.issue))
  const today = localDateString(new Date())
  const lateDays = $derived(
    due
      ? Math.round((Date.parse(`${today}T00:00:00Z`) - Date.parse(`${due}T00:00:00Z`)) / 86400000)
      : 0,
  )
  const closeEvent = $derived(events.find((event) => event.type === 'issue.closed'))
  function actorName(actor: string): string {
    return actor === selfActor ? 'You' : actor
  }
  function closingProof(event: KataTaskEvent): string[] {
    const evidence = event.payload?.evidence
    if (!Array.isArray(evidence)) return []
    return evidence.flatMap((item: unknown) => {
      if (!item || typeof item !== 'object') return []
      const value = item as Record<string, unknown>
      const fields: Record<string, string> = {
        test: 'command',
        commit: 'sha',
        pr: 'url',
        external: 'account',
      }
      if (typeof value.type !== 'string') return []
      if (value.type === 'reviewed-paths' && Array.isArray(value.paths))
        return value.paths.filter((path): path is string => typeof path === 'string')
      const field = fields[value.type]
      return field && typeof value[field] === 'string' ? [value[field] as string] : []
    })
  }

  function startEditingTitle(): void {
    if (actionsDisabled) return
    cancelingTitle = false
    titleDraft = issue.issue.title
    titleStart = issue.issue.title
    titleBodyStart = issue.issue.body
    titleRevision = issue.issue.content_revision
    titleConflict = false
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
      const result = await onEditIssue(mutationUID, {
        title: next,
        ...(titleRevision === undefined ? {} : { expected_content_revision: titleRevision }),
      })
      if (result === true) {
        if (issue.issue.uid !== mutationUID) return
        if (draftResetGeneration !== resetGeneration) {
          resetTitleDraft()
        } else {
          pendingTitleResetUID = mutationUID
          pendingTitleResetGeneration = resetGeneration
        }
      } else {
        await tick()
        if (
          issue.issue.uid === mutationUID &&
          (issue.issue.title !== titleStart || issue.issue.body !== titleBodyStart)
        ) {
          titleConflict = true
          titleRevision = issue.issue.content_revision
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
      titleConflict = false
    }
  }

  function startEditingBody(): void {
    if (actionsDisabled) return
    bodyDraft = issue.issue.body
    bodyStart = issue.issue.body
    bodyTitleStart = issue.issue.title
    bodyRevision = issue.issue.content_revision
    bodyConflict = false
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
      const result = await onEditIssue(mutationUID, {
        body: next,
        ...(bodyRevision === undefined ? {} : { expected_content_revision: bodyRevision }),
      })
      if (result === true) {
        if (issue.issue.uid !== mutationUID) return
        if (draftResetGeneration !== resetGeneration) {
          resetBodyDraft()
        } else {
          pendingBodyResetUID = mutationUID
          pendingBodyResetGeneration = resetGeneration
        }
      } else {
        await tick()
        if (
          issue.issue.uid === mutationUID &&
          (issue.issue.body !== bodyStart || issue.issue.title !== bodyTitleStart)
        ) {
          bodyConflict = true
          bodyRevision = issue.issue.content_revision
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
      <div class="detail-kicker">
        <span class="crumb-id">{issue.issue.short_id}</span>
        <CopyButton
          text={issue.issue.short_id}
          ariaLabel={`Copy task ID ${issue.issue.short_id}`}
        />
      </div>
      <div class="detail-actions">
        {#if onOpenGraph}
          <button
            type="button"
            class="icon-detail-action"
            aria-label="Graph"
            aria-pressed={graphOpen}
            onclick={() => (graphOpen ? onCloseGraph?.() : onOpenGraph?.(issue.issue))}
            ><NetworkIcon size={14} strokeWidth={1.9} aria-hidden="true" />Graph</button
          >
        {/if}
        {#key draftFenceGeneration}<IssueStateDialog
            bind:this={stateDialog}
            {issue}
            disabled={actionsDisabled || detailInert}
            {onCloseIssue}
            {onReopenIssue}
          />{/key}
      </div>
    </div>
    <h2 aria-label={issue.issue.title}>
      {#if editingTitle}
        <input
          class="title-edit"
          aria-label="Edit title"
          bind:this={titleInput}
          bind:value={titleDraft}
          disabled={savingTitle}
          onkeydown={handleTitleKeydown}
          onblur={() => void commitTitle()}
        />
      {:else}
        <button
          type="button"
          class="title-button"
          aria-label="Edit title"
          disabled={actionsDisabled || detailInert}
          onclick={startEditingTitle}>{issue.issue.title}</button
        >
      {/if}
    </h2>
    {#if titleConflict}
      <Notice tone="warning"
        >The task changed while you were editing. Press Enter to save your title, or Esc to keep
        theirs.</Notice
      >
    {/if}
    {#if issue.issue.status === 'closed'}
      <p class="state-line">
        {closeEvent
          ? describeResolution(closeEvent)
          : (closedReasonLabel(issue.issue.closed_reason) ?? 'Closed')}{#if closeEvent}
          · {actorName(closeEvent.actor)}{/if}{#if issue.issue.closed_at}
          · {localDateTimeLabel(issue.issue.closed_at)}{/if}
      </p>
      {#if typeof closeEvent?.payload?.message === 'string'}<p>{closeEvent.payload.message}</p>{/if}
      {#if closeEvent}{#each closingProof(closeEvent) as proof, index (index)}<p>
            {proof}
          </p>{/each}{/if}
      {#if closeEvent}{#each closingAttachments(closeEvent, issue.issue.metadata) as attachment, index (index)}<p
          >
            {#if attachment.href}<a href={attachment.href}>{attachment.label}</a
              >{:else}{attachment.label}{/if}
          </p>{/each}{/if}
    {:else if blocker}
      <p class="state-line">
        <TriangleAlertIcon size={14} aria-hidden="true" /> Blocked by {blocker.short_id}
        {blockerTitle}{#if (issue.issue.open_blocked_by?.length ?? 0) > 1}, +{issue.issue
            .open_blocked_by!.length - 1} more{/if}
      </p>
    {:else if due && lateDays >= 0}
      <p class="state-line">
        <TriangleAlertIcon size={14} aria-hidden="true" />
        {lateDays === 0
          ? 'Due today'
          : `Due ${due}, ${lateDays} ${lateDays === 1 ? 'day' : 'days'} late`}
      </p>
    {/if}
    <p class="created-line">
      Created {localDateTimeLabel(issue.issue.created_at)} by <Avatar
        name={issue.issue.author}
        initials={issue.issue.author.slice(0, 1).toUpperCase()}
        size={20}
        decorative
      />
      {actorName(issue.issue.author)} · Updated {timeAgo(issue.issue.updated_at)}
    </p>
  </fieldset>

  <IssueProperties
    {issue}
    {ownerOptions}
    actionsDisabled={actionsDisabled || detailInert}
    {administrativeActionsAllowed}
    onMove={() => void moveDialog?.open()}
    onCreateRecurrence={() => recurrenceDialogs?.openCreateRecurrence()}
    canCreateRecurrence={administrativeActionsAllowed && canCreateRecurrence}
    repeats={administrativeActionsAllowed && visibleRecurrences.length ? repeats : undefined}
    {draftResetGeneration}
    {draftFenceGeneration}
    {onPatchMetadata}
    {onAssignOwner}
    {onClaimAssignment}
    {onUnassignOwner}
    {onSetPriority}
    {onAddLabel}
    {onRemoveLabel}
  />

  <MoveIssueDialog
    bind:this={moveDialog}
    {issue}
    {projects}
    {movePending}
    allowMove={administrativeActionsAllowed && !actionsDisabled && !detailInert}
    {onMoveIssue}
  />

  <fieldset class="mutation-controls" disabled={actionsDisabled || detailInert}>
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
        {#if bodyConflict}<Notice tone="warning"
            >The task changed while you were editing. Save to keep your description, or cancel to
            keep theirs.</Notice
          >{/if}
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
        <Markdown source={issue.issue.body} class="body-display" />
      {:else}
        <button type="button" class="text-button" onclick={startEditingBody}>Add description</button
        >
      {/if}
    </section>

    <Checklist
      {issue}
      disabled={actionsDisabled}
      {draftResetGeneration}
      {draftFenceGeneration}
      {onPatchMetadata}
    />
  </fieldset>

  {#key issue.issue.uid}
    <IssueLinks
      {issue}
      {issueCatalog}
      {searchReferences}
      {linkFilters}
      {onLinkFiltersChange}
      actionsDisabled={actionsDisabled || detailInert}
      {draftResetGeneration}
      {draftFenceGeneration}
      {onEditIssue}
      {onSelectIssue}
    />
    <div class="detail-section">
      <IssueAttachments
        {attachments}
        showEmpty
        baseURL={`/api/v1/projects/${issue.issue.project_id}/issues/${issue.issue.uid}/attachments`}
        issueRef={issue.issue.qualified_id}
        fetchAttachment={fetchAttachmentBlob}
        uploadAttachment={onUploadAttachment
          ? (file) => onUploadAttachment!(issue.issue.uid, file)
          : undefined}
        {attachmentsEnabled}
        actionsDisabled={actionsDisabled || detailInert}
      />
    </div>
    <div class="detail-section">
      <Comments
        {selfActor}
        {selectedCommentUID}
        {navigationActive}
        {pending}
        {issue}
        {searchReferences}
        {actionsDisabled}
        {draftResetGeneration}
        {draftFenceGeneration}
        {onAddComment}
        {commentError}
        onOpenComment={(uid, commentUID, returnComment) =>
          void onSelectIssue?.({ uid, commentUID, returnComment })}
      />
    </div>
    <IssueHistory
      {events}
      {selfActor}
      metadata={issue.issue.metadata}
      attachmentLinks={false}
      {pending}
    />
  {/key}
</section>

{#snippet repeats()}
  <RecurrencePanel
    compact
    recurrences={visibleRecurrences}
    readOnly={!administrativeActionsAllowed}
    onEdit={(recurrence) => recurrenceDialogs?.openEditRecurrence(recurrence)}
    onDelete={(recurrence) => recurrenceDialogs?.openDeleteRecurrence(recurrence)}
  />
{/snippet}

<RecurrenceDialogs
  bind:this={recurrenceDialogs}
  selectedIssue={issue}
  recurrences={selectedRecurrences}
  actor=""
  disabled={actionsDisabled || !administrativeActionsAllowed}
  {draftFenceGeneration}
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
    scrollbar-gutter: stable;
    background: var(--bg-surface);
    padding: 0 16px 24px;
    container-type: inline-size;
    container-name: kata-detail;
  }
  .mutation-controls {
    display: contents;
  }
  .detail-heading {
    display: flex;
    align-items: center;
    justify-content: space-between;
    min-height: 48px;
    gap: var(--space-5);
    margin-bottom: 8px;
  }
  .detail-kicker,
  .detail-actions,
  .created-line {
    display: flex;
    align-items: center;
    gap: 6px;
  }
  .crumb-id {
    color: var(--text-secondary);
    font: 650 var(--font-size-xs) var(--font-mono);
  }
  h2 {
    margin: 0 0 8px;
    font-size: var(--font-size-xl);
  }
  .state-line,
  .created-line {
    color: var(--text-muted);
    font-size: var(--font-size-sm);
    margin: 4px 0 12px;
  }
  .icon-detail-action {
    min-height: 30px;
    display: inline-flex;
    align-items: center;
    gap: 6px;
    border: 1px solid var(--border-default);
    border-radius: 6px;
    background: var(--bg-primary);
    color: var(--text-secondary);
    padding: 0 8px;
    cursor: pointer;
  }
  .title-button {
    width: 100%;
    border: 0;
    background: transparent;
    color: var(--text-primary);
    padding: 0;
    font: inherit;
    text-align: left;
    cursor: pointer;
  }
  .title-edit {
    width: 100%;
    border: 1px solid var(--border-default);
    border-radius: 6px;
    background: var(--bg-primary);
    color: var(--text-primary);
    font: inherit;
    padding: 6px 8px;
  }
  .detail-description {
    margin: 0 0 24px;
  }
  .detail-section {
    margin-bottom: 24px;
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
    font-size: var(--font-size-md);
    font-weight: var(--font-weight-semibold);
  }
  .text-button {
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
  .body-edit {
    width: 100%;
    resize: vertical;
    border: 1px solid var(--border-default);
    border-radius: 6px;
    background: var(--bg-primary);
    color: var(--text-primary);
    font: inherit;
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
    border: 1px solid var(--border-default);
    border-radius: 6px;
    background: var(--bg-primary);
    color: var(--text-secondary);
    font: inherit;
    padding: 4px 10px;
    cursor: pointer;
  }
  .accent-button {
    border-color: var(--accent-blue);
    background: var(--accent-blue);
    color: var(--bg-surface);
  }
</style>
