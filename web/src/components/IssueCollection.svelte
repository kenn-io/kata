<script lang="ts">
  /* eslint-disable svelte/prefer-svelte-reactivity */
  import { onDestroy, tick } from 'svelte'
  import ChevronDownIcon from '@lucide/svelte/icons/chevron-down'
  import ChevronRightIcon from '@lucide/svelte/icons/chevron-right'
  import ChevronUpIcon from '@lucide/svelte/icons/chevron-up'
  import ListChevronsDownUpIcon from '@lucide/svelte/icons/list-chevrons-down-up'
  import ListChevronsUpDownIcon from '@lucide/svelte/icons/list-chevrons-up-down'
  import NetworkIcon from '@lucide/svelte/icons/network'
  import { relativeTime, shortDate } from '../lib/kata/dates'
  import { kataTaskStatusMatchesFilter } from '../lib/kata/filters'
  import type { KataTaskSearchFilters, KataTaskSummary } from '../lib/kata/types'
  import type { KataCurrentView } from '../lib/kata/authority'
  import {
    DEFAULT_KATA_TASK_SORT,
    sortKataTasks,
    toggleKataTaskSort,
    type KataTaskSort,
    type KataTaskSortKey,
  } from '../lib/kata/sort'
  import ColumnPicker from './ColumnPicker.svelte'
  import {
    KATA_OPTIONAL_TASK_COLUMNS,
    defaultKataTaskColumnVisibility,
    loadKataTaskColumnVisibility,
    persistKataTaskColumnVisibility,
    type KataOptionalTaskColumn,
    type KataTaskColumnVisibility,
  } from '../lib/kata/columns'

  export interface KataIssueRevealRequest {
    uid: string
    chain: readonly KataTaskSummary[]
    generation: number
    restoreFocus?: boolean
  }

  interface Props {
    currentView: KataCurrentView
    issueCatalog: readonly KataTaskSummary[]
    scopeLabel?: string
    scopedProjectName?: string | null
    selectedIssueUID?: string | null
    loading?: boolean
    statusFilter?: KataTaskSearchFilters['status']
    readyIssueUIDs?: ReadonlySet<string>
    resetGeneration?: number
    navigationGeneration?: number
    revealRequest?: KataIssueRevealRequest | null
    onSelect: (issue: KataTaskSummary) => void
    onOpenGraph?: ((issue: KataTaskSummary) => void) | undefined
  }

  const EMPTY_READY_ISSUE_UIDS: ReadonlySet<string> = new Set()

  let {
    currentView,
    issueCatalog,
    scopeLabel = undefined,
    scopedProjectName = null,
    selectedIssueUID = null,
    loading = false,
    statusFilter = 'all',
    readyIssueUIDs = EMPTY_READY_ISSUE_UIDS,
    resetGeneration = 0,
    navigationGeneration = 0,
    revealRequest = null,
    onSelect,
    onOpenGraph = undefined,
  }: Props = $props()

  const SORT_STORAGE_KEY = 'kata:issue-sort/v1'
  const restoredColumnVisibility = loadKataTaskColumnVisibility()
  const restoredSort = loadSort()
  const initialSort = sortForColumnVisibility(restoredSort, restoredColumnVisibility)
  let sort: KataTaskSort = $state(initialSort)
  let columnVisibility = $state(restoredColumnVisibility)
  if (initialSort !== restoredSort) persistSort(initialSort)

  type TaskGridLayout = 'wide' | 'medium' | 'compact' | 'narrow'

  const TASK_COLUMN_TRACKS: Record<
    TaskGridLayout,
    Record<KataOptionalTaskColumn, string | null>
  > = {
    wide: {
      updated: 'minmax(64px, 80px)',
      priority: 'minmax(68px, 80px)',
      due: 'minmax(56px, 70px)',
      owner: 'minmax(72px, 110px)',
      tags: 'minmax(96px, 200px)',
    },
    medium: {
      updated: 'minmax(64px, 80px)',
      priority: 'minmax(68px, 80px)',
      due: 'minmax(56px, 70px)',
      owner: 'minmax(72px, 110px)',
      tags: null,
    },
    compact: {
      updated: 'minmax(60px, 76px)',
      priority: 'minmax(64px, 78px)',
      due: 'minmax(54px, 68px)',
      owner: null,
      tags: null,
    },
    narrow: {
      updated: 'minmax(58px, 72px)',
      priority: 'minmax(62px, 74px)',
      due: null,
      owner: null,
      tags: null,
    },
  }

  const TASK_TITLE_TRACKS: Record<TaskGridLayout, string> = {
    wide: 'minmax(220px, 1fr)',
    medium: 'minmax(220px, 1fr)',
    compact: 'minmax(180px, 1fr)',
    narrow: 'minmax(140px, 1fr)',
  }

  function taskGridColumns(layout: TaskGridLayout): string {
    return [
      'var(--table-id-col)',
      TASK_TITLE_TRACKS[layout],
      ...KATA_OPTIONAL_TASK_COLUMNS.flatMap((column) => {
        const track = taskColumnTrack(layout, column.id)
        return columnVisibility[column.id] && track ? [track] : []
      }),
    ].join(' ')
  }

  function taskColumnTrack(layout: TaskGridLayout, column: KataOptionalTaskColumn): string | null {
    const track = TASK_COLUMN_TRACKS[layout][column]
    if (track || column !== 'owner' || sort.key !== 'owner') return track
    return TASK_COLUMN_TRACKS.medium.owner
  }

  let wideGridColumns = $derived(taskGridColumns('wide'))
  let mediumGridColumns = $derived(taskGridColumns('medium'))
  let compactGridColumns = $derived(taskGridColumns('compact'))
  let narrowGridColumns = $derived(taskGridColumns('narrow'))

  let expanded: Record<string, boolean> = $state({})
  let tableBody: HTMLDivElement | null = $state(null)
  let lastResetGeneration = $state<number | null>(null)
  let temporaryRevealChain = $state<readonly KataTaskSummary[]>([])
  let revealOwnedExpansionUIDs = $state<ReadonlySet<string>>(new Set())
  let lastRevealGeneration: number | null = null
  let anchorMeasurementGeneration = 0
  let catalogByUID = $derived(new Map(issueCatalog.map((issue) => [issue.uid, issue])))
  let childrenByParentUID = $derived.by(() => {
    const byHierarchyKey = new Map(issueCatalog.map((issue) => [issueHierarchyKey(issue), issue]))
    const children = new Map<string, KataTaskSummary[]>()
    for (const issue of issueCatalog) {
      const parentUID =
        issue.parent?.uid ??
        (issue.parent_short_id
          ? byHierarchyKey.get(`${issue.project_uid}:${issue.parent_short_id}`)?.uid
          : undefined)
      if (!parentUID) continue
      children.set(parentUID, [...(children.get(parentUID) ?? []), issue])
    }
    return children
  })

  // When the user is scoped to a single project, server-side groupings
  // like "Today / This Evening" feel like noise — they're a kata
  // today-bucket detail that doesn't carry inside a project view.
  // Collapse to a flat list and let the sort drive the order.
  let isProjectScoped = $derived(Boolean(scopedProjectName))
  let statusVisibleGroups = $derived(filterGroupsByStatus(currentView.groups, statusFilter))
  let flatIssues = $derived(
    isProjectScoped ? topLevelIssues(statusVisibleGroups.flatMap((group) => group.issues)) : [],
  )

  // For the Today view, the kata daemon hands us a "This evening"
  // sub-bucket alongside "Today". That bucket is a daemon-side
  // concept that confuses users coming from Things-style apps, so we
  // merge it into the Today group on the way to the renderer. The
  // raw group data is untouched — purely a visual collapse, and the
  // merge only runs on the Today view so a future daemon-provided
  // "evening" bucket in any other view passes through unchanged.
  let visibleGroups = $derived.by(() => {
    if (isProjectScoped) return []
    const groups = statusVisibleGroups.map((group) => ({ ...group, issues: [...group.issues] }))
    if (currentView.name === 'today') {
      const todayIdx = groups.findIndex((group) => group.id === 'today')
      const eveningIdx = groups.findIndex((group) => group.id === 'evening')
      if (eveningIdx >= 0) {
        if (todayIdx >= 0) {
          groups[todayIdx]!.issues.push(...groups[eveningIdx]!.issues)
          groups.splice(eveningIdx, 1)
        } else {
          groups[eveningIdx] = { ...groups[eveningIdx]!, id: 'today', title: 'Today' }
        }
      }
    }
    const allIssues = groups.flatMap((group) => group.issues)
    return groups
      .map((group) => ({ ...group, issues: topLevelIssues(group.issues, allIssues) }))
      .filter((group) => group.issues.length > 0)
  })

  // Sorting by "updated" collapses multi-group views (e.g. Today's
  // Overdue/Today buckets) into one global list so the order isn't reset
  // per group. A single-group view (like Inbox) has nothing to collapse,
  // so keep its labeled region instead of dropping it to a bare list.
  let shouldFlatten = $derived(
    !isProjectScoped && sort.key === 'updated' && visibleGroups.length > 1,
  )
  let globalSortedIssues = $derived(
    shouldFlatten
      ? sortKataTasks(
          visibleGroups.flatMap((group) => group.issues),
          sort,
        )
      : [],
  )
  let ordinaryRootIssues = $derived.by(() => {
    if (isProjectScoped) return sortKataTasks(flatIssues, sort)
    if (shouldFlatten) return globalSortedIssues
    return visibleGroups.flatMap((group) => sortKataTasks(group.issues, sort))
  })
  let visibleTemporaryRevealChain = $derived(revealChainForStatus(temporaryRevealChain))
  let hasTemporaryReveal = $derived(visibleTemporaryRevealChain.length > 0)
  let visibleRootIssues = $derived.by(() => {
    const chainUIDs = new Set(visibleTemporaryRevealChain.map((issue) => issue.uid))
    const ordinary = ordinaryRootIssues.filter(
      (issue) => !chainUIDs.has(issue.uid) || issue.uid === visibleTemporaryRevealChain[0]?.uid,
    )
    const root = visibleTemporaryRevealChain[0]
    return root && !ordinary.some((issue) => issue.uid === root.uid)
      ? [root, ...ordinary]
      : ordinary
  })
  let knownExpandableIssues = $derived.by(() => collectKnownExpandableIssues(visibleRootIssues))
  let hasExpandableVisibleRows = $derived(knownExpandableIssues.length > 0)
  let allKnownExpandableRowsExpanded = $derived(
    hasExpandableVisibleRows &&
      knownExpandableIssues.every((issue) => expanded[issue.uid] === true),
  )
  let hasAnyExpandedRows = $derived(Object.values(expanded).some(Boolean))

  function loadSort(): KataTaskSort {
    if (typeof window === 'undefined') return DEFAULT_KATA_TASK_SORT
    try {
      const raw = window.localStorage.getItem(SORT_STORAGE_KEY)
      if (!raw) return DEFAULT_KATA_TASK_SORT
      const parsed = JSON.parse(raw) as Partial<KataTaskSort>
      const validKeys: KataTaskSortKey[] = ['priority', 'title', 'updated', 'owner', 'id']
      if (
        parsed.key &&
        validKeys.includes(parsed.key) &&
        (parsed.direction === 'asc' || parsed.direction === 'desc')
      ) {
        return { key: parsed.key, direction: parsed.direction }
      }
    } catch {
      // Corrupt — fall back to defaults silently.
    }
    return DEFAULT_KATA_TASK_SORT
  }

  function persistSort(next: KataTaskSort) {
    if (typeof window === 'undefined') return
    try {
      window.localStorage.setItem(SORT_STORAGE_KEY, JSON.stringify(next))
    } catch {
      // Storage unavailable — best-effort.
    }
  }

  function handleSortClick(key: KataTaskSortKey) {
    sort = toggleKataTaskSort(sort, key)
    persistSort(sort)
  }

  function optionalColumnForSort(key: KataTaskSortKey): KataOptionalTaskColumn | null {
    if (key === 'updated' || key === 'priority' || key === 'owner') return key
    return null
  }

  function sortForColumnVisibility(
    current: KataTaskSort,
    visibility: KataTaskColumnVisibility,
  ): KataTaskSort {
    const activeSortColumn = optionalColumnForSort(current.key)
    return activeSortColumn && !visibility[activeSortColumn]
      ? { key: 'title', direction: 'asc' }
      : current
  }

  function setColumnVisibility(next: KataTaskColumnVisibility): void {
    const nextSort = sortForColumnVisibility(sort, next)
    if (nextSort !== sort) {
      sort = nextSort
      persistSort(sort)
    }
    columnVisibility = next
    persistKataTaskColumnVisibility(next)
  }

  function showAllColumns(): void {
    setColumnVisibility(defaultKataTaskColumnVisibility())
  }

  function viewTitle(view: KataCurrentView): string {
    if (scopeLabel) return scopeLabel
    return view.name.charAt(0).toUpperCase() + view.name.slice(1)
  }

  // Kept source-aligned with the port while the count display uses the filtered total.
  // eslint-disable-next-line @typescript-eslint/no-unused-vars
  function totalIssues(view: KataCurrentView): number {
    return view.groups.reduce((sum, group) => sum + group.issues.length, 0)
  }

  function totalFilteredIssues(): number {
    return statusVisibleGroups.reduce((sum, group) => sum + group.issues.length, 0)
  }

  function isSelected(issue: KataTaskSummary): boolean {
    return selectedIssueUID === issue.uid
  }

  function rowElement(body: HTMLDivElement, uid: string): HTMLElement | null {
    return (
      Array.from(body.querySelectorAll<HTMLElement>('button.row[data-uid]')).find(
        (row) => row.dataset.uid === uid,
      ) ?? null
    )
  }

  function revealSuccessor(issue: KataTaskSummary): KataTaskSummary | undefined {
    const index = visibleTemporaryRevealChain.findIndex((candidate) => candidate.uid === issue.uid)
    return index >= 0 ? visibleTemporaryRevealChain[index + 1] : undefined
  }

  function hasChildren(issue: KataTaskSummary): boolean {
    return revealSuccessor(issue) !== undefined || visibleChildren(issue).length > 0
  }

  function priorityLabel(priority: number | undefined): string | null {
    if (priority === undefined) return null
    return `P${priority}`
  }

  function displayId(issue: KataTaskSummary): string {
    // Inside a project the project prefix on every row is noise — show
    // just the short id. At the top level we keep the qualified form
    // so the project is identifiable at a glance.
    return isProjectScoped ? issue.short_id : issue.qualified_id
  }

  function issueHierarchyKey(issue: KataTaskSummary): string {
    return `${issue.project_uid}:${issue.short_id}`
  }

  function parentHierarchyKey(issue: KataTaskSummary): string | null {
    const parent = issue.parent?.uid ? catalogByUID.get(issue.parent.uid) : undefined
    if (parent) return issueHierarchyKey(parent)
    if (issue.parent_short_id) return `${issue.project_uid}:${issue.parent_short_id}`
    return null
  }

  function issueMatchesStatusFilter(issue: KataTaskSummary): boolean {
    return kataTaskStatusMatchesFilter(issue, statusFilter, readyIssueUIDs)
  }

  function revealChainForStatus(chain: readonly KataTaskSummary[]): readonly KataTaskSummary[] {
    if (statusFilter !== 'ready') return chain
    const target = chain[chain.length - 1]
    if (!target || !issueMatchesStatusFilter(target)) return []
    return chain.every(issueMatchesStatusFilter) ? chain : [target]
  }

  function filterGroupsByStatus(
    groups: KataCurrentView['groups'],
    status: KataTaskSearchFilters['status'],
  ): KataCurrentView['groups'] {
    return groups
      .map((group) => ({
        ...group,
        issues: group.issues.filter((issue) =>
          kataTaskStatusMatchesFilter(issue, status, readyIssueUIDs),
        ),
      }))
      .filter((group) => group.issues.length > 0)
  }

  function topLevelIssues(
    issues: readonly KataTaskSummary[],
    allIssues: readonly KataTaskSummary[] = issues,
  ): KataTaskSummary[] {
    // A child collapses into its parent only when that parent is actually
    // present in the visible set. Search and filter results often surface a
    // matching child without its parent; those rows must still render as
    // their own top-level row instead of vanishing into a missing ancestor.
    const visibleKeys = new Set(allIssues.map(issueHierarchyKey))
    return issues.filter((issue) => {
      const parentKey = parentHierarchyKey(issue)
      return parentKey === null || !visibleKeys.has(parentKey)
    })
  }

  function collectKnownExpandableIssues(issues: readonly KataTaskSummary[]): KataTaskSummary[] {
    const expandable: KataTaskSummary[] = []
    const seen = new Set<string>()

    const visit = (issue: KataTaskSummary) => {
      if (seen.has(issue.uid)) return
      seen.add(issue.uid)
      if (hasChildren(issue)) expandable.push(issue)
      if (expanded[issue.uid] !== true) return
      for (const child of visibleChildren(issue)) visit(child)
    }

    for (const issue of issues) visit(issue)
    return expandable
  }

  function expandIssueTree(
    issue: KataTaskSummary,
    nextExpanded: Record<string, boolean>,
    seen: Set<string>,
  ): void {
    if (!hasChildren(issue) || seen.has(issue.uid)) return
    seen.add(issue.uid)
    revealOwnedExpansionUIDs = new Set(
      [...revealOwnedExpansionUIDs].filter((uid) => uid !== issue.uid),
    )
    nextExpanded[issue.uid] = true
    expanded = { ...expanded, ...nextExpanded }

    for (const child of visibleChildren(issue)) expandIssueTree(child, nextExpanded, seen)
  }

  function expandAllVisible(): void {
    if (allKnownExpandableRowsExpanded) return
    const nextExpanded = { ...expanded }
    const seen = new Set<string>()
    for (const issue of visibleRootIssues) expandIssueTree(issue, nextExpanded, seen)
  }

  function collapseAllVisible() {
    if (!hasAnyExpandedRows) return
    expanded = {}
    revealOwnedExpansionUIDs = new Set()
    cancelPendingKeyboardSelect()
  }

  function toggleExpand(issue: KataTaskSummary, event: MouseEvent | KeyboardEvent): void {
    event.stopPropagation()
    const uid = issue.uid
    const currentlyExpanded = expanded[uid] === true
    revealOwnedExpansionUIDs = new Set(
      [...revealOwnedExpansionUIDs].filter((ownedUID) => ownedUID !== uid),
    )
    expanded = { ...expanded, [uid]: !currentlyExpanded }
  }

  function sortIndicator(key: KataTaskSortKey): 'asc' | 'desc' | null {
    return sort.key === key ? sort.direction : null
  }

  function sortLabel(key: KataTaskSortKey, label: string): string {
    const ind = sortIndicator(key)
    if (ind === 'asc') return `Sort by ${label}, currently ascending`
    if (ind === 'desc') return `Sort by ${label}, currently descending`
    return `Sort by ${label}`
  }

  // Keyboard navigation selects on every step, and each selection kicks
  // off a detail + events fetch upstream. Selection therefore only
  // commits once the keyboard settles: 50ms after the last navigation
  // keydown, and never while a navigation key is still physically held.
  // The held-key gate matters because OS key-repeat intervals are often
  // longer than any reasonable debounce — a timer alone would still
  // commit one fetch per repeated row. Focus itself moves instantly;
  // only the upstream notification waits for the cursor to settle.
  const KEYBOARD_SELECT_DEBOUNCE_MS = 50
  let keyboardSelectTimer: ReturnType<typeof setTimeout> | undefined
  let pendingKeyboardSelectUID: string | null = null
  // Tracked by event.code (physical key), not event.key: "G" is Shift+g,
  // so releasing Shift before g would make keydown record "G" but keyup
  // report "g", stranding the entry and blocking selection until blur.
  const heldNavKeys = new Set<string>()

  function cancelPendingKeyboardSelect() {
    pendingKeyboardSelectUID = null
    if (keyboardSelectTimer !== undefined) {
      clearTimeout(keyboardSelectTimer)
      keyboardSelectTimer = undefined
    }
  }

  onDestroy(cancelPendingKeyboardSelect)

  function clearTemporaryReveal() {
    if (revealOwnedExpansionUIDs.size > 0) {
      expanded = Object.fromEntries(
        Object.entries(expanded).filter(([uid]) => !revealOwnedExpansionUIDs.has(uid)),
      )
      revealOwnedExpansionUIDs = new Set()
    }
    if (temporaryRevealChain.length > 0) temporaryRevealChain = []
  }

  function selectNow(issue: KataTaskSummary) {
    clearTemporaryReveal()
    cancelPendingKeyboardSelect()
    onSelect(issue)
  }

  function openGraph(issue: KataTaskSummary, event: MouseEvent | KeyboardEvent) {
    event.preventDefault()
    event.stopPropagation()
    onOpenGraph?.(issue)
  }

  function restartKeyboardSelectTimer() {
    if (keyboardSelectTimer !== undefined) clearTimeout(keyboardSelectTimer)
    keyboardSelectTimer = setTimeout(() => {
      keyboardSelectTimer = undefined
      // A navigation key is still held: stay pending. The matching keyup
      // restarts the timer, so even a slow OS key-repeat never commits
      // intermediate rows mid-hold.
      if (heldNavKeys.size > 0) return
      commitKeyboardSelect()
    }, KEYBOARD_SELECT_DEBOUNCE_MS)
  }

  function commitKeyboardSelect() {
    const uid = pendingKeyboardSelectUID
    pendingKeyboardSelectUID = null
    if (!uid) return
    // Re-resolve at commit time: a live refresh inside the settle window
    // can drop the row, and selecting a vanished issue would surface an
    // error for something the user can no longer see.
    const issue = findIssueByUID(uid)
    if (issue) onSelect(issue)
  }

  function focusRow(target: HTMLElement | null) {
    if (!target) return
    target.focus()
    const uid = target.dataset.uid
    if (!uid || !findIssueByUID(uid)) return
    pendingKeyboardSelectUID = uid
    restartKeyboardSelectTimer()
  }

  // Window-level so a release outside the table (focus moved mid-hold)
  // can't strand a key in the held set and block selection forever.
  function handleWindowKeyup(event: KeyboardEvent) {
    if (!heldNavKeys.delete(event.code)) return
    if (
      heldNavKeys.size === 0 &&
      pendingKeyboardSelectUID !== null &&
      keyboardSelectTimer === undefined
    ) {
      restartKeyboardSelectTimer()
    }
  }

  // Keyups are lost entirely when the window loses focus mid-hold; treat
  // blur as releasing everything so the pending selection still settles.
  function handleWindowBlur() {
    heldNavKeys.clear()
    if (pendingKeyboardSelectUID !== null && keyboardSelectTimer === undefined) {
      restartKeyboardSelectTimer()
    }
  }

  function handleListKeydown(event: KeyboardEvent) {
    const target = event.target
    if (!(target instanceof HTMLElement) || !target.classList.contains('row')) return
    if (!tableBody) return
    if (event.metaKey || event.ctrlKey || event.altKey) return

    const rows = Array.from(tableBody.querySelectorAll<HTMLElement>('button.row'))
    const idx = rows.indexOf(target)
    if (idx === -1) return

    let nextIdx: number | null = null
    switch (event.key) {
      case 'ArrowDown':
      case 'j':
        nextIdx = Math.min(rows.length - 1, idx + 1)
        break
      case 'ArrowUp':
      case 'k':
        nextIdx = Math.max(0, idx - 1)
        break
      case 'Home':
      case 'g':
        nextIdx = 0
        break
      case 'End':
      case 'G':
        nextIdx = rows.length - 1
        break
      case 'ArrowRight': {
        const uid = target.dataset.uid
        if (uid && expanded[uid] !== true) {
          const issue = findIssueByUID(uid)
          if (issue && hasChildren(issue)) {
            event.preventDefault()
            void toggleExpand(issue, event)
          }
        }
        return
      }
      case 'ArrowLeft': {
        const uid = target.dataset.uid
        if (uid && expanded[uid] === true) {
          const issue = findIssueByUID(uid)
          if (issue) {
            event.preventDefault()
            void toggleExpand(issue, event)
          }
        }
        return
      }
      default:
        return
    }
    event.preventDefault()
    heldNavKeys.add(event.code)
    // Boundary keys (j on last, k on first, Home/End at the edge) can
    // resolve to the row already focused; skip the re-focus so we
    // don't double-fire the click handler and refetch the same issue.
    if (nextIdx === idx) return
    focusRow(rows[nextIdx] ?? null)
  }

  function findIssueByUID(uid: string): KataTaskSummary | undefined {
    const temporary = temporaryRevealChain.find((issue) => issue.uid === uid)
    if (temporary) return temporary
    for (const group of statusVisibleGroups) {
      const match = group.issues.find((issue) => issue.uid === uid)
      if (match) return match
    }
    const catalogIssue = catalogByUID.get(uid)
    return catalogIssue && issueMatchesStatusFilter(catalogIssue) ? catalogIssue : undefined
  }

  function visibleChildren(issue: KataTaskSummary): KataTaskSummary[] {
    const children = childrenByParentUID.get(issue.uid) ?? []
    const successor = revealSuccessor(issue)
    const visible = children.filter(
      (child) => issueMatchesStatusFilter(child) || child.uid === successor?.uid,
    )
    if (successor && !visible.some((child) => child.uid === successor.uid)) {
      visible.push(successor)
    }
    return visible
  }

  $effect.pre(() => {
    void currentView
    void issueCatalog
    void scopedProjectName
    void statusFilter
    void readyIssueUIDs
    void resetGeneration

    const generation = ++anchorMeasurementGeneration
    const body = tableBody
    const nextSelectedIssueUID = selectedIssueUID

    if (!body || !nextSelectedIssueUID) return
    const selectedRow = rowElement(body, nextSelectedIssueUID)
    if (!selectedRow) return

    const bodyRect = body.getBoundingClientRect()
    const rowRect = selectedRow.getBoundingClientRect()
    if (rowRect.bottom <= bodyRect.top || rowRect.top >= bodyRect.bottom) return
    const selectedRowTop = rowRect.top

    void tick().then(() => {
      if (
        generation !== anchorMeasurementGeneration ||
        tableBody !== body ||
        selectedIssueUID !== nextSelectedIssueUID
      )
        return
      const refreshedSelectedRow = rowElement(body, nextSelectedIssueUID)
      if (!refreshedSelectedRow) return
      body.scrollTop += refreshedSelectedRow.getBoundingClientRect().top - selectedRowTop
    })

    return () => {
      if (anchorMeasurementGeneration === generation) anchorMeasurementGeneration += 1
    }
  })

  // Clear stale expansion before the reveal effect below so a snapshot that
  // changes structure can still restore its hidden selected ancestor chain.
  $effect(() => {
    if (lastResetGeneration === null) {
      lastResetGeneration = resetGeneration
      return
    }
    if (resetGeneration === lastResetGeneration) return
    lastResetGeneration = resetGeneration
    clearTemporaryReveal()
    expanded = {}
  })

  $effect(() => {
    if (!revealRequest) {
      clearTemporaryReveal()
      lastRevealGeneration = null
      return
    }
    if (revealRequest.generation === lastRevealGeneration) return
    clearTemporaryReveal()
    lastRevealGeneration = revealRequest.generation
    const revealChain = revealChainForStatus(revealRequest.chain)
    const restoreFocusedTarget =
      revealRequest.restoreFocus === true ||
      (typeof document !== 'undefined' &&
        document.activeElement instanceof HTMLElement &&
        document.activeElement.dataset.uid === revealRequest.uid)
    temporaryRevealChain = statusFilter === 'ready' || revealChain.length > 1 ? revealChain : []
    void (async () => {
      const request = revealRequest
      for (const issue of revealChain.slice(0, -1)) {
        if (revealRequest?.generation !== request.generation) return
        if (expanded[issue.uid] !== true) {
          expanded = { ...expanded, [issue.uid]: true }
          revealOwnedExpansionUIDs = new Set([...revealOwnedExpansionUIDs, issue.uid])
        }
      }
      await tick()
      if (revealRequest?.generation !== request.generation) return
      const targetRow = tableBody?.querySelector<HTMLElement>(
        `button.row[data-uid="${request.uid}"]`,
      )
      if (restoreFocusedTarget) targetRow?.focus({ preventScroll: true })
      targetRow?.scrollIntoView({ block: 'nearest' })
    })()
  })

  // A pending keyboard selection dies the moment the workspace starts
  // any navigation (view/scope/route/daemon change). Waiting for the
  // post-load list remount is too late: a held key released while the
  // new view's data is still in flight would commit against the old
  // view and its onSelect would supersede the in-progress navigation.
  let lastNavigationGeneration: number | null = null
  $effect(() => {
    if (lastNavigationGeneration !== null && navigationGeneration !== lastNavigationGeneration) {
      clearTemporaryReveal()
      cancelPendingKeyboardSelect()
    }
    lastNavigationGeneration = navigationGeneration
  })
</script>

<svelte:window onkeyup={handleWindowKeyup} onblur={handleWindowBlur} />

<section class="issue-list" aria-label="Issues">
  <div class="pane-header">
    <div class="heading-row">
      <div class="heading">
        <h2>{viewTitle(currentView)}</h2>
        <span class="count"
          >{totalFilteredIssues()} {totalFilteredIssues() === 1 ? 'task' : 'tasks'}</span
        >
      </div>
      <div class="header-actions">
        <ColumnPicker
          visibility={columnVisibility}
          onchange={setColumnVisibility}
          onShowAll={showAllColumns}
        />
        {#if hasExpandableVisibleRows || hasAnyExpandedRows}
          <div class="tree-actions" aria-label="Task tree controls">
            <button
              class="tree-action"
              type="button"
              aria-label="Expand all tasks"
              title="Expand all tasks"
              disabled={allKnownExpandableRowsExpanded}
              onclick={expandAllVisible}
            >
              <ListChevronsUpDownIcon size={13} strokeWidth={2} />
              <span class="action-label">Expand all</span>
            </button>
            <button
              class="tree-action"
              type="button"
              aria-label="Collapse all tasks"
              title="Collapse all tasks"
              disabled={!hasAnyExpandedRows}
              onclick={collapseAllVisible}
            >
              <ListChevronsDownUpIcon size={13} strokeWidth={2} />
              <span class="action-label">Collapse all</span>
            </button>
          </div>
        {/if}
      </div>
    </div>
    {#if loading}
      <span class="kit-sr-only" aria-live="polite">Loading snapshot</span>
    {/if}
  </div>

  <div
    class="table"
    class:table--project-scoped={isProjectScoped}
    class:table--keep-owner-sort={sort.key === 'owner' && columnVisibility.owner}
    style:--table-cols-wide={wideGridColumns}
    style:--table-cols-medium={mediumGridColumns}
    style:--table-cols-compact={compactGridColumns}
    style:--table-cols-narrow={narrowGridColumns}
  >
    <!-- The table-body is the keyboard-nav root: any row inside this
         container is reachable via ↓/↑ (j/k), Home/End (g/G), and
         ↔ to expand or collapse subtasks. -->
    <!-- svelte-ignore a11y_no_static_element_interactions -->
    <div class="table-body" bind:this={tableBody} onkeydown={handleListKeydown}>
      <div class="table-header">
        <button
          class="col col-id"
          type="button"
          aria-label={sortLabel('id', 'ID')}
          aria-pressed={sortIndicator('id') !== null}
          onclick={() => handleSortClick('id')}
        >
          <span>ID</span>
          {#if sortIndicator('id') === 'asc'}
            <ChevronUpIcon size={11} strokeWidth={2} />
          {:else if sortIndicator('id') === 'desc'}
            <ChevronDownIcon size={11} strokeWidth={2} />
          {/if}
        </button>
        <button
          class="col col-title"
          type="button"
          aria-label={sortLabel('title', 'Title')}
          aria-pressed={sortIndicator('title') !== null}
          onclick={() => handleSortClick('title')}
        >
          <!-- Spacer matches the chevron column in body rows so the
               "Title" label aligns with the title text underneath. -->
          <span class="chevron chevron--placeholder" aria-hidden="true"></span>
          <span>Title</span>
          {#if sortIndicator('title') === 'asc'}
            <ChevronUpIcon size={11} strokeWidth={2} />
          {:else if sortIndicator('title') === 'desc'}
            <ChevronDownIcon size={11} strokeWidth={2} />
          {/if}
        </button>
        {#if columnVisibility.updated}
          <button
            class="col col-updated"
            type="button"
            aria-label={sortLabel('updated', 'Updated')}
            aria-pressed={sortIndicator('updated') !== null}
            onclick={() => handleSortClick('updated')}
          >
            <span>Updated</span>
            {#if sortIndicator('updated') === 'asc'}
              <ChevronUpIcon size={11} strokeWidth={2} />
            {:else if sortIndicator('updated') === 'desc'}
              <ChevronDownIcon size={11} strokeWidth={2} />
            {/if}
          </button>
        {/if}
        {#if columnVisibility.priority}
          <button
            class="col col-priority"
            type="button"
            aria-label={sortLabel('priority', 'Priority')}
            aria-pressed={sortIndicator('priority') !== null}
            onclick={() => handleSortClick('priority')}
          >
            <span>Priority</span>
            {#if sortIndicator('priority') === 'asc'}
              <ChevronUpIcon size={11} strokeWidth={2} />
            {:else if sortIndicator('priority') === 'desc'}
              <ChevronDownIcon size={11} strokeWidth={2} />
            {/if}
          </button>
        {/if}
        {#if columnVisibility.due}<span class="col col-due col-static">Due</span>{/if}
        {#if columnVisibility.owner}
          <button
            class="col col-owner"
            type="button"
            aria-label={sortLabel('owner', 'Owner')}
            aria-pressed={sortIndicator('owner') !== null}
            onclick={() => handleSortClick('owner')}
          >
            <span>Owner</span>
            {#if sortIndicator('owner') === 'asc'}
              <ChevronUpIcon size={11} strokeWidth={2} />
            {:else if sortIndicator('owner') === 'desc'}
              <ChevronDownIcon size={11} strokeWidth={2} />
            {/if}
          </button>
        {/if}
        {#if columnVisibility.tags}<span class="col col-tags col-static">Tags</span>{/if}
      </div>

      {#if visibleRootIssues.length === 0}
        <div class="empty">No tasks</div>
      {/if}

      {#if hasTemporaryReveal}
        {#each visibleRootIssues as issue (issue.uid)}
          {@render row(issue)}
        {/each}
      {:else if isProjectScoped}
        {#each visibleRootIssues as issue (issue.uid)}
          {@render row(issue)}
        {/each}
      {:else if shouldFlatten}
        {#each globalSortedIssues as issue (issue.uid)}
          {@render row(issue)}
        {/each}
      {:else}
        {#each visibleGroups as group (group.id)}
          <section class="group" aria-labelledby={`group-${group.id}`}>
            <h3 class="group-title" id={`group-${group.id}`}>
              <span>{group.title}</span>
              <span class="group-count">{group.issues.length}</span>
            </h3>
            {#each sortKataTasks(group.issues, sort) as issue (issue.uid)}
              {@render row(issue)}
            {/each}
          </section>
        {/each}
      {/if}
    </div>
  </div>
</section>

{#snippet row(issue: KataTaskSummary, depth = 0)}
  {@const priority = priorityLabel(issue.priority)}
  {@const labels = issue.labels?.join(' · ') ?? ''}
  {@const expandable = hasChildren(issue)}
  {@const isExpanded = expanded[issue.uid] === true}
  {@const titleId = `kata-issue-title-${issue.uid}`}
  <div class="row-frame" style:--task-depth={String(depth)}>
    <button
      class="row issue-row"
      class:row--child={depth > 0}
      class:selected={isSelected(issue)}
      aria-current={isSelected(issue) ? 'true' : undefined}
      aria-expanded={expandable ? isExpanded : undefined}
      data-uid={issue.uid}
      onclick={() => selectNow(issue)}
    >
      <span class="cell cell-id"><span class="id-badge">{displayId(issue)}</span></span>
      <span class="cell cell-title">
        {#if expandable}
          <!-- A span (not a button) inside the row's outer <button> — nesting
               real interactives is invalid HTML. Clicks still toggle expand
               via the onclick handler; keyboard equivalents are ArrowRight
               and ArrowLeft handled at the table level. -->
          <span
            class="chevron"
            class:open={isExpanded}
            aria-hidden="true"
            onclick={(event) => toggleExpand(issue, event)}
          >
            <ChevronRightIcon size={12} strokeWidth={2} />
          </span>
        {:else}
          <span class="chevron chevron--placeholder" aria-hidden="true"></span>
        {/if}
        <span class="title-text" id={titleId}>{issue.title}</span>
      </span>
      {#if columnVisibility.updated}
        <span class="cell cell-updated" title={issue.updated_at}>
          {relativeTime(issue.updated_at)}
        </span>
      {/if}
      {#if columnVisibility.priority}
        <span class="cell cell-priority">
          {#if priority}
            <span class={`pri-pill priority-${issue.priority}`}>{priority}</span>
          {/if}
        </span>
      {/if}
      {#if columnVisibility.due}
        <span class="cell cell-due" title={issue.metadata.deadline_on ?? ''}>
          {#if issue.metadata.deadline_on}{shortDate(issue.metadata.deadline_on)}{/if}
        </span>
      {/if}
      {#if columnVisibility.owner}<span class="cell cell-owner">{issue.owner ?? ''}</span>{/if}
      {#if columnVisibility.tags}
        <span class="cell cell-tags" title={labels}>
          {#if labels}{labels}{/if}
        </span>
      {/if}
      <span class="kit-sr-only">
        <span>project: {issue.project_name}</span>
        {#if issue.metadata.deadline_on}<span>
            · Due {shortDate(issue.metadata.deadline_on)}</span
          >{/if}
        {#if issue.owner}<span> · owner: {issue.owner}</span>{/if}
        {#if issue.priority !== undefined}<span> · priority: {issue.priority}</span>{/if}
      </span>
    </button>
    {#if onOpenGraph}
      <button
        type="button"
        class="graph-action"
        aria-label="Open reachable graph"
        aria-describedby={titleId}
        title="Open reachable graph"
        onclick={(event) => openGraph(issue, event)}
      >
        <NetworkIcon size={13} strokeWidth={1.9} aria-hidden="true" />
      </button>
    {/if}
  </div>

  {#if isExpanded}
    {#if visibleChildren(issue).length === 0}
      <div class="children-status" style:--task-depth={String(depth + 1)}>No subtasks.</div>
    {:else}
      {#each visibleChildren(issue) as child (child.uid)}
        {@render row(child, depth + 1)}
      {/each}
    {/if}
  {/if}
{/snippet}

<style>
  .issue-list {
    min-width: 0;
    overflow: hidden;
    display: flex;
    flex-direction: column;
    background: var(--bg-primary);
    /* Establish a container so column visibility can respond to the
       pane's own width — the horizontal-split layout can leave the list
       under 400px even when the viewport is huge, and a viewport-based
       media query would miss that. */
    container-type: inline-size;
    container-name: list;
  }

  .pane-header {
    position: relative;
    flex-shrink: 0;
    padding: 10px 16px 8px;
    background: var(--bg-surface);
    border-bottom: 1px solid var(--border-default);
    display: flex;
    flex-direction: column;
    gap: 2px;
  }

  .heading-row {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 12px;
    min-width: 0;
  }

  .heading {
    display: flex;
    align-items: baseline;
    gap: var(--space-4);
    min-width: 0;
  }

  .header-actions {
    display: flex;
    align-items: center;
    gap: var(--space-3);
    flex-shrink: 0;
    white-space: nowrap;
  }

  .pane-header h2 {
    font-size: var(--font-size-xl);
    line-height: 1.1;
    font-weight: 600;
    letter-spacing: -0.01em;
  }

  .count {
    color: var(--text-muted);
    font-size: var(--font-size-xs);
    font-variant-numeric: tabular-nums;
  }

  .tree-actions {
    display: flex;
    align-items: center;
    gap: 6px;
    flex-shrink: 0;
  }

  .tree-action {
    display: inline-flex;
    align-items: center;
    justify-content: center;
    gap: var(--space-2);
    min-height: 26px;
    padding: 0 8px;
    border: 1px solid var(--border-default);
    border-radius: var(--radius-sm);
    background: var(--bg-surface);
    color: var(--text-secondary);
    font-size: var(--font-size-xs);
    font-weight: 500;
    white-space: nowrap;
    cursor: pointer;
  }

  .tree-action:hover:not(:disabled),
  .tree-action:focus-visible {
    border-color: var(--border-strong);
    color: var(--text-primary);
  }

  .tree-action:disabled {
    cursor: default;
    opacity: 0.45;
  }

  .table {
    flex: 1;
    min-height: 0;
    display: flex;
    flex-direction: column;
    overflow: hidden;
    /* Shared grid template — header and rows live in the same scroll
       plane and inherit this template, so horizontal movement stays
       aligned in split views. The ID column uses a fixed absolute
       width per scope so the header and rows do not resolve `ch`
       against their different font sizes. This keeps every title at
       the same x; max-content also let different IDs leave different
       gaps. The two widths are tight enough for qualified ids at the
       top level / short ids in a project to fit without truncation.
       The title takes the
       leftover via `1fr` so the metadata cluster anchors at the
       right edge with no whitespace pocket. */
    --table-id-col: 112px; /* room for a qualified ID plus badge padding */
    --table-cols: var(--table-cols-wide);
    --table-gap: 14px;
    --table-min-width: 720px;
  }

  .table.table--project-scoped {
    /* Short ids inside a project are typically 4–6 chars so the
       column can collapse and give the title more room. The badge
       around each id adds 7px of padding on both sides, so include
       14px here — otherwise a 6-char id clips against the badge
       background. */
    --table-id-col: 60px;
  }

  .table-header {
    position: sticky;
    top: 0;
    z-index: 3;
    display: grid;
    grid-template-columns: var(--table-cols);
    gap: var(--table-gap);
    width: 100%;
    min-width: var(--table-min-width);
    padding: 5px 6px;
    align-items: center;
    background: var(--bg-surface);
    border-bottom: 1px solid var(--border-default);
    color: var(--text-faint);
    font-size: var(--font-size-3xs);
    font-weight: 600;
    letter-spacing: 0.08em;
    text-transform: uppercase;
  }

  .col {
    display: inline-flex;
    align-items: center;
    gap: var(--space-1);
    min-width: 0;
    padding: 0;
    border: 0;
    background: transparent;
    color: inherit;
    font: inherit;
    letter-spacing: inherit;
    text-transform: inherit;
    text-align: left;
    cursor: pointer;
    border-radius: var(--radius-sm);
    transition: color 0.1s;
  }

  .col:hover,
  .col:focus-visible {
    color: var(--text-primary);
  }

  .col-static {
    cursor: default;
  }

  .col-static:hover {
    color: inherit;
  }

  .col-priority,
  .col-due,
  .col-updated {
    justify-content: flex-end;
    text-align: right;
  }

  .col-updated,
  .col-due {
    font-variant-numeric: tabular-nums;
  }

  .table-body {
    flex: 1;
    overflow: auto;
    padding: 0 0 12px;
  }

  .empty {
    padding: 32px 12px;
    color: var(--text-muted);
    font-size: var(--font-size-xs);
    text-align: center;
  }

  .group {
    margin-top: 6px;
  }

  .group:first-child {
    margin-top: 0;
  }

  .group-title {
    display: flex;
    align-items: center;
    gap: 8px;
    padding: 8px 8px 4px;
    color: var(--text-secondary);
    font-size: var(--font-size-2xs);
    font-weight: 600;
    letter-spacing: 0.05em;
    text-transform: uppercase;
    border-top: 1px solid var(--border-muted);
    margin-top: 4px;
  }

  .group:first-child .group-title {
    border-top: 0;
    margin-top: 0;
  }

  .group-count {
    color: var(--text-faint);
    font-variant-numeric: tabular-nums;
    text-transform: none;
    letter-spacing: 0;
    font-weight: 500;
  }

  .row {
    width: 100%;
    min-width: var(--table-min-width);
    display: grid;
    grid-template-columns: var(--table-cols);
    gap: var(--table-gap);
    align-items: center;
    padding: 3px 6px;
    border-radius: 0;
    text-align: left;
    border: 0;
    background: transparent;
    color: inherit;
    min-height: 26px;
    transition: background 0.08s;
  }

  .row-frame {
    position: relative;
    min-width: var(--table-min-width);
  }

  .row:hover {
    background: var(--bg-surface-hover);
  }

  .row.selected {
    background: color-mix(in srgb, var(--accent-blue) 20%, var(--bg-primary));
    box-shadow:
      inset 3px 0 0 var(--accent-blue),
      inset 0 0 0 1px color-mix(in srgb, var(--accent-blue) 24%, transparent);
    color: var(--text-primary);
  }

  .row:focus-visible {
    outline: none;
    background: var(--accent-blue-soft);
  }

  .cell {
    min-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
    font-size: var(--font-size-xs);
    color: var(--text-secondary);
  }

  .cell-id {
    display: inline-flex;
    align-items: center;
  }

  .id-badge {
    display: inline-flex;
    align-items: center;
    height: 18px;
    padding: 0 7px;
    border-radius: var(--radius-sm);
    background: var(--bg-inset);
    color: var(--text-secondary);
    font-family: var(--font-mono);
    font-size: var(--font-size-2xs);
    font-weight: 500;
    font-variant-numeric: tabular-nums;
    letter-spacing: 0.01em;
  }

  .row.selected .id-badge,
  .row:focus-visible .id-badge {
    background: color-mix(in srgb, var(--accent-blue) 12%, var(--bg-surface));
    color: var(--text-primary);
  }

  .row.selected .title-text {
    font-weight: 600;
  }

  .cell-title {
    display: inline-flex;
    align-items: center;
    gap: 6px;
    min-width: 0;
    color: var(--text-primary);
    font-size: var(--font-size-sm);
    white-space: normal;
    overflow-wrap: anywhere;
    word-break: break-word;
  }

  .title-text {
    flex: 1;
    min-width: 0;
    line-height: 1.35;
    /* Cap at two lines so a runaway title can't push the row off the
       viewport; longer titles ellipsize in the second line. */
    display: -webkit-box;
    -webkit-line-clamp: 2;
    line-clamp: 2;
    -webkit-box-orient: vertical;
    overflow: hidden;
  }

  .chevron {
    display: inline-flex;
    align-items: center;
    justify-content: center;
    flex-shrink: 0;
    width: 14px;
    height: 14px;
    border-radius: var(--radius-sm);
    color: var(--text-muted);
    transition:
      transform 0.1s,
      background 0.1s,
      color 0.1s;
  }

  .chevron:hover:not(.chevron--placeholder) {
    background: var(--bg-inset);
    color: var(--text-primary);
  }

  .chevron.open {
    transform: rotate(90deg);
    color: var(--accent-blue);
  }

  .chevron--placeholder {
    pointer-events: none;
  }

  .graph-action {
    position: absolute;
    top: 50%;
    right: 8px;
    transform: translateY(-50%);
    width: 22px;
    height: 22px;
    border-radius: var(--radius-sm);
    display: inline-flex;
    align-items: center;
    justify-content: center;
    padding: 0;
    border: 0;
    background: transparent;
    color: var(--text-muted);
    opacity: 0;
    pointer-events: none;
    cursor: pointer;
  }

  .row-frame:hover .graph-action,
  .row-frame:focus-within .graph-action,
  .graph-action:focus-visible {
    opacity: 1;
    pointer-events: auto;
  }

  .graph-action:hover,
  .graph-action:focus-visible {
    background: var(--bg-inset);
    color: var(--accent-blue);
    outline: none;
  }

  .cell-priority {
    display: inline-flex;
    justify-content: flex-end;
    align-items: center;
  }

  .pri-pill {
    display: inline-flex;
    align-items: center;
    justify-content: center;
    min-width: 24px;
    height: 17px;
    padding: 0 6px;
    border-radius: var(--radius-sm);
    background: var(--accent-amber-soft);
    color: var(--accent-amber);
    font-size: var(--font-size-3xs);
    font-weight: 600;
    font-variant-numeric: tabular-nums;
  }

  .priority-0 {
    background: var(--accent-red-soft);
    color: var(--accent-red);
  }

  /* Every metadata cell holds at the base 12px set on .cell — only
     the title (13px) climbs above the row baseline. The old setup mixed
     11/12/11 across tags/owner/due so each row read as a hand-laid
     collage instead of one stride of metadata. Color carries the
     hierarchy: muted for low-signal columns, secondary for owner,
     primary for the title. */
  .cell-tags {
    color: var(--text-muted);
  }

  .cell-owner {
    color: var(--text-secondary);
  }

  .cell-due,
  .cell-updated {
    color: var(--text-muted);
    font-variant-numeric: tabular-nums;
    text-align: right;
  }

  .row--child .cell-title {
    padding-left: calc(var(--task-depth, 1) * 18px);
  }

  .children-status {
    padding: 4px 14px 4px calc(14px + (var(--task-depth, 1) * 18px));
    color: var(--text-muted);
    font-size: var(--font-size-2xs);
    font-style: italic;
  }

  /* Pane-width-driven column visibility. The list narrows whenever the
     user picks the side-by-side layout and drags the list pane down to
     its minimum; viewport queries miss this because the *window* is
     still wide. Tags + Owner drop first (low scanning value), then
     Due, leaving the irreducible quartet of ID / title / updated /
     priority that you need to triage a row. */
  @container list (max-width: 880px) {
    .col-tags,
    .cell-tags {
      display: none;
    }

    .table {
      --table-cols: var(--table-cols-medium);
      --table-min-width: 580px;
    }
  }

  @container list (max-width: 680px) {
    .table:not(.table--keep-owner-sort) .col-owner,
    .table:not(.table--keep-owner-sort) .cell-owner {
      display: none;
    }

    .table {
      --table-cols: var(--table-cols-compact);
      --table-gap: 12px;
      --table-min-width: 460px;
    }

    .table.table--keep-owner-sort {
      --table-min-width: 560px;
    }

    .header-actions :global(.action-label) {
      display: none;
    }
  }

  @container list (max-width: 520px) {
    .col-due,
    .cell-due {
      display: none;
    }

    .table {
      --table-cols: var(--table-cols-narrow);
      --table-gap: 10px;
      --table-min-width: 320px;
    }
  }
</style>
