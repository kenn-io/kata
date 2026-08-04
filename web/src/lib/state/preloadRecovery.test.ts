import { describe, expect, it, vi } from 'vitest'

import { installPreloadRecovery } from './preloadRecovery'

describe('preload recovery', () => {
  it('allows one guarded reload and shows a version mismatch on a second failure', () => {
    const target = new EventTarget()
    const storage = new Map<string, string>()
    const reload = vi.fn()
    const showMismatch = vi.fn()
    const cleanup = installPreloadRecovery({
      target,
      storage: {
        getItem: (key) => storage.get(key) ?? null,
        setItem: (key, value) => {
          storage.set(key, value)
        },
        removeItem: (key) => {
          storage.delete(key)
        },
      },
      entrypoint: '/assets/index-a.js',
      reload,
      showMismatch,
    })

    const first = new Event('vite:preloadError', { cancelable: true })
    target.dispatchEvent(first)
    expect(first.defaultPrevented).toBe(true)
    expect(reload).toHaveBeenCalledTimes(1)
    expect(showMismatch).not.toHaveBeenCalled()

    const second = new Event('vite:preloadError', { cancelable: true })
    target.dispatchEvent(second)
    expect(second.defaultPrevented).toBe(true)
    expect(reload).toHaveBeenCalledTimes(1)
    expect(showMismatch).toHaveBeenCalledTimes(1)

    cleanup()
  })
})
