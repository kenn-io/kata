export const sessionStorageKey = 'kata.web.session.v1'
export const authenticationModeStorageKey = 'kata.web.authentication-mode.v1'

export type AuthenticationMode = 'login'

export class AuthenticationRequiredError extends Error {
  constructor(message: string) {
    super(message)
    this.name = 'AuthenticationRequiredError'
  }
}

export function isAuthenticationRequiredError(error: unknown): boolean {
  return (
    error instanceof AuthenticationRequiredError ||
    (error instanceof Error && error.name === 'AuthenticationRequiredError')
  )
}

export interface SessionCredentials {
  session: string
  csrf: string
}

export interface SessionCapabilities {
  writable: boolean
  updates: 'sse' | 'poll'
  actorPolicy: string
}

export interface AuthenticatedSession {
  kind: 'authenticated'
  credentials: SessionCredentials
  capabilities: SessionCapabilities
  returnPath: string
}

export type LaunchState =
  | { kind: 'login'; returnPath: string }
  | { kind: 'none'; returnPath: string }

export function selectAuthenticationMode(
  launch: LaunchState,
  storage: Storage = sessionStorage,
): AuthenticationMode | undefined {
  if (launch.kind === 'login') {
    storage.setItem(authenticationModeStorageKey, 'login')
    return 'login'
  }
  const stored = storage.getItem(authenticationModeStorageKey)
  return stored === 'login' ? stored : undefined
}

interface SessionResponse {
  session: string
  csrf: string
  return_path: string
  writable: boolean
  updates: 'sse' | 'poll'
  actor_policy: string
}

export function consumeLaunchFragment(
  location: Location,
  replaceState: History['replaceState'],
): LaunchState {
  const currentPath = location.pathname + location.search
  if (!location.hash) {
    return { kind: 'none', returnPath: currentPath }
  }

  const fragment = new URLSearchParams(location.hash.slice(1))
  replaceState(null, '', currentPath)
  const returnPath = normalizeReturnPath(fragment.get('return_path'), currentPath)
  if (fragment.get('login') === '1') {
    return { kind: 'login', returnPath }
  }
  return { kind: 'none', returnPath: currentPath }
}

export async function openLocalSession(
  returnPath: string,
  fetcher: typeof fetch = fetch,
  storage: Storage = sessionStorage,
): Promise<AuthenticatedSession | undefined> {
  const response = await fetcher('/api/v1/ui/session/local', {
    method: 'POST',
    credentials: 'same-origin',
    redirect: 'error',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ return_path: normalizeReturnPath(returnPath, '/kata') }),
  })
  if ([401, 403, 404].includes(response.status)) return undefined
  return acceptSessionResponse(response, storage, 'Local authorization failed')
}

export async function openTrustedProxySession(
  returnPath: string,
  fetcher: typeof fetch = fetch,
  storage: Storage = sessionStorage,
): Promise<AuthenticatedSession> {
  const response = await fetcher('/api/v1/ui/session/proxy', {
    method: 'POST',
    credentials: 'same-origin',
    redirect: 'error',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ return_path: normalizeReturnPath(returnPath, '/kata') }),
  })
  return acceptSessionResponse(response, storage, 'Trusted proxy authorization failed')
}

export async function exchangeLoginToken(
  token: string,
  returnPath: string,
  fetcher: typeof fetch = fetch,
  storage: Storage = sessionStorage,
): Promise<AuthenticatedSession> {
  const response = await fetcher('/api/v1/ui/session/login', {
    method: 'POST',
    credentials: 'same-origin',
    redirect: 'error',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ token, return_path: normalizeReturnPath(returnPath, '/kata') }),
  })
  return acceptSessionResponse(response, storage, 'Login failed')
}

export function loadSessionCredentials(
  storage: Storage = sessionStorage,
): SessionCredentials | undefined {
  const raw = storage.getItem(sessionStorageKey)
  if (!raw) return undefined
  try {
    const parsed = JSON.parse(raw) as Partial<SessionCredentials>
    if (
      typeof parsed.session === 'string' &&
      parsed.session &&
      typeof parsed.csrf === 'string' &&
      parsed.csrf
    ) {
      return { session: parsed.session, csrf: parsed.csrf }
    }
  } catch {
    // Invalid tab-local state is equivalent to an expired session.
  }
  storage.removeItem(sessionStorageKey)
  return undefined
}

export function clearSessionCredentials(storage: Storage = sessionStorage): void {
  storage.removeItem(sessionStorageKey)
}

async function acceptSessionResponse(
  response: Response,
  storage: Storage,
  failureMessage: string,
): Promise<AuthenticatedSession> {
  if (!response.ok) {
    throw new Error(failureMessage)
  }
  const body = (await response.json()) as Partial<SessionResponse>
  if (
    typeof body.session !== 'string' ||
    !body.session ||
    typeof body.csrf !== 'string' ||
    !body.csrf ||
    typeof body.return_path !== 'string' ||
    typeof body.writable !== 'boolean' ||
    (body.updates !== 'sse' && body.updates !== 'poll') ||
    typeof body.actor_policy !== 'string'
  ) {
    throw new Error(failureMessage)
  }
  const credentials = { session: body.session, csrf: body.csrf }
  storage.setItem(sessionStorageKey, JSON.stringify(credentials))
  return {
    kind: 'authenticated',
    credentials,
    capabilities: {
      writable: body.writable,
      updates: body.updates,
      actorPolicy: body.actor_policy,
    },
    returnPath: normalizeReturnPath(body.return_path, '/kata'),
  }
}

function normalizeReturnPath(value: string | null, fallback: string): string {
  if (!value || !value.startsWith('/') || value.startsWith('//')) return fallback
  try {
    const parsed = new URL(value, 'https://daemon.example')
    if (parsed.origin !== 'https://daemon.example') return fallback
    return parsed.pathname + parsed.search
  } catch {
    return fallback
  }
}
