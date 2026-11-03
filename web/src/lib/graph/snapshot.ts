import type { KataReachableGraphResponse } from '../kata/types'
import type { KataGraphDepthLimit } from './layout'
import { graphDistanceByUID } from './distance'

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
    return distanceByUID.has(node.uid)
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
