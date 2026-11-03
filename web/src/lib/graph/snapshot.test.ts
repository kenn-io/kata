import { describe, expect, test } from 'vitest'

import type { KataReachableGraphResponse } from '../kata/types'
import { task } from '../kata/test-helpers'
import { graphDistanceByUID } from './distance'
import { filterSnapshotGraph } from './snapshot'

describe('filterSnapshotGraph', () => {
  test.each([
    ['1', false, ['root', 'a', 'b'], [0, 1, 6, 7], [0, 1, 1]],
    ['2', false, ['root', 'a', 'b', 'c'], [0, 1, 2, 3, 6, 7], [0, 1, 1, 2]],
    ['3', false, ['root', 'a', 'b', 'c', 'd'], [0, 1, 2, 3, 4, 6, 7], [0, 1, 1, 2, 3]],
    [
      'full',
      false,
      ['root', 'a', 'b', 'c', 'd', 'e'],
      [0, 1, 2, 3, 4, 5, 6, 7],
      [0, 1, 1, 2, 3, 4],
    ],
    ['1', true, ['root', 'a'], [0, 6, 7], [0, 1]],
    ['2', true, ['root', 'a', 'c'], [0, 3, 6, 7], [0, 1, 2]],
    ['3', true, ['root', 'a', 'c', 'd'], [0, 3, 4, 6, 7], [0, 1, 2, 3]],
    ['full', true, ['root', 'a', 'c', 'd', 'e'], [0, 3, 4, 5, 6, 7], [0, 1, 2, 3, 4]],
  ] as const)(
    'preserves output at depth %s with hide-done %s',
    (depth, hideDone, uids, edgeIndexes, distances) => {
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
          task({ uid: 'isolated' }),
        ],
        edges: [
          { from_uid: 'a', to_uid: 'root', kind: 'parent', layout: true },
          { from_uid: 'root', to_uid: 'b', kind: 'blocks', layout: true },
          { from_uid: 'b', to_uid: 'c', kind: 'related', layout: false },
          { from_uid: 'c', to_uid: 'a', kind: 'blocks', layout: true },
          { from_uid: 'd', to_uid: 'c', kind: 'parent', layout: true },
          { from_uid: 'd', to_uid: 'e', kind: 'related', layout: false },
          { from_uid: 'root', to_uid: 'a', kind: 'related', layout: false },
          { from_uid: 'a', to_uid: 'a', kind: 'related', layout: false },
        ],
        unresolved_refs: ['root', 'a', 'b', 'c', 'd', 'e', 'isolated'].map((uid) => ({
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
      const distanceByUID = graphDistanceByUID(
        'root',
        snapshot.edges.filter(
          (edge) => !hideDone || (edge.from_uid !== 'b' && edge.to_uid !== 'b'),
        ),
        depth === 'full' ? Number.POSITIVE_INFINITY : Number(depth),
      )
      expect(distanceByUID.size).toBe(uids.length)
      expect(uids.map((uid) => distanceByUID.get(uid))).toEqual(distances)
    },
  )

  test('stops at the selected depth on a longer chain', () => {
    const snapshot: KataReachableGraphResponse = {
      source_uid: 'root',
      depth: 'full',
      hide_done: false,
      nodes: ['root', 'a', 'b', 'c', 'd'].map((uid) => task({ uid })),
      edges: [
        { from_uid: 'root', to_uid: 'a', kind: 'blocks', layout: true },
        { from_uid: 'a', to_uid: 'b', kind: 'blocks', layout: true },
        { from_uid: 'b', to_uid: 'c', kind: 'blocks', layout: true },
        { from_uid: 'c', to_uid: 'd', kind: 'blocks', layout: true },
      ],
      unresolved_refs: [],
      fetched_at: '2026-08-01T12:00:00Z',
    }

    expect([...graphDistanceByUID('root', snapshot.edges, 2)]).toEqual([
      ['root', 0],
      ['a', 1],
      ['b', 2],
    ])
    const result = filterSnapshotGraph(snapshot, 'root', '2', false)
    expect(result.nodes.map((node) => node.uid)).toEqual(['root', 'a', 'b'])
    expect(result.edges).toEqual(snapshot.edges.slice(0, 2))
  })

  test('scales with the number of edges', () => {
    const timings = [1000, 4000, 16000].map((size) => {
      const snapshot: KataReachableGraphResponse = {
        source_uid: 'root',
        depth: 'full',
        hide_done: false,
        nodes: [
          task({ uid: 'root' }),
          ...Array.from({ length: size }, (_, index) => task({ uid: `issue-${index}` })),
        ],
        edges: Array.from({ length: size }, (_, index) => ({
          from_uid: 'root',
          to_uid: `issue-${index}`,
          kind: 'related',
          layout: false,
        })),
        unresolved_refs: [],
        fetched_at: '2026-08-01T12:00:00Z',
      }
      expect(filterSnapshotGraph(snapshot, 'root', 'full', false).nodes).toHaveLength(size + 1)
      const samples = Array.from({ length: 3 }, () => {
        const start = performance.now()
        filterSnapshotGraph(snapshot, 'root', 'full', false)
        return performance.now() - start
      })
      const median = samples.sort((a, b) => a - b)[1]!
      console.info(`${size} edges: ${median.toFixed(2)}ms`)
      return median
    })
    // Allow noise above linear growth, while rejecting a full edge scan per node.
    expect(timings[2]!).toBeLessThan(timings[1]! * 4 ** 1.5)
  }, 60_000)
})
