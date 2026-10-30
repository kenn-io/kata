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
  it('defaults every optional column to visible except Blocked by', () => {
    expect(defaultKataTaskColumnVisibility()).toEqual({
      updated: true,
      priority: true,
      due: true,
      owner: true,
      blockedBy: false,
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
    qualified_id: `example-project#${short_id}`,
  }))
  const row = 'example-project#row'

  it('returns empty parts without blockers', () => {
    expect(summarizeBlockedBy(undefined, row)).toEqual({ ids: '', more: '', title: '' })
    expect(summarizeBlockedBy([], row)).toEqual({ ids: '', more: '', title: '' })
  })

  it('lists up to three ids without a remainder', () => {
    expect(summarizeBlockedBy(peers.slice(0, 3), row)).toEqual({
      ids: 'a1, b2, c3',
      more: '',
      title: 'example-project#a1, example-project#b2, example-project#c3',
    })
  })

  it('cuts after three ids, reports +N separately, and titles every blocker', () => {
    const summary = summarizeBlockedBy(peers, row)
    expect(summary.ids).toBe('a1, b2, c3')
    expect(summary.more).toBe('+2')
    expect(summary.title).toBe(peers.map((peer) => peer.qualified_id).join(', '))
  })

  it('qualifies only blockers from another project', () => {
    const mixed = [peers[0]!, { uid: 'uid-z9', short_id: 'z9', qualified_id: 'other-project#z9' }]
    expect(summarizeBlockedBy(mixed, row).ids).toBe('a1, other-project#z9')
  })
})
