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
function historyKey(intent: CronIntent): string {
  return JSON.stringify([
    intent.daemonID,
    intent.authorityGeneration,
    intent.projects
      .map((project) => [project.id, project.uid])
      .sort((a, b) => String(a[1]).localeCompare(String(b[1]))),
    [...new Set(intent.route.statuses.filter((status) => status !== 'never_ran'))].sort(),
  ])
}
function catalogKey(intent: CronIntent): string {
  return JSON.stringify([
    historyKey(intent),
    intent.route.tab === 'jobs' ? [...new Set(intent.route.statuses)].sort() : [],
  ])
}
function selectionKey(selection: CronSelection | undefined): string {
  return selection ? JSON.stringify([selection.kind, selection.projectUID, selection.uid]) : ''
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
export class CronController {
  state = emptyCronState()
  #listeners = new Set<(state: CronState) => void>()
  #intent: CronIntent | undefined
  #abort?: AbortController
  #selectionAbort?: AbortController
  #catalogLoaded = false
  #catalogNeedsRefresh = false
  #generation = 0
  #selectionGeneration = 0
  #peekTimer: ReturnType<typeof setTimeout> | undefined
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
  async load(intent: CronIntent): Promise<void> {
    const sameAuthority =
      this.#intent?.daemonID === intent.daemonID &&
      this.#intent?.authorityGeneration === intent.authorityGeneration
    const sameHistory =
      this.#catalogLoaded &&
      this.#intent !== undefined &&
      historyKey(this.#intent) === historyKey(intent)
    const previousProjects = new Map(
      (sameHistory ? this.state.projects : []).map((row) => [row.project.uid, row]),
    )
    const keepSelection =
      sameHistory && selectionKey(this.state.selected) === selectionKey(intent.route.selection)
    const retained = sameAuthority
      ? {
          failedRuns24h: this.state.failedRuns24h,
          badgePartial: this.state.badgePartial,
          ...(sameHistory
            ? { projects: this.state.projects.map((row) => ({ ...row, loadingMore: false })) }
            : {}),
          ...(keepSelection
            ? {
                selected: this.state.selected,
                detail: this.state.detail,
                detailLoading: this.state.detailLoading,
                detailError: this.state.detailError,
                recentRuns: this.state.recentRuns,
                peek: this.state.peek,
              }
            : {}),
        }
      : {}
    if (!sameHistory) this.#catalogLoaded = false
    this.#intent = intent
    this.#catalogNeedsRefresh = true
    this.#abort?.abort()
    this.#cancelSelection()
    const generation = ++this.#generation,
      abort = new AbortController()
    this.#abort = abort
    this.#set({ ...emptyCronState(), ...retained, loading: true })
    const rows: CronProjectState[] = [],
      counts = new Map<string, number>()
    let partial = false,
      index = 0
    const current = () => !abort.signal.aborted && generation === this.#generation
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
            const summary = await this.client.summary(project.id, abort.signal)
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
          const jobs = await this.client.jobs(project.id, abort.signal, {
            statuses: intent.route.tab === 'jobs' ? intent.route.statuses : [],
          })
          if (!current()) return
          const workflows = await this.client.workflows(project.id, abort.signal)
          if (!current()) return
          const runs = await this.#runsThrough(
            project.id,
            abort.signal,
            {
              statuses: intent.route.statuses.filter((s) => s !== 'never_ran'),
            },
            oldestRun(previousProjects.get(project.uid)?.runs ?? []),
          )
          if (!current()) return
          row.jobs = jobs.jobs
          row.workflows = workflows.workflows
          row.runs = runs.runs
          row.nextBeforeUID = runs.next_before_uid
          row.hasRecords = Boolean(row.jobs.length || row.workflows.length || row.runs.length)
          if (!row.hasRecords && intent.route.statuses.length) {
            const all = await this.client.runs(project.id, abort.signal)
            if (!current()) return
            row.hasRecords = all.runs.length > 0
            if (!row.hasRecords) {
              const allJobs = await this.client.jobs(project.id, abort.signal)
              if (!current()) return
              row.hasRecords = allJobs.jobs.length > 0
            }
          }
        } catch (error) {
          if (!current()) return
          if (isAuthenticationRequiredError(error)) {
            this.fence()
            this.reject()
            return
          }
          partial = true
          if (error instanceof CronReadError && error.status === 403) counts.delete(project.uid)
          const previous = previousProjects.get(project.uid)
          if (
            previous &&
            !previous.error &&
            !(error instanceof CronReadError && error.status === 403)
          ) {
            Object.assign(row, previous, {
              project,
              loadingMore: false,
              historyError: 'History refresh unavailable',
            })
          } else {
            row.error =
              error instanceof CronReadError && error.status === 403
                ? projectNotReadable
                : 'Cron unavailable'
          }
        }
        if (!current()) return
        rows.push(row)
      }
    }
    await Promise.all(Array.from({ length: Math.min(4, intent.projects.length) }, worker))
    if (!current()) return
    rows.sort((a, b) => a.project.name.localeCompare(b.project.name))
    this.#catalogLoaded = true
    this.#catalogNeedsRefresh = false
    this.#set({
      loading: false,
      projects: rows,
      failedRuns24h: [...counts.values()].reduce((a, b) => a + b, 0),
      badgePartial: partial,
    })
    if (this.#intent?.route.selection)
      await this.select(this.#intent.route.selection, keepSelection)
  }
  async sync(intent: CronIntent, unchangedSnapshot: boolean): Promise<void> {
    const sameCatalog =
      this.#intent !== undefined && catalogKey(this.#intent) === catalogKey(intent)
    if (
      unchangedSnapshot &&
      sameCatalog &&
      (this.state.loading ||
        (this.#catalogLoaded &&
          !this.#catalogNeedsRefresh &&
          this.state.projects.every(
            (row) => (!row.error || row.error === projectNotReadable) && !row.historyError,
          )))
    ) {
      this.#intent = intent
      if (!this.#abort || this.#abort.signal.aborted) this.#abort = new AbortController()
      if (selectionKey(this.state.selected) !== selectionKey(intent.route.selection))
        await this.select(intent.route.selection)
      return
    }
    await this.load(intent)
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
    const abort = this.#abort
    if (this.state.loading || !row?.nextBeforeUID || row.loadingMore || !abort || !this.#intent)
      return
    const generation = this.#generation
    row.loadingMore = true
    row.historyError = undefined
    this.#set({ projects: [...this.state.projects] })
    try {
      const page = await this.client.runs(row.project.id, abort.signal, {
        beforeUID: row.nextBeforeUID,
        statuses: this.#intent.route.statuses.filter((s) => s !== 'never_ran'),
      })
      if (generation !== this.#generation || abort.signal.aborted) return
      const map = new Map(row.runs.map((run) => [run.uid, run]))
      for (const run of page.runs) map.set(run.uid, run)
      row.runs = [...map.values()]
      row.nextBeforeUID = page.next_before_uid
    } catch (error) {
      if (generation !== this.#generation || abort.signal.aborted) return
      if (isAuthenticationRequiredError(error)) {
        this.fence()
        this.reject()
        return
      }
      row.historyError = 'More history unavailable'
    } finally {
      if (generation === this.#generation) {
        row.loadingMore = false
        this.#set({ projects: [...this.state.projects] })
      }
    }
  }
  async select(selection: CronSelection | undefined, retainDetail = false): Promise<void> {
    if (this.#intent)
      this.#intent = { ...this.#intent, route: { ...this.#intent.route, selection } }
    this.#cancelSelection()
    const generation = this.#selectionGeneration,
      abort = new AbortController()
    this.#selectionAbort = abort
    this.#set({
      selected: selection,
      detail: retainDetail ? this.state.detail : undefined,
      detailError: undefined,
      detailLoading: Boolean(selection) && !(retainDetail && this.state.detail),
      recentRuns: retainDetail ? this.state.recentRuns : [],
      peek: retainDetail ? this.state.peek : { status: selection ? 'loading' : 'no-issue' },
    })
    if (!selection) return
    const project = this.#intent?.projects.find((p) => p.uid === selection.projectUID)
    if (!project) {
      this.#set({
        detailLoading: false,
        detailError: projectNotReadable,
        peek: { status: 'unreadable' },
      })
      return
    }
    const current = () => !abort.signal.aborted && generation === this.#selectionGeneration
    try {
      const detail =
        selection.kind === 'run'
          ? await this.client.run(project.id, selection.uid, abort.signal)
          : selection.kind === 'job'
            ? await this.client.job(project.id, selection.uid, abort.signal)
            : await this.client.workflow(project.id, selection.uid, abort.signal)
      if (!current()) return
      const issueUID = selection.kind === 'run' ? (detail as CronRun).issue_uid : undefined
      const keepPeek = retainDetail && issueUID && this.state.peek.issue?.issue.uid === issueUID
      this.#set({
        detail,
        detailLoading: false,
        peek: keepPeek ? this.state.peek : { status: issueUID ? 'loading' : 'no-issue' },
      })
      if (selection.kind === 'job') {
        const page = await this.client.runs(project.id, abort.signal, {
          jobUID: selection.uid,
        })
        if (current()) this.#set({ recentRuns: page.runs })
        return
      }
      if (!issueUID) return
      this.#peekTimer = setTimeout(() => {
        this.#peekTimer = undefined
        void this.client
          .issue(project.id, issueUID, abort.signal)
          .then((issue) => {
            if (current())
              this.#set({ peek: { status: issue.comments.length ? 'ready' : 'empty', issue } })
          })
          .catch((error) => {
            if (!current()) return
            if (isAuthenticationRequiredError(error)) {
              this.fence()
              this.reject()
              return
            }
            this.#set({ peek: { status: 'unreadable' } })
          })
      }, 150)
    } catch (error) {
      if (!current()) return
      if (isAuthenticationRequiredError(error)) {
        this.fence()
        this.reject()
        return
      }
      this.#set({
        detail: undefined,
        detailLoading: false,
        detailError: 'Detail not readable',
        peek: { status: 'unreadable' },
      })
    }
  }
  async loadBadge(intent: CronIntent): Promise<void> {
    this.#catalogLoaded = false
    this.#intent = intent
    this.#cancelSelection()
    this.#set({
      selected: undefined,
      detail: undefined,
      detailLoading: false,
      peek: { status: 'no-issue' },
    })
    this.#abort?.abort()
    const generation = ++this.#generation,
      abort = new AbortController()
    this.#abort = abort
    let index = 0,
      total = 0,
      partial = false
    const current = () => generation === this.#generation && !abort.signal.aborted
    const worker = async () => {
      while (index < intent.projects.length && current()) {
        const project = intent.projects[index++]!
        try {
          const result = await this.client.summary(project.id, abort.signal)
          if (current()) total += result.failed_runs_24h
        } catch (error) {
          if (!current()) return
          if (isAuthenticationRequiredError(error)) {
            this.fence()
            this.reject()
            return
          }
          partial = true
        }
      }
    }
    await Promise.all(Array.from({ length: Math.min(4, intent.projects.length) }, worker))
    if (current()) this.#set({ failedRuns24h: total, badgePartial: partial })
  }
  async refreshSummaries() {
    if (!this.#intent) return
    const intent = this.#intent,
      generation = this.#generation,
      abort = this.#abort
    if (!abort) return
    let index = 0,
      total = 0,
      partial = false
    const current = () => generation === this.#generation && !abort.signal.aborted
    const worker = async () => {
      while (index < intent.projects.length && current()) {
        const p = intent.projects[index++]!
        try {
          const summary = await this.client.summary(p.id, abort.signal)
          if (current()) total += summary.failed_runs_24h
        } catch (error) {
          if (!current()) return
          if (isAuthenticationRequiredError(error)) {
            this.fence()
            this.reject()
            return
          }
          partial = true
        }
      }
    }
    await Promise.all(Array.from({ length: Math.min(4, intent.projects.length) }, worker))
    if (current()) this.#set({ failedRuns24h: total, badgePartial: partial })
  }
  async refresh() {
    if (this.#intent) await this.load(this.#intent)
  }
  async invalidate() {
    if (this.#intent) await this.load(this.#intent)
  }
  #cancelSelection() {
    this.#selectionGeneration++
    this.#selectionAbort?.abort()
    if (this.#peekTimer) clearTimeout(this.#peekTimer)
    this.#peekTimer = undefined
  }
  cancelNavigation(): void {
    this.#generation++
    this.#abort?.abort()
    this.#cancelSelection()
    this.#set({
      ...emptyCronState(),
      projects: this.state.projects.map((row) => ({ ...row, loadingMore: false })),
      failedRuns24h: this.state.failedRuns24h,
      badgePartial: this.state.badgePartial,
    })
  }
  fence() {
    this.#generation++
    this.#abort?.abort()
    this.#cancelSelection()
    this.#intent = undefined
    this.#catalogLoaded = false
    this.#set(emptyCronState())
  }
  destroy() {
    this.fence()
    this.#listeners.clear()
  }
}
