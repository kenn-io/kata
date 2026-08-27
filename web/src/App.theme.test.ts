// @vitest-environment-options { "url": "http://127.0.0.2/kata" }

import { cleanup, render } from '@testing-library/svelte'
import { tick } from 'svelte'
import { afterEach, describe, expect, it, vi } from 'vitest'

const themeMedia = vi.hoisted(() => {
  const listeners = new Set<EventListener>()
  Object.defineProperty(window, 'matchMedia', {
    configurable: true,
    value: vi.fn(() => ({
      matches: false,
      media: '(prefers-color-scheme: dark)',
      onchange: null,
      addEventListener: (_type: string, listener: EventListener) => listeners.add(listener),
      removeEventListener: (_type: string, listener: EventListener) => listeners.delete(listener),
      addListener: vi.fn(),
      removeListener: vi.fn(),
      dispatchEvent: vi.fn(),
    })),
  })
  return { listeners }
})

import App from './App.svelte'

describe('App theme lifecycle', () => {
  afterEach(() => {
    cleanup()
    vi.unstubAllGlobals()
  })

  it('removes the system-theme listener when the app unmounts', async () => {
    const view = render(App)
    await tick()
    expect(themeMedia.listeners.size).toBe(1)

    view.unmount()
    expect(themeMedia.listeners.size).toBe(0)
  })
})
