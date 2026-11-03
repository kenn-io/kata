import { describe, expect, test } from 'vitest'

import type { KataReachableGraphResponse } from '../kata/types'
import { task } from '../kata/test-helpers'
import { graphDistanceByUID } from './distance'
import { buildKataReachableGraph } from './layout'
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

  test('matches an edge-scan traversal on seeded graphs', () => {
    let seed = 0x5928a17
    const random = (limit: number) => {
      seed = (Math.imul(seed, 1664525) + 1013904223) >>> 0
      return Math.floor((seed / 2 ** 32) * limit)
    }
    for (let sample = 0; sample < 50; sample++) {
      const count = 3 + random(10)
      const uids = Array.from({ length: count }, (_, index) => `issue-${index}`)
      const snapshot: KataReachableGraphResponse = {
        source_uid: uids[0]!,
        depth: 'full',
        hide_done: false,
        nodes: uids.map((uid) => task({ uid, status: random(3) === 0 ? 'closed' : 'open' })),
        edges: Array.from({ length: random(count * 3) }, () => ({
          from_uid: uids[random(count)]!,
          to_uid: uids[random(count)]!,
          kind: 'related',
          layout: false,
        })),
        unresolved_refs: uids.map((uid) => ({
          uid: `missing-${uid}`,
          other_uid: uid,
          side: 'to',
          kind: 'related',
        })),
        fetched_at: '2026-08-01T12:00:00Z',
      }
      for (const hideDone of [false, true]) {
        const hidden = new Set(
          snapshot.nodes
            .filter(
              (node) => hideDone && node.uid !== snapshot.source_uid && node.status === 'closed',
            )
            .map((node) => node.uid),
        )
        const edges = snapshot.edges.filter(
          (edge) => !hidden.has(edge.from_uid) && !hidden.has(edge.to_uid),
        )
        const distances = new Map([[snapshot.source_uid, 0]])
        const queue = [snapshot.source_uid]
        while (queue.length > 0) {
          const uid = queue.shift()!
          const distance = distances.get(uid)!
          for (const edge of edges) {
            const adjacent =
              edge.from_uid === uid ? edge.to_uid : edge.to_uid === uid ? edge.from_uid : null
            if (!adjacent || distances.has(adjacent)) continue
            distances.set(adjacent, distance + 1)
            queue.push(adjacent)
          }
        }
        for (const depth of ['1', '2', '3', 'full'] as const) {
          const maxDepth = depth === 'full' ? Infinity : Number(depth)
          const nodes = snapshot.nodes.filter(
            (node) =>
              !hidden.has(node.uid) &&
              (distances.get(node.uid) ?? Infinity) <= maxDepth &&
              distances.has(node.uid),
          )
          const visible = new Set(nodes.map((node) => node.uid))
          expect(filterSnapshotGraph(snapshot, snapshot.source_uid, depth, hideDone)).toEqual({
            ...snapshot,
            depth,
            hide_done: hideDone,
            nodes,
            edges: edges.filter((edge) => visible.has(edge.from_uid) && visible.has(edge.to_uid)),
            unresolved_refs: snapshot.unresolved_refs.filter((ref) => visible.has(ref.other_uid)),
          })
        }
      }
    }
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
          layout: true,
        })),
        unresolved_refs: [],
        fetched_at: '2026-08-01T12:00:00Z',
      }
      const filterAndBuild = () =>
        buildKataReachableGraph({
          sourceUID: 'root',
          selectedUID: 'root',
          graph: filterSnapshotGraph(snapshot, 'root', 'full', false),
          contextDepth: '1',
        })
      const graph = filterAndBuild()
      expect(graph.nodes).toHaveLength(size + 1)
      expect(graph.edges).toHaveLength(size)
      const samples = Array.from({ length: 3 }, () => {
        const start = performance.now()
        filterAndBuild()
        return performance.now() - start
      })
      const median = samples.sort((a, b) => a - b)[1]!
      console.info(`${size} edges: ${median.toFixed(2)}ms`)
      return median
    })
    // Allow noise above linear growth while separating it from quadratic growth.
    expect(timings[2]!).toBeLessThan(timings[1]! * 4 ** 1.5)
  }, 120_000)
})
