import type {
  KataProjectSummary,
  KataTaskSearchFilters,
  KataTaskSummary,
  KataTaskViewName,
  KataTaskViewResponse,
} from './types'
import { buildKataTaskView } from './view'

export interface KataAreaSummary {
  name: string
  projects: KataProjectSummary[]
}

export interface KataCurrentView {
  name: KataTaskViewName
  groups: KataTaskViewResponse['groups']
  fetched_at?: string | undefined
}

export interface ProjectKataWorkspaceViewOptions {
  view: KataTaskViewName
  filters: KataTaskSearchFilters
  snapshot: {
    projects: readonly KataProjectSummary[]
    fetched_at: string
  }
  issues: readonly KataTaskSummary[]
  today?: string | undefined
}

function defaultStatusForView(view: KataTaskViewName): KataTaskSearchFilters['status'] {
  return view === 'logbook' ? 'closed' : 'open'
}

function hasActiveFilters(view: KataTaskViewName, filters: KataTaskSearchFilters): boolean {
  return (
    filters.status !== defaultStatusForView(view) ||
    filters.owner.trim() !== '' ||
    filters.label.trim() !== '' ||
    filters.query.trim() !== '' ||
    (filters.relationships?.length ?? 0) > 0
  )
}

export function defaultKataTaskSearchFilters(
  view: KataTaskViewName = 'all',
): KataTaskSearchFilters {
  return {
    scope: { kind: 'all' },
    status: defaultStatusForView(view),
    owner: '',
    label: '',
    query: '',
    relationships: [],
  }
}

function projectArea(project: KataProjectSummary): string {
  const area = project.metadata.area?.trim()
  return area && area !== 'Unfiled' ? area : 'Unfiled'
}

function compareProjectOrder(a: KataProjectSummary, b: KataProjectSummary): number {
  const leftOrder = a.metadata.sidebar_order ?? Number.MAX_SAFE_INTEGER
  const rightOrder = b.metadata.sidebar_order ?? Number.MAX_SAFE_INTEGER
  if (leftOrder !== rightOrder) return leftOrder - rightOrder
  return a.name.localeCompare(b.name)
}

export function deriveKataAreas(projects: readonly KataProjectSummary[]): KataAreaSummary[] {
  const groups = new Map<string, KataProjectSummary[]>()
  for (const project of projects) {
    if (project.metadata.role === 'inbox') continue
    const area = projectArea(project)
    groups.set(area, [...(groups.get(area) ?? []), project])
  }

  const preferred = ['Personal', 'Work', 'Unfiled']
  return [...groups.entries()]
    .sort(([left], [right]) => {
      const leftIndex = preferred.indexOf(left)
      const rightIndex = preferred.indexOf(right)
      if (leftIndex !== -1 || rightIndex !== -1) {
        return (
          (leftIndex === -1 ? Number.MAX_SAFE_INTEGER : leftIndex) -
          (rightIndex === -1 ? Number.MAX_SAFE_INTEGER : rightIndex)
        )
      }
      return left.localeCompare(right)
    })
    .map(([name, areaProjects]) => ({
      name,
      projects: [...areaProjects].sort(compareProjectOrder),
    }))
}

export function projectKataWorkspaceView(
  options: ProjectKataWorkspaceViewOptions,
): KataTaskViewResponse {
  const issues = options.issues
    .filter(
      (issue) =>
        options.filters.scope.kind !== 'project' ||
        issue.project_uid === options.filters.scope.project_uid,
    )
    .map((issue) => ({ ...issue }))
  if (hasActiveFilters(options.view, options.filters)) {
    return {
      view: options.view,
      groups: issues.length > 0 ? [{ id: 'search-results', title: 'Results', issues }] : [],
      fetched_at: options.snapshot.fetched_at,
    }
  }

  return buildKataTaskView({
    view: options.view,
    issues,
    projects: options.snapshot.projects.map((project) => ({ ...project })),
    ...(options.today ? { today: options.today } : {}),
    fetched_at: options.snapshot.fetched_at,
  })
}
