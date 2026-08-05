export type KataGraphDebugEventKind =
  | 'detail-load-abort'
  | 'detail-load-complete'
  | 'detail-load-stale'
  | 'detail-load-start'
  | 'events-load-error'
  | 'graph-load-complete'
  | 'graph-load-error'
  | 'graph-load-start'
  | 'graph-layout-complete'
  | 'graph-layout-error'
  | 'graph-layout-start'
  | 'graph-render'
  | 'selection-start'

export interface KataGraphDebugEvent {
  id: number
  at: number
  kind: KataGraphDebugEventKind
  detail?: Record<string, unknown> | undefined
}

export interface KataGraphDebugGraphSnapshot {
  sourceUID: string
  selectedUID: string | null
  hideDone: boolean
  graphLoading?: boolean | undefined
  graphError?: string | null | undefined
  contextDepth: string
  depthLimit: string
  layoutMode: string
  layoutDirection: string
  layoutReady: boolean
  nodeIds: string[]
  edges: Array<{
    id: string
    source: string
    target: string
    kind: string | null
    isDepthContext: boolean
  }>
  nodePositions: Array<{ id: string; x: number; y: number }>
  disabledNodeIds: string[]
  missingRefKeys: string[]
  nodeCount: number
  edgeCount: number
  layoutEdgeCount: number
  layoutBounds: { width: number; height: number; aspectRatio: number }
}

export interface KataGraphDebugStoreSnapshot {
  queueKeys: string[]
  graphLoadActive: boolean
  issueRefreshActive: boolean
  pendingSelectionUID: string | null
  selectedIssueUID: string | null
  cachedTaskCount: number
}

export interface KataGraphDebugSnapshot {
  events: KataGraphDebugEvent[]
  latestGraph?: KataGraphDebugGraphSnapshot | undefined
  store?: KataGraphDebugStoreSnapshot | undefined
}

export interface KataGraphDebugAPI {
  snapshot: () => KataGraphDebugSnapshot
  reset: () => void
}

const maxEvents = 200
let nextEventID = 1
let events: KataGraphDebugEvent[] = []
let latestGraph: KataGraphDebugGraphSnapshot | undefined
let store: KataGraphDebugStoreSnapshot | undefined

function now(): number {
  if (typeof performance !== 'undefined' && typeof performance.now === 'function') {
    return performance.now()
  }
  return Date.now()
}

function cloneSnapshot(): KataGraphDebugSnapshot {
  return {
    events: events.map((event) => ({
      ...event,
      detail: event.detail ? { ...event.detail } : undefined,
    })),
    latestGraph: latestGraph
      ? {
          ...latestGraph,
          nodeIds: [...latestGraph.nodeIds],
          edges: latestGraph.edges.map((edge) => ({ ...edge })),
          nodePositions: latestGraph.nodePositions.map((position) => ({ ...position })),
          disabledNodeIds: [...latestGraph.disabledNodeIds],
          missingRefKeys: [...latestGraph.missingRefKeys],
          layoutBounds: { ...latestGraph.layoutBounds },
        }
      : undefined,
    store: store ? { ...store, queueKeys: [...store.queueKeys] } : undefined,
  }
}

function installKataGraphDebugHook(): void {
  if (typeof window === 'undefined') return
  ;(window as Window & { __kata_graph_debug?: KataGraphDebugAPI }).__kata_graph_debug =
    kataGraphDebug
}

export function recordKataGraphDebugEvent(
  kind: KataGraphDebugEventKind,
  detail?: Record<string, unknown>,
): void {
  installKataGraphDebugHook()
  events = [...events, { id: nextEventID++, at: now(), kind, detail }].slice(-maxEvents)
}

export function setKataGraphDebugGraph(snapshot: KataGraphDebugGraphSnapshot): void {
  installKataGraphDebugHook()
  latestGraph = {
    ...snapshot,
    nodeIds: [...snapshot.nodeIds],
    edges: snapshot.edges.map((edge) => ({ ...edge })),
    nodePositions: snapshot.nodePositions.map((position) => ({ ...position })),
    disabledNodeIds: [...snapshot.disabledNodeIds],
    missingRefKeys: [...snapshot.missingRefKeys],
    layoutBounds: { ...snapshot.layoutBounds },
  }
}

export function setKataGraphDebugStore(snapshot: KataGraphDebugStoreSnapshot): void {
  installKataGraphDebugHook()
  store = { ...snapshot, queueKeys: [...snapshot.queueKeys] }
}

export function getKataGraphDebugSnapshot(): KataGraphDebugSnapshot {
  installKataGraphDebugHook()
  return cloneSnapshot()
}

export function resetKataGraphDebug(): void {
  installKataGraphDebugHook()
  nextEventID = 1
  events = []
  latestGraph = undefined
  store = undefined
}

export const kataGraphDebug: KataGraphDebugAPI = {
  snapshot: getKataGraphDebugSnapshot,
  reset: resetKataGraphDebug,
}

installKataGraphDebugHook()
