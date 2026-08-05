import { describe, expect, it } from 'vitest'

import {
  defaultPreferences,
  loadPreferences,
  originStabilityWarning,
  savePreferences,
} from './preferences'

describe('origin-local preferences', () => {
  it('defaults to the source stacked layout and pixel sash size', () => {
    expect(loadPreferences(new MapStorage())).toMatchObject({
      splitDirection: 'vertical',
      splitSize: 420,
    })
  })

  it('persists only presentation preferences under the versioned key', () => {
    const storage = new MapStorage()
    savePreferences(
      {
        ...defaultPreferences,
        theme: 'dark',
        columns: ['owner', 'priority'],
        splitDirection: 'vertical',
        splitSize: 520,
        collapsedGroups: ['system'],
        session: 'must-not-persist',
      },
      storage,
    )

    const raw = [...storage.values()][0]
    expect(raw).toContain('"theme":"dark"')
    expect(raw).not.toContain('session')
    expect(loadPreferences(storage)).toMatchObject({
      theme: 'dark',
      columns: ['owner', 'priority'],
      splitDirection: 'vertical',
      splitSize: 520,
      collapsedGroups: ['system'],
    })
  })

  it('reports degraded origins without copying preference state', () => {
    expect(originStabilityWarning(false)).toContain('temporary browser origin')
    expect(originStabilityWarning(true)).toBeUndefined()
  })
})

class MapStorage implements Storage {
  readonly #data = new Map<string, string>()

  get length(): number {
    return this.#data.size
  }

  clear(): void {
    this.#data.clear()
  }

  getItem(key: string): string | null {
    return this.#data.get(key) ?? null
  }

  key(index: number): string | null {
    return [...this.#data.keys()][index] ?? null
  }

  removeItem(key: string): void {
    this.#data.delete(key)
  }

  setItem(key: string, value: string): void {
    this.#data.set(key, value)
  }

  values(): IterableIterator<string> {
    return this.#data.values()
  }
}
