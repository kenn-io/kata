import type { components } from '../api/schema'
import type { UISnapshot } from '../state/snapshot'
import type {
  KataLinkPeer,
  KataProjectSummary,
  KataReachableGraphEdge,
  KataReachableGraphResponse,
  KataReachableGraphUnresolvedRef,
  KataRecurrence,
  KataTaskDetail,
  KataTaskEvent,
  KataTaskLink,
  KataTaskSummary,
} from './types'

type Immutable<T> = T extends (...args: never[]) => unknown
  ? T
  : T extends ReadonlyArray<infer Item>
    ? readonly Immutable<Item>[]
    : T extends object
      ? { readonly [Key in keyof T]: Immutable<T[Key]> }
      : T

type UIIssue = components['schemas']['UIIssue']
type UILink = components['schemas']['UILink']

export interface KataUISnapshotProjection {
  readonly fetched_at: string
  readonly projects: readonly KataProjectSummary[]
  readonly issues: readonly KataTaskSummary[]
  readonly member_issue_uids: readonly string[]
  readonly member_issue_uid_set: ReadonlySet<string>
  readonly selected_state?: string
  readonly selected_revision?: string
  readonly selected_detail?: KataTaskDetail
  readonly selected_recurrences: readonly KataRecurrence[]
  readonly selected_history: readonly KataTaskEvent[]
  readonly selected_graph?: KataReachableGraphResponse
}

export function normalizeKataUISnapshot(
  snapshot: UISnapshot,
  fetchedAt = new Date().toISOString(),
): KataUISnapshotProjection {
  const projects = (snapshot.catalog ?? []).map(({ project, stats }) => ({
    id: project.id,
    uid: project.uid,
    name: project.name,
    metadata: { ...project.metadata },
    revision: project.revision,
    created_at: project.created_at,
    deleted_at: project.deleted_at,
    open_count: stats.Open,
  }))
  const projectsByID = new Map(projects.map((project) => [project.id, project]))
  const issues = (snapshot.collection ?? []).map((issue) => normalizeIssue(issue, projectsByID))
  enrichRelationships(issues, snapshot.collection_links ?? [])
  const memberIssueUIDs = Object.freeze(issues.map((issue) => issue.uid))
  const selectedIssue = snapshot.selected?.issue
    ? normalizeIssue(snapshot.selected.issue, projectsByID)
    : undefined
  if (selectedIssue) enrichRelationships([selectedIssue], snapshot.selected?.links ?? [])
  const selectedDetail = selectedIssue
    ? normalizeSelectedDetail(selectedIssue, snapshot.selected!, issues)
    : undefined
  const selectedGraph = selectedIssue
    ? normalizeGraph(snapshot.graph, selectedIssue.uid, projectsByID, fetchedAt)
    : undefined

  return Object.freeze({
    fetched_at: fetchedAt,
    projects: immutableCopy(projects) as readonly KataProjectSummary[],
    issues: immutableCopy(issues) as readonly KataTaskSummary[],
    member_issue_uids: memberIssueUIDs,
    member_issue_uid_set: immutableSet(memberIssueUIDs),
    ...(snapshot.selected ? { selected_state: snapshot.selected.state } : {}),
    ...(selectedIssue ? { selected_revision: `"rev-${selectedIssue.revision}"` } : {}),
    ...(selectedDetail ? { selected_detail: immutableCopy(selectedDetail) as KataTaskDetail } : {}),
    selected_recurrences: immutableCopy(
      snapshot.selected?.recurrences ?? [],
    ) as unknown as readonly KataRecurrence[],
    selected_history: immutableCopy(
      (snapshot.selected?.history ?? []).map(normalizeEvent),
    ) as readonly KataTaskEvent[],
    ...(selectedGraph
      ? {
          selected_graph: immutableCopy(selectedGraph) as KataReachableGraphResponse,
        }
      : {}),
  })
}

function normalizeGraph(
  graph: UISnapshot['graph'],
  sourceUID: string,
  projectsByID: ReadonlyMap<number, KataProjectSummary>,
  fetchedAt: string,
): KataReachableGraphResponse | undefined {
  if (!graph) return undefined
  const issues = (graph.issues ?? []).map((issue) => normalizeIssue(issue, projectsByID))
  const issuesByUID = new Map(issues.map((issue) => [issue.uid, issue]))
  if (!issuesByUID.has(sourceUID)) return undefined

  const materializedEdges = (graph.links ?? []).flatMap((link): KataReachableGraphEdge[] => {
    const kind = normalizeLinkType(link.type)
    if (!kind) return []
    const fromUID = kind === 'parent' ? link.to_issue_uid : link.from_issue_uid
    const toUID = kind === 'parent' ? link.from_issue_uid : link.to_issue_uid
    if (!issuesByUID.has(fromUID) || !issuesByUID.has(toUID)) return []
    return [{ from_uid: fromUID, to_uid: toUID, kind, layout: true }]
  })
  const unresolvedEdges = (graph.edges ?? []).flatMap((edge): KataReachableGraphEdge[] => {
    const kind = normalizeLinkType(edge.kind)
    if (!kind) return []
    return [{ from_uid: edge.from_uid, to_uid: edge.to_uid, kind, layout: edge.layout }]
  })
  const edges = [...materializedEdges, ...unresolvedEdges]
  const reachable = reachableGraphUIDs(sourceUID, edges)
  const reachableEdges = edges.filter(
    (edge) => reachable.has(edge.from_uid) && reachable.has(edge.to_uid),
  )
  markTransitiveBlockLayout(reachableEdges)
  const nodes = issues.filter((issue) => reachable.has(issue.uid))
  const unresolvedRefs = (graph.unresolved_refs ?? []).flatMap(
    (reference): KataReachableGraphUnresolvedRef[] => {
      const kind = normalizeLinkType(reference.kind)
      if (!kind || (reference.side !== 'from' && reference.side !== 'to')) return []
      if (!reachable.has(reference.uid) || !reachable.has(reference.other_uid)) return []
      return [
        {
          uid: reference.uid,
          side: reference.side,
          kind,
          other_uid: reference.other_uid,
        },
      ]
    },
  )
  enrichRelationships(nodes, graph.links ?? [])
  return {
    source_uid: sourceUID,
    depth: 'full',
    hide_done: false,
    nodes,
    edges: reachableEdges,
    unresolved_refs: unresolvedRefs,
    fetched_at: fetchedAt,
  }
}

function reachableGraphUIDs(
  sourceUID: string,
  edges: readonly KataReachableGraphEdge[],
): ReadonlySet<string> {
  const reached = new Set([sourceUID])
  const queue = [sourceUID]
  while (queue.length > 0) {
    const uid = queue.shift()!
    for (const edge of edges) {
      const adjacent =
        edge.from_uid === uid ? edge.to_uid : edge.to_uid === uid ? edge.from_uid : undefined
      if (!adjacent || reached.has(adjacent)) continue
      reached.add(adjacent)
      queue.push(adjacent)
    }
  }
  return reached
}

function markTransitiveBlockLayout(edges: KataReachableGraphEdge[]): void {
  const adjacency = new Map<string, string[]>()
  for (const edge of edges) {
    if (edge.kind !== 'blocks') continue
    adjacency.set(edge.from_uid, [...(adjacency.get(edge.from_uid) ?? []), edge.to_uid])
  }
  for (const peers of adjacency.values()) peers.sort()
  for (const edge of edges) {
    if (edge.kind === 'blocks' && hasAlternateBlockPath(adjacency, edge.from_uid, edge.to_uid)) {
      edge.layout = false
    }
  }
}

function hasAlternateBlockPath(
  adjacency: ReadonlyMap<string, readonly string[]>,
  fromUID: string,
  toUID: string,
): boolean {
  const seen = new Set([fromUID])
  const queue = [...(adjacency.get(fromUID) ?? [])]
  while (queue.length > 0) {
    const next = queue.shift()!
    if (next === toUID || seen.has(next)) continue
    seen.add(next)
    for (const peer of adjacency.get(next) ?? []) {
      if (peer === toUID) return true
      if (!seen.has(peer)) queue.push(peer)
    }
  }
  return false
}

function normalizeEvent(
  event: NonNullable<NonNullable<UISnapshot['selected']>['history']>[number],
): KataTaskEvent {
  return {
    event_id: event.id,
    event_uid: event.uid,
    origin_instance_uid: event.origin_instance_uid,
    type: event.type,
    project_id: event.project_id,
    project_uid: event.project_uid,
    project_name: event.project_name,
    issue_id: event.issue_id,
    issue_uid: event.issue_uid,
    issue_short_id: event.issue_short_id,
    related_issue_id: event.related_issue_id,
    related_issue_uid: event.related_issue_uid,
    related_issue_short_id: event.related_issue_short_id,
    actor: event.actor,
    payload: parseEventPayload(event.payload),
    created_at: event.created_at,
  }
}

function parseEventPayload(payload: string): Record<string, unknown> {
  try {
    const value: unknown = JSON.parse(payload)
    return typeof value === 'object' && value !== null && !Array.isArray(value)
      ? (value as Record<string, unknown>)
      : {}
  } catch {
    return {}
  }
}

function normalizeSelectedDetail(
  selectedIssue: KataTaskSummary,
  selected: NonNullable<UISnapshot['selected']>,
  collection: readonly KataTaskSummary[],
): KataTaskDetail {
  const issuesByUID = new Map(collection.map((issue) => [issue.uid, issue]))
  const parent = selectedIssue.parent ? issuesByUID.get(selectedIssue.parent.uid) : undefined
  return {
    issue: { ...selectedIssue, body: selectedIssue.body ?? '' },
    comments: (selected.comments ?? []).map((comment) => ({
      id: comment.id,
      issue_id: comment.issue_id,
      author: comment.author,
      body: comment.body,
      created_at: comment.created_at,
    })),
    labels: (selected.labels ?? []).map((label) => ({ ...label })),
    links: (selected.links ?? []).flatMap((link) => {
      const type = normalizeLinkType(link.type)
      if (!type) return []
      return [
        {
          id: link.id,
          project_id: selectedIssue.project_id,
          from: linkEndpoint(link.from_issue_uid, link.from_qualified_id, link.from_status),
          to: linkEndpoint(link.to_issue_uid, link.to_qualified_id, link.to_status),
          type,
          author: link.author,
          created_at: link.created_at,
        } satisfies KataTaskLink,
      ]
    }),
    ...(parent
      ? {
          parent: {
            uid: parent.uid,
            short_id: parent.short_id,
            qualified_id: parent.qualified_id,
            title: parent.title,
            status: parent.status,
          },
        }
      : {}),
    children: collection.filter((issue) => issue.parent?.uid === selectedIssue.uid),
  }
}

function normalizeLinkType(value: string): KataTaskLink['type'] | undefined {
  return value === 'parent' || value === 'blocks' || value === 'related' ? value : undefined
}

function normalizeIssue(
  issue: UIIssue,
  projectsByID: ReadonlyMap<number, KataProjectSummary>,
): KataTaskSummary {
  if (issue.status !== 'open' && issue.status !== 'closed') {
    throw new Error(`Invalid Kata snapshot collection issue status: ${issue.status}`)
  }
  const project = projectsByID.get(issue.project_id)
  return {
    id: issue.id,
    uid: issue.uid,
    project_id: issue.project_id,
    short_id: issue.short_id,
    qualified_id: issue.qualified_id,
    title: issue.title,
    body: issue.body,
    status: issue.status,
    project_uid: issue.project_uid ?? project?.uid ?? '',
    project_name:
      issue.project_name || project?.name || projectNameFromQualifiedID(issue.qualified_id),
    scheduled_on_date: issue.scheduled_on_date,
    deadline_on_date: issue.deadline_on_date,
    metadata: { ...issue.metadata },
    revision: issue.revision,
    owner: issue.owner,
    author: issue.author,
    priority: issue.priority,
    labels: [...(issue.labels ?? [])],
    blocks: [],
    blocked_by: [],
    related: [],
    recurrence_id: issue.recurrence_id,
    occurrence_key: issue.occurrence_key,
    created_at: issue.created_at,
    updated_at: issue.updated_at,
    closed_reason: normalizeClosedReason(issue.closed_reason),
    closed_at: issue.closed_at,
    deleted_at: issue.deleted_at,
  }
}

function enrichRelationships(issues: KataTaskSummary[], links: readonly UILink[]): void {
  const issuesByUID = new Map(issues.map((issue) => [issue.uid, issue]))
  for (const link of links) {
    const from = issuesByUID.get(link.from_issue_uid)
    const to = issuesByUID.get(link.to_issue_uid)
    const fromPeer = linkPeer(link.from_issue_uid, link.from_qualified_id)
    const toPeer = linkPeer(link.to_issue_uid, link.to_qualified_id)
    switch (link.type) {
      case 'parent':
        if (from) {
          from.parent = toPeer
          from.parent_short_id = toPeer.short_id
        }
        if (to) {
          const counts = to.child_counts ?? { open: 0, total: 0 }
          counts.total += 1
          if (link.from_status === 'open') counts.open += 1
          to.child_counts = counts
        }
        break
      case 'blocks':
        if (from) from.blocks = appendPeer(from.blocks, toPeer)
        if (to) to.blocked_by = appendPeer(to.blocked_by, fromPeer)
        break
      case 'related':
        if (from) from.related = appendPeer(from.related, toPeer)
        if (to) to.related = appendPeer(to.related, fromPeer)
        break
    }
  }
}

function appendPeer(peers: KataLinkPeer[] | undefined, peer: KataLinkPeer): KataLinkPeer[] {
  return [...(peers ?? []), peer]
}

function linkPeer(uid: string, qualifiedID: string): KataLinkPeer {
  const separator = qualifiedID.lastIndexOf('#')
  return { uid, short_id: separator === -1 ? qualifiedID : qualifiedID.slice(separator + 1) }
}

function linkEndpoint(uid: string, qualifiedID: string, status: string): KataTaskLink['from'] {
  if (status !== 'open' && status !== 'closed') {
    throw new Error(`Invalid Kata snapshot link endpoint status: ${status}`)
  }
  const separator = qualifiedID.lastIndexOf('#')
  return {
    uid,
    short_id: separator === -1 ? qualifiedID : qualifiedID.slice(separator + 1),
    qualified_id: qualifiedID,
    status,
  }
}

function projectNameFromQualifiedID(qualifiedID: string): string {
  const separator = qualifiedID.indexOf('#')
  return separator > 0 ? qualifiedID.slice(0, separator) : ''
}

function normalizeClosedReason(value: string | undefined): KataTaskSummary['closed_reason'] {
  if (
    value === 'done' ||
    value === 'wontfix' ||
    value === 'duplicate' ||
    value === 'superseded' ||
    value === 'audit-no-change'
  ) {
    return value
  }
  return undefined
}

function immutableCopy<T>(value: T): Immutable<T> {
  if (Array.isArray(value)) {
    return Object.freeze(value.map((item) => immutableCopy(item))) as Immutable<T>
  }
  if (typeof value === 'object' && value !== null) {
    const entries = Object.entries(value).map(([key, item]) => [key, immutableCopy(item)])
    return Object.freeze(Object.fromEntries(entries)) as Immutable<T>
  }
  return value as Immutable<T>
}

function immutableSet<T>(values: readonly T[]): ReadonlySet<T> {
  const source = new Set(values)
  const projection: ReadonlySet<T> = Object.freeze({
    get size(): number {
      return source.size
    },
    has(value: T): boolean {
      return source.has(value)
    },
    entries(): SetIterator<[T, T]> {
      return source.entries()
    },
    keys(): SetIterator<T> {
      return source.keys()
    },
    values(): SetIterator<T> {
      return source.values()
    },
    union<U>(other: ReadonlySetLike<U>): Set<T | U> {
      return source.union(other)
    },
    intersection<U>(other: ReadonlySetLike<U>): Set<T & U> {
      return source.intersection(other)
    },
    difference<U>(other: ReadonlySetLike<U>): Set<T> {
      return source.difference(other)
    },
    symmetricDifference<U>(other: ReadonlySetLike<U>): Set<T | U> {
      return source.symmetricDifference(other)
    },
    isSubsetOf(other: ReadonlySetLike<unknown>): boolean {
      return source.isSubsetOf(other)
    },
    isSupersetOf(other: ReadonlySetLike<unknown>): boolean {
      return source.isSupersetOf(other)
    },
    isDisjointFrom(other: ReadonlySetLike<unknown>): boolean {
      return source.isDisjointFrom(other)
    },
    [Symbol.iterator](): SetIterator<T> {
      return source[Symbol.iterator]()
    },
    forEach(callback: (value: T, key: T, set: ReadonlySet<T>) => void, thisArg?: unknown): void {
      source.forEach((value, key) => callback.call(thisArg, value, key, projection))
    },
  })
  return projection
}
