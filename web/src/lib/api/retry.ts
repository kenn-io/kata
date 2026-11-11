import { ReconnectBackoff, waitForReconnect } from '../events/controller'

// Use only for safe GET requests.
export async function retryTransientRead<T extends { status: number }>(
  read: () => Promise<T>,
  signal: AbortSignal,
  retries = 3,
): Promise<T> {
  const backoff = new ReconnectBackoff()
  for (let attempt = 0; ; attempt++) {
    try {
      const response = await read()
      if (
        attempt >= retries ||
        (response.status !== 408 && response.status !== 429 && response.status < 500)
      )
        return response
    } catch (error) {
      if (
        signal.aborted ||
        (error instanceof Error && error.name === 'AbortError') ||
        attempt >= retries
      )
        throw error
    }
    if (signal.aborted) throw new DOMException('Aborted', 'AbortError')
    await waitForReconnect(backoff.afterDisconnect(false), signal)
  }
}
