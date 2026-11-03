import { SvelteMap } from 'svelte/reactivity'

import type { KataReachableGraphEdge, KataReachableGraphResponse } from '../kata/types'
import type { KataGraphDepthLimit } from './layout'

export function graphDistanceByUID(
  sourceUID: string,
  edges: readonly KataReachableGraphEdge[],
  maxDepth: number,
): ReadonlyMap<string, number> {
  const adjacency = new Map<string, string[]>()
  for (const edge of edges) {
    const from = adjacency.get(edge.from_uid)
    if (from) from.push(edge.to_uid)
    else adjacency.set(edge.from_uid, [edge.to_uid])
    const to = adjacency.get(edge.to_uid)
    if (to) to.push(edge.from_uid)
    else adjacency.set(edge.to_uid, [edge.from_uid])
  }
  const distanceByUID = new SvelteMap<string, number>([[sourceUID, 0]])
  const queue = [sourceUID]
  for (let head = 0; head < queue.length; head++) {
    const uid = queue[head]!
    const distance = distanceByUID.get(uid) ?? 0
    if (distance >= maxDepth) continue
    for (const adjacentUID of adjacency.get(uid) ?? []) {
      if (!adjacentUID || distanceByUID.has(adjacentUID)) continue
      distanceByUID.set(adjacentUID, distance + 1)
      queue.push(adjacentUID)
    }
  }
  return distanceByUID
}

export function filterSnapshotGraph(
  snapshot: KataReachableGraphResponse,
  sourceUID: string,
  depth: KataGraphDepthLimit,
  shouldHideDone: boolean,
): KataReachableGraphResponse {
  const maxDepth = depth === 'full' ? Number.POSITIVE_INFINITY : Number(depth)
  const hiddenUIDs = new Set(
    snapshot.nodes
      .filter((node) => shouldHideDone && node.uid !== sourceUID && node.status === 'closed')
      .map((node) => node.uid),
  )
  const traversableEdges = snapshot.edges.filter(
    (edge) => !hiddenUIDs.has(edge.from_uid) && !hiddenUIDs.has(edge.to_uid),
  )
  const distanceByUID = graphDistanceByUID(sourceUID, traversableEdges, maxDepth)
  const nodes = snapshot.nodes.filter((node) => {
    if (hiddenUIDs.has(node.uid)) return false
    const distance = distanceByUID.get(node.uid)
    if (distance === undefined || distance > maxDepth) return false
    return true
  })
  const visibleUIDs = new Set(nodes.map((node) => node.uid))
  return {
    ...snapshot,
    depth,
    hide_done: shouldHideDone,
    nodes,
    edges: traversableEdges.filter(
      (edge) => visibleUIDs.has(edge.from_uid) && visibleUIDs.has(edge.to_uid),
    ),
    unresolved_refs: snapshot.unresolved_refs.filter((ref) => visibleUIDs.has(ref.other_uid)),
  }
}
