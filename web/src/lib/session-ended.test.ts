import { afterEach, expect, it, vi } from 'vitest'
import { startSessionEndedReporting } from './session-ended'

let stop: (() => void) | undefined
afterEach(() => {
  stop?.()
  vi.restoreAllMocks()
})

it.each([
  [59_999, 'under_1m'],
  [60_000, '1_to_5m'],
  [120_000, '1_to_5m'],
  [300_000, '5_to_30m'],
  [1_800_000, '5_to_30m'],
  [1_800_001, 'over_30m'],
])('reports %i visible milliseconds as %s once', (elapsed, bucket) => {
  let now = 0
  let hidden = false
  vi.spyOn(performance, 'now').mockImplementation(() => now)
  vi.spyOn(document, 'hidden', 'get').mockImplementation(() => hidden)
  const post = vi.fn(async () => undefined)
  stop = startSessionEndedReporting(post).stop
  now = elapsed
  hidden = true
  document.dispatchEvent(new Event('visibilitychange'))
  window.dispatchEvent(new Event('pagehide'))
  expect(post).toHaveBeenCalledOnce()
  expect(post).toHaveBeenCalledWith(bucket)
})

it('excludes hidden time and removes listeners on disposal', () => {
  let now = 0
  let hidden = true
  vi.spyOn(performance, 'now').mockImplementation(() => now)
  vi.spyOn(document, 'hidden', 'get').mockImplementation(() => hidden)
  const post = vi.fn(async () => undefined)
  stop = startSessionEndedReporting(post).stop
  now = 600_000
  window.dispatchEvent(new Event('pagehide'))
  hidden = false
  window.dispatchEvent(new Event('pageshow'))
  now += 120_000
  hidden = true
  document.dispatchEvent(new Event('visibilitychange'))
  now += 600_000
  hidden = false
  document.dispatchEvent(new Event('visibilitychange'))
  now += 120_000
  window.dispatchEvent(new Event('pagehide'))
  expect(post.mock.calls).toEqual([['1_to_5m'], ['1_to_5m']])
  stop()
  window.dispatchEvent(new Event('pageshow'))
  window.dispatchEvent(new Event('pagehide'))
  expect(post).toHaveBeenCalledTimes(2)
})

it.each([false, true])(
  'keeps elapsed time while authentication recovers, hidden=%s',
  (hideDuringRecovery) => {
    let now = 0
    let hidden = false
    vi.spyOn(performance, 'now').mockImplementation(() => now)
    vi.spyOn(document, 'hidden', 'get').mockImplementation(() => hidden)
    const post = vi.fn(async () => undefined)
    const tracker = startSessionEndedReporting(post)
    stop = tracker.stop
    now = 120_000
    tracker.pause()
    if (hideDuringRecovery) {
      hidden = true
      document.dispatchEvent(new Event('visibilitychange'))
      window.dispatchEvent(new Event('pagehide'))
    }
    now += 600_000
    expect(post).not.toHaveBeenCalled()
    tracker.resume()
    if (!hideDuringRecovery) {
      now += 10_000
      window.dispatchEvent(new Event('pagehide'))
    }
    expect(post.mock.calls).toEqual([['1_to_5m']])
  },
)
