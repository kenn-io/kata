<script lang="ts">
  import {
    Button,
    FlashBanner,
    Chip,
    Menu,
    MenuContent,
    MenuItem,
    showFlash,
    type MenuAnchorPoint,
  } from '@kenn-io/kit-ui'
  import CalendarIcon from '@lucide/svelte/icons/calendar'
  import CirclePauseIcon from '@lucide/svelte/icons/circle-pause'
  import HourglassIcon from '@lucide/svelte/icons/hourglass'
  import CornerDownRightIcon from '@lucide/svelte/icons/corner-down-right'
  import FoldHorizontalIcon from '@lucide/svelte/icons/fold-horizontal'
  import UnfoldHorizontalIcon from '@lucide/svelte/icons/unfold-horizontal'
  import EllipsisIcon from '@lucide/svelte/icons/ellipsis'
  import MessageSquareIcon from '@lucide/svelte/icons/message-square'
  import ListTreeIcon from '@lucide/svelte/icons/list-tree'
  import OctagonAlertIcon from '@lucide/svelte/icons/octagon-alert'
  import PlusIcon from '@lucide/svelte/icons/plus'
  import { SvelteMap, SvelteSet } from 'svelte/reactivity'

  import {
    askRecipient,
    buildBoard,
    cardMarkers,
    columnFor,
    defaultDoneLimit,
    maxDoneLimit,
    isEpic,
    moveTargets,
    planBoardMove,
    type BoardColumn,
    type BoardColumnID,
    type BoardMovePlan,
  } from '../lib/kata/board'
  import {
    attentionBadgeLabels,
    attentionReasons,
    needsReviewLabel,
    type AttentionViewer,
  } from '../lib/kata/attention'
  import { localDateString } from '../lib/kata/dates'
  import type { KataTaskCloseRequest, KataTaskSummary } from '../lib/kata/types'
  import CompleteIssueDialog from './CompleteIssueDialog.svelte'
  import Modal from './Modal.svelte'

  interface Props {
    issues: readonly KataTaskSummary[]
    selectedIssueUID: string | null
    /** Show each card's project, for boards that span projects. */
    showProject: boolean
    loading?: boolean | undefined
    /** Set when cards cannot move; the board shows it and stays read-only. */
    moveBlockedReason?: string | undefined
    onOpenIssue: (issue: KataTaskSummary) => void
    onMoveIssue: (issue: KataTaskSummary, plan: BoardMovePlan) => Promise<boolean>
    /** Sends a `kata notify` request about the issue; the card does not move. */
    onAskIssue?:
      | ((issue: KataTaskSummary, to: string, message: string) => Promise<boolean>)
      | undefined
    /** Whose attention the board reads; see AttentionViewer. */
    viewer?: AttentionViewer | undefined
    onCompleteIssue: (issue: KataTaskSummary, request: KataTaskCloseRequest) => Promise<boolean>
    /** Closed issues the server counted; set when Done is paged on the server. */
    doneTotal?: number | undefined
    /** Asks the server for the next page of closed issues. */
    onShowMoreDone?: (() => void) | undefined
    /** Another change is saving; the board starts no new write until it finishes. */
    busy?: boolean | undefined
    /**
     * Rises when the app fences drafts, as after a lost session: open forms
     * and menus close and drop what was typed, so nothing typed under the old
     * authority is sent under the new one.
     */
    draftFenceGeneration?: number | undefined
    /** Opens Logbook, which lists every closed issue, once Done can page no further. */
    onOpenLogbook?: (() => void) | undefined
    /** One-line cards with ID, priority, and title only. */
    compact?: boolean | undefined
    /** Column IDs shown as narrow strips. */
    collapsedColumns?: readonly string[] | undefined
    /** Collapses or expands a column; collapse is offered for Done and empty columns. */
    onToggleColumn?: ((columnID: BoardColumnID) => void) | undefined
    /** Creates an issue from the Open column. */
    onAddCard?: ((title: string) => Promise<boolean>) | undefined
    /** Flash cards that someone else moved or added while the board is open. */
    highlightRemoteChanges?: boolean | undefined
    /** Identifies what the board shows (project, filters); a change resets remote tracking. */
    scopeKey?: string | undefined
  }

  let {
    issues,
    selectedIssueUID,
    showProject,
    loading = false,
    moveBlockedReason = undefined,
    onOpenIssue,
    onMoveIssue,
    onCompleteIssue,
    onAskIssue = undefined,
    viewer = undefined,
    doneTotal = undefined,
    onShowMoreDone = undefined,
    busy = false,
    draftFenceGeneration = 0,
    onOpenLogbook = undefined,
    compact = false,
    collapsedColumns = [],
    onToggleColumn = undefined,
    onAddCard = undefined,
    highlightRemoteChanges = true,
    scopeKey = '',
  }: Props = $props()

  const uid = $props.id()
  const helpID = `${uid}-help`
  const pending = new SvelteMap<string, BoardColumnID>()
  // Cards that just landed, highlighted briefly so the eye can find them.
  const justMoved = new SvelteSet<string>()
  const highlightMs = 1200
  // Cards that changed because someone else moved or added them.
  const remoteChanged = new SvelteSet<string>()
  const remoteHighlightMs = 2400
  // Cards this viewer is moving or just added; their refreshes are not remote.
  const ownChanges = new SvelteSet<string>()
  const ownTitles = new SvelteSet<string>()
  let seenColumns: Map<string, BoardColumnID> | null = null
  let seenKey = ''

  $effect(() => {
    const key = scopeKey
    const current = new Map(issues.map((issue) => [issue.uid, columnFor(issue, { viewer })]))
    const previous = seenColumns
    seenColumns = current
    // A new scope or filter is a new view, not a change to flag.
    if (previous === null || key !== seenKey) {
      seenKey = key
      return
    }
    if (!highlightRemoteChanges) return
    for (const issue of issues) {
      if (ownChanges.has(issue.uid)) continue
      const before = previous.get(issue.uid)
      const changed =
        before === undefined
          ? // Older closed cards arrive with the next page of Done; only new open cards count.
            issue.status === 'open' && !ownTitles.has(issue.title)
          : before !== current.get(issue.uid)
      if (changed) flashRemote(issue.uid)
    }
  })

  function flashRemote(uid: string): void {
    remoteChanged.add(uid)
    setTimeout(() => remoteChanged.delete(uid), remoteHighlightMs)
  }

  function markOwnChange(uid: string): void {
    ownChanges.add(uid)
  }

  function releaseOwnChange(uid: string): void {
    // A refresh can land just after the save returns, so keep it ours a little longer.
    setTimeout(() => ownChanges.delete(uid), highlightMs)
  }
  let doneLimit = $state(defaultDoneLimit)
  let announcement = $state('')
  let draggingUID = $state<string | null>(null)
  let dropTarget = $state<BoardColumnID | null>(null)
  let completing = $state<KataTaskSummary | null>(null)
  let menuIssue = $state<KataTaskSummary | null>(null)
  let menuOpen = $state(false)
  let menuAnchor = $state<MenuAnchorPoint | undefined>()
  let menuReturn = $state<HTMLElement | undefined>()
  let boardElement = $state<HTMLElement>()
  let columnsElement = $state<HTMLElement>()
  // The column most in view, which the phone strip marks as current.
  let columnInView = $state<BoardColumnID>('open')

  $effect(() => {
    const root = columnsElement
    if (!root || typeof IntersectionObserver === 'undefined') return
    // How much of each column shows; the most visible one is current.
    const ratios: Record<string, number> = {}
    const observer = new IntersectionObserver(
      (entries) => {
        for (const entry of entries) {
          const id = (entry.target as HTMLElement).dataset.column
          if (id) ratios[id] = entry.intersectionRatio
        }
        let best: string | undefined
        for (const [id, ratio] of Object.entries(ratios)) {
          if (best === undefined || ratio > ratios[best]!) best = id
        }
        if (best) columnInView = best as BoardColumnID
      },
      { root, threshold: [0, 0.25, 0.5, 0.75, 1] },
    )
    for (const section of root.querySelectorAll<HTMLElement>('[data-column]')) {
      observer.observe(section)
    }
    return () => observer.disconnect()
  })

  // Keep the current column's name visible in the strip as the board scrolls.
  let stripElement = $state<HTMLElement>()
  $effect(() => {
    const id = columnInView
    stripElement
      ?.querySelector<HTMLElement>(`[data-strip="${id}"]`)
      ?.scrollIntoView({ inline: 'nearest', block: 'nearest' })
  })

  function showColumn(id: BoardColumnID): void {
    columnInView = id
    const section = columnsElement?.querySelector<HTMLElement>(`[data-column="${id}"]`)
    const reduced = globalThis.matchMedia?.('(prefers-reduced-motion: reduce)').matches ?? false
    section?.scrollIntoView({
      inline: 'start',
      block: 'nearest',
      behavior: reduced ? 'auto' : 'smooth',
    })
  }

  let canMove = $derived(moveBlockedReason === undefined)
  let serverPaged = $derived(doneTotal !== undefined && onShowMoreDone !== undefined)
  let board = $derived(
    buildBoard(issues, {
      doneLimit,
      pending,
      viewer,
      doneTotal: serverPaged ? doneTotal : undefined,
    }),
  )
  let collapsedSet = $derived(new Set(collapsedColumns))
  // Narrowing empty columns helps only when other columns have cards to show.
  let boardEmpty = $derived(board.columns.every((column) => column.total === 0))
  const emptyText: Record<BoardColumnID, string> = {
    open: 'No open tasks',
    'in-progress': 'Nothing claimed',
    attention: 'Nothing needs you',
    done: 'Nothing done yet',
  }
  let issuesByUID = $derived(new Map(issues.map((issue) => [issue.uid, issue])))
  const today = localDateString()

  function columnTitle(id: BoardColumnID): string {
    return board.columns.find((column) => column.id === id)?.title ?? id
  }

  function currentColumn(issue: KataTaskSummary): BoardColumnID {
    return pending.get(issue.uid) ?? columnFor(issue, { viewer })
  }

  function canDropOn(issue: KataTaskSummary, target: BoardColumnID): boolean {
    return planBoardMove(issue, target, { viewer }) !== undefined
  }

  async function moveTo(issue: KataTaskSummary, target: BoardColumnID): Promise<void> {
    if (!canMove || busy || pending.has(issue.uid)) return
    const plan = planBoardMove(issue, target, { viewer })
    if (!plan) return
    if (plan.close) {
      completing = issue
      return
    }
    await performMove(issue, target, plan)
  }

  async function performMove(
    issue: KataTaskSummary,
    target: BoardColumnID,
    plan: BoardMovePlan,
  ): Promise<boolean> {
    markOwnChange(issue.uid)
    pending.set(issue.uid, target)
    announcement = `Moving ${issue.title} to ${columnTitle(target)}…`
    let moved = false
    try {
      moved = await onMoveIssue(issue, plan)
    } finally {
      pending.delete(issue.uid)
      releaseOwnChange(issue.uid)
    }
    if (!moved) {
      // A move can take several kata actions, and an earlier one (such as the
      // reopen) may have saved; the refreshed board shows where the card is.
      announcement = `Could not finish moving ${issue.title}.`
      return false
    }
    // The banner is a status region, so it carries the success message.
    announcement = ''
    highlight(issue.uid)
    // Every move is one kata action, so moving the card back reverses it.
    showFlash(
      `Moved ${issue.title} to ${columnTitle(target)}${plan.clearAttention ? ' and marked it handled' : ''}.`,
    )
    return true
  }

  // "Ask…" sends a kata notify request to the owner (or author) and waits here
  // for its message. Only the waiting party raises attention, so the card stays put.
  let asking = $state<{ issue: KataTaskSummary; to: string } | null>(null)
  let askMessage = $state('')
  let askSaving = $state(false)

  function canAsk(issue: KataTaskSummary): boolean {
    return (
      onAskIssue !== undefined &&
      issue.status === 'open' &&
      askRecipient(issue) !== (viewer?.actor ?? '')
    )
  }

  let lastDraftFence: number | null = null
  $effect(() => {
    const generation = draftFenceGeneration
    if (lastDraftFence === null || generation === lastDraftFence) {
      lastDraftFence = generation
      return
    }
    lastDraftFence = generation
    menuOpen = false
    menuIssue = null
    completing = null
    asking = null
    askMessage = ''
    addingTo = null
    addDraft = ''
  })

  function openAsk(issue: KataTaskSummary): void {
    asking = { issue, to: askRecipient(issue) }
    askMessage = ''
  }

  async function submitAsk(): Promise<void> {
    const request = asking
    const message = askMessage.trim()
    if (!request || !message || askSaving || busy || !onAskIssue) return
    askSaving = true
    try {
      if (await onAskIssue(request.issue, request.to, message)) {
        asking = null
        showFlash(`Asked ${request.to} about ${request.issue.title}.`)
      }
    } finally {
      askSaving = false
    }
  }

  function highlight(uid: string): void {
    justMoved.add(uid)
    setTimeout(() => justMoved.delete(uid), highlightMs)
  }

  async function complete(request: KataTaskCloseRequest): Promise<boolean> {
    const issue = completing
    if (!issue) return false
    markOwnChange(issue.uid)
    pending.set(issue.uid, 'done')
    announcement = `Completing ${issue.title}…`
    let closed = false
    try {
      closed = await onCompleteIssue(issue, request)
    } finally {
      pending.delete(issue.uid)
      releaseOwnChange(issue.uid)
    }
    // The form stays open until the daemon accepts the close, so a refused
    // close keeps the reason, note, and evidence for another try.
    if (closed) completing = null
    announcement = closed ? `Completed ${issue.title}.` : `Could not complete ${issue.title}.`
    return closed
  }

  // The nearest column in that direction the card can move to; Alt+→ from In
  // progress skips Needs attention, which no move enters.
  function adjacentColumn(issue: KataTaskSummary, step: -1 | 1): BoardColumnID | undefined {
    const index = board.columns.findIndex((column) => column.id === currentColumn(issue))
    for (let next = index + step; next >= 0 && next < board.columns.length; next += step) {
      const id = board.columns[next]!.id
      if (canDropOn(issue, id)) return id
    }
    return undefined
  }

  function focusCard(uid: string): void {
    queueMicrotask(() => {
      const cards = boardElement?.querySelectorAll<HTMLElement>('[data-card-uid]') ?? []
      for (const card of cards) if (card.dataset.cardUid === uid) card.focus()
    })
  }

  function handleCardKeydown(event: KeyboardEvent, issue: KataTaskSummary): void {
    const horizontal = event.key === 'ArrowLeft' || event.key === 'ArrowRight'
    const vertical = event.key === 'ArrowUp' || event.key === 'ArrowDown'
    if (!horizontal && !vertical) return
    event.preventDefault()
    const step = event.key === 'ArrowLeft' || event.key === 'ArrowUp' ? -1 : 1
    if (event.altKey && horizontal) {
      const target = adjacentColumn(issue, step)
      if (target) {
        void moveTo(issue, target)
        focusCard(issue.uid)
      }
      return
    }
    const columnIndex = board.columns.findIndex((column) => column.id === currentColumn(issue))
    const cards = board.columns[columnIndex]?.issues ?? []
    const row = cards.findIndex((card) => card.uid === issue.uid)
    if (vertical) {
      const next = cards[row + step]
      if (next) focusCard(next.uid)
      return
    }
    // Left/Right: the nearest card at the same height in the next non-empty column.
    for (
      let index = columnIndex + step;
      index >= 0 && index < board.columns.length;
      index += step
    ) {
      const target = board.columns[index]!.issues
      if (target.length === 0 || collapsedSet.has(board.columns[index]!.id)) continue
      focusCard(target[Math.min(row, target.length - 1)]!.uid)
      return
    }
  }

  function openMenu(event: MouseEvent, issue: KataTaskSummary): void {
    const button = event.currentTarget as HTMLElement
    const rect = button.getBoundingClientRect()
    menuIssue = issue
    menuAnchor = { x: rect.right, y: rect.bottom + 4 }
    menuReturn = button
    menuOpen = true
  }

  function handleDragStart(event: DragEvent, issue: KataTaskSummary): void {
    if (!canMove || busy || pending.has(issue.uid)) {
      event.preventDefault()
      return
    }
    draggingUID = issue.uid
    event.dataTransfer?.setData('text/plain', issue.uid)
    if (event.dataTransfer) event.dataTransfer.effectAllowed = 'move'
  }

  function handleDragOver(event: DragEvent, column: BoardColumn<KataTaskSummary>): void {
    if (!draggingUID) return
    const issue = issuesByUID.get(draggingUID)
    // Not cancelling the event shows the browser's "no drop" cursor.
    if (!issue || !canDropOn(issue, column.id)) return
    event.preventDefault()
    if (event.dataTransfer) event.dataTransfer.dropEffect = 'move'
    dropTarget = column.id
  }

  function handleDragLeave(event: DragEvent, column: BoardColumn<KataTaskSummary>): void {
    const next = event.relatedTarget as Node | null
    if (next && (event.currentTarget as HTMLElement).contains(next)) return
    if (dropTarget === column.id) dropTarget = null
  }

  function handleDrop(event: DragEvent, column: BoardColumn<KataTaskSummary>): void {
    event.preventDefault()
    const uid = draggingUID ?? event.dataTransfer?.getData('text/plain')
    draggingUID = null
    dropTarget = null
    const issue = uid ? issuesByUID.get(uid) : undefined
    if (issue) void moveTo(issue, column.id)
  }

  function endDrag(): void {
    draggingUID = null
    dropTarget = null
  }

  function columnColor(column: BoardColumn<KataTaskSummary>): string {
    return {
      open: 'var(--text-muted)',
      'in-progress': 'var(--accent-blue)',
      attention: 'var(--accent-amber)',
      done: 'var(--accent-green)',
    }[column.id]
  }

  // The column whose add-card form is open, its draft, and whether it is saving.
  let addingTo = $state<BoardColumnID | null>(null)
  let addDraft = $state('')
  let addSaving = $state(false)

  function openAddCard(column: BoardColumn<KataTaskSummary>): void {
    addingTo = column.id
    addDraft = ''
    queueMicrotask(() => addInput()?.focus())
  }

  function addInput(): HTMLTextAreaElement | null | undefined {
    return boardElement?.querySelector<HTMLTextAreaElement>('textarea.add-card-input')
  }

  function closeAddCard(column: BoardColumn<KataTaskSummary>): void {
    addingTo = null
    addDraft = ''
    queueMicrotask(() =>
      boardElement?.querySelector<HTMLElement>(`[data-add-card="${column.id}"]`)?.focus(),
    )
  }

  async function submitAddCard(column: BoardColumn<KataTaskSummary>): Promise<void> {
    const title = addDraft.trim()
    if (!title || addSaving || busy || !onAddCard) return
    addSaving = true
    // The new card arrives in a refresh before its ID is known, so match its title.
    ownTitles.add(title)
    setTimeout(() => ownTitles.delete(title), 10_000)
    try {
      if (await onAddCard(title)) {
        // Stay open so several cards can be added in a row.
        addDraft = ''
        announcement = `Added ${title} to ${column.title}.`
      } else {
        announcement = `Could not add ${title}.`
      }
    } finally {
      addSaving = false
      queueMicrotask(() => addInput()?.focus())
    }
  }

  function handleAddKeydown(event: KeyboardEvent, column: BoardColumn<KataTaskSummary>): void {
    if (event.key === 'Enter' && !event.shiftKey) {
      event.preventDefault()
      void submitAddCard(column)
    } else if (event.key === 'Escape') {
      event.preventDefault()
      event.stopPropagation()
      closeAddCard(column)
    }
  }

  function columnLabel(column: BoardColumn<KataTaskSummary>): string {
    return `${column.title}, ${cardCount(column.total)}`
  }

  function commentLabel(count: number | undefined): string {
    const value = count ?? 0
    return `${value} ${value === 1 ? 'comment' : 'comments'}`
  }

  function cardCount(count: number): string {
    return `${count} ${count === 1 ? 'card' : 'cards'}`
  }

  function deadlineOf(issue: KataTaskSummary): string | undefined {
    return (issue.deadline_on_date ?? issue.metadata.deadline_on)?.slice(0, 10)
  }

  function shortDate(value: string): string {
    const date = new Date(`${value}T00:00:00`)
    return date.toLocaleDateString(undefined, { month: 'short', day: 'numeric' })
  }
</script>

<section
  class="board"
  aria-label="Issue board"
  aria-busy={loading}
  aria-describedby={helpID}
  bind:this={boardElement}
>
  {#if moveBlockedReason}
    <p class="board-note" id={helpID}>
      <OctagonAlertIcon size={12} strokeWidth={2} aria-hidden="true" />
      {moveBlockedReason}
    </p>
  {:else}
    <!-- Cards also advertise the shortcuts through aria-keyshortcuts. -->
    <p class="kit-sr-only" id={helpID}>
      Drag cards between columns, or focus a card and press Alt plus Left arrow or Alt plus Right
      arrow to move it.
    </p>
  {/if}
  <!-- Phones show one column at a time; this strip says which, and jumps to the others. -->
  <nav class="column-strip" aria-label="Board columns" bind:this={stripElement}>
    {#each board.columns as column (column.id)}
      <button
        type="button"
        class="strip-item"
        data-strip={column.id}
        aria-current={columnInView === column.id ? 'true' : undefined}
        style:--column-color={columnColor(column)}
        onclick={() => showColumn(column.id)}
      >
        <span class="column-dot" aria-hidden="true"></span>
        {column.title}
        <span class="strip-count">{column.total}</span>
      </button>
    {/each}
  </nav>
  <div class="board-columns" bind:this={columnsElement}>
    {#each board.columns as column (column.id)}
      {@const headingID = `board-heading-${column.id}`}
      {@const collapsed = collapsedSet.has(column.id)}
      {@const canCollapse = column.id === 'done' || (column.total === 0 && !boardEmpty)}
      <section
        class="board-column"
        data-column={column.id}
        class:collapsed
        class:drop-target={dropTarget === column.id && draggingUID !== null}
        class:column--done={column.id === 'done'}
        aria-label={columnLabel(column)}
        style:--column-color={columnColor(column)}
        ondragover={(event) => handleDragOver(event, column)}
        ondragleave={(event) => handleDragLeave(event, column)}
        ondrop={(event) => handleDrop(event, column)}
      >
        {#if collapsed}
          <header class="column-header column-header--collapsed">
            {#if onToggleColumn}
              <button
                type="button"
                class="column-toggle"
                aria-label={`Expand ${column.title}`}
                aria-expanded="false"
                title={`Expand ${column.title}`}
                onclick={() => onToggleColumn(column.id)}
              >
                <UnfoldHorizontalIcon size={14} strokeWidth={2} aria-hidden="true" />
              </button>
            {/if}
            <span class="column-dot" aria-hidden="true"></span>
            <span class="column-count" aria-hidden="true">{column.total}</span>
            <h2 id={headingID} class="column-title column-title--vertical" title={column.title}>
              {column.title}
            </h2>
          </header>
        {:else}
          <header class="column-header">
            <span class="column-dot" aria-hidden="true"></span>
            <h2 id={headingID} class="column-title" title={column.title}>{column.title}</h2>
            <span class="column-count" aria-hidden="true">{column.total}</span>
            {#if onToggleColumn && canCollapse}
              <button
                type="button"
                class="column-toggle"
                aria-label={`Collapse ${column.title}`}
                aria-expanded="true"
                title={`Collapse ${column.title}`}
                onclick={() => onToggleColumn(column.id)}
              >
                <FoldHorizontalIcon size={14} strokeWidth={2} aria-hidden="true" />
              </button>
            {/if}
          </header>
        {/if}
        {#if collapsed}
          <!-- Collapsed: the strip keeps its count and still accepts drops. -->
        {:else if column.issues.length === 0}
          <p class="column-empty">
            {emptyText[column.id]}
          </p>
        {:else}
          <ul class="column-cards" aria-labelledby={headingID}>
            {#each column.issues as issue (issue.uid)}
              {@const titleID = `board-card-title-${issue.uid}`}
              {@const metaID = `board-card-meta-${issue.uid}`}
              {@const relationID = `board-card-relation-${issue.uid}`}
              {@const depth = column.depths.get(issue.uid) ?? 0}
              {@const parent = issue.parent ? issuesByUID.get(issue.parent.uid) : undefined}
              {@const extraLabels = (issue.labels ?? []).filter(
                (label) => label !== needsReviewLabel,
              )}
              {@const reasons = column.id === 'attention' ? attentionReasons(issue, viewer) : []}
              {@const markers = cardMarkers(issue, today)}
              {@const deadline = deadlineOf(issue)}
              <li
                class="card"
                class:selected={issue.uid === selectedIssueUID}
                class:dragging={issue.uid === draggingUID}
                class:pending={pending.has(issue.uid)}
                class:closed={issue.status === 'closed'}
                class:child={depth > 0}
                class:just-moved={justMoved.has(issue.uid)}
                class:remote-change={remoteChanged.has(issue.uid)}
                class:compact
                data-depth={depth}
                style:--depth={Math.min(depth, 3)}
                draggable={canMove ? 'true' : 'false'}
                ondragstart={(event) => handleDragStart(event, issue)}
                ondragend={endDrag}
              >
                <button
                  type="button"
                  class="card-open"
                  data-card-uid={issue.uid}
                  aria-labelledby={titleID}
                  aria-describedby={[
                    `board-card-identity-${issue.uid}`,
                    issue.parent ? relationID : '',
                    reasons.length > 0 ? `board-card-reason-${issue.uid}` : '',
                    column.id === 'attention' && canMove ? 'board-handled-hint' : '',
                    compact ? '' : metaID,
                  ]
                    .filter(Boolean)
                    .join(' ') || undefined}
                  aria-current={issue.uid === selectedIssueUID ? 'true' : undefined}
                  aria-keyshortcuts={canMove ? 'Alt+ArrowLeft Alt+ArrowRight' : undefined}
                  onclick={() => onOpenIssue(issue)}
                  onkeydown={(event) => handleCardKeydown(event, issue)}
                ></button>
                <div class="card-top">
                  <span class="card-id" title={issue.qualified_id}>{issue.short_id}</span>
                  {#if issue.priority !== undefined}
                    <span class={`card-priority priority-${issue.priority}`}>P{issue.priority}</span
                    >
                  {/if}
                  {#if compact}
                    <span class="card-title card-title--inline" id={titleID}>{issue.title}</span>
                  {/if}
                  {#if showProject && !compact}
                    <span class="card-project">{issue.project_name}</span>
                  {/if}
                  {#if depth === 0 && issue.parent && !compact}
                    <!-- The parent sits in another column; name it on the card. -->
                    <span
                      class="card-parent"
                      title={parent ? `Subtask of ${parent.title}` : 'Subtask'}
                      aria-hidden="true"
                    >
                      <CornerDownRightIcon size={11} strokeWidth={2} />
                      <span>{issue.parent.short_id}</span>
                    </span>
                  {/if}
                  {#if reasons.length > 0}
                    <span class={`card-attention attention-${reasons[0]!.kind}`}
                      >{attentionBadgeLabels[reasons[0]!.kind]}</span
                    >
                  {/if}
                  {#if isEpic(issue)}
                    <span class="card-epic">Epic</span>
                  {/if}
                </div>
                <!-- Compact cards hide the project, so the description always names both. -->
                <span class="kit-sr-only" id={`board-card-identity-${issue.uid}`}
                  >{issue.qualified_id}{showProject ? `, project ${issue.project_name}` : ''}</span
                >
                {#if issue.parent}
                  <span class="kit-sr-only" id={relationID}
                    >Subtask of {parent?.title ?? issue.parent.short_id}</span
                  >
                {/if}
                {#if !compact}
                  <p class="card-title" id={titleID}>{issue.title}</p>
                {/if}
                {#if reasons.length > 0}
                  <!-- Compact cards show only the badge; screen readers still hear why. -->
                  <p
                    class={compact ? 'kit-sr-only' : 'card-reason'}
                    id={`board-card-reason-${issue.uid}`}
                  >
                    {reasons.map((reason) => reason.text).join(' · ')}
                  </p>
                {/if}
                {#if !compact}
                  <div class="card-meta" id={metaID}>
                    {#if issue.status === 'open' && markers.waitingOn.length > 0}
                      <span class="meta waiting" title="Not ready until these close">
                        <HourglassIcon size={11} strokeWidth={2} aria-hidden="true" />
                        Waiting on {markers.waitingOn.join(', ')}
                      </span>
                    {/if}
                    {#if issue.status === 'open' && markers.parked}
                      <span class="meta parked" title="Parked: kata next skips it">
                        <CirclePauseIcon size={11} strokeWidth={2} aria-hidden="true" />
                        {markers.parked.kind === 'someday'
                          ? 'Someday'
                          : `Starts ${shortDate(markers.parked.date)}`}
                      </span>
                    {/if}
                    {#if issue.child_counts && issue.child_counts.total > 0}
                      {@const done = issue.child_counts.total - issue.child_counts.open}
                      <span class="meta" title="Subtasks done">
                        <ListTreeIcon size={11} strokeWidth={2} aria-hidden="true" />
                        <span class="kit-sr-only">Subtasks done</span>
                        {done}/{issue.child_counts.total}
                      </span>
                    {/if}
                    {#if deadline && issue.status === 'open'}
                      <span class="meta" class:overdue={deadline < today} title="Deadline">
                        <CalendarIcon size={11} strokeWidth={2} aria-hidden="true" />
                        <span class="kit-sr-only">Due</span>
                        {shortDate(deadline)}
                      </span>
                    {/if}
                    {#each extraLabels as label (label)}
                      <Chip size="xs" tone="neutral">{label}</Chip>
                    {/each}
                    {#if issue.owner}
                      <span class="meta owner"
                        ><span class="kit-sr-only">Owner</span>{issue.owner}</span
                      >
                    {/if}
                    {#if issue.comment_count}
                      <span
                        class="comment-pill"
                        class:alone={!issue.owner}
                        title={commentLabel(issue.comment_count)}
                      >
                        <MessageSquareIcon size={12} strokeWidth={2} aria-hidden="true" />
                        <span aria-hidden="true">{issue.comment_count}</span>
                        <span class="kit-sr-only">{commentLabel(issue.comment_count)}</span>
                      </span>
                    {/if}
                  </div>
                {/if}
                {#if canMove}
                  <button
                    type="button"
                    class="card-menu"
                    aria-label={`Actions for ${issue.title}`}
                    aria-haspopup="menu"
                    title="Move or ask…"
                    disabled={busy || pending.has(issue.uid)}
                    onclick={(event) => openMenu(event, issue)}
                  >
                    <EllipsisIcon size={14} strokeWidth={2} aria-hidden="true" />
                  </button>
                {/if}
              </li>
            {/each}
          </ul>
        {/if}
        {#if !collapsed && onAddCard && column.id === 'open'}
          {#if addingTo === column.id}
            <div class="add-card">
              <textarea
                class="add-card-input"
                aria-label={`New card in ${column.title}`}
                placeholder="Card title — Enter to add, Esc to close"
                rows="2"
                bind:value={addDraft}
                disabled={addSaving}
                onkeydown={(event) => handleAddKeydown(event, column)}
              ></textarea>
              <div class="add-card-actions">
                <Button
                  size="sm"
                  tone="info"
                  surface="solid"
                  label={addSaving ? 'Adding…' : 'Add'}
                  disabled={addSaving || busy || !addDraft.trim()}
                  onclick={() => void submitAddCard(column)}
                />
                <Button size="sm" label="Cancel" onclick={() => closeAddCard(column)} />
              </div>
            </div>
          {:else}
            <button
              type="button"
              class="column-add"
              data-add-card={column.id}
              aria-label={`Add card to ${column.title}`}
              onclick={() => openAddCard(column)}
            >
              <PlusIcon size={13} strokeWidth={2} aria-hidden="true" />
              <span>Add card</span>
            </button>
          {/if}
        {/if}
        {#if !collapsed && column.id === 'done' && serverPaged && column.issues.length >= maxDoneLimit && column.total > column.issues.length}
          <!-- The daemon pages at most this many closed issues; Logbook lists the rest. -->
          <p class="column-more-note">
            Showing the newest {maxDoneLimit.toLocaleString('en-US')}.
            {#if onOpenLogbook}
              <button type="button" class="column-more" onclick={() => onOpenLogbook()}
                >Open Logbook</button
              >
            {/if}
          </p>
        {:else if !collapsed && column.id === 'done' && column.total > column.issues.length}
          {@const more = Math.min(defaultDoneLimit, column.total - column.issues.length)}
          <button
            type="button"
            class="column-more"
            disabled={serverPaged && loading}
            onclick={() => {
              if (serverPaged) onShowMoreDone?.()
              else doneLimit += defaultDoneLimit
            }}>Show {more} more</button
          >
        {/if}
      </section>
    {/each}
  </div>
  <p class="kit-sr-only" role="status" aria-live="polite">{announcement}</p>
  <span class="kit-sr-only" id="board-handled-hint"
    >Moving it out of Needs attention marks it handled.</span
  >
</section>

<FlashBanner top="72px" />

<Menu bind:open={menuOpen} anchor={menuAnchor} returnFocus={menuReturn} align="end">
  <MenuContent ariaLabel="Card actions">
    {#if menuIssue}
      {@const issue = menuIssue}
      {#each moveTargets(issue, { viewer }) as target (target)}
        <MenuItem onselect={() => void moveTo(issue, target)}
          >{planBoardMove(issue, target, { viewer })?.clearAttention
            ? `Mark handled and move to ${columnTitle(target)}`
            : `Move to ${columnTitle(target)}`}</MenuItem
        >
      {/each}
      {#if canAsk(issue)}
        <MenuItem onselect={() => openAsk(issue)}>Ask {askRecipient(issue)}…</MenuItem>
      {/if}
    {/if}
  </MenuContent>
</Menu>

<Modal
  open={asking !== null}
  title={asking ? `Ask ${asking.to}` : 'Ask'}
  width={440}
  onClose={() => {
    if (!askSaving) asking = null
  }}
>
  <div class="ask-dialog">
    <p class="ask-context">{asking?.issue.title ?? ''}</p>
    <label class="ask-field">
      <span>What do you need?</span>
      <input
        bind:value={askMessage}
        disabled={askSaving}
        maxlength="1024"
        placeholder="e.g. Which of the two designs should ship?"
        onkeydown={(event) => {
          if (event.key === 'Enter') {
            event.preventDefault()
            void submitAsk()
          }
        }}
      />
    </label>
    <p class="ask-hint">
      Sent as a <code>kata notify</code> request; it shows in their inbox until they clear it. A
      task holds one request per person, so this replaces any earlier request to {asking?.to} on this
      task.
    </p>
  </div>
  {#snippet footer()}
    <Button
      size="sm"
      label="Cancel"
      disabled={askSaving}
      onclick={() => {
        asking = null
      }}
    />
    <Button
      size="sm"
      tone="info"
      surface="solid"
      label={askSaving ? 'Sending…' : 'Send'}
      ariaLabel="Send request"
      disabled={askSaving || busy || !askMessage.trim()}
      onclick={() => void submitAsk()}
    />
  {/snippet}
</Modal>

<CompleteIssueDialog
  open={completing !== null}
  title={completing?.title ?? ''}
  qualifiedId={completing?.qualified_id ?? ''}
  onComplete={complete}
  onCancel={() => {
    completing = null
  }}
/>

<style>
  .board {
    min-width: 0;
    min-height: 0;
    flex: 1;
    display: flex;
    flex-direction: column;
    background: var(--bg-primary);
  }

  .board-note {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    margin: 0;
    padding: var(--space-2) var(--space-5) 0;
    color: var(--text-muted);
    font-size: var(--font-size-xs);
  }

  .board-columns {
    min-height: 0;
    flex: 1;
    display: flex;
    gap: var(--space-4);
    padding: var(--space-3) var(--space-5) var(--space-5);
    overflow-x: auto;
    overscroll-behavior-x: contain;
    scroll-snap-type: x proximity;
  }

  .board-column {
    /* Fixed-width columns that share any spare width, up to a readable max. */
    flex: 1 0 264px;
    max-width: 380px;
    min-height: 0;
    display: flex;
    flex-direction: column;
    border: 1px solid var(--border-muted);
    border-radius: var(--radius-md);
    background: var(--bg-inset);
    scroll-snap-align: start;
    transition:
      border-color 120ms ease,
      background-color 120ms ease;
  }

  .board-column.drop-target {
    border-color: var(--column-color);
    background: color-mix(in srgb, var(--column-color) 8%, var(--bg-inset));
  }

  .column-header {
    display: flex;
    align-items: center;
    gap: var(--space-3);
    /* Every header is as tall as one with a collapse button, so titles line up. */
    min-height: calc(22px + 2 * var(--space-3));
    padding: var(--space-3) var(--space-4);
  }

  .column-toggle {
    display: inline-flex;
    align-items: center;
    justify-content: center;
    width: 22px;
    height: 22px;
    flex: 0 0 auto;
    padding: 0;
    border: 0;
    border-radius: var(--radius-sm);
    background: transparent;
    color: var(--text-muted);
    cursor: pointer;
  }

  .column-toggle:hover {
    background: var(--bg-surface-hover);
    color: var(--text-primary);
  }

  .column-toggle:focus-visible {
    outline: var(--focus-ring);
    outline-offset: 1px;
  }

  /* A collapsed column is a narrow strip: toggle, count, and a vertical title. */
  .board-column.collapsed {
    flex: 0 0 44px;
    max-width: 44px;
    scroll-snap-align: none;
  }

  .column-header--collapsed {
    flex-direction: column;
    gap: var(--space-3);
    padding: var(--space-3) 0;
  }

  .column-header--collapsed .column-count {
    margin-inline-start: 0;
  }

  .column-title--vertical {
    max-height: 240px;
    writing-mode: vertical-rl;
  }

  .column-dot {
    width: 8px;
    height: 8px;
    flex: 0 0 auto;
    border-radius: var(--radius-dot);
    background: var(--column-color);
  }

  .column-title {
    min-width: 0;
    margin: 0;
    overflow: hidden;
    color: var(--text-primary);
    font-size: var(--font-size-sm);
    font-weight: var(--font-weight-semibold);
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .column-count {
    margin-inline-start: auto;
    color: var(--text-muted);
    font-size: var(--font-size-xs);
    font-variant-numeric: tabular-nums;
  }

  .column-empty {
    margin: 0 var(--space-3) var(--space-3);
    padding: var(--space-5) var(--space-3);
    border: 1px dashed var(--border-default);
    border-radius: var(--radius-sm);
    color: var(--text-muted);
    font-size: var(--font-size-xs);
    text-align: center;
  }

  .column-cards {
    min-height: 0;
    flex: 1;
    display: flex;
    flex-direction: column;
    gap: var(--space-3);
    margin: 0;
    padding: 0 var(--space-3) var(--space-3);
    overflow-y: auto;
    list-style: none;
  }

  .card {
    position: relative;
    display: grid;
    gap: var(--space-2);
    padding: var(--space-3) var(--space-4);
    border: 1px solid var(--border-default);
    border-radius: var(--radius-sm);
    background: var(--bg-surface);
    box-shadow: var(--shadow-sm);
    cursor: pointer;
    transition:
      border-color 120ms ease,
      box-shadow 120ms ease,
      opacity 120ms ease;
  }

  /* Subtasks sit under their parent, indented one step per level. */
  .card.child {
    margin-inline-start: calc(var(--depth) * var(--space-5));
    border-inline-start: 3px solid var(--border-default);
    border-start-start-radius: 0;
    border-end-start-radius: 0;
  }

  .card[draggable='true'] {
    cursor: grab;
  }

  .card:hover {
    border-color: var(--text-muted);
  }

  .card.selected {
    border-color: var(--accent-blue);
    box-shadow: 0 0 0 1px var(--accent-blue);
  }

  .card.dragging {
    opacity: 0.4;
  }

  .card.pending {
    opacity: 0.65;
  }

  .card.closed .card-title {
    color: var(--text-secondary);
  }

  /* The card's whole surface is one button; its text sits above it, and
     pointer events fall through to the button so the card stays one target. */
  .card-open {
    position: absolute;
    inset: 0;
    width: 100%;
    margin: 0;
    padding: 0;
    border: 0;
    border-radius: inherit;
    background: transparent;
    cursor: inherit;
  }

  .card-open:focus-visible {
    outline: var(--focus-ring);
    outline-offset: 1px;
  }

  .card-top,
  .card-title,
  .card-meta {
    position: relative;
    pointer-events: none;
  }

  .card-top {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    min-width: 0;
    padding-inline-end: 22px;
  }

  .card-id {
    color: var(--text-muted);
    font-family: var(--font-mono);
    font-size: var(--font-size-2xs);
  }

  /* Why a card is in Needs attention: a badge in the top row and, on
     comfortable cards, the reason under the title. */
  .card-attention {
    padding: 0 6px;
    border-radius: 999px;
    background: color-mix(in srgb, var(--accent-amber) 16%, transparent);
    color: color-mix(in srgb, var(--accent-amber) 70%, var(--text-primary));
    font-size: var(--font-size-2xs);
    font-weight: var(--font-weight-semibold);
    line-height: 16px;
    white-space: nowrap;
  }

  .card-attention.attention-stuck,
  .card-attention.attention-request {
    background: color-mix(in srgb, var(--accent-blue) 14%, transparent);
    color: color-mix(in srgb, var(--accent-blue) 75%, var(--text-primary));
  }

  .card-attention.attention-needs-review {
    background: color-mix(in srgb, var(--accent-purple) 14%, transparent);
    color: color-mix(in srgb, var(--accent-purple) 70%, var(--text-primary));
  }

  .card-top .card-attention {
    margin-inline-start: auto;
  }

  .card-attention + .card-epic {
    margin-inline-start: 0;
  }

  .card-reason {
    position: relative;
    display: -webkit-box;
    margin: 0;
    overflow: hidden;
    color: var(--text-secondary);
    font-size: var(--font-size-xs);
    line-height: 1.35;
    pointer-events: none;
    -webkit-box-orient: vertical;
    -webkit-line-clamp: 2;
    line-clamp: 2;
  }

  .card-epic {
    margin-inline-start: auto;
    padding: 0 6px;
    border-radius: 999px;
    background: color-mix(in srgb, var(--accent-purple) 14%, transparent);
    color: color-mix(in srgb, var(--accent-purple) 70%, var(--text-primary));
    font-size: var(--font-size-2xs);
    font-weight: var(--font-weight-semibold);
    letter-spacing: 0.02em;
    line-height: 16px;
    text-transform: uppercase;
  }

  .card-parent {
    display: inline-flex;
    align-items: center;
    gap: var(--space-1);
    color: var(--text-muted);
    font-family: var(--font-mono);
    font-size: var(--font-size-2xs);
  }

  .card-project {
    min-width: 0;
    overflow: hidden;
    color: var(--text-muted);
    font-size: var(--font-size-2xs);
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .card-priority {
    padding: 0 5px;
    border-radius: var(--radius-sm);
    background: color-mix(in srgb, var(--text-muted) 14%, transparent);
    color: var(--text-secondary);
    font-size: var(--font-size-2xs);
    font-weight: var(--font-weight-semibold);
    line-height: 16px;
  }

  .priority-0 {
    background: color-mix(in srgb, var(--accent-red) 14%, transparent);
    color: color-mix(in srgb, var(--accent-red) 80%, var(--text-primary));
  }

  .priority-1 {
    background: color-mix(in srgb, var(--accent-amber) 14%, transparent);
    color: color-mix(in srgb, var(--accent-amber) 70%, var(--text-primary));
  }

  /* Compact cards: one line with ID, priority, and the title. */
  .card.compact {
    gap: 0;
    padding-block: var(--space-2);
  }

  .card-title.card-title--inline {
    display: block;
    min-width: 0;
    flex: 1;
    overflow: hidden;
    color: var(--text-primary);
    font-size: var(--font-size-sm);
    font-weight: var(--font-weight-medium);
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .card.compact .card-epic {
    margin-inline-start: 0;
  }

  .card-title {
    display: -webkit-box;
    margin: 0;
    overflow: hidden;
    color: var(--text-primary);
    font-size: var(--font-size-sm);
    font-weight: var(--font-weight-medium);
    line-height: 1.35;
    overflow-wrap: anywhere;
    -webkit-box-orient: vertical;
    -webkit-line-clamp: 3;
    line-clamp: 3;
  }

  .card-meta {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: var(--space-2);
  }

  .card-meta:empty {
    display: none;
  }

  .meta {
    display: inline-flex;
    align-items: center;
    gap: var(--space-1);
    color: var(--text-muted);
    font-size: var(--font-size-2xs);
    font-variant-numeric: tabular-nums;
  }

  /* Not ready yet, but nobody's fault: readable, not alarming. */
  .meta.waiting,
  .meta.parked {
    color: var(--text-secondary);
  }

  .meta.overdue {
    color: var(--accent-red);
  }

  /* Comment count: quiet secondary ink at the right of the detail row, shown
     only when an issue has comments, so it never competes with attention. */
  .comment-pill {
    display: inline-flex;
    align-items: center;
    gap: var(--space-1);
    color: var(--text-secondary);
    font-size: var(--font-size-xs);
    font-variant-numeric: tabular-nums;
    font-weight: var(--font-weight-medium);
    line-height: 18px;
  }

  .comment-pill.alone {
    margin-inline-start: auto;
  }

  .meta.owner {
    margin-inline-start: auto;
    color: var(--text-secondary);
  }

  .card-menu {
    position: absolute;
    top: 6px;
    right: 6px;
    display: inline-flex;
    align-items: center;
    justify-content: center;
    width: 22px;
    height: 22px;
    padding: 0;
    border: 0;
    border-radius: var(--radius-sm);
    background: transparent;
    color: var(--text-muted);
    cursor: pointer;
    opacity: 0;
    transition: opacity 120ms ease;
  }

  .card:hover .card-menu,
  .card:focus-within .card-menu,
  .card-menu:focus-visible {
    opacity: 1;
  }

  .card-menu:hover {
    background: var(--bg-surface-hover);
    color: var(--text-primary);
  }

  @media (hover: none) {
    .card-menu {
      opacity: 1;
    }
  }

  /* Touch: a 44×44 target for the card's move menu. */
  @media (max-width: 640px), (pointer: coarse) {
    .card-menu {
      top: 0;
      right: 0;
      width: 44px;
      height: 44px;
    }

    .card-top {
      padding-inline-end: 36px;
    }

    .card.compact {
      min-height: 44px;
    }
  }

  /* A card that just moved flashes once so the eye can find where it landed. */
  .card.just-moved {
    animation: card-landed 700ms cubic-bezier(0.25, 0.1, 0.25, 1);
  }

  /* Someone else moved or added this card: a longer, purple-tinted flash. */
  .card.remote-change {
    animation: card-remote 1600ms cubic-bezier(0.25, 0.1, 0.25, 1);
  }

  @keyframes card-remote {
    0%,
    35% {
      background: color-mix(in srgb, var(--accent-purple) 18%, var(--bg-surface));
      border-color: color-mix(in srgb, var(--accent-purple) 55%, var(--border-default));
    }
  }

  @keyframes card-landed {
    from {
      background: color-mix(in srgb, var(--accent-blue) 22%, var(--bg-surface));
    }
  }

  .ask-dialog {
    display: grid;
    gap: var(--space-4);
  }

  .ask-context {
    margin: 0;
    color: var(--text-primary);
    font-weight: var(--font-weight-semibold);
  }

  .ask-field {
    display: grid;
    gap: var(--space-2);
    color: var(--text-muted);
    font-size: var(--font-size-xs);
  }

  .ask-hint {
    margin: 0;
    color: var(--text-muted);
    font-size: var(--font-size-xs);
  }

  .ask-field input {
    height: 32px;
    padding: 0 9px;
    border: 1px solid var(--border-default);
    border-radius: var(--radius-sm);
    background: var(--bg-primary);
    color: var(--text-primary);
    font: inherit;
    font-size: var(--font-size-sm);
  }

  .ask-field input:focus {
    outline: var(--focus-ring);
    outline-offset: 1px;
  }

  .column-add {
    display: inline-flex;
    align-items: center;
    gap: var(--space-2);
    margin: 0 var(--space-3) var(--space-3);
    padding: var(--space-2) var(--space-3);
    border: 0;
    border-radius: var(--radius-sm);
    background: transparent;
    color: var(--text-muted);
    font: inherit;
    font-size: var(--font-size-xs);
    cursor: pointer;
  }

  .column-add:hover,
  .column-add:focus-visible {
    background: var(--bg-surface-hover);
    color: var(--text-primary);
  }

  .column-add:focus-visible,
  .add-card-input:focus {
    outline: var(--focus-ring);
    outline-offset: 1px;
  }

  .add-card {
    display: grid;
    gap: var(--space-2);
    margin: 0 var(--space-3) var(--space-3);
  }

  .add-card-input {
    width: 100%;
    min-height: 52px;
    padding: var(--space-2) var(--space-3);
    border: 1px solid var(--border-default);
    border-radius: var(--radius-sm);
    background: var(--bg-surface);
    color: var(--text-primary);
    font: inherit;
    font-size: var(--font-size-sm);
    line-height: 1.35;
    resize: vertical;
  }

  .add-card-actions {
    display: flex;
    gap: var(--space-2);
  }

  @media (max-width: 640px), (pointer: coarse) {
    .column-add,
    .add-card-actions :global(.kit-button) {
      min-height: 44px;
    }
  }

  .column-more {
    margin: 0 var(--space-3) var(--space-3);
    padding: var(--space-2);
    border: 0;
    border-radius: var(--radius-sm);
    background: transparent;
    /* The column's inset background needs a darker blue than the accent for 4.5:1. */
    color: color-mix(in srgb, var(--accent-blue) 80%, var(--text-primary));
    font: inherit;
    font-size: var(--font-size-xs);
    cursor: pointer;
  }

  .column-more-note {
    margin: 0 var(--space-3) var(--space-3);
    color: var(--text-secondary);
    font-size: var(--font-size-xs);
    text-align: center;
  }

  .column-more-note .column-more {
    margin: 0;
  }

  .column-more:hover {
    background: var(--bg-surface-hover);
  }

  @media (prefers-reduced-motion: reduce) {
    .card.just-moved {
      animation: none;
      background: color-mix(in srgb, var(--accent-blue) 12%, var(--bg-surface));
    }

    .card.remote-change {
      animation: none;
      background: color-mix(in srgb, var(--accent-purple) 10%, var(--bg-surface));
    }

    .board-column,
    .card,
    .card-menu {
      transition: none;
    }
  }

  .column-strip {
    display: none;
  }

  @media (max-width: 640px) {
    .column-strip {
      display: flex;
      gap: var(--space-2);
      padding: var(--space-3) var(--space-5) 0;
      overflow-x: auto;
      scroll-padding-inline: var(--space-5);
      scrollbar-width: none;
    }

    .strip-item {
      display: inline-flex;
      flex: 0 0 auto;
      align-items: center;
      gap: var(--space-3);
      min-height: 36px;
      padding: 0 var(--space-5);
      border: 1px solid transparent;
      border-radius: var(--radius-md);
      background: transparent;
      color: var(--text-secondary);
      font: inherit;
      font-size: var(--font-size-sm);
      white-space: nowrap;
      cursor: pointer;
    }

    .strip-item[aria-current='true'] {
      border-color: var(--border-default);
      background: var(--bg-surface);
      color: var(--text-primary);
      font-weight: var(--font-weight-medium);
    }

    .strip-item:focus-visible {
      outline: var(--focus-ring);
      outline-offset: 1px;
    }

    .strip-count {
      color: var(--text-muted);
      font-variant-numeric: tabular-nums;
    }

    /* Columns snap to the same inset as the page title. */
    .board-columns {
      padding-inline: var(--space-5);
      scroll-padding-inline: var(--space-5);
      scroll-snap-type: x mandatory;
    }

    /* One column fills the screen, leaving the next one peeking in by 32px. */
    .board-column {
      flex: 0 0 calc(100vw - var(--space-5) - var(--space-4) - var(--space-8));
      max-width: none;
    }
  }
</style>
