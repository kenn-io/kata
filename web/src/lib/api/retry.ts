import { ReconnectBackoff, waitForReconnect } from '../events/controller'

// Use only for safe GET requests.
export async function retryTransientRead(
  read: () => Promise<Response>,
  signal: AbortSignal,
  readBody: (response: Response) => Promise<unknown>,
): Promise<{ response: Response; body?: unknown }> {
  const backoff = new ReconnectBackoff()
  let waited = 0
  for (let attempt = 0; ; attempt++) {
    if (signal.aborted) throw new DOMException('Aborted', 'AbortError')
    let response: Response | undefined
    let failure: unknown
    try {
      response = await read()
      if (response.ok) return { response, body: await readBody(response) }
    } catch (error) {
      if (signal.aborted || (error instanceof Error && error.name === 'AbortError') || attempt >= 3)
        throw error
      failure = error
      response = undefined
    }
    if (response && (attempt >= 3 || !isTransient(response.status))) return { response }
    const delay = Math.max(backoff.afterDisconnect(false), response ? retryAfter(response) : 0)
    if (waited + delay > 30_000) {
      if (response) return { response }
      throw failure
    }
    waited += delay
    if (response?.body) void response.body.cancel().catch(() => {})
    if (signal.aborted) throw new DOMException('Aborted', 'AbortError')
    await waitForReconnect(delay, signal)
  }
}

function isTransient(status: number): boolean {
  return status === 408 || status === 429 || (status >= 500 && status <= 599)
}

function retryAfter(response: Response): number {
  if (response.status !== 429 && response.status !== 503) return 0
  const value = response.headers.get('Retry-After')?.trim()
  if (!value) return 0
  if (/^\d+$/.test(value)) return Number(value) * 1000
  const date = Date.parse(value)
  return Number.isNaN(date) ? 0 : Math.max(0, date - Date.now())
}
