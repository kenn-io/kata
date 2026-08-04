import { MarkerType, Position, type Edge, type Node } from '@xyflow/svelte'

import type {
  KataReachableGraphEdge,
  KataReachableGraphEdgeKind,
  KataReachableGraphResponse,
  KataReachableGraphUnresolvedRef,
  KataTaskSummary,
} from '../kata/types'

export interface KataGraphNodeData extends Record<string, unknown> {
  label: string
  title: string
  idLabel: string
  projectLabel: string
  qualifiedLabel: string
  accessibleLabel: string
  status: KataTaskSummary['status'] | 'uncached'
  closedReason?: KataTaskSummary['closed_reason'] | undefined
  priorityLabel: string | null
  isSource: boolean
  isSelected: boolean
  selectable: boolean
  isDepthContext: boolean
  onSelect?: ((uid: string) => void) | undefined
  adjacentRelation: KataGraphAdjacentRelation
  layoutDirection: KataGraphLayoutDirection
}

export type KataGraphAdjacentRelation =
  | 'blocks'
  | 'blockedBy'
  | 'child'
  | 'parent'
  | 'related'
  | null
export type KataGraphDepthLimit = 'full' | '1' | '2' | '3'
export type KataGraphContextDepth = 'all' | '1' | '2' | '3'
export type KataGraphLayoutDirection = 'LR' | 'TB'
export type KataGraphNode = Node<KataGraphNodeData, 'kataTask'>
export type KataGraphEdge = Edge<KataGraphEdgeData>

export interface KataGraphMissingRef {
  uid: string
  side: 'from' | 'to'
  kind: KataReachableGraphEdgeKind
  otherUID: string
}

interface KataGraphEdgeData extends Record<string, unknown> {
  kind: KataReachableGraphEdgeKind
  layout: boolean
  isDepthContext?: boolean | undefined
  isSelectedAdjacent?: boolean | undefined
}

export interface BuildKataReachableGraphInput {
  sourceUID: string
  selectedUID: string | null
  graph: KataReachableGraphResponse | null | undefined
  contextDepth?: KataGraphContextDepth | undefined
  layoutDirection?: KataGraphLayoutDirection | undefined
}

const KATA_GRAPH_NODE_WIDTH = 250
const KATA_GRAPH_NODE_HEIGHT = 74
const KATA_GRAPH_X_SPACING = 320
const KATA_GRAPH_Y_SPACING = 108

function priorityLabel(priority: number | undefined): string | null {
  if (priority === undefined) return null
  return `P${priority}`
}

function maxContextDepth(limit: KataGraphContextDepth | undefined): number {
  return limit === undefined || limit === 'all' ? Number.POSITIVE_INFINITY : Number(limit)
}

function missingRefKey(ref: KataReachableGraphUnresolvedRef): string {
  return `${ref.side}:${ref.kind}:${ref.uid}:${ref.other_uid}`
}

function missingRefsByUID(
  refs: readonly KataReachableGraphUnresolvedRef[],
): Map<string, KataGraphMissingRef> {
  const out = new Map<string, KataGraphMissingRef>()
  for (const ref of refs) {
    if (!ref.uid) continue
    out.set(ref.uid, {
      uid: ref.uid,
      side: ref.side,
      kind: ref.kind,
      otherUID: ref.other_uid,
    })
  }
  return out
}

function nodeClass(
  task: KataTaskSummary | undefined,
  sourceUID: string,
  selectedUID: string | null,
  isDepthContext: boolean,
): string {
  return [
    'kata-graph-node',
    task?.status === 'closed' ? 'kata-graph-node--closed' : 'kata-graph-node--open',
    task?.uid === sourceUID ? 'kata-graph-node--source' : '',
    task?.uid === selectedUID ? 'kata-graph-node--selected' : '',
    task ? '' : 'kata-graph-node--uncached',
    isDepthContext ? 'kata-graph-node--depth-context' : '',
  ]
    .filter(Boolean)
    .join(' ')
}

function graphNodeSortKey(id: string, task: KataTaskSummary | undefined): string {
  if (!task) return `1:${id}`
  return `0:${task.project_name}:${task.short_id}:${task.title}:${task.uid}`
}

function compareGraphNodeEntries(
  [leftID, leftTask]: [string, KataTaskSummary | undefined],
  [rightID, rightTask]: [string, KataTaskSummary | undefined],
  sourceUID: string,
): number {
  if (leftID === sourceUID) return -1
  if (rightID === sourceUID) return 1
  return graphNodeSortKey(leftID, leftTask).localeCompare(
    graphNodeSortKey(rightID, rightTask),
    undefined,
    {
      numeric: true,
      sensitivity: 'base',
    },
  )
}

function edgeMarkerColor(kind: KataReachableGraphEdgeKind): string {
  return kind === 'related' ? 'var(--kata-graph-edge-related)' : 'var(--kata-graph-edge-ambient)'
}

function makeEdge(edge: KataReachableGraphEdge): KataGraphEdge {
  return {
    id: `${edge.kind}:${edge.from_uid}:${edge.to_uid}`,
    source: edge.from_uid,
    target: edge.to_uid,
    type: 'smoothstep',
    class: `kata-graph-edge kata-graph-edge--${edge.kind}`,
    data: { kind: edge.kind, layout: edge.layout },
    markerEnd: {
      type: MarkerType.ArrowClosed,
      color: edgeMarkerColor(edge.kind),
      width: 18,
      height: 18,
    },
    ariaLabel: `${edge.kind} relationship from ${edge.from_uid} to ${edge.to_uid}`,
    interactionWidth: 12,
    selectable: false,
    zIndex: 2,
  }
}

function edgeID(edge: KataGraphEdge): string {
  return edge.id
}

function dedupeEdges(edges: readonly KataGraphEdge[]): KataGraphEdge[] {
  const seen = new Set<string>()
  const out: KataGraphEdge[] = []
  for (const edge of edges) {
    const id = edgeID(edge)
    if (seen.has(id)) continue
    seen.add(id)
    out.push(edge)
  }
  return out
}

function nodeData(
  id: string,
  task: KataTaskSummary | undefined,
  sourceUID: string,
  selectedUID: string | null,
  adjacentRelation: KataGraphAdjacentRelation,
  projectLabel: string,
  missingRef: KataGraphMissingRef | undefined,
  layoutDirection: KataGraphLayoutDirection,
  isDepthContext: boolean,
): KataGraphNodeData {
  if (!task) {
    const label = missingRef?.uid.slice(-4) || id.slice(-4) || id
    return {
      label,
      title: label,
      idLabel: label,
      projectLabel,
      qualifiedLabel: label,
      accessibleLabel: `Uncached linked task ${label}`,
      status: 'uncached',
      priorityLabel: null,
      isSource: false,
      isSelected: false,
      selectable: false,
      isDepthContext,
      adjacentRelation,
      layoutDirection,
    }
  }

  return {
    label: task.title,
    title: task.title,
    idLabel: task.short_id,
    projectLabel,
    qualifiedLabel: task.qualified_id || task.short_id,
    accessibleLabel: [
      task.uid === sourceUID ? 'Source task' : 'Task',
      task.uid === selectedUID ? 'selected' : '',
      task.title,
      task.qualified_id || task.short_id,
      task.status === 'closed'
        ? `closed${task.closed_reason ? ` ${task.closed_reason}` : ''}`
        : 'open',
      isDepthContext ? 'outside depth context' : '',
      adjacentRelation ? `adjacent ${adjacentRelation}` : '',
    ]
      .filter(Boolean)
      .join(', '),
    status: task.status,
    closedReason: task.closed_reason,
    priorityLabel: priorityLabel(task.priority),
    isSource: task.uid === sourceUID,
    isSelected: task.uid === selectedUID,
    selectable: true,
    isDepthContext,
    adjacentRelation,
    layoutDirection,
  }
}

function selectedAdjacentRelation(
  edge: KataGraphEdge,
  nodeID: string,
  selectedUID: string | null,
): KataGraphAdjacentRelation {
  if (!selectedUID) return null
  if (edge.source !== selectedUID && edge.target !== selectedUID) return null
  if (nodeID !== edge.source && nodeID !== edge.target) return null
  if (nodeID === selectedUID) return null

  if (edge.data?.kind === 'parent') {
    return edge.source === selectedUID ? 'child' : 'parent'
  }
  if (edge.data?.kind === 'blocks') {
    return edge.source === selectedUID ? 'blocks' : 'blockedBy'
  }
  return 'related'
}

function selectedAdjacentRelations(
  edges: readonly KataGraphEdge[],
  selectedUID: string | null,
): Map<string, KataGraphAdjacentRelation> {
  const relations = new Map<string, KataGraphAdjacentRelation>()
  for (const edge of edges) {
    if (edge.source === selectedUID) {
      mergeAdjacentRelation(
        relations,
        edge.target,
        selectedAdjacentRelation(edge, edge.target, selectedUID),
      )
    } else if (edge.target === selectedUID) {
      mergeAdjacentRelation(
        relations,
        edge.source,
        selectedAdjacentRelation(edge, edge.source, selectedUID),
      )
    }
  }
  return relations
}

function adjacentRelationPriority(relation: KataGraphAdjacentRelation): number {
  if (relation === 'blockedBy') return 5
  if (relation === 'blocks') return 4
  if (relation === 'parent') return 3
  if (relation === 'child') return 2
  if (relation === 'related') return 1
  return 0
}

function mergeAdjacentRelation(
  relations: Map<string, KataGraphAdjacentRelation>,
  nodeID: string,
  relation: KataGraphAdjacentRelation,
): void {
  const current = relations.get(nodeID) ?? null
  if (adjacentRelationPriority(relation) > adjacentRelationPriority(current)) {
    relations.set(nodeID, relation)
  }
}

function layoutNode(
  id: string,
  task: KataTaskSummary | undefined,
  position: { x: number; y: number },
  sourceUID: string,
  selectedUID: string | null,
  adjacentRelation: KataGraphAdjacentRelation,
  projectLabel: string,
  missingRef: KataGraphMissingRef | undefined,
  layoutDirection: KataGraphLayoutDirection,
  isDepthContext: boolean,
): KataGraphNode {
  const sourcePosition = layoutDirection === 'TB' ? Position.Bottom : Position.Right
  const targetPosition = layoutDirection === 'TB' ? Position.Top : Position.Left
  return {
    id,
    type: 'kataTask',
    position,
    data: nodeData(
      id,
      task,
      sourceUID,
      selectedUID,
      adjacentRelation,
      projectLabel,
      missingRef,
      layoutDirection,
      isDepthContext,
    ),
    class: nodeClass(task, sourceUID, selectedUID, isDepthContext),
    draggable: false,
    selectable: task !== undefined,
    sourcePosition,
    targetPosition,
    width: KATA_GRAPH_NODE_WIDTH,
    height: KATA_GRAPH_NODE_HEIGHT,
  }
}

function graphPositions(
  entries: readonly [string, KataTaskSummary | undefined][],
  edges: readonly KataGraphEdge[],
  layoutDirection: KataGraphLayoutDirection,
): Map<string, { x: number; y: number }> {
  const ids = entries.map(([id]) => id)
  const idSet = new Set(ids)
  const stableOrder = new Map(ids.map((id, index) => [id, index]))
  const outgoing = new Map<string, string[]>()
  const indegree = new Map<string, number>()
  for (const id of ids) {
    outgoing.set(id, [])
    indegree.set(id, 0)
  }

  for (const edge of edges) {
    if (!idSet.has(edge.source) || !idSet.has(edge.target)) continue
    outgoing.set(edge.source, [...(outgoing.get(edge.source) ?? []), edge.target])
    indegree.set(edge.target, (indegree.get(edge.target) ?? 0) + 1)
  }

  const compareByStableOrder = (left: string, right: string) =>
    (stableOrder.get(left) ?? 0) - (stableOrder.get(right) ?? 0)
  const ready = ids.filter((id) => (indegree.get(id) ?? 0) === 0).sort(compareByStableOrder)
  const topoOrder: string[] = []

  while (ready.length > 0) {
    const id = ready.shift()!
    topoOrder.push(id)
    for (const target of [...(outgoing.get(id) ?? [])].sort(compareByStableOrder)) {
      const nextIndegree = (indegree.get(target) ?? 0) - 1
      indegree.set(target, nextIndegree)
      if (nextIndegree === 0) {
        ready.push(target)
        ready.sort(compareByStableOrder)
      }
    }
  }

  const emitted = new Set(topoOrder)
  for (const id of ids) {
    if (!emitted.has(id)) topoOrder.push(id)
  }

  const rankByID = new Map(ids.map((id) => [id, 0]))
  for (const id of topoOrder) {
    const nextRank = (rankByID.get(id) ?? 0) + 1
    for (const target of outgoing.get(id) ?? []) {
      rankByID.set(target, Math.max(rankByID.get(target) ?? 0, nextRank))
    }
  }

  const layers = new Map<number, string[]>()
  for (const id of topoOrder) {
    const rank = rankByID.get(id) ?? 0
    layers.set(rank, [...(layers.get(rank) ?? []), id])
  }

  const positions = new Map<string, { x: number; y: number }>()
  for (const [depth, layerIDs] of layers) {
    const layerOffset =
      layoutDirection === 'TB'
        ? -((layerIDs.length - 1) * KATA_GRAPH_X_SPACING) / 2
        : -((layerIDs.length - 1) * KATA_GRAPH_Y_SPACING) / 2
    layerIDs.forEach((id, index) => {
      if (layoutDirection === 'TB') {
        positions.set(id, {
          x: layerOffset + index * KATA_GRAPH_X_SPACING,
          y: depth * KATA_GRAPH_Y_SPACING,
        })
      } else {
        positions.set(id, {
          x: depth * KATA_GRAPH_X_SPACING,
          y: layerOffset + index * KATA_GRAPH_Y_SPACING,
        })
      }
    })
  }
  return positions
}

function visibleProjectLabels(
  entries: readonly [string, KataTaskSummary | undefined][],
): Map<string, string> {
  const projectUIDs = new Set<string>()
  const shortIDCounts = new Map<string, number>()
  for (const [, task] of entries) {
    if (!task) continue
    projectUIDs.add(task.project_uid)
    shortIDCounts.set(task.short_id, (shortIDCounts.get(task.short_id) ?? 0) + 1)
  }

  const labels = new Map<string, string>()
  for (const [id, task] of entries) {
    if (!task) continue
    if (projectUIDs.size <= 1 && (shortIDCounts.get(task.short_id) ?? 0) <= 1) continue
    labels.set(id, task.project_name || task.project_uid)
  }
  return labels
}

function missingProjectLabel(
  ref: KataGraphMissingRef | undefined,
  projectLabels: Map<string, string>,
): string {
  return projectLabels.size > 0 ? (ref?.uid.slice(-4) ?? '') : ''
}

function graphEdgeData(
  edge: KataGraphEdge,
  kind: KataReachableGraphEdgeKind,
  overrides: Partial<Pick<KataGraphEdgeData, 'isDepthContext' | 'isSelectedAdjacent'>>,
): KataGraphEdgeData {
  return {
    kind,
    layout: edge.data?.layout ?? true,
    ...(edge.data?.isDepthContext !== undefined
      ? { isDepthContext: edge.data.isDepthContext }
      : {}),
    ...(edge.data?.isSelectedAdjacent !== undefined
      ? { isSelectedAdjacent: edge.data.isSelectedAdjacent }
      : {}),
    ...overrides,
  }
}

function depthContextEdge(edge: KataGraphEdge): KataGraphEdge {
  const kind = edge.data?.kind
  if (!kind) return edge
  const className = typeof edge.class === 'string' ? edge.class : ''
  const next: KataGraphEdge = {
    ...edge,
    class: `${className} kata-graph-edge--depth-context`.trim(),
    data: graphEdgeData(edge, kind, { isDepthContext: true }),
    zIndex: 0,
  }
  if (!edge.markerEnd || typeof edge.markerEnd !== 'object') return next
  return {
    ...next,
    markerEnd: { ...edge.markerEnd, color: 'var(--kata-graph-edge-context)' },
  } as KataGraphEdge
}

function selectedEdgeMarkerColor(kind: KataReachableGraphEdgeKind): string {
  if (kind === 'blocks') return 'var(--kata-graph-edge-selected-blocks)'
  if (kind === 'parent') return 'var(--kata-graph-edge-selected)'
  if (kind === 'related') return 'var(--kata-graph-edge-related)'
  return 'var(--kata-graph-edge-selected)'
}

function ambientActiveEdge(edge: KataGraphEdge): KataGraphEdge {
  const kind = edge.data?.kind
  if (!kind) return edge
  const className = typeof edge.class === 'string' ? edge.class : ''
  const next: KataGraphEdge = {
    ...edge,
    class: `${className} kata-graph-edge--ambient`.trim(),
    data: graphEdgeData(edge, kind, { isSelectedAdjacent: false }),
    zIndex: 1,
  }
  if (!edge.markerEnd || typeof edge.markerEnd !== 'object') return next
  return {
    ...next,
    markerEnd: { ...edge.markerEnd, color: 'var(--kata-graph-edge-ambient)' },
  } as KataGraphEdge
}

function selectedActiveEdge(edge: KataGraphEdge): KataGraphEdge {
  const kind = edge.data?.kind
  if (!kind) return edge
  const className = typeof edge.class === 'string' ? edge.class : ''
  const next: KataGraphEdge = {
    ...edge,
    class: `${className} kata-graph-edge--selected-adjacent`.trim(),
    data: graphEdgeData(edge, kind, { isSelectedAdjacent: true }),
    zIndex: 4,
  }
  if (!edge.markerEnd || typeof edge.markerEnd !== 'object') return next
  return {
    ...next,
    markerEnd: { ...edge.markerEnd, color: selectedEdgeMarkerColor(kind) },
  } as KataGraphEdge
}

function activeEdge(edge: KataGraphEdge, selectedUID: string | null): KataGraphEdge {
  if (selectedUID && (edge.source === selectedUID || edge.target === selectedUID)) {
    return selectedActiveEdge(edge)
  }
  return ambientActiveEdge(edge)
}

function visibleContextIDs(
  rootID: string,
  edges: readonly KataGraphEdge[],
  maxDepth: number,
): { nodeIDs: Set<string>; edgeIDs: Set<string> } {
  const nodeIDs = new Set<string>([rootID])
  const edgeIDs = new Set<string>()
  const distances = new Map<string, number>([[rootID, 0]])
  const adjacency = new Map<string, KataGraphEdge[]>()
  for (const edge of edges) {
    adjacency.set(edge.source, [...(adjacency.get(edge.source) ?? []), edge])
    adjacency.set(edge.target, [...(adjacency.get(edge.target) ?? []), edge])
  }

  const queued = [rootID]
  while (queued.length > 0) {
    const id = queued.shift()!
    const distance = distances.get(id) ?? 0
    if (distance >= maxDepth) continue
    for (const edge of adjacency.get(id) ?? []) {
      const nextID = edge.source === id ? edge.target : edge.source
      edgeIDs.add(edge.id)
      if (distances.has(nextID)) continue
      distances.set(nextID, distance + 1)
      nodeIDs.add(nextID)
      queued.push(nextID)
    }
  }

  return { nodeIDs, edgeIDs }
}

function compareGraphEdges(left: KataGraphEdge, right: KataGraphEdge): number {
  const leftDepthContext = left.data?.isDepthContext ? 0 : 1
  const rightDepthContext = right.data?.isDepthContext ? 0 : 1
  return (
    leftDepthContext - rightDepthContext ||
    left.id.localeCompare(right.id, undefined, { numeric: true, sensitivity: 'base' })
  )
}

function nodeEntriesFromGraph(
  graph: KataReachableGraphResponse,
  edges: readonly KataGraphEdge[],
): [string, KataTaskSummary | undefined][] {
  const nodeTasks = new Map<string, KataTaskSummary | undefined>()
  for (const task of graph.nodes) {
    nodeTasks.set(task.uid, task)
  }
  for (const edge of edges) {
    if (!nodeTasks.has(edge.source)) nodeTasks.set(edge.source, undefined)
    if (!nodeTasks.has(edge.target)) nodeTasks.set(edge.target, undefined)
  }
  return [...nodeTasks.entries()].sort((left, right) =>
    compareGraphNodeEntries(left, right, graph.source_uid),
  )
}

export function buildKataReachableGraph(input: BuildKataReachableGraphInput): {
  nodes: KataGraphNode[]
  edges: KataGraphEdge[]
  layoutEdges: KataGraphEdge[]
  missingRefs: KataGraphMissingRef[]
} {
  const graph = input.graph
  if (!graph) return { nodes: [], edges: [], layoutEdges: [], missingRefs: [] }

  const layoutDirection = input.layoutDirection ?? 'LR'
  const sourceUID = graph.source_uid || input.sourceUID
  const rawEdges = dedupeEdges(graph.edges.map(makeEdge))
  const visibleNodeEntries = nodeEntriesFromGraph(graph, rawEdges)
  const visibleIDs = new Set(visibleNodeEntries.map(([id]) => id))
  const visibleBaseEdges = rawEdges.filter(
    (edge) => visibleIDs.has(edge.source) && visibleIDs.has(edge.target),
  )
  const hasDepthContext = input.contextDepth !== undefined && input.contextDepth !== 'all'
  const contextRootID =
    input.selectedUID && visibleIDs.has(input.selectedUID) ? input.selectedUID : sourceUID
  const visibleContext = hasDepthContext
    ? visibleContextIDs(contextRootID, visibleBaseEdges, maxContextDepth(input.contextDepth))
    : { nodeIDs: visibleIDs, edgeIDs: new Set(visibleBaseEdges.map((edge) => edge.id)) }
  const visibleEdges = visibleBaseEdges
    .map((edge) =>
      hasDepthContext &&
      (!visibleContext.edgeIDs.has(edge.id) ||
        !visibleContext.nodeIDs.has(edge.source) ||
        !visibleContext.nodeIDs.has(edge.target))
        ? depthContextEdge(edge)
        : activeEdge(edge, input.selectedUID),
    )
    .sort(compareGraphEdges)
  const layoutEdges = visibleBaseEdges
    .filter((edge) => edge.data?.layout !== false)
    .sort(compareGraphEdges)
  const positions = graphPositions(visibleNodeEntries, layoutEdges, layoutDirection)
  const projectLabels = visibleProjectLabels(visibleNodeEntries)
  const adjacentRelations = selectedAdjacentRelations(visibleEdges, input.selectedUID)
  const missingRefs = missingRefsByUID(graph.unresolved_refs)
  const nodes = visibleNodeEntries.map(([id, task]) =>
    layoutNode(
      id,
      task,
      positions.get(id) ?? { x: 0, y: 0 },
      sourceUID,
      input.selectedUID,
      adjacentRelations.get(id) ?? null,
      task
        ? (projectLabels.get(id) ?? '')
        : missingProjectLabel(missingRefs.get(id), projectLabels),
      missingRefs.get(id),
      layoutDirection,
      hasDepthContext && !visibleContext.nodeIDs.has(id),
    ),
  )

  return {
    nodes,
    edges: visibleEdges,
    layoutEdges,
    missingRefs: [...graph.unresolved_refs]
      .sort((left, right) =>
        missingRefKey(left).localeCompare(missingRefKey(right), undefined, { numeric: true }),
      )
      .map((ref) => ({
        uid: ref.uid,
        side: ref.side,
        kind: ref.kind,
        otherUID: ref.other_uid,
      })),
  }
}
