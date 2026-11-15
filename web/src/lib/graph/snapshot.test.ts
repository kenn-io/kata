import { describe, expect, test } from 'vitest'

import type { KataReachableGraphResponse } from '../kata/types'
import { task } from '../../test/task'
import { filterSnapshotGraph } from './snapshot'

describe('filterSnapshotGraph', () => {
  test.each([
    ['1', false, ['root', 'a', 'b'], [0, 1, 6]],
    ['2', false, ['root', 'a', 'b', 'c', 'd'], [0, 1, 2, 3, 4, 6]],
    ['3', false, ['root', 'a', 'b', 'c', 'd', 'e'], [0, 1, 2, 3, 4, 5, 6]],
    ['full', false, ['root', 'a', 'b', 'c', 'd', 'e', 'f'], [0, 1, 2, 3, 4, 5, 6, 7]],
    ['1', true, ['root', 'a'], [0, 6]],
    ['2', true, ['root', 'a', 'c'], [0, 3, 6]],
    ['3', true, ['root', 'a', 'c'], [0, 3, 6]],
    ['full', true, ['root', 'a', 'c'], [0, 3, 6]],
  ] as const)(
    'preserves output at depth %s with hide-done %s',
    (depth, hideDone, uids, edgeIndexes) => {
      const snapshot: KataReachableGraphResponse = {
        source_uid: 'root',
        depth: 'full',
        hide_done: false,
        nodes: [
          task({ uid: 'root', status: 'closed' }),
          task({ uid: 'a' }),
          task({ uid: 'b', status: 'closed' }),
          task({ uid: 'c' }),
          task({ uid: 'd' }),
          task({ uid: 'e' }),
          task({ uid: 'f' }),
          task({ uid: 'isolated' }),
        ],
        edges: [
          { from_uid: 'a', to_uid: 'root', kind: 'parent', layout: true },
          { from_uid: 'root', to_uid: 'b', kind: 'blocks', layout: true },
          { from_uid: 'b', to_uid: 'c', kind: 'related', layout: false },
          { from_uid: 'c', to_uid: 'a', kind: 'blocks', layout: true },
          { from_uid: 'd', to_uid: 'b', kind: 'parent', layout: true },
          { from_uid: 'd', to_uid: 'e', kind: 'related', layout: false },
          { from_uid: 'root', to_uid: 'a', kind: 'related', layout: false },
          { from_uid: 'e', to_uid: 'f', kind: 'blocks', layout: true },
        ],
        unresolved_refs: ['root', 'a', 'b', 'c', 'd', 'e', 'f', 'isolated'].map((uid) => ({
          uid: `missing-${uid}`,
          other_uid: uid,
          side: 'to',
          kind: 'blocks',
        })),
        fetched_at: '2026-08-01T12:00:00Z',
      }
      const result = filterSnapshotGraph(snapshot, 'root', depth, hideDone)
      expect(result).toEqual({
        source_uid: 'root',
        depth,
        hide_done: hideDone,
        nodes: uids.map((uid) =>
          task({ uid, status: uid === 'root' || uid === 'b' ? 'closed' : 'open' }),
        ),
        edges: edgeIndexes.map((index) => snapshot.edges[index]),
        unresolved_refs: uids.map((uid) => ({
          uid: `missing-${uid}`,
          other_uid: uid,
          side: 'to',
          kind: 'blocks',
        })),
        fetched_at: '2026-08-01T12:00:00Z',
      })
    },
  )
})
