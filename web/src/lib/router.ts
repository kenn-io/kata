import { applicationRoutePath } from './applicationBase'

export const systemViews = [
  'inbox',
  'today',
  'upcoming',
  'deadlines',
  'all-open',
  'logbook',
] as const

export type SystemView = (typeof systemViews)[number]

export interface ShareableFilters {
  status: string[]
  owner: string[]
  label: string[]
  relationship: string[]
  text?: string
}

interface RouteBase {
  filters: ShareableFilters
}

export type KataRoute =
  | (RouteBase & {
      kind: 'kata'
      view?: SystemView
      projectUID?: string
      issueUID?: string
      graph: boolean
    })
  | {
      kind: 'route-error'
      path: string
      reason: 'path' | 'view' | 'project_uid' | 'issue_uid'
      searchRef?: string
    }

export function parseRoute(url: URL, routePath = applicationRoutePath()): KataRoute {
  const filters = parseFilters(url.searchParams)
  if (normalizeRoutePath(url.pathname) !== normalizeRoutePath(routePath)) {
    return { kind: 'route-error', path: url.pathname, reason: 'path' }
  }
  const view = url.searchParams.get('view')?.trim()
  if (view && !isSystemView(view)) {
    return { kind: 'route-error', path: url.pathname + url.search, reason: 'view' }
  }
  const normalizedView = view && isSystemView(view) ? view : undefined
  const projectUID = url.searchParams.get('scope')?.trim()
  if (projectUID && !validUID(projectUID)) {
    return { kind: 'route-error', path: url.pathname + url.search, reason: 'project_uid' }
  }
  const issueUID = url.searchParams.get('issue')?.trim()
  if (issueUID && !validUID(issueUID)) {
    const routeError: Extract<KataRoute, { kind: 'route-error' }> = {
      kind: 'route-error',
      path: url.pathname + url.search,
      reason: 'issue_uid',
    }
    if (issueUID.length > 0 && issueUID.length < 26) routeError.searchRef = issueUID
    return routeError
  }
  return {
    kind: 'kata',
    ...(normalizedView ? { view: normalizedView } : {}),
    ...(projectUID ? { projectUID } : {}),
    ...(issueUID ? { issueUID } : {}),
    graph: Boolean(issueUID) && url.searchParams.get('graph') === '1',
    filters,
  }
}

export function serializeRoute(route: KataRoute, routePath = applicationRoutePath()): string {
  if (route.kind === 'route-error') return route.path
  const query = new URLSearchParams()
  if (route.view) query.set('view', route.view)
  if (route.projectUID) query.set('scope', route.projectUID)
  if (route.issueUID) query.set('issue', route.issueUID)
  if (route.graph && route.issueUID) query.set('graph', '1')
  for (const [name, values] of [
    ['label', route.filters.label],
    ['owner', route.filters.owner],
    ['relationship', route.filters.relationship],
    ['status', route.filters.status],
  ] as const) {
    for (const value of sortedUnique(values)) query.append(name, value)
  }
  if (route.filters.text) query.set('text', route.filters.text)
  const encoded = query.toString()
  return encoded ? `${routePath}?${encoded}` : routePath
}

function normalizeRoutePath(value: string): string {
  return value.length > 1 ? value.replace(/\/+$/, '') : value
}

function parseFilters(query: URLSearchParams): ShareableFilters {
  const filters: ShareableFilters = {
    status: sortedUnique(query.getAll('status')),
    owner: sortedUnique(query.getAll('owner')),
    label: sortedUnique(query.getAll('label')),
    relationship: sortedUnique(query.getAll('relationship')),
  }
  const text = query.get('text')?.trim()
  if (text) filters.text = text
  return filters
}

function sortedUnique(values: readonly string[]): string[] {
  return [...new Set(values.map((value) => value.trim()).filter(Boolean))].sort()
}

function isSystemView(value: string | undefined): value is SystemView {
  return systemViews.includes(value as SystemView)
}

function validUID(value: string): boolean {
  return /^[0-7][0-9A-HJKMNP-TV-Z]{25}$/.test(value)
}
