import { describe, expect, test, vi } from 'vitest'

import type { captureTelemetryEventResponse } from '../api/generated'
import { createAppOpenedReporter } from './appOpened'

type Send = (body: { event: string }) => Promise<captureTelemetryEventResponse>

interface Pending {
  day: string
  session: string | undefined
  resolve: (status: number) => void
  reject: (error: unknown) => void
}

function harness(start: string, session: string | null = 'session-a') {
  let clock = new Date(start)
  let current = session ?? undefined
  const requests: Pending[] = []
  const send = vi.fn<Send>(
    (body) =>
      new Promise<captureTelemetryEventResponse>((resolve, reject) => {
        expect(body).toEqual({ event: 'app_opened' })
        requests.push({
          day: clock.toISOString().slice(0, 10),
          session: current,
          resolve: (status) =>
            resolve({ status, data: undefined, headers: new Headers() } as never),
          reject,
        })
      }),
  )
  const reporter = createAppOpenedReporter(
    send,
    () => clock,
    () => current,
  )
  return {
    reporter,
    send,
    requests,
    at(time: string) {
      clock = new Date(time)
    },
    useSession(next: string | undefined) {
      current = next
    },
  }
}

const settle = () => new Promise<void>((resolve) => setTimeout(resolve, 0))

describe('app_opened reporter', () => {
  test('sends once with a session and waits for one without it', async () => {
    const withSession = harness('2026-10-02T12:00:00Z')
    withSession.reporter.opened()
    expect(withSession.send).toHaveBeenCalledTimes(1)
    expect(withSession.send).toHaveBeenCalledWith({ event: 'app_opened' })

    const anonymous = harness('2026-10-02T12:00:00Z', null)
    anonymous.reporter.opened()
    expect(anonymous.send).not.toHaveBeenCalled()
    anonymous.useSession('session-b')
    anonymous.reporter.sessionAvailable()
    expect(anonymous.send).toHaveBeenCalledTimes(1)
  })

  test('sends nothing for a second opening on the same UTC date or with nothing armed', async () => {
    const h = harness('2026-10-02T12:00:00Z')
    h.reporter.opened()
    h.requests[0]!.resolve(202)
    await settle()
    h.reporter.opened()
    h.reporter.sessionAvailable()
    expect(h.send).toHaveBeenCalledTimes(1)
  })

  test('sends again on the next UTC date', async () => {
    const h = harness('2026-10-02T23:59:59Z')
    h.reporter.opened()
    h.requests[0]!.resolve(202)
    await settle()
    h.at('2026-10-03T00:00:01Z')
    h.reporter.opened()
    expect(h.send).toHaveBeenCalledTimes(2)
  })

  test('keys the day on UTC rather than local time', async () => {
    const h = harness('2026-10-02T00:30:00Z')
    h.reporter.opened()
    h.requests[0]!.resolve(202)
    await settle()
    h.at('2026-10-02T23:30:00Z')
    h.reporter.opened()
    expect(h.send).toHaveBeenCalledTimes(1)
  })

  test.each([
    ['403', (pending: Pending) => pending.resolve(403)],
    ['500', (pending: Pending) => pending.resolve(500)],
    ['rejection', (pending: Pending) => pending.reject(new Error('offline'))],
  ])('a %s keeps the day marked', async (_name, fail) => {
    const h = harness('2026-10-02T12:00:00Z')
    h.reporter.opened()
    fail(h.requests[0]!)
    await settle()
    h.reporter.opened()
    h.useSession('session-b')
    h.reporter.sessionAvailable()
    expect(h.send).toHaveBeenCalledTimes(1)
  })

  test('a 401 re-arms the day for the next session', async () => {
    const h = harness('2026-10-02T12:00:00Z')
    h.reporter.opened()
    h.requests[0]!.resolve(401)
    await settle()
    expect(h.send).toHaveBeenCalledTimes(1)
    h.useSession('session-b')
    h.reporter.sessionAvailable()
    expect(h.send).toHaveBeenCalledTimes(2)
    expect(h.requests[1]!.session).toBe('session-b')
  })

  test('a 401 that settles after the session changed sends right away', async () => {
    const h = harness('2026-10-02T12:00:00Z')
    h.reporter.opened()
    h.useSession('session-b')
    h.requests[0]!.resolve(401)
    await settle()
    expect(h.send).toHaveBeenCalledTimes(2)
    expect(h.requests[1]!.session).toBe('session-b')
  })

  test('a late 401 for an older day never blocks the next day', async () => {
    const h = harness('2026-10-02T23:59:00Z')
    h.reporter.opened()
    h.at('2026-10-03T00:01:00Z')
    h.reporter.opened()
    expect(h.send).toHaveBeenCalledTimes(2)
    h.requests[1]!.resolve(202)
    await settle()
    h.requests[0]!.resolve(401)
    await settle()
    h.useSession('session-b')
    h.reporter.sessionAvailable()
    h.reporter.opened()
    expect(h.send).toHaveBeenCalledTimes(2)
  })

  test('a request pending across midnight does not block the next day', () => {
    const h = harness('2026-10-02T23:59:59Z')
    h.reporter.opened()
    h.at('2026-10-03T00:00:30Z')
    h.reporter.opened()
    expect(h.send).toHaveBeenCalledTimes(2)
    expect(h.requests.map((request) => request.day)).toEqual(['2026-10-02', '2026-10-03'])
  })

  test('an armed opening expires at UTC midnight when credentials arrive late', () => {
    const h = harness('2026-10-02T23:59:59Z', null)
    h.reporter.opened()
    h.at('2026-10-03T00:00:01Z')
    h.useSession('session-b')
    h.reporter.sessionAvailable()
    expect(h.send).not.toHaveBeenCalled()
    h.at('2026-10-03T00:00:02Z')
    h.reporter.opened()
    expect(h.send).toHaveBeenCalledTimes(1)
    expect(h.requests[0]!.day).toBe('2026-10-03')
  })

  test('a 401 settling after midnight drops the old day', async () => {
    const h = harness('2026-10-02T23:59:00Z')
    h.reporter.opened()
    h.at('2026-10-03T00:00:01Z')
    h.useSession('session-b')
    h.requests[0]!.resolve(401)
    await settle()
    expect(h.requests.map((request) => request.day)).toEqual(['2026-10-02'])
    h.reporter.opened()
    expect(h.requests.map((request) => request.day)).toEqual(['2026-10-02', '2026-10-03'])
  })
})
