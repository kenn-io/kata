import { describe, expect, it } from 'vitest'

import {
  defaultPreferences,
  loadPreferences,
  originStabilityWarning,
  savePreferences,
} from './preferences'

describe('origin-local preferences', () => {
  it('defaults to the beside-list layout and pixel sash size', () => {
    expect(loadPreferences(new MapStorage())).toMatchObject({
      splitDirection: 'horizontal',
      splitSize: 480,
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

  it('remembers compact board cards and collapsed board columns', () => {
    const storage = new MapStorage()
    savePreferences(
      {
        ...defaultPreferences,
        boardCompact: true,
        collapsedBoardColumns: ['done', 'label:review', 'done'],
      },
      storage,
    )

    expect(loadPreferences(storage)).toMatchObject({
      boardCompact: true,
      collapsedBoardColumns: ['done', 'label:review'],
    })
  })

  it('starts with comfortable cards, every board column open, and remote highlights on', () => {
    expect(loadPreferences(new MapStorage())).toMatchObject({
      boardCompact: false,
      collapsedBoardColumns: [],
      boardHighlightRemote: true,
    })
  })

  it('remembers turning remote-change highlights off', () => {
    const storage = new MapStorage()
    savePreferences({ ...defaultPreferences, boardHighlightRemote: false }, storage)

    expect(loadPreferences(storage).boardHighlightRemote).toBe(false)
  })

  it('ignores malformed board settings', () => {
    const storage = new MapStorage()
    storage.setItem(
      'kata.preferences.v1',
      JSON.stringify({ boardCompact: 'yes', collapsedBoardColumns: [1, 2] }),
    )

    expect(loadPreferences(storage)).toMatchObject({
      boardCompact: false,
      collapsedBoardColumns: [],
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
