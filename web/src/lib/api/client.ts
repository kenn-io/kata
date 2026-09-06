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

const mutationMethods = new Set(['POST', 'PUT', 'PATCH', 'DELETE'])
