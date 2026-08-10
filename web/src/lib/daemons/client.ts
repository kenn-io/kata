import { AuthenticationRequiredError } from '../auth/session'
import { responseTriggeredAuthenticationTransition } from '../api/client'

export type WebDaemonHealth = 'connected' | 'auth_required' | 'down' | 'upgrade_required'

export interface WebDaemonInfo {
  id: string
  url: string
  default: boolean
  auth: 'none' | 'token'
  health: WebDaemonHealth
  hint?: string
}

const daemonHeader = 'X-Kata-Web-Daemon'
const daemonProxyPrefix = '/api/v1/ui/proxy'

export function createDaemonFetch(
  readDaemon: () => string | undefined,
  upstream: typeof fetch = fetch,
): typeof fetch {
  return async (input: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
    const daemonID = readDaemon()
    const rawURL =
      input instanceof Request ? input.url : new URL(String(input), window.location.origin).href
    if (!daemonID || !proxyablePath(rawURL)) return upstream(input, init)

    const request = requestFrom(input, init)
    const target = new URL(request.url)
    target.pathname = daemonProxyPrefix + target.pathname
    const headers = new Headers(request.headers)
    headers.set(daemonHeader, daemonID)
    const body = request.body === null ? undefined : await request.arrayBuffer()
    return upstream(target, {
      method: request.method,
      headers,
      ...(body === undefined ? {} : { body }),
      credentials: request.credentials,
      redirect: request.redirect,
      signal: request.signal,
    })
  }
}

export async function fetchWebDaemons(fetcher: typeof fetch = fetch): Promise<WebDaemonInfo[]> {
  const response = await fetcher('/api/v1/ui/daemons', {
    method: 'GET',
    credentials: 'same-origin',
    redirect: 'error',
  })
  if (response.status === 401) {
    throw new AuthenticationRequiredError(
      'Configured daemons are unavailable',
      responseTriggeredAuthenticationTransition(response),
    )
  }
  if (!response.ok) throw new Error('Configured daemons are unavailable')
  const value = (await response.json()) as unknown
  if (!isRecord(value) || !Array.isArray(value.daemons)) {
    throw new Error('Configured daemons are unavailable')
  }
  const daemons = value.daemons.map(parseDaemon)
  if (daemons.length === 0 || daemons.filter((daemon) => daemon.default).length !== 1) {
    throw new Error('Configured daemons are unavailable')
  }
  return daemons
}

function requestFrom(input: RequestInfo | URL, init?: RequestInit): Request {
  if (input instanceof Request) return new Request(input, init)
  return new Request(new URL(String(input), window.location.origin), init)
}

function proxyablePath(raw: string): boolean {
  const path = new URL(raw).pathname
  if (!path.startsWith('/api/v1/')) return false
  if (path === '/api/v1/ui/daemons' || path.startsWith('/api/v1/ui/session')) return false
  return !path.startsWith(daemonProxyPrefix)
}

function parseDaemon(value: unknown): WebDaemonInfo {
  if (!isRecord(value)) throw new Error('Configured daemons are unavailable')
  const { id, url, default: isDefault, auth, health, hint } = value
  if (
    typeof id !== 'string' ||
    id.length === 0 ||
    typeof url !== 'string' ||
    typeof isDefault !== 'boolean' ||
    (auth !== 'none' && auth !== 'token') ||
    (health !== 'connected' &&
      health !== 'auth_required' &&
      health !== 'down' &&
      health !== 'upgrade_required') ||
    (hint !== undefined && typeof hint !== 'string')
  ) {
    throw new Error('Configured daemons are unavailable')
  }
  return {
    id,
    url,
    default: isDefault,
    auth,
    health,
    ...(typeof hint === 'string' && hint ? { hint } : {}),
  }
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value)
}
