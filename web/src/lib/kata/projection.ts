import type { UIIssue, UILink } from '../api/generated'
import type { KataCreationAttributionWire } from '@kenn-io/kata-ui'
import type { UISnapshot } from '../state/snapshot'
import { graphDistanceByUID } from '../graph/distance'
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
  // True while selected_detail is the loaded list's copy of the task, before the
  // snapshot carrying its comments, links, and history arrives.
  readonly selected_pending?: boolean
}

export function normalizeKataUISnapshot(
  snapshot: UISnapshot,
  fetchedAt = new Date().toISOString(),
): KataUISnapshotProjection {
  const projects = (snapshot.catalog ?? []).map(({ project, stats }) => ({
    id: project.id,
    uid: project.uid,
    name: project.name,
    metadata: { ...(project.metadata ?? {}) },
    revision: project.revision,
    created_at: project.created_at ?? undefined,
    deleted_at: project.deleted_at,
    open_count: stats?.Open ?? 0,
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

// Shows a task picked from the loaded list at once: the list already holds its
// title, body, properties, parent, and children. Comments, links, history, and
// recurrences stay empty and selected_pending stays true until the snapshot
// for that task arrives. Returns undefined when the list does not hold the task.
export function withPendingSelection(
  projection: KataUISnapshotProjection,
  issueUID: string,
): KataUISnapshotProjection | undefined {
  if (projection.selected_detail?.issue.uid === issueUID) return projection
  const issue = projection.issues.find((candidate) => candidate.uid === issueUID)
  if (!issue) return undefined
  const detail = normalizeSelectedDetail(
    issue,
    { state: 'available', comments: [], history: [], labels: [], links: [], recurrences: [] },
    projection.issues,
  )
  return Object.freeze({
    fetched_at: projection.fetched_at,
    projects: projection.projects,
    issues: projection.issues,
    member_issue_uids: projection.member_issue_uids,
    member_issue_uid_set: projection.member_issue_uid_set,
    selected_state: 'available',
    selected_revision: `"rev-${issue.revision}"`,
    selected_detail: immutableCopy(detail) as KataTaskDetail,
    selected_recurrences: Object.freeze([]),
    selected_history: Object.freeze([]),
    selected_pending: true,
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
  const reachable = graphDistanceByUID(sourceUID, edges, Number.POSITIVE_INFINITY)
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

// Keep in sync with markTransitiveBlockLayout in internal/daemon/handlers_graph.go.
function markTransitiveBlockLayout(edges: KataReachableGraphEdge[]): void {
  const ids = new Map<string, number>()
  const idOf = (uid: string): number => {
    let id = ids.get(uid)
    if (id === undefined) ids.set(uid, (id = ids.size))
    return id
  }
  const blocks = edges.filter((edge) => edge.kind === 'blocks')
  const ends = blocks.map((edge) => [idOf(edge.from_uid), idOf(edge.to_uid)] as const)
  const count = ids.size
  const targets: number[][] = Array.from({ length: count }, () => [])
  for (const [from, to] of ends) {
    targets[from]!.push(to)
  }

  // Iterative Tarjan traversal numbers strongly connected components sinks-first.
  const componentOf = new Int32Array(count).fill(-1)
  const order = new Int32Array(count).fill(-1)
  const low = new Int32Array(count)
  const cursor = new Int32Array(count)
  const stack: number[] = []
  const groups: number[][] = []
  let visited = 0
  for (let root = 0; root < count; root++) {
    if (order[root]! >= 0) continue
    const path = [root]
    order[root] = low[root] = visited++
    stack.push(root)
    while (path.length > 0) {
      const node = path[path.length - 1]!
      if (cursor[node]! < targets[node]!.length) {
        const child = targets[node]![cursor[node]!]!
        cursor[node]!++
        if (order[child]! < 0) {
          order[child] = low[child] = visited++
          stack.push(child)
          path.push(child)
        } else if (componentOf[child]! < 0) {
          low[node] = Math.min(low[node]!, order[child]!)
        }
        continue
      }
      path.pop()
      if (path.length > 0) {
        const parent = path[path.length - 1]!
        low[parent] = Math.min(low[parent]!, low[node]!)
      }
      if (low[node] === order[node]) {
        const group: number[] = []
        let member: number
        do {
          member = stack.pop()!
          componentOf[member] = groups.length
          group.push(member)
        } while (member !== node)
        groups.push(group)
      }
    }
  }

  // Only components with multiple incoming links can be hidden targets.
  const incoming = new Int32Array(groups.length)
  for (let node = 0; node < count; node++) {
    for (const to of targets[node]!) {
      if (componentOf[to] !== componentOf[node]) incoming[componentOf[to]!]!++
    }
  }
  const bit = new Int32Array(groups.length).fill(-1)
  let bits = 0
  for (let component = 0; component < groups.length; component++) {
    if (incoming[component]! > 1) bit[component] = bits++
  }
  const words = Math.ceil(bits / 32)
  const reach: Uint32Array[] = []
  const absorb = (row: Uint32Array, component: number): void => {
    const slot = bit[component]!
    if (slot >= 0) row[slot >>> 5]! |= 1 << (slot & 31)
    for (let word = 0; word < words; word++) row[word]! |= reach[component]![word]!
  }
  const hidden = new Set<number>()
  for (let component = 0; component < groups.length; component++) {
    const group = groups[component]!
    const row = (reach[component] = new Uint32Array(words))
    if (group.length > 1) {
      for (const member of group) {
        for (const target of targets[member]!) {
          if (componentOf[target] !== component) absorb(row, componentOf[target]!)
        }
      }
      continue
    }
    // Acyclic sources cannot lie on a path from one target to another.
    // Read nearest targets first, then hide targets already in the row.
    const node = group[0]!
    const near = targets[node]!.slice().sort((a, b) => componentOf[b]! - componentOf[a]!)
    for (let index = 0; index < near.length; index++) {
      const target = near[index]!
      const targetComponent = componentOf[target]!
      const slot = bit[targetComponent]!
      if (slot >= 0) {
        if ((row[slot >>> 5]! & (1 << (slot & 31))) !== 0) hidden.add(node * count + target)
        if (index > 0 && componentOf[near[index - 1]!] === targetComponent)
          hidden.add(node * count + near[index - 1]!)
      }
      absorb(row, targetComponent)
    }
  }

  const seen = new Int32Array(count).fill(-1)
  const first = new Int32Array(count)
  const second = new Int32Array(count)
  const queue = new Int32Array(2 * count)
  const origins = new Int32Array(2 * count)

  for (const group of groups) {
    if (group.length === 1) continue
    // Cyclic sources need paths that never return through the source.
    // Two distinct starting targets per task suffice to detect alternate paths.
    for (const source of group) {
      if (targets[source]!.length < 2) continue
      const pending = targets[source]!.slice().sort((a, b) => componentOf[a]! - componentOf[b]!)
      let remaining = pending.length
      let lowest = 0
      let floor = componentOf[pending[lowest]!]!
      let tail = 0
      for (const target of pending) {
        seen[target] = source
        first[target] = target
        second[target] = -1
        origins[tail] = target
        queue[tail++] = target
      }
      for (let head = 0; head < tail && remaining > 0; head++) {
        const current = queue[head]!
        if (componentOf[current]! < floor) continue
        for (const target of targets[current]!) {
          if (target === source || componentOf[target]! < floor) continue
          const origin = origins[head]!
          if (seen[target] !== source) {
            seen[target] = source
            first[target] = origin
            second[target] = -1
          } else if (first[target] === origin || second[target] !== -1) continue
          else {
            second[target] = origin
            if (first[target] === target) {
              hidden.add(source * count + target)
              if (--remaining === 0) break
              // Hidden targets no longer require exploring lower components.
              while (hidden.has(source * count + pending[lowest]!)) lowest++
              floor = componentOf[pending[lowest]!]!
            }
          }
          origins[tail] = origin
          queue[tail++] = target
        }
      }
    }
  }

  blocks.forEach((edge, index) => {
    const [from, to] = ends[index]!
    if (hidden.has(from * count + to)) edge.layout = false
  })
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
      ...(comment.uid === undefined ? {} : { uid: comment.uid }),
      ...(comment.handle === undefined ? {} : { handle: comment.handle }),
      ...(comment.reply === undefined ? {} : { reply: { ...comment.reply } }),
      ...(comment.backlinks === undefined
        ? {}
        : { backlinks: comment.backlinks.map((link) => ({ ...link })) }),
      ...(comment.edited_at === undefined ? {} : { edited_at: comment.edited_at }),
      ...(comment.backlinks_truncated === undefined
        ? {}
        : { backlinks_truncated: comment.backlinks_truncated }),
      issue_id: comment.issue_id,
      author: comment.author,
      ...creationFields(comment),
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
    assignment_expires_on: issue.assignment_expires_on,
    author: issue.author,
    ...creationFields(issue),
    priority: issue.priority,
    labels: [...(issue.labels ?? [])],
    blocks: [],
    blocked_by: [],
    open_blocked_by: [],
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

function creationFields(source: KataCreationAttributionWire): KataCreationAttributionWire {
  return {
    ...(source.verification === undefined ? {} : { verification: source.verification }),
    ...(source.accountable_actor === undefined
      ? {}
      : { accountable_actor: source.accountable_actor }),
    ...(source.source_actor === undefined ? {} : { source_actor: source.source_actor }),
    ...(source.teammate === undefined ? {} : { teammate: source.teammate }),
    ...(source.authority_uid === undefined ? {} : { authority_uid: source.authority_uid }),
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
        if (from) from.blocks!.push(toPeer)
        if (to) {
          to.blocked_by!.push(fromPeer)
          if (link.from_status === 'open') {
            to.open_blocked_by!.push({ ...fromPeer, qualified_id: link.from_qualified_id })
          }
        }
        break
      case 'related':
        if (from) from.related!.push(toPeer)
        if (to) to.related!.push(fromPeer)
        break
    }
  }
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
