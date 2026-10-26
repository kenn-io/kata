import { compareInstants } from './time'
import { CronClient, CronReadError } from './client'
import { isAuthenticationRequiredError } from '../auth/session'
import type {
  CronRun,
  CronJob,
  CronWorkflow,
  CronProject,
  CronRouteState,
  CronSelection,
  ShowIssueResponseBody,
} from './types'
const projectNotReadable = 'Project not readable'
export interface CronIntent {
  daemonID: string | undefined
  authorityGeneration: number
  projects: readonly CronProject[]
  route: CronRouteState
}
export interface CronProjectState {
  project: CronProject
  jobs: CronJob[]
  runs: CronRun[]
  workflows: CronWorkflow[]
  nextBeforeUID?: string | undefined
  error?: string
  hasRecords: boolean
  historyError?: string | undefined
  jobsError?: string | undefined
  workflowsError?: string | undefined
  loadingMore?: boolean
}
export interface CronPeek {
  status: 'loading' | 'no-issue' | 'unreadable' | 'empty' | 'ready'
  issue?: ShowIssueResponseBody
}
export interface CronState {
  loading: boolean
  projects: CronProjectState[]
  failedRuns24h: number
  badgePartial: boolean
  selected?: CronSelection | undefined
  detail?: CronRun | CronJob | CronWorkflow | undefined
  detailLoading: boolean
  detailError?: string | undefined
  recentRuns: CronRun[]
  peek: CronPeek
}
export const emptyCronState = (): CronState => ({
  loading: false,
  projects: [],
  failedRuns24h: 0,
  badgePartial: false,
  selected: undefined,
  detail: undefined,
  detailError: undefined,
  detailLoading: false,
  recentRuns: [],
  peek: { status: 'no-issue' },
})
export function mergedRuns(
  projects: readonly CronProjectState[],
): { project: CronProject; run: CronRun }[] {
  const seen = new Set<string>()
  return projects
    .flatMap((p) => p.runs.map((run) => ({ project: p.project, run })))
    .filter(({ project, run }) => {
      const key = project.uid + ':' + run.uid
      if (seen.has(key)) return false
      seen.add(key)
      return true
    })
    .sort(
      (a, b) =>
        compareInstants(b.run.created_at, a.run.created_at) ||
        b.run.uid.localeCompare(a.run.uid) ||
        a.project.uid.localeCompare(b.project.uid),
    )
}
// Each key names the authority and query a piece of state was read under.
function authorityKey(intent: CronIntent): string {
  return JSON.stringify([
    intent.daemonID,
    intent.authorityGeneration,
    intent.projects
      .map((project) => [project.id, project.uid])
      .sort((a, b) => String(a[1]).localeCompare(String(b[1]))),
  ])
}
function historyKey(intent: CronIntent): string {
  return JSON.stringify([
    authorityKey(intent),
    [...new Set(intent.route.statuses.filter((status) => status !== 'never_ran'))].sort(),
  ])
}
function catalogKey(intent: CronIntent): string {
  return JSON.stringify([
    historyKey(intent),
    intent.route.tab === 'jobs' ? [...new Set(intent.route.statuses)].sort() : [],
  ])
}
function selectionKey(intent: CronIntent | undefined): string {
  const selection = intent?.route.selection
  return selection && intent
    ? JSON.stringify([authorityKey(intent), selection.kind, selection.projectUID, selection.uid])
    : ''
}
function runOrder(a: CronRun, b: CronRun): number {
  return compareInstants(b.created_at, a.created_at) || b.uid.localeCompare(a.uid)
}
function oldestRun(runs: readonly CronRun[]): CronRun | undefined {
  return runs.reduce<CronRun | undefined>(
    (oldest, run) => (!oldest || runOrder(run, oldest) > 0 ? run : oldest),
    undefined,
  )
}
/**
 * One keyed background read. A request for the running key never restarts
 * it: the request sets `again` and the task runs once more after it finishes.
 * A request for another key aborts it.
 */
interface Task {
  key: string
  abort: AbortController
  again: boolean
  done: Promise<void>
}
interface SelectionTask extends Task {
  /** Settles once the drawer's detail (and a job's recent runs) is shown. */
  detail: Promise<void>
}
/**
 * The Cron page's reads, as three independent tasks:
 *
 * - catalog: rows for the latest intent's catalog key.
 * - selection: the drawer's detail, a job's recent runs and a run's issue peek.
 * - badge: failed-run counts for the authority and project set.
 *
 * Changing authority discards every task and all state. Within one authority,
 * the catalog never cancels selection work, and the selection always follows
 * the latest intent.
 */
export class CronController {
  state = emptyCronState()
  #listeners = new Set<(state: CronState) => void>()
  #intent: CronIntent | undefined
  #catalog: Task | undefined
  /** Catalog key whose completed result is on screen and current. */
  #catalogFresh: string | undefined
  /** Keys the published rows' runs, jobs and workflows were read under. */
  #rowKeys: { runs?: string; jobs?: string; workflows?: string } = {}
  #more: AbortController | undefined
  #selection: SelectionTask | undefined
  /** Selection key whose detail is on screen or loading. */
  #selectionKey = ''
  /** Orders read starts, so a catalog revalidates only selection reads older than it. */
  #clock = 0
  #selectionStarted = 0
  #badge: Task | undefined
  #badgeFresh: string | undefined
  /** An unmounted page may still deliver late snapshots; they must not start reads. */
  #destroyed = false
  constructor(
    private readonly client = new CronClient(),
    private readonly reject: () => void = () => {},
  ) {}
  subscribe(listener: (state: CronState) => void) {
    this.#listeners.add(listener)
    return () => {
      this.#listeners.delete(listener)
    }
  }
  #set(patch: Partial<CronState>) {
    this.state = { ...this.state, ...patch }
    for (const listener of this.#listeners) listener(this.state)
  }
  #unauthenticated() {
    this.fence()
    this.reject()
  }
  /** Records the latest intent, discarding everything read under another authority. */
  #adopt(intent: CronIntent) {
    const previous = this.#intent
    this.#intent = intent
    if (previous && authorityKey(previous) !== authorityKey(intent)) {
      this.#abortAll()
      this.#set(emptyCronState())
    }
  }
  #abortAll() {
    this.#catalog?.abort.abort()
    this.#catalog = undefined
    this.#catalogFresh = undefined
    this.#rowKeys = {}
    this.#more?.abort()
    this.#more = undefined
    this.#badge?.abort.abort()
    this.#badge = undefined
    this.#badgeFresh = undefined
    this.#cancelSelection()
  }
  /** Forces a catalog read for intent; one already running for it is not restarted. */
  load(intent: CronIntent): Promise<void> {
    return this.#requestCatalog(intent, true)
  }
  /** Applies an accepted snapshot on the Cron page. */
  sync(intent: CronIntent, unchangedSnapshot: boolean): Promise<void> {
    return this.#requestCatalog(intent, !unchangedSnapshot)
  }
  async refresh() {
    if (this.#intent) await this.#requestCatalog(this.#intent, true)
  }
  async invalidate() {
    await this.refresh()
  }
  #rowsHealthy(): boolean {
    return this.state.projects.every(
      (row) =>
        (!row.error || row.error === projectNotReadable) &&
        !row.historyError &&
        !row.jobsError &&
        !row.workflowsError,
    )
  }
  #requestCatalog(intent: CronIntent, changed: boolean): Promise<void> {
    if (this.#destroyed) return Promise.resolve()
    this.#adopt(intent)
    const key = catalogKey(intent)
    const active = this.#catalog
    let catalog: Promise<void> | undefined
    if (active?.key === key) {
      if (changed) active.again = true
      catalog = active.done
    } else if (changed || active || this.#catalogFresh !== key || !this.#rowsHealthy())
      catalog = this.#startCatalog(key)
    // Requested after the catalog starts, so its reads count as newer than it.
    const selection = this.#requestSelection(false)
    return catalog ?? selection
  }
  #startCatalog(key: string): Promise<void> {
    this.#catalog?.abort.abort()
    // Older history keeps loading while the same history revalidates.
    if (!this.#intent || this.#rowKeys.runs !== historyKey(this.#intent)) {
      this.#more?.abort()
      this.#more = undefined
    }
    this.#catalogFresh = undefined
    const task: Task = { key, abort: new AbortController(), again: false, done: Promise.resolve() }
    this.#catalog = task
    const current = () => this.#catalog === task && !task.abort.signal.aborted
    task.done = (async () => {
      let started: number
      do {
        task.again = false
        started = ++this.#clock
        await this.#readCatalog(this.#intent!, task, current)
        if (current() && this.#selectionStarted < started) await this.#requestSelection(true)
      } while (task.again && current())
      if (!current()) return
      this.#catalog = undefined
      this.#catalogFresh = key
    })()
    return task.done
  }
  async #readCatalog(intent: CronIntent, task: Task, current: () => boolean): Promise<void> {
    const signal = task.abort.signal
    const keys = {
      runs: historyKey(intent),
      jobs: catalogKey(intent),
      workflows: authorityKey(intent),
    }
    const keep = {
      runs: this.#rowKeys.runs === keys.runs,
      jobs: this.#rowKeys.jobs === keys.jobs,
      workflows: this.#rowKeys.workflows === keys.workflows,
    }
    const previous = new Map(this.state.projects.map((row) => [row.project.uid, row]))
    // Rows stay on screen while they revalidate the same history; only a
    // read with nothing to show is a loading state.
    if (!keep.runs) this.#set({ loading: true, projects: [] })
    const rows: CronProjectState[] = [],
      counts = new Map<string, number>()
    let partial = false,
      index = 0
    const worker = async () => {
      while (index < intent.projects.length && current()) {
        const project = intent.projects[index++]!
        const row: CronProjectState = {
          project,
          jobs: [],
          runs: [],
          workflows: [],
          hasRecords: false,
        }
        try {
          try {
            const summary = await this.client.summary(project.id, signal)
            if (!current()) return
            counts.set(project.uid, summary.failed_runs_24h)
          } catch (error) {
            if (!current()) return
            if (
              isAuthenticationRequiredError(error) ||
              (error instanceof CronReadError && error.status === 403)
            )
              throw error
            partial = true
          }
          const jobs = await this.client.jobs(project.id, signal, {
            statuses: intent.route.tab === 'jobs' ? intent.route.statuses : [],
          })
          if (!current()) return
          const workflows = await this.client.workflows(project.id, signal)
          if (!current()) return
          const runs = await this.#runsThrough(
            project.id,
            signal,
            { statuses: intent.route.statuses.filter((s) => s !== 'never_ran') },
            keep.runs ? oldestRun(previous.get(project.uid)?.runs ?? []) : undefined,
          )
          if (!current()) return
          row.jobs = jobs.jobs
          row.workflows = workflows.workflows
          row.runs = runs.runs
          row.nextBeforeUID = runs.next_before_uid
          row.hasRecords = Boolean(row.jobs.length || row.workflows.length || row.runs.length)
          if (!row.hasRecords && intent.route.statuses.length) {
            const all = await this.client.runs(project.id, signal)
            if (!current()) return
            row.hasRecords = all.runs.length > 0
            if (!row.hasRecords) {
              const allJobs = await this.client.jobs(project.id, signal)
              if (!current()) return
              row.hasRecords = allJobs.jobs.length > 0
            }
          }
        } catch (error) {
          if (!current()) return
          if (isAuthenticationRequiredError(error)) {
            this.#unauthenticated()
            return
          }
          partial = true
          const denied = error instanceof CronReadError && error.status === 403
          if (denied) counts.delete(project.uid)
          const earlier = previous.get(project.uid)
          if (!denied && earlier && !earlier.error) {
            // Keep each earlier result only under the query it answered.
            Object.assign(row, {
              runs: keep.runs ? earlier.runs : [],
              nextBeforeUID: keep.runs ? earlier.nextBeforeUID : undefined,
              jobs: keep.jobs ? earlier.jobs : [],
              workflows: keep.workflows ? earlier.workflows : [],
              hasRecords: earlier.hasRecords,
              historyError: keep.runs ? 'History refresh unavailable' : 'Cron unavailable',
              jobsError: keep.jobs ? 'Jobs refresh unavailable' : 'Cron unavailable',
              workflowsError: keep.workflows ? 'Workflows refresh unavailable' : 'Cron unavailable',
            })
          } else row.error = denied ? projectNotReadable : 'Cron unavailable'
        }
        if (!current()) return
        rows.push(row)
      }
    }
    await Promise.all(Array.from({ length: Math.min(4, intent.projects.length) }, worker))
    if (!current()) return
    rows.sort((a, b) => a.project.name.localeCompare(b.project.name))
    if (keep.runs) {
      // Older pages loaded while this sweep ran stay, with their cursor.
      const shown = new Map(this.state.projects.map((row) => [row.project.uid, row]))
      for (const row of rows) {
        const earlier = shown.get(row.project.uid)
        if (!earlier || row.error) continue
        row.loadingMore = earlier.loadingMore ?? false
        const oldest = oldestRun(row.runs),
          before = oldestRun(earlier.runs)
        if (!before || (oldest && runOrder(before, oldest) <= 0)) continue
        const seen = new Set(row.runs.map((run) => run.uid))
        row.runs = [
          ...row.runs,
          ...earlier.runs.filter(
            (run) => !seen.has(run.uid) && (!oldest || runOrder(run, oldest) > 0),
          ),
        ].sort(runOrder)
        row.nextBeforeUID = earlier.nextBeforeUID
      }
    }
    this.#rowKeys = keys
    if (!this.#badge) this.#badgeFresh = authorityKey(intent)
    this.#set({
      loading: false,
      projects: rows,
      failedRuns24h: [...counts.values()].reduce((a, b) => a + b, 0),
      badgePartial: partial,
    })
  }
  async #runsThrough(
    project: number,
    signal: AbortSignal,
    query: Parameters<CronClient['runs']>[2],
    boundary: CronRun | undefined,
  ) {
    let page = await this.client.runs(project, signal, query)
    const runs = new Map(page.runs.map((run) => [run.uid, run])),
      cursors = new Set<string>()
    let cursor = page.next_before_uid
    while (
      cursor &&
      boundary &&
      (!runs.size || runOrder(oldestRun([...runs.values()])!, boundary) < 0)
    ) {
      if (signal.aborted) throw signal.reason
      if (cursors.has(cursor)) throw new Error('Cron history cursor did not advance')
      cursors.add(cursor)
      page = await this.client.runs(project, signal, { ...query, beforeUID: cursor })
      if (signal.aborted) throw signal.reason
      for (const run of page.runs) runs.set(run.uid, run)
      cursor = page.next_before_uid
    }
    return { runs: [...runs.values()].sort(runOrder), next_before_uid: cursor }
  }
  async loadMore(projectUID: string): Promise<void> {
    const row = this.state.projects.find((p) => p.project.uid === projectUID)
    if (this.state.loading || !row?.nextBeforeUID || row.loadingMore || !this.#intent) return
    const abort = (this.#more ??= new AbortController())
    const current = () => this.#more === abort && !abort.signal.aborted
    const cursor = row.nextBeforeUID
    // A background sweep may replace the row while the page loads; apply the
    // result to whichever row is on screen when it arrives.
    const update = (change: (shown: CronProjectState) => void) => {
      this.#set({
        projects: this.state.projects.map((shown) => {
          if (shown.project.uid !== projectUID) return shown
          const next = { ...shown }
          change(next)
          return next
        }),
      })
    }
    update((shown) => {
      shown.loadingMore = true
      shown.historyError = undefined
    })
    try {
      const page = await this.client.runs(row.project.id, abort.signal, {
        beforeUID: cursor,
        statuses: this.#intent.route.statuses.filter((s) => s !== 'never_ran'),
      })
      if (!current()) return
      update((shown) => {
        const before = oldestRun(shown.runs),
          oldest = oldestRun(page.runs)
        const map = new Map(shown.runs.map((run) => [run.uid, run]))
        for (const run of page.runs) map.set(run.uid, run)
        shown.runs = [...map.values()].sort(runOrder)
        if (shown.nextBeforeUID === cursor || (oldest && (!before || runOrder(oldest, before) > 0)))
          shown.nextBeforeUID = page.next_before_uid
        shown.loadingMore = false
      })
    } catch (error) {
      if (!current()) return
      if (isAuthenticationRequiredError(error)) {
        this.#unauthenticated()
        return
      }
      update((shown) => {
        shown.historyError = 'More history unavailable'
        shown.loadingMore = false
      })
    }
  }
  /** Opens, changes or closes the drawer without touching the catalog. */
  async select(selection: CronSelection | undefined): Promise<void> {
    if (this.#destroyed) return
    if (this.#intent)
      this.#intent = { ...this.#intent, route: { ...this.#intent.route, selection } }
    else this.#set({ selected: selection })
    await this.#requestSelection(false)
  }
  /**
   * Follows the latest intent's selection. A revalidation of the selection on
   * screen keeps its detail and is coalesced with work still running for it.
   */
  #requestSelection(revalidate: boolean): Promise<void> {
    const key = selectionKey(this.#intent)
    if (key === this.#selectionKey) {
      if (!revalidate || !key) return this.#selection?.detail ?? Promise.resolve()
      if (this.#selection) {
        this.#selection.again = true
        return Promise.resolve()
      }
      return this.#startSelection(key, true)
    }
    return this.#startSelection(key, false)
  }
  #startSelection(key: string, retain: boolean): Promise<void> {
    this.#cancelSelection()
    this.#selectionKey = key
    const selection = this.#intent?.route.selection
    this.#set({
      selected: selection,
      detail: retain ? this.state.detail : undefined,
      detailError: undefined,
      detailLoading: Boolean(selection) && !(retain && this.state.detail),
      recentRuns: retain ? this.state.recentRuns : [],
      peek: retain ? this.state.peek : { status: selection ? 'loading' : 'no-issue' },
    })
    if (!selection) return Promise.resolve()
    let shown!: () => void
    const task: SelectionTask = {
      key,
      abort: new AbortController(),
      again: false,
      done: Promise.resolve(),
      detail: new Promise<void>((resolve) => (shown = resolve)),
    }
    this.#selection = task
    const current = () => this.#selection === task && !task.abort.signal.aborted
    task.abort.signal.addEventListener('abort', () => shown(), { once: true })
    task.done = (async () => {
      let keepDetail = retain
      do {
        task.again = false
        this.#selectionStarted = ++this.#clock
        await this.#readSelection(selection, task, current, keepDetail, shown)
        keepDetail = true
      } while (task.again && current())
      shown()
      if (this.#selection === task) this.#selection = undefined
    })()
    return task.detail
  }
  async #readSelection(
    selection: CronSelection,
    task: SelectionTask,
    current: () => boolean,
    retain: boolean,
    shown: () => void,
  ): Promise<void> {
    const signal = task.abort.signal
    const project = this.#intent?.projects.find((p) => p.uid === selection.projectUID)
    if (!project) {
      this.#set({
        detailLoading: false,
        detailError: projectNotReadable,
        peek: { status: 'unreadable' },
      })
      return
    }
    let issueUID: string | undefined
    try {
      const detail =
        selection.kind === 'run'
          ? await this.client.run(project.id, selection.uid, signal)
          : selection.kind === 'job'
            ? await this.client.job(project.id, selection.uid, signal)
            : await this.client.workflow(project.id, selection.uid, signal)
      if (!current()) return
      issueUID = selection.kind === 'run' ? (detail as CronRun).issue_uid : undefined
      const keepPeek = retain && issueUID && this.state.peek.issue?.issue.uid === issueUID
      this.#set({
        detail,
        detailLoading: false,
        detailError: undefined,
        peek: keepPeek ? this.state.peek : { status: issueUID ? 'loading' : 'no-issue' },
      })
      if (selection.kind === 'job') {
        const page = await this.client.runs(project.id, signal, { jobUID: selection.uid })
        if (current()) this.#set({ recentRuns: page.runs })
      }
    } catch (error) {
      if (!current()) return
      if (isAuthenticationRequiredError(error)) {
        this.#unauthenticated()
        return
      }
      this.#set({
        detail: undefined,
        detailLoading: false,
        detailError: 'Detail not readable',
        peek: { status: 'unreadable' },
      })
      return
    } finally {
      shown()
    }
    if (!issueUID || !current()) return
    // A short delay keeps quick drawer browsing from reading every issue.
    await new Promise<void>((resolve) => {
      const timer = setTimeout(resolve, 150)
      signal.addEventListener(
        'abort',
        () => {
          clearTimeout(timer)
          resolve()
        },
        { once: true },
      )
    })
    if (!current()) return
    try {
      const issue = await this.client.issue(project.id, issueUID, signal)
      if (current())
        this.#set({ peek: { status: issue.comments.length ? 'ready' : 'empty', issue } })
    } catch (error) {
      if (!current()) return
      if (isAuthenticationRequiredError(error)) {
        this.#unauthenticated()
        return
      }
      this.#set({ peek: { status: 'unreadable' } })
    }
  }
  #cancelSelection() {
    this.#selection?.abort.abort()
    this.#selection = undefined
    this.#selectionKey = ''
  }
  /** Applies an accepted snapshot outside the Cron page: only the badge is read. */
  loadBadge(intent: CronIntent, unchangedSnapshot = false): Promise<void> {
    if (this.#destroyed) return Promise.resolve()
    this.#adopt({ ...intent, route: { ...intent.route, selection: undefined } })
    this.#catalog?.abort.abort()
    this.#catalog = undefined
    this.#catalogFresh = undefined
    void this.#requestSelection(false)
    return this.#requestBadge(!unchangedSnapshot)
  }
  async refreshSummaries() {
    if (this.#intent) await this.#requestBadge(true)
  }
  #requestBadge(changed: boolean): Promise<void> {
    const intent = this.#intent
    if (!intent) return Promise.resolve()
    const key = authorityKey(intent)
    const active = this.#badge
    if (active?.key === key) {
      if (changed) active.again = true
      return active.done
    }
    if (!changed && !active && this.#badgeFresh === key) return Promise.resolve()
    active?.abort.abort()
    this.#badgeFresh = undefined
    const task: Task = { key, abort: new AbortController(), again: false, done: Promise.resolve() }
    this.#badge = task
    const current = () => this.#badge === task && !task.abort.signal.aborted
    task.done = (async () => {
      do {
        task.again = false
        await this.#readBadge(this.#intent!, task.abort.signal, current)
      } while (task.again && current())
      if (!current()) return
      this.#badge = undefined
      this.#badgeFresh = key
    })()
    return task.done
  }
  async #readBadge(intent: CronIntent, signal: AbortSignal, current: () => boolean) {
    let index = 0,
      total = 0,
      partial = false
    const worker = async () => {
      while (index < intent.projects.length && current()) {
        const project = intent.projects[index++]!
        try {
          const summary = await this.client.summary(project.id, signal)
          if (current()) total += summary.failed_runs_24h
        } catch (error) {
          if (!current()) return
          if (isAuthenticationRequiredError(error)) {
            this.#unauthenticated()
            return
          }
          partial = true
        }
      }
    }
    await Promise.all(Array.from({ length: Math.min(4, intent.projects.length) }, worker))
    if (current()) this.#set({ failedRuns24h: total, badgePartial: partial })
  }
  /** Leaving the page or its filters: catalog and drawer reads stop; the badge continues. */
  cancelNavigation(): void {
    // An interrupted read already cleared #catalogFresh; a completed one stays current.
    this.#catalog?.abort.abort()
    this.#catalog = undefined
    this.#more?.abort()
    this.#more = undefined
    this.#cancelSelection()
    this.#set({
      ...emptyCronState(),
      projects: this.state.projects.map((row) => ({ ...row, loadingMore: false })),
      failedRuns24h: this.state.failedRuns24h,
      badgePartial: this.state.badgePartial,
    })
  }
  fence() {
    this.#abortAll()
    this.#intent = undefined
    this.#set(emptyCronState())
  }
  destroy() {
    this.#destroyed = true
    this.fence()
    this.#listeners.clear()
  }
}
