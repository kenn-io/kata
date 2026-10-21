import type { SessionCredentials } from '../auth/session'
import { loadSessionCredentials } from '../auth/session'
import { applicationRequest, applicationURL } from '../applicationBase'

type CredentialReader = () => SessionCredentials | undefined
type AuthenticationRequiredHandler = () => boolean

const authenticationTransitionResponses = new WeakSet<Response>()

export function responseTriggeredAuthenticationTransition(response: Response): boolean {
  return authenticationTransitionResponses.has(response)
}

export function createCredentialedFetch(
  readCredentials: CredentialReader = () => loadSessionCredentials(),
  upstream: typeof fetch = fetch,
  onAuthenticationRequired: AuthenticationRequiredHandler = () => false,
): typeof fetch {
  return async (input: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
    const request = input instanceof Request ? input : undefined
    const headers = new Headers(request?.headers)
    if (init?.headers) {
      new Headers(init.headers).forEach((value, name) => headers.set(name, value))
    }
    const credentials = readCredentials()
    if (credentials) {
      headers.set('X-Kata-Web-Session', credentials.session)
      const method = (init?.method ?? request?.method ?? 'GET').toUpperCase()
      if (mutationMethods.has(method)) {
        headers.set('X-Kata-CSRF', credentials.csrf)
      }
    }
    const response = await upstream(applicationRequest(input), {
      ...init,
      credentials: 'same-origin',
      redirect: 'error',
      headers,
    })
    if (response.status === 401 && readCredentials()?.session === credentials?.session) {
      if (onAuthenticationRequired()) authenticationTransitionResponses.add(response)
    }
    return response
  }
}

let generatedFetch: typeof fetch = createCredentialedFetch()

export function setGeneratedFetch(fetcher: typeof fetch): void {
  generatedFetch = fetcher
}

export async function orvalFetch<T>(
  url: string,
  options: RequestInit,
  fetcher: typeof fetch = generatedFetch,
): Promise<T> {
  const response = await fetcher(applicationURL(url), options)
  const text = await response.text()
  const data = response.headers.get('Content-Type')?.includes('json') ? JSON.parse(text) : text
  return {
    data: text ? data : undefined,
    status: response.status,
    headers: response.headers,
  } as T
}

// Uses the active daemon transport, tab credentials, and authentication rejection path.
export async function fetchAttachmentBlob(url: string): Promise<Blob> {
  const response = await generatedFetch(applicationURL(url), { method: 'GET' })
  if (!response.ok) {
    const body = (await response.json().catch(() => undefined)) as
      | { error?: { code?: string } }
      | undefined
    throw new Error(body?.error?.code ?? 'docbank_unreachable')
  }
  return response.blob()
}

export async function uploadAttachment(
  projectID: number,
  ref: string,
  file: File,
  actor: string,
  headers?: Headers,
): Promise<{ data: unknown; status: number; headers: Headers }> {
  const query = new URLSearchParams({ name: file.name })
  if (actor) query.set('actor', actor)
  const response = await generatedFetch(
    applicationURL(
      `/api/v1/projects/${projectID}/issues/${encodeURIComponent(ref)}/attachments?${query}`,
    ),
    { method: 'POST', body: file, ...(headers ? { headers } : {}) },
  )
  const data: unknown = await response.json()
  return { data, status: response.status, headers: response.headers }
}

const mutationMethods = new Set(['POST', 'PUT', 'PATCH', 'DELETE'])
