import { describe, expect, it, vi } from 'vitest'

import { openEventStream, parseEventStream } from './sse'

describe('fetch-based event streams', () => {
  it('parses multiline data, ids, event names, and split chunks', async () => {
    const stream = textStream([
      ': connected\n\nid: 17\nevent: issue.updated\ndata: first',
      ' line\ndata: second line\n\n',
    ])

    const frames = []
    for await (const frame of parseEventStream(stream)) frames.push(frame)

    expect(frames).toEqual([{ id: '17', event: 'issue.updated', data: 'first line\nsecond line' }])
  })

  it('resumes from the accepted cursor through Last-Event-ID', async () => {
    const fetcher = vi.fn(async (_input: RequestInfo | URL, init?: RequestInit) => {
      expect(new Headers(init?.headers).get('Last-Event-ID')).toBe('41')
      return new Response(textStream(['id: 42\nevent: issue.updated\ndata: {}\n\n']), {
        status: 200,
        headers: { 'Content-Type': 'text/event-stream' },
      })
    })

    const frames = []
    for await (const frame of openEventStream(fetcher as typeof fetch, 41)) frames.push(frame)
    expect(frames[0]?.id).toBe('42')
  })

  it('does not surface response details in stream failures', async () => {
    const fetcher = vi.fn(async () => new Response('credential-shaped diagnostic', { status: 401 }))

    const consume = async () => {
      for await (const frame of openEventStream(fetcher as typeof fetch, 0)) {
        void frame
        // No frames are expected.
      }
    }
    await expect(consume()).rejects.toThrow('Event stream unavailable')
  })

  it('marks a 401 as expired browser authority without exposing its response', async () => {
    const fetcher = vi.fn(async () => new Response('credential-shaped diagnostic', { status: 401 }))
    const consume = async () => {
      for await (const frame of openEventStream(fetcher as typeof fetch, 0)) void frame
    }

    await expect(consume()).rejects.toMatchObject({
      name: 'AuthenticationRequiredError',
      message: 'Event stream unavailable',
    })
  })
})

function textStream(chunks: string[]): ReadableStream<Uint8Array> {
  const encoder = new TextEncoder()
  return new ReadableStream({
    start(controller) {
      for (const chunk of chunks) controller.enqueue(encoder.encode(chunk))
      controller.close()
    },
  })
}
