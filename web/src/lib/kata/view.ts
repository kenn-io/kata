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

function compareIssues(a: KataTaskSummary, b: KataTaskSummary): number {
  const ap = a.priority ?? Number.MAX_SAFE_INTEGER
  const bp = b.priority ?? Number.MAX_SAFE_INTEGER
  if (ap !== bp) return ap - bp
  const title = a.title.localeCompare(b.title)
  if (title !== 0) return title
  return a.uid.localeCompare(b.uid)
}

function compareByDeadline(a: KataTaskSummary, b: KataTaskSummary): number {
  const ad = issueDate(a.metadata.deadline_on) ?? ''
  const bd = issueDate(b.metadata.deadline_on) ?? ''
  if (ad !== bd) return ad.localeCompare(bd)
  return compareIssues(a, b)
}

function projectLookup(projects: KataProjectSummary[]): ProjectLookup {
  return new Map(projects.map((project) => [project.uid, project]))
}

function projectTitle(issue: KataTaskSummary, projects: ProjectLookup): string {
  return projects.get(issue.project_uid)?.name || issue.project_name || issue.project_uid
}

function isInboxProject(project: KataProjectSummary | undefined): boolean {
  if (!project) return false
  return project.metadata.role === 'inbox'
}

function compareInboxIssues(
  projects: ProjectLookup,
): (a: KataTaskSummary, b: KataTaskSummary) => number {
  return (a, b) => {
    const aInbox = isInboxProject(projects.get(a.project_uid))
    const bInbox = isInboxProject(projects.get(b.project_uid))
    if (aInbox !== bInbox) return aInbox ? -1 : 1
    return compareIssues(a, b)
  }
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
    const scheduledOn = issueDate(issue.metadata.scheduled_on)
    const deadlineOn = issueDate(issue.metadata.deadline_on)
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

function buildUpcoming(issues: KataTaskSummary[], today: string): KataTaskGroup[] {
  const groups = new Map<string, KataTaskGroup>()
  for (const issue of issues) {
    if (issue.status !== 'open') continue
    const scheduledOn = issueDate(issue.metadata.scheduled_on)
    if (!scheduledOn || scheduledOn <= today) continue

    const group = groups.get(scheduledOn) ?? { id: scheduledOn, title: scheduledOn, issues: [] }
    group.issues.push(issue)
    groups.set(scheduledOn, group)
  }

  return [...groups.values()]
    .map((group) => ({ ...group, issues: [...group.issues].sort(compareIssues) }))
    .sort((a, b) => a.id.localeCompare(b.id))
}

function buildInbox(issues: KataTaskSummary[], projects: ProjectLookup): KataTaskGroup[] {
  const inboxIssues = issues
    .filter((issue) => issue.status === 'open' && isInboxProject(projects.get(issue.project_uid)))
    .sort(compareInboxIssues(projects))
  return inboxIssues.length > 0 ? [{ id: 'inbox', title: 'Inbox', issues: inboxIssues }] : []
}

function buildAll(issues: KataTaskSummary[], projects: ProjectLookup): KataTaskGroup[] {
  return groupByProject(
    issues.filter((issue) => issue.status === 'open'),
    projects,
  )
}

function buildDeadlines(issues: KataTaskSummary[], today: string): KataTaskGroup[] {
  const overdue: KataTaskGroup = { id: 'overdue', title: 'Overdue', issues: [] }
  const dueToday: KataTaskGroup = { id: 'today', title: 'Today', issues: [] }
  const futureByDate = new Map<string, KataTaskGroup>()

  for (const issue of issues) {
    if (issue.status !== 'open') continue
    const deadlineOn = issueDate(issue.metadata.deadline_on)
    if (!deadlineOn) continue

    if (deadlineOn < today) {
      overdue.issues.push(issue)
    } else if (deadlineOn === today) {
      dueToday.issues.push(issue)
    } else {
      const group = futureByDate.get(deadlineOn) ?? {
        id: deadlineOn,
        title: deadlineOn,
        issues: [],
      }
      group.issues.push(issue)
      futureByDate.set(deadlineOn, group)
    }
  }

  const pinned = [overdue, dueToday]
    .map((group) => ({ ...group, issues: [...group.issues].sort(compareByDeadline) }))
    .filter((group) => group.issues.length > 0)
  const future = [...futureByDate.values()]
    .map((group) => ({ ...group, issues: [...group.issues].sort(compareIssues) }))
    .sort((a, b) => a.id.localeCompare(b.id))
  return [...pinned, ...future]
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
    case 'upcoming':
      groups = buildUpcoming(options.issues, today)
      break
    case 'inbox':
      groups = buildInbox(options.issues, projects)
      break
    case 'deadlines':
      groups = buildDeadlines(options.issues, today)
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
