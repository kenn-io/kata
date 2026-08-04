import { AuthenticationRequiredError } from '../auth/session'

export interface EventFrame {
  id: string
  event: string
  data: string
}

export async function* openEventStream(
  fetcher: typeof fetch,
  cursor: number,
  signal?: AbortSignal,
): AsyncGenerator<EventFrame> {
  const headers = new Headers({ Accept: 'text/event-stream' })
  if (cursor > 0) headers.set('Last-Event-ID', String(cursor))
  let response: Response
  try {
    const init: RequestInit = {
      method: 'GET',
      credentials: 'same-origin',
      headers,
    }
    if (signal) init.signal = signal
    response = await fetcher('/api/v1/events/stream', init)
  } catch {
    throw new Error('Event stream unavailable')
  }
  if (response.status === 401) throw new AuthenticationRequiredError('Event stream unavailable')
  if (!response.ok || !response.body) throw new Error('Event stream unavailable')
  yield* parseEventStream(response.body)
}

export async function* parseEventStream(
  stream: ReadableStream<Uint8Array>,
): AsyncGenerator<EventFrame> {
  const reader = stream.getReader()
  const decoder = new TextDecoder()
  let buffer = ''
  let id = ''
  let event = ''
  let data: string[] = []

  const dispatch = (): EventFrame | undefined => {
    if (data.length === 0) return undefined
    const frame = { id, event: event || 'message', data: data.join('\n') }
    event = ''
    data = []
    return frame
  }

  const consumeLine = (line: string): EventFrame | undefined => {
    if (line === '') return dispatch()
    if (line.startsWith(':')) return undefined
    const separator = line.indexOf(':')
    const field = separator < 0 ? line : line.slice(0, separator)
    let value = separator < 0 ? '' : line.slice(separator + 1)
    if (value.startsWith(' ')) value = value.slice(1)
    if (field === 'data') data.push(value)
    if (field === 'event') event = value
    if (field === 'id' && !value.includes('\0')) id = value
    return undefined
  }

  try {
    while (true) {
      const { done, value } = await reader.read()
      if (done) break
      buffer += decoder.decode(value, { stream: true })
      let newline = buffer.indexOf('\n')
      while (newline >= 0) {
        let line = buffer.slice(0, newline)
        if (line.endsWith('\r')) line = line.slice(0, -1)
        buffer = buffer.slice(newline + 1)
        const frame = consumeLine(line)
        if (frame) yield frame
        newline = buffer.indexOf('\n')
      }
    }
    buffer += decoder.decode()
    if (buffer) {
      const frame = consumeLine(buffer.endsWith('\r') ? buffer.slice(0, -1) : buffer)
      if (frame) yield frame
    }
    const frame = dispatch()
    if (frame) yield frame
  } finally {
    reader.releaseLock()
  }
}
