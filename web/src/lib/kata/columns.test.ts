import { describe, expect, it, vi } from 'vitest'

import {
  KATA_OPTIONAL_TASK_COLUMNS,
  KATA_TASK_COLUMNS_STORAGE_KEY,
  defaultKataTaskColumnVisibility,
  loadKataTaskColumnVisibility,
  persistKataTaskColumnVisibility,
  summarizeBlockedBy,
} from './columns'

describe('Kata task column visibility', () => {
  it('defaults every optional column to visible', () => {
    expect(defaultKataTaskColumnVisibility()).toEqual({
      updated: true,
      priority: true,
      due: true,
      owner: true,
      blockedBy: true,
      tags: true,
    })
  })

  it('restores known columns and ignores duplicate or unknown keys', () => {
    const storage = {
      getItem: vi.fn(() => JSON.stringify(['tags', 'future-column', 'updated', 'tags'])),
      setItem: vi.fn(),
    }

    expect(loadKataTaskColumnVisibility(storage)).toEqual({
      updated: true,
      priority: false,
      due: false,
      owner: false,
      blockedBy: false,
      tags: true,
    })
  })

  it.each(['not-json', JSON.stringify({ visible: ['updated'] }), JSON.stringify(['updated', 3])])(
    'falls back for malformed storage: %s',
    (raw) => {
      const storage = { getItem: vi.fn(() => raw), setItem: vi.fn() }
      expect(loadKataTaskColumnVisibility(storage)).toEqual(defaultKataTaskColumnVisibility())
    },
  )

  it('falls back when storage reads throw', () => {
    const storage = {
      getItem: vi.fn(() => {
        throw new Error('blocked')
      }),
      setItem: vi.fn(),
    }

    expect(loadKataTaskColumnVisibility(storage)).toEqual(defaultKataTaskColumnVisibility())
  })

  it('persists visible keys and tolerates write failures', () => {
    const setItem = vi.fn()
    persistKataTaskColumnVisibility(
      { updated: true, priority: false, due: true, owner: false, blockedBy: false, tags: false },
      { getItem: vi.fn(), setItem },
    )
    expect(setItem).toHaveBeenCalledWith(
      KATA_TASK_COLUMNS_STORAGE_KEY,
      JSON.stringify(['updated', 'due']),
    )

    expect(() =>
      persistKataTaskColumnVisibility(defaultKataTaskColumnVisibility(), {
        getItem: vi.fn(),
        setItem: vi.fn(() => {
          throw new Error('quota')
        }),
      }),
    ).not.toThrow()
    expect(KATA_OPTIONAL_TASK_COLUMNS.map((column) => column.id)).toEqual([
      'updated',
      'priority',
      'due',
      'owner',
      'blockedBy',
      'tags',
    ])
  })
})

describe('summarizeBlockedBy', () => {
  const peers = ['a1', 'b2', 'c3', 'd4', 'e5'].map((short_id) => ({
    uid: `uid-${short_id}`,
    short_id,
  }))

  it('returns nothing without blockers', () => {
    expect(summarizeBlockedBy(undefined)).toEqual({ ids: [], more: 0 })
    expect(summarizeBlockedBy([])).toEqual({ ids: [], more: 0 })
  })

  it('lists up to three ids without a remainder', () => {
    expect(summarizeBlockedBy(peers.slice(0, 3))).toEqual({ ids: ['a1', 'b2', 'c3'], more: 0 })
  })

  it('cuts after three ids and counts the rest', () => {
    expect(summarizeBlockedBy(peers)).toEqual({ ids: ['a1', 'b2', 'c3'], more: 2 })
  })
})
