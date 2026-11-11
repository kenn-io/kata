import type { SessionCredentials } from '../auth/session'
import { loadSessionCredentials } from '../auth/session'
import { applicationRequest, applicationURL } from '../applicationBase'
import { retryTransientRead } from './retry'

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

async function readRawResponseBody(response: Response): Promise<string | Blob | undefined> {
  const contentType = response.headers.get('Content-Type')
  if (contentType && !contentType.includes('json') && !contentType.startsWith('text/')) {
    return response.blob()
  }
  const text = await response.text()
  return text || undefined
}

export async function orvalFetch<T>(
  url: string,
  options: RequestInit & ({ retry: true; signal: AbortSignal } | { retry?: false }),
  fetcher: typeof fetch = generatedFetch,
): Promise<T> {
  const { retry, ...requestOptions } = options
  const read = () => fetcher(applicationURL(url), requestOptions)
  const result = retry
    ? await retryTransientRead(read, options.signal, readRawResponseBody)
    : { response: await read() }
  const response = result.response
  const body = 'body' in result ? result.body : await readRawResponseBody(response)
  const data =
    typeof body === 'string' && response.headers.get('Content-Type')?.includes('json')
      ? JSON.parse(body)
      : body
  return {
    data,
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

const mutationMethods = new Set(['POST', 'PUT', 'PATCH', 'DELETE'])
