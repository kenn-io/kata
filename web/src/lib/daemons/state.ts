import { parseRoute, serializeRoute } from '../router'

export const daemonWorkspaceStorageKey = 'kata.web.workspace-state.v1'

interface StoredRoutes {
  version: 1
  daemons: Record<string, string>
}

export function loadDaemonRoute(
  daemonID: string,
  storage: Storage = localStorage,
): string | undefined {
  if (!daemonID) return undefined
  const state = read(storage)
  return canonicalRoute(state?.daemons[daemonID])
}

export function saveDaemonRoute(
  daemonID: string,
  route: string,
  storage: Storage = localStorage,
): void {
  const canonical = canonicalRoute(route)
  if (!daemonID || !canonical) return
  const current = read(storage)
  const daemons = { ...(current?.daemons ?? {}), [daemonID]: canonical }
  try {
    storage.setItem(daemonWorkspaceStorageKey, JSON.stringify({ version: 1, daemons }))
  } catch {
    // Workspace persistence is optional and must not block navigation.
  }
}

function read(storage: Storage): StoredRoutes | undefined {
  try {
    const raw = storage.getItem(daemonWorkspaceStorageKey)
    if (!raw) return { version: 1, daemons: {} }
    const parsed = JSON.parse(raw) as unknown
    if (!isRecord(parsed) || parsed.version !== 1 || !isRecord(parsed.daemons)) return undefined
    const daemons: Record<string, string> = {}
    for (const [id, route] of Object.entries(parsed.daemons)) {
      if (typeof route === 'string') daemons[id] = route
    }
    return { version: 1, daemons }
  } catch {
    return undefined
  }
}

function canonicalRoute(value: string | undefined): string | undefined {
  if (!value) return undefined
  try {
    const url = new URL(value, window.location.origin)
    if (url.origin !== window.location.origin || url.pathname !== '/kata' || url.hash)
      return undefined
    const parsed = parseRoute(url)
    return parsed.kind === 'route-error' ? undefined : serializeRoute(parsed)
  } catch {
    return undefined
  }
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value)
}
