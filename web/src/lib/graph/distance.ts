import type { KataReachableGraphEdge } from '../kata/types'

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
  const distanceByUID = new Map<string, number>([[sourceUID, 0]])
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
