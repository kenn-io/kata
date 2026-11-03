import { describe, expect, it } from 'vitest'

import type { KataReachableGraphResponse } from '../kata/types'
import { task } from '../kata/test-helpers'
import { buildKataReachableGraph } from './layout'

function graph(overrides: Partial<KataReachableGraphResponse> = {}): KataReachableGraphResponse {
  return {
    source_uid: overrides.source_uid ?? 'issue-root',
    depth: overrides.depth ?? 'full',
    hide_done: overrides.hide_done ?? false,
    nodes: overrides.nodes ?? [],
    edges: overrides.edges ?? [],
    unresolved_refs: overrides.unresolved_refs ?? [],
    fetched_at: overrides.fetched_at ?? '2026-08-01T12:00:00Z',
  }
}

describe('buildKataReachableGraph', () => {
  it('keeps stable layer ordering when new nodes become ready', () => {
    const nodes = ['root', 'a', 'b', 'c', 'd', 'z'].map((uid) =>
      task({ uid, short_id: uid, title: uid }),
    )
    const built = buildKataReachableGraph({
      sourceUID: 'root',
      selectedUID: null,
      graph: graph({
        source_uid: 'root',
        nodes,
        edges: [
          { from_uid: 'root', to_uid: 'b', kind: 'blocks', layout: true },
          { from_uid: 'root', to_uid: 'c', kind: 'blocks', layout: true },
          { from_uid: 'root', to_uid: 'd', kind: 'blocks', layout: true },
          { from_uid: 'b', to_uid: 'z', kind: 'blocks', layout: true },
          { from_uid: 'c', to_uid: 'a', kind: 'blocks', layout: true },
        ],
      }),
    })

    expect(
      built.nodes
        .filter((node) => node.id === 'a' || node.id === 'z')
        .map((node) => [node.id, node.position]),
    ).toEqual([
      ['a', { x: 640, y: -54 }],
      ['z', { x: 640, y: 54 }],
    ])
  })

  it('uses the native graph node and edge set directly', () => {
    const root = task({ uid: 'issue-root', short_id: 'root', title: 'Root' })
    const child = task({ uid: 'issue-child', short_id: 'child', title: 'Child' })
    const built = buildKataReachableGraph({
      sourceUID: root.uid,
      selectedUID: null,
      graph: graph({
        source_uid: root.uid,
        nodes: [root, child],
        edges: [{ from_uid: root.uid, to_uid: child.uid, kind: 'parent', layout: true }],
      }),
    })

    expect(built.nodes.map((node) => node.id)).toEqual([root.uid, child.uid])
    expect(built.edges.map((edge) => [edge.source, edge.target, edge.data?.kind])).toEqual([
      [root.uid, child.uid, 'parent'],
    ])
    expect(built.layoutEdges.map((edge) => edge.id)).toEqual([`parent:${root.uid}:${child.uid}`])
  })

  it('draws layout-pruned server edges but excludes them from layout edges', () => {
    const a = task({ uid: 'issue-a', short_id: 'a' })
    const b = task({ uid: 'issue-b', short_id: 'b' })
    const c = task({ uid: 'issue-c', short_id: 'c' })
    const built = buildKataReachableGraph({
      sourceUID: a.uid,
      selectedUID: null,
      graph: graph({
        source_uid: a.uid,
        nodes: [a, b, c],
        edges: [
          { from_uid: a.uid, to_uid: b.uid, kind: 'blocks', layout: true },
          { from_uid: b.uid, to_uid: c.uid, kind: 'blocks', layout: true },
          { from_uid: a.uid, to_uid: c.uid, kind: 'blocks', layout: false },
        ],
      }),
    })

    expect(built.edges.map((edge) => edge.id)).toContain(`blocks:${a.uid}:${c.uid}`)
    expect(built.layoutEdges.map((edge) => edge.id)).toEqual([
      `blocks:${a.uid}:${b.uid}`,
      `blocks:${b.uid}:${c.uid}`,
    ])
  })

  it('context depth only changes emphasis, not the node set', () => {
    const root = task({ uid: 'issue-root', short_id: 'root' })
    const one = task({ uid: 'issue-one', short_id: 'one' })
    const two = task({ uid: 'issue-two', short_id: 'two' })
    const payload = graph({
      source_uid: root.uid,
      nodes: [root, one, two],
      edges: [
        { from_uid: root.uid, to_uid: one.uid, kind: 'blocks', layout: true },
        { from_uid: one.uid, to_uid: two.uid, kind: 'blocks', layout: true },
      ],
    })
    const allContext = buildKataReachableGraph({
      sourceUID: root.uid,
      selectedUID: root.uid,
      graph: payload,
      contextDepth: 'all',
    })
    const oneEdgeContext = buildKataReachableGraph({
      sourceUID: root.uid,
      selectedUID: root.uid,
      graph: payload,
      contextDepth: '1',
    })

    expect(oneEdgeContext.nodes.map((node) => node.id)).toEqual(
      allContext.nodes.map((node) => node.id),
    )
    expect(oneEdgeContext.nodes.find((node) => node.id === two.uid)?.data.isDepthContext).toBe(true)
    expect(allContext.nodes.find((node) => node.id === two.uid)?.data.isDepthContext).toBe(false)
  })

  it('labels adjacent nodes according to directed edge semantics', () => {
    const parent = task({ uid: 'issue-parent', short_id: 'parent' })
    const child = task({ uid: 'issue-child', short_id: 'child' })
    const blocked = task({ uid: 'issue-blocked', short_id: 'blocked' })
    const blocker = task({ uid: 'issue-blocker', short_id: 'blocker' })
    const built = buildKataReachableGraph({
      sourceUID: parent.uid,
      selectedUID: child.uid,
      graph: graph({
        source_uid: parent.uid,
        nodes: [parent, child, blocked, blocker],
        edges: [
          { from_uid: parent.uid, to_uid: child.uid, kind: 'parent', layout: true },
          { from_uid: child.uid, to_uid: blocked.uid, kind: 'blocks', layout: true },
          { from_uid: blocker.uid, to_uid: child.uid, kind: 'blocks', layout: true },
        ],
      }),
    })

    expect(built.nodes.find((node) => node.id === parent.uid)?.data.adjacentRelation).toBe('parent')
    expect(built.nodes.find((node) => node.id === blocked.uid)?.data.adjacentRelation).toBe(
      'blocks',
    )
    expect(built.nodes.find((node) => node.id === blocker.uid)?.data.adjacentRelation).toBe(
      'blockedBy',
    )
  })

  it('renders unresolved graph endpoints as non-selectable nodes', () => {
    const root = task({ uid: 'issue-root', short_id: 'root' })
    const missingUID = 'issue-missing'
    const built = buildKataReachableGraph({
      sourceUID: root.uid,
      selectedUID: root.uid,
      graph: graph({
        source_uid: root.uid,
        nodes: [root],
        edges: [{ from_uid: root.uid, to_uid: missingUID, kind: 'blocks', layout: true }],
        unresolved_refs: [{ uid: missingUID, side: 'to', kind: 'blocks', other_uid: root.uid }],
      }),
    })

    const missing = built.nodes.find((node) => node.id === missingUID)
    expect(missing?.data.selectable).toBe(false)
    expect(built.missingRefs).toEqual([
      { uid: missingUID, side: 'to', kind: 'blocks', otherUID: root.uid },
    ])
  })
})
