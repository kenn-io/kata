<script lang="ts">
  import { tick } from 'svelte'
  import { Avatar, CopyButton, Markdown, Notice, type TypeaheadOption } from '@kenn-io/kit-ui'
  import { CreationAttribution, IssueAttachments, projectIssueDetail } from '@kenn-io/kata-ui'
  import NetworkIcon from '@lucide/svelte/icons/network'
  import TriangleAlertIcon from '@lucide/svelte/icons/triangle-alert'
  import CircleAlertIcon from '@lucide/svelte/icons/circle-alert'
  import CircleCheckIcon from '@lucide/svelte/icons/circle-check'
  import CircleSlashIcon from '@lucide/svelte/icons/circle-slash'
  import PencilIcon from '@lucide/svelte/icons/pencil'
  import { formatRelativeTime as timeAgo } from '@kenn-io/kit-ui/utils/time'

  import type { UIIssueReference } from '../lib/api/generated'
  import { fetchAttachmentBlob } from '../lib/api/client'
  import { deadlineDate, closedReasonLabel } from '../lib/kata/view'
  import { formatDay, localDateString } from '../lib/kata/dates'
  import { describeResolution } from '../lib/history/format'
  import { closingAttachments } from '../lib/history/attachments'
  import { createKataLinkFilters, type KataLinkFilters } from '../lib/kata/linkFilters'
  import type {
    KataCreateRecurrenceInput,
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
  import DetailSection from './DetailSection.svelte'
  import IssueStateDialog from './IssueStateDialog.svelte'
  import MoveIssueDialog from './MoveIssueDialog.svelte'
  import IssueProperties from './IssueProperties.svelte'
  import RecurrenceDialogs from './RecurrenceDialogs.svelte'
  import { formatRRule } from '../lib/recurrence/rrule'

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
    today?: string | undefined
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
    today = localDateString(new Date()),
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
  let titleStart = $state('')
  let titleBodyStart = $state('')
  let bodyTitleStart = $state('')
  let bodyStart = $state('')
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
    return []
  })

  $effect(() => {
    const uid = issue.issue.uid
    if (uid === lastIssueUID) return
    lastIssueUID = uid
    editingTitle = false
    titleConflict = false
    editingBody = false
    bodyConflict = false
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
    pendingTitleResetUID = null
    pendingTitleResetGeneration = null
  }

  function resetBodyDraft(): void {
    editingBody = false
    bodyDraft = ''
    bodyConflict = false
    pendingBodyResetUID = null
    pendingBodyResetGeneration = null
  }

  const blocker = $derived(issue.issue.open_blocked_by?.[0])
  const blockerTitleFromCatalog = $derived(
    issueCatalog.find((task) => task.uid === blocker?.uid)?.title,
  )
  let fetchedBlockerTitle = $state<{ uid: string; title: string }>()
  $effect(() => {
    const uid = blocker?.uid
    const qualifiedID = blocker?.qualified_id
    if (!uid || !qualifiedID || blockerTitleFromCatalog) {
      if (fetchedBlockerTitle?.uid !== uid) fetchedBlockerTitle = undefined
      return
    }

    let active = true
    void searchReferences(qualifiedID)
      .then((references) => {
        if (!active) return
        const match = references.find((reference) => reference.uid === uid)
        if (match) fetchedBlockerTitle = { uid, title: match.title }
      })
      .catch(() => {})
    return () => {
      active = false
    }
  })
  const blockerTitle = $derived(
    blockerTitleFromCatalog ??
      (fetchedBlockerTitle && fetchedBlockerTitle.uid === blocker?.uid
        ? fetchedBlockerTitle.title
        : ''),
  )
  const due = $derived(deadlineDate(issue.issue))
  const lateDays = $derived(
    due
      ? Math.round((Date.parse(`${today}T00:00:00Z`) - Date.parse(`${due}T00:00:00Z`)) / 86400000)
      : 0,
  )
  const closeEvent = $derived(issue.effective_close_event)
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
          (issue.issue.content_revision !== titleRevision ||
            issue.issue.title !== titleStart ||
            issue.issue.body !== titleBodyStart)
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
          (issue.issue.content_revision !== bodyRevision ||
            issue.issue.title !== bodyTitleStart ||
            issue.issue.body !== bodyStart)
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
  <fieldset class="mutation-controls">
    <header class="detail-top">
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
              class="graph-toggle"
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
            disabled={savingTitle || actionsDisabled}
            onkeydown={handleTitleKeydown}
            onblur={() => {
              if (!titleConflict) void commitTitle()
            }}
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
        <Notice tone="error" class="detail-conflict">
          {#snippet icon()}<CircleAlertIcon size={14} aria-hidden="true" />{/snippet}
          {issue.issue.title !== titleStart
            ? `Someone renamed this to "${issue.issue.title}" while you were editing.`
            : 'Someone changed this task while you were editing.'} Press Enter to save yours, or Esc to
          keep theirs.
        </Notice>
      {/if}
      {#if issue.issue.status === 'closed'}
        {@const reason = closeEvent?.payload?.reason ?? issue.issue.closed_reason ?? 'done'}
        <p class="state-line">
          <span class="state-icon" class:state-icon--done={reason === 'done'} aria-hidden="true"
            >{#if reason === 'done'}<CircleCheckIcon size={14} />{:else}<CircleSlashIcon
                size={14}
              />{/if}</span
          >{closeEvent
            ? describeResolution(closeEvent)
            : (closedReasonLabel(issue.issue.closed_reason) ?? 'Closed')}{#if closeEvent}
            · {actorName(closeEvent.actor)}{/if}{#if issue.issue.closed_at}
            · {formatDay(issue.issue.closed_at)}{/if}
        </p>
        {#if typeof closeEvent?.payload?.message === 'string'}<p class="state-detail">
            {closeEvent.payload.message}
          </p>{/if}
        {#if closeEvent}{#each closingProof(closeEvent) as proof, index (index)}<p
              class="state-detail state-detail--code"
            >
              {proof}
            </p>{/each}{/if}
        {#if closeEvent}{#each closingAttachments(closeEvent, issue.issue.metadata) as attachment, index (index)}<p
              class="state-detail"
            >
              {#if attachment.href}<a href={attachment.href}>{attachment.label}</a
                >{:else}{attachment.label}{/if}
            </p>{/each}{/if}
      {:else if blocker || (due && lateDays > 0)}
        <p class="state-line">
          {#if blocker}
            <span class="state-icon state-icon--warning" aria-hidden="true"
              ><TriangleAlertIcon size={14} /></span
            >Blocked by <code>{blocker.short_id}</code>
            {blockerTitle}{#if (issue.issue.open_blocked_by?.length ?? 0) > 1}, +{issue.issue
                .open_blocked_by!.length - 1} more{/if}
          {/if}
          {#if due && lateDays > 0}
            {#if blocker}<span class="state-separator" aria-hidden="true">·</span>{/if}
            <span class="state-icon state-icon--danger" aria-hidden="true"
              ><CircleAlertIcon size={14} /></span
            >{`Due ${formatDay(due)}, ${lateDays} ${lateDays === 1 ? 'day' : 'days'} late`}
          {/if}
        </p>
      {/if}
      <p class="created-line">
        Created {formatDay(issue.issue.created_at)} by
        <span class="created-person"
          ><Avatar name={issue.issue.author} size={16} decorative />{actorName(
            issue.issue.author,
          )}</span
        >
        · Updated {timeAgo(issue.issue.updated_at)}
      </p>
      <CreationAttribution creation={projectIssueDetail(issue).issue.creation} />
    </header>
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

  <fieldset class="mutation-controls">
    <DetailSection
      title="Description"
      empty={editingBody || issue.issue.body ? undefined : 'No description'}
    >
      {#snippet actions()}
        {#if !editingBody}
          <button
            type="button"
            class="text-button"
            aria-label="Edit description"
            disabled={actionsDisabled || detailInert}
            onclick={startEditingBody}
          >
            <PencilIcon size={13} strokeWidth={1.8} aria-hidden="true" />
            <span>Edit</span>
          </button>
        {/if}
      {/snippet}
      {#if editingBody}
        {#if bodyConflict}
          <Notice tone="error" class="detail-conflict">
            {#snippet icon()}<CircleAlertIcon size={14} aria-hidden="true" />{/snippet}
            Someone changed this task while you were editing. Save again to keep your description, or
            cancel to keep the current version.
          </Notice>
        {/if}
        <textarea
          class="body-edit"
          aria-label="Edit description"
          rows="8"
          bind:this={bodyTextarea}
          bind:value={bodyDraft}
          disabled={savingBody || actionsDisabled}
          onkeydown={handleBodyKeydown}
        ></textarea>
        <div class="body-edit-actions">
          <span>Cmd/Ctrl+Enter saves</span>
          <div>
            <button
              type="button"
              class="ghost-button"
              disabled={savingBody || actionsDisabled}
              onclick={() => {
                editingBody = false
                bodyConflict = false
              }}>Cancel</button
            >
            <button
              type="button"
              class="accent-button"
              disabled={savingBody || actionsDisabled}
              onclick={() => {
                void commitBody()
              }}
            >
              {savingBody ? 'Saving…' : 'Save'}
            </button>
          </div>
        </div>
      {:else}
        <Markdown source={issue.issue.body} class="body-display" />
      {/if}
    </DetailSection>

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
      {draftFenceGeneration}
      {onEditIssue}
      {onSelectIssue}
    />
    <div>
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
    <div>
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
    <IssueHistory {events} {selfActor} metadata={issue.issue.metadata} {pending} />
  {/key}
</section>

{#snippet repeats()}
  {#each visibleRecurrences as recurrence (recurrence.uid)}
    <span>{recurrence.template_title} · {formatRRule(recurrence.rrule)}</span>
  {/each}
{/snippet}

<MoveIssueDialog
  bind:this={moveDialog}
  {issue}
  {projects}
  {movePending}
  allowMove={administrativeActionsAllowed && !actionsDisabled && !detailInert}
  {onMoveIssue}
  error={commentError}
/>

<RecurrenceDialogs
  bind:this={recurrenceDialogs}
  selectedIssue={issue}
  recurrences={selectedRecurrences}
  actor=""
  disabled={actionsDisabled || !administrativeActionsAllowed}
  {draftFenceGeneration}
  onCreate={onCreateRecurrence}
  onPatch={async () => {}}
  onDelete={async () => false}
/>

<style>
  /* Sections stack 24px apart; the header block keeps its own tighter
     rhythm. */
  .kata-detail {
    display: flex;
    flex-direction: column;
    gap: 24px;
    flex: 1 1 auto;
    min-width: 0;
    min-height: 0;
    overflow: auto;
    scrollbar-gutter: stable;
    background: var(--bg-surface);
    padding: 0 16px 32px;
    container-type: inline-size;
    container-name: kata-detail;
  }
  .mutation-controls {
    display: contents;
  }
  .detail-top {
    display: grid;
    gap: 2px;
    min-width: 0;
  }
  .detail-top :global(.detail-conflict) {
    margin: 2px 0 6px;
  }
  /* The 48px header band, level with the list's header beside it; the title
     starts 8px under it. */
  .detail-heading {
    box-sizing: border-box;
    display: flex;
    align-items: center;
    justify-content: space-between;
    min-height: 48px;
    padding-top: 8px;
    gap: 12px;
    margin-bottom: 6px;
  }
  .detail-kicker,
  .detail-actions {
    display: flex;
    align-items: center;
    gap: 8px;
  }
  .detail-kicker {
    gap: 4px;
    min-width: 0;
  }
  .crumb-id {
    color: var(--text-secondary);
    font-family: var(--font-mono);
    font-size: var(--font-size-md);
  }
  /* A labelled secondary button, pressed while the graph shows. */
  .graph-toggle {
    display: inline-flex;
    align-items: center;
    gap: 4px;
    height: 28px;
    padding: 0 12px;
    border: 1px solid var(--border-default);
    border-radius: var(--radius-md);
    background: var(--bg-surface);
    color: var(--text-primary);
    font: inherit;
    font-size: var(--font-size-md);
    white-space: nowrap;
    cursor: pointer;
  }
  /* Header actions are 28px controls, Complete… included. */
  .detail-actions :global(.kit-button) {
    min-height: 28px;
    padding: 0 12px;
    font-size: var(--font-size-md);
    font-weight: var(--font-weight-normal);
  }
  .graph-toggle:hover,
  .graph-toggle[aria-pressed='true'] {
    background: var(--bg-surface-hover);
  }
  h2 {
    margin: 0 0 6px;
    font-size: var(--font-size-xl);
    font-weight: var(--font-weight-semibold);
    line-height: 1.25;
  }
  /* The title edits in place: the button and its input share one box that
     hangs its border and padding into the inset, so the text stays put and
     clicking shows only the border and caret. */
  .title-button,
  .title-edit {
    box-sizing: border-box;
    display: block;
    width: calc(100% + 18px);
    margin: -3px -9px;
    border: 1px solid transparent;
    border-radius: var(--radius-md);
    padding: 2px 8px;
    font: inherit;
    line-height: inherit;
  }
  .title-button {
    background: transparent;
    color: var(--text-primary);
    text-align: left;
    cursor: pointer;
  }
  .title-button:hover:not(:disabled) {
    background: var(--bg-surface-hover);
  }
  .title-edit {
    border-color: var(--border-default);
    background: var(--bg-primary);
    color: var(--text-primary);
  }
  .state-line,
  .state-detail,
  .created-line {
    margin: 0;
    color: var(--text-secondary);
    font-size: var(--font-size-sm);
    line-height: 18px;
  }
  .state-detail {
    padding-left: 18px;
    overflow-wrap: anywhere;
  }
  .state-detail--code,
  .state-line code {
    font-family: var(--font-mono);
  }
  /* Each state carries its mark: blocked an amber warning, overdue a red
     alert, Done a green check, other resolutions a muted slashed circle. */
  .state-icon {
    display: inline-flex;
    margin-inline-end: 4px;
    vertical-align: -2px;
    color: var(--text-muted);
  }
  .state-icon--done {
    color: var(--accent-green);
  }
  .state-icon--warning {
    color: var(--accent-amber);
  }
  .state-icon--danger {
    color: var(--accent-red);
  }
  .state-separator {
    padding-inline: 6px;
    color: var(--text-muted);
  }
  .created-person {
    display: inline-flex;
    align-items: center;
    gap: 4px;
    vertical-align: bottom;
  }
  .text-button {
    border: 0;
    border-radius: var(--radius-md);
    background: transparent;
    color: var(--text-secondary);
    display: inline-flex;
    align-items: center;
    gap: 4px;
    height: 24px;
    padding: 0 8px;
    font: inherit;
    font-size: var(--font-size-md);
    cursor: pointer;
  }
  .text-button:hover {
    background: var(--bg-surface-hover);
    color: var(--text-primary);
  }
  /* A heading inside the description reads below the section's own. */
  .kata-detail :global(.body-display :is(h1, h2, h3, h4)) {
    margin: 0 0 8px;
    font-size: var(--font-size-lg);
    font-weight: var(--font-weight-semibold);
  }
  .kata-detail :global(.body-display) {
    font-size: var(--font-size-md);
  }
  .body-edit {
    box-sizing: border-box;
    display: block;
    width: calc(100% + 18px);
    margin: -7px -9px 0;
    resize: vertical;
    border: 1px solid var(--border-default);
    border-radius: var(--radius-md);
    background: var(--bg-primary);
    color: var(--text-primary);
    font: inherit;
    font-size: var(--font-size-md);
    padding: 6px 8px;
  }
  .body-edit-actions {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 12px;
    margin-top: 8px;
    color: var(--text-secondary);
    font-size: var(--font-size-sm);
  }
  .body-edit-actions div {
    display: inline-flex;
    gap: 8px;
  }
  /* Cancel is quiet; Save finishes the edit, so it is the primary. */
  .ghost-button,
  .accent-button {
    height: 28px;
    border: 1px solid transparent;
    border-radius: var(--radius-md);
    background: transparent;
    color: var(--text-secondary);
    font: inherit;
    font-size: var(--font-size-md);
    padding: 0 12px;
    cursor: pointer;
  }
  .ghost-button:hover:not(:disabled) {
    background: var(--bg-surface-hover);
    color: var(--text-primary);
  }
  .accent-button {
    border-color: var(--accent-blue);
    background: var(--accent-blue);
    color: var(--bg-surface);
  }
  @container kata-detail (max-width: 480px) {
    .detail-heading {
      flex-wrap: wrap;
    }
  }
</style>
