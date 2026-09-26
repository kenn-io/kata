import { localDateString } from './dates'
import type {
  KataProjectSummary,
  KataTaskGroup,
  KataTaskSummary,
  KataTaskViewName,
  KataTaskViewResponse,
} from './types'

interface BuildKataTaskViewOptions {
  view: KataTaskViewName
  issues: KataTaskSummary[]
  projects: KataProjectSummary[]
  today?: string
  fetched_at?: string
}

type ProjectLookup = Map<string, KataProjectSummary>

function issueDate(value: string | undefined): string | undefined {
  return value?.slice(0, 10)
}

function scheduledDate(issue: KataTaskSummary): string | undefined {
  return issue.scheduled_on_date ?? issueDate(issue.metadata.scheduled_on)
}

function deadlineDate(issue: KataTaskSummary): string | undefined {
  return issue.deadline_on_date ?? issueDate(issue.metadata.deadline_on)
}

/**
 * Date membership for views assembled from the all-open collection. It
 * ignores status so explicit status filters still apply to the same set.
 * Inbox follows Kata's tickler rule: a future start date holds an issue back
 * until that day, while deadlines never hide work.
 */
export function kataViewIncludesDates(
  view: KataTaskViewName,
  issue: KataTaskSummary,
  today: string,
): boolean {
  if (view === 'scheduled') return Boolean(scheduledDate(issue) || deadlineDate(issue))
  if (view === 'inbox') {
    const scheduledOn = scheduledDate(issue)
    return scheduledOn === undefined || scheduledOn <= today
  }
  return true
}

function compareIssues(a: KataTaskSummary, b: KataTaskSummary): number {
  const ap = a.priority ?? Number.MAX_SAFE_INTEGER
  const bp = b.priority ?? Number.MAX_SAFE_INTEGER
  if (ap !== bp) return ap - bp
  const title = a.title.localeCompare(b.title)
  if (title !== 0) return title
  return a.uid.localeCompare(b.uid)
}

function projectLookup(projects: KataProjectSummary[]): ProjectLookup {
  return new Map(projects.map((project) => [project.uid, project]))
}

function projectTitle(issue: KataTaskSummary, projects: ProjectLookup): string {
  return projects.get(issue.project_uid)?.name || issue.project_name || issue.project_uid
}

function groupByProject(issues: KataTaskSummary[], projects: ProjectLookup): KataTaskGroup[] {
  const groups = new Map<string, KataTaskGroup>()
  for (const issue of issues) {
    const group = groups.get(issue.project_uid) ?? {
      id: issue.project_uid,
      title: projectTitle(issue, projects),
      issues: [],
    }
    group.issues.push(issue)
    groups.set(issue.project_uid, group)
  }

  return [...groups.values()]
    .map((group) => ({ ...group, issues: [...group.issues].sort(compareIssues) }))
    .sort((a, b) => {
      const title = a.title.localeCompare(b.title)
      if (title !== 0) return title
      return a.id.localeCompare(b.id)
    })
}

function buildToday(issues: KataTaskSummary[], today: string): KataTaskGroup[] {
  const groups: KataTaskGroup[] = [
    { id: 'overdue', title: 'Overdue', issues: [] },
    { id: 'today', title: 'Today', issues: [] },
    { id: 'evening', title: 'This evening', issues: [] },
  ]

  for (const issue of issues) {
    if (issue.status !== 'open') continue
    const scheduledOn = scheduledDate(issue)
    const deadlineOn = deadlineDate(issue)
    const scheduledDue = scheduledOn !== undefined && scheduledOn <= today
    const deadlineDue = deadlineOn !== undefined && deadlineOn <= today
    if (!scheduledDue && !deadlineDue) continue

    if (
      (deadlineOn !== undefined && deadlineOn < today) ||
      (scheduledOn !== undefined && scheduledOn < today)
    ) {
      groups[0]!.issues.push(issue)
    } else if (issue.metadata.today_bucket === 'evening') {
      groups[2]!.issues.push(issue)
    } else {
      groups[1]!.issues.push(issue)
    }
  }

  return groups
    .map((group) => ({ ...group, issues: [...group.issues].sort(compareIssues) }))
    .filter((group) => group.issues.length > 0)
}

function buildInbox(issues: KataTaskSummary[], today: string): KataTaskGroup[] {
  const inboxIssues = issues
    .filter((issue) => issue.status === 'open' && kataViewIncludesDates('inbox', issue, today))
    .sort(compareIssues)
  return inboxIssues.length > 0 ? [{ id: 'inbox', title: 'Inbox', issues: inboxIssues }] : []
}

const teammateHandlePattern = /^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$/

function buildDelegated(issues: KataTaskSummary[]): KataTaskGroup[] {
  const groups = new Map<string, KataTaskGroup>()
  for (const issue of issues) {
    const teammate = issue.metadata.teammate
    if (
      issue.status !== 'open' ||
      typeof teammate !== 'string' ||
      !teammateHandlePattern.test(teammate)
    ) {
      continue
    }
    const attribution = `${issue.author}/${teammate}`
    const group = groups.get(attribution) ?? {
      id: attribution,
      title: attribution,
      issues: [],
    }
    group.issues.push(issue)
    groups.set(attribution, group)
  }

  return [...groups.values()]
    .map((group) => ({ ...group, issues: [...group.issues].sort(compareIssues) }))
    .sort((a, b) => a.title.localeCompare(b.title) || a.id.localeCompare(b.id))
}

function buildAll(issues: KataTaskSummary[], projects: ProjectLookup): KataTaskGroup[] {
  return groupByProject(
    issues.filter((issue) => issue.status === 'open'),
    projects,
  )
}

// Scheduled folds start dates and deadlines into one agenda. A missed deadline
// is overdue; otherwise an issue sits on its earliest date, and a start date
// that has already passed means the work is actionable today.
function buildScheduled(issues: KataTaskSummary[], today: string): KataTaskGroup[] {
  const overdue: KataTaskGroup = { id: 'overdue', title: 'Overdue', issues: [] }
  const dueToday: KataTaskGroup = { id: 'today', title: 'Today', issues: [] }
  const futureByDate = new Map<string, KataTaskGroup>()

  for (const issue of issues) {
    if (issue.status !== 'open') continue
    const scheduledOn = scheduledDate(issue)
    const deadlineOn = deadlineDate(issue)
    if (!kataViewIncludesDates('scheduled', issue, today)) continue
    const dates = [scheduledOn, deadlineOn].filter((date): date is string => Boolean(date))

    if (deadlineOn !== undefined && deadlineOn < today) {
      overdue.issues.push(issue)
      continue
    }
    const keyDate = dates.sort()[0]!
    if (keyDate <= today) {
      dueToday.issues.push(issue)
      continue
    }
    const group = futureByDate.get(keyDate) ?? { id: keyDate, title: keyDate, issues: [] }
    group.issues.push(issue)
    futureByDate.set(keyDate, group)
  }

  const future = [...futureByDate.values()].sort((a, b) => a.id.localeCompare(b.id))
  return [overdue, dueToday, ...future]
    .map((group) => ({ ...group, issues: [...group.issues].sort(compareIssues) }))
    .filter((group) => group.issues.length > 0)
}

function buildLogbook(issues: KataTaskSummary[]): KataTaskGroup[] {
  const groups = new Map<string, KataTaskGroup>()
  for (const issue of issues) {
    if (issue.status !== 'closed') continue
    const closedOn = issueDate(issue.closed_at)
    if (!closedOn) continue

    const group = groups.get(closedOn) ?? { id: closedOn, title: closedOn, issues: [] }
    group.issues.push(issue)
    groups.set(closedOn, group)
  }

  return [...groups.values()]
    .map((group) => ({ ...group, issues: [...group.issues].sort(compareIssues) }))
    .sort((a, b) => b.id.localeCompare(a.id))
}

export function buildKataTaskView(options: BuildKataTaskViewOptions): KataTaskViewResponse {
  const today = options.today ?? localDateString()
  const projects = projectLookup(options.projects)
  let groups: KataTaskGroup[]

  switch (options.view) {
    case 'today':
      groups = buildToday(options.issues, today)
      break
    case 'inbox':
      groups = buildInbox(options.issues, today)
      break
    case 'delegated':
      groups = buildDelegated(options.issues)
      break
    case 'scheduled':
      groups = buildScheduled(options.issues, today)
      break
    case 'all':
      groups = buildAll(options.issues, projects)
      break
    case 'logbook':
      groups = buildLogbook(options.issues)
      break
  }

  return {
    view: options.view,
    groups,
    fetched_at: options.fetched_at ?? new Date().toISOString(),
  }
}
