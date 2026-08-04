<script lang="ts">
  import ELK, { type ElkNode } from 'elkjs/lib/elk.bundled.js'
  import ArrowLeftIcon from '@lucide/svelte/icons/arrow-left'
  import { FilterDropdown } from '@kenn-io/kit-ui'
  import { SvelteMap } from 'svelte/reactivity'
  import {
    Background,
    BackgroundVariant,
    Controls,
    MiniMap,
    SvelteFlow,
    type Node as SvelteFlowNode,
    type NodeTypes,
  } from '@xyflow/svelte'
  import '@xyflow/svelte/dist/style.css'

  import type {
    KataReachableGraphEdge,
    KataReachableGraphResponse,
    KataTaskSummary,
  } from '../lib/kata/types'
  import {
    recordKataGraphDebugEvent,
    resetKataGraphDebug,
    setKataGraphDebugGraph,
  } from '../lib/graph/debug'
  import GraphIssueNode from './GraphIssueNode.svelte'
  import {
    buildKataReachableGraph,
    type KataGraphContextDepth,
    type KataGraphDepthLimit,
    type KataGraphEdge,
    type KataGraphLayoutDirection,
    type KataGraphNode,
  } from '../lib/graph/layout'

  type KataGraphLayoutMode = 'compact' | 'elk'
  type KataGraphDirectionChoice = 'follow' | KataGraphLayoutDirection

  interface KataGraphPreferences {
    contextDepth: KataGraphContextDepth
    depthLimit: KataGraphDepthLimit
    layoutMode: KataGraphLayoutMode
    layoutDirection: KataGraphLayoutDirection | null
  }

  interface LayoutPosition {
    x: number
    y: number
  }

  interface LayoutBounds {
    width: number
    height: number
    aspectRatio: number
  }

  interface Props {
    graph: KataReachableGraphResponse
    sourceIssue: KataTaskSummary
    selectedUID: string | null
    layoutDirection?: KataGraphLayoutDirection | undefined
    onBack: () => void
    onSelectIssue: (uid: string) => void
  }

  let {
    graph: snapshotGraph,
    sourceIssue,
    selectedUID,
    layoutDirection = 'LR',
    onBack,
    onSelectIssue,
  }: Props = $props()

  const graphPreferencesStorageKey = 'kata.graph.preferences.v1'
  const defaultGraphPreferences: KataGraphPreferences = {
    contextDepth: 'all',
    depthLimit: 'full',
    layoutMode: 'compact',
    layoutDirection: null,
  }
  const initialGraphPreferences = readGraphPreferences()

  let hideDone = $state(false)
  let contextDepth = $state<KataGraphContextDepth>(initialGraphPreferences.contextDepth)
  let depthLimit = $state<KataGraphDepthLimit>(initialGraphPreferences.depthLimit)
  let layoutMode = $state<KataGraphLayoutMode>(initialGraphPreferences.layoutMode)
  let graphDirectionOverride = $state<KataGraphLayoutDirection | null>(
    initialGraphPreferences.layoutDirection,
  )
  let effectiveLayoutDirection = $derived(graphDirectionOverride ?? layoutDirection)
  let layoutedPositions = $state.raw<ReadonlyMap<string, LayoutPosition>>(new Map())
  let layoutedKey = $state('')
  let graphResponse = $derived(
    filterSnapshotGraph(snapshotGraph, sourceIssue.uid, depthLimit, hideDone),
  )
  let graph = $derived(
    buildKataReachableGraph({
      sourceUID: sourceIssue.uid,
      selectedUID,
      graph: graphResponse,
      contextDepth,
      layoutDirection: effectiveLayoutDirection,
    }),
  )
  let graphSignature = $derived(graphLayoutSignature(graph.nodes, graph.layoutEdges))
  let activeLayoutKey = $derived(`${layoutMode}:${effectiveLayoutDirection}:${graphSignature}`)
  let flowNodes = $derived(
    layoutedKey === activeLayoutKey
      ? applyLayoutPositions(graph.nodes, layoutedPositions)
      : graph.nodes,
  )
  let interactiveNodes = $derived(flowNodes.map((node) => withNodeActivation(node)))
  let layoutReady = $derived(layoutMode === 'compact' || layoutedKey === activeLayoutKey)
  let source = $derived(
    graphResponse?.nodes.find((task) => task.uid === sourceIssue.uid) ?? sourceIssue,
  )
  let graphFilterActive = $derived(
    hideDone ||
      depthLimit !== defaultGraphPreferences.depthLimit ||
      contextDepth !== defaultGraphPreferences.contextDepth ||
      layoutMode !== defaultGraphPreferences.layoutMode ||
      graphDirectionOverride !== defaultGraphPreferences.layoutDirection,
  )
  let layoutRun = 0
  const elkDefaultLayoutOptions = {
    'elk.algorithm': 'layered',
    'elk.edgeRouting': 'ORTHOGONAL',
    'elk.aspectRatio': '1.0',
    'elk.separateConnectedComponents': 'true',
    'elk.spacing.nodeNode': '18',
    'elk.spacing.componentComponent': '32',
    'elk.layered.spacing.nodeNodeBetweenLayers': '42',
    'elk.layered.spacing.edgeNodeBetweenLayers': '10',
    'elk.layered.spacing.edgeEdgeBetweenLayers': '8',
    'elk.layered.nodePlacement.strategy': 'BRANDES_KOEPF',
    'elk.layered.nodePlacement.favorStraightEdges': 'false',
    'elk.layered.compaction.connectedComponents': 'true',
    'elk.layered.compaction.postCompaction.strategy': 'EDGE_LENGTH',
    'elk.layered.compaction.postCompaction.constraints': 'SCANLINE',
    'elk.layered.wrapping.strategy': 'MULTI_EDGE',
    'elk.layered.wrapping.cutting.strategy': 'MSD',
    'elk.layered.wrapping.cutting.msd.freedom': '2',
    'elk.layered.wrapping.additionalEdgeSpacing': '8',
    'elk.layered.wrapping.multiEdge.improveCuts': 'true',
    'elk.layered.wrapping.multiEdge.improveWrappedEdges': 'true',
  }
  const elk = new ELK({
    defaultLayoutOptions: elkDefaultLayoutOptions,
  })
  const nodeTypes: NodeTypes = {
    kataTask: GraphIssueNode,
  }
  const depthOptions = [
    { value: 'full', label: 'Full' },
    { value: '1', label: '1 edge' },
    { value: '2', label: '2 edges' },
    { value: '3', label: '3 edges' },
  ] as const satisfies readonly { value: KataGraphDepthLimit; label: string }[]
  const contextOptions = [
    { value: 'all', label: 'All' },
    { value: '1', label: '1 edge' },
    { value: '2', label: '2 edges' },
    { value: '3', label: '3 edges' },
  ] as const satisfies readonly { value: KataGraphContextDepth; label: string }[]
  const layoutOptions = [
    { value: 'compact', label: 'Compact' },
    { value: 'elk', label: 'ELK' },
  ] as const satisfies readonly { value: KataGraphLayoutMode; label: string }[]
  const graphDirectionOptions = [
    { value: 'follow', label: 'Follow split' },
    { value: 'LR', label: 'Left to right' },
    { value: 'TB', label: 'Top to bottom' },
  ] as const satisfies readonly { value: KataGraphDirectionChoice; label: string }[]
  let graphDirectionDetail = $derived(
    graphDirectionOverride === null
      ? `Follow ${effectiveLayoutDirection}`
      : `Pinned ${effectiveLayoutDirection}`,
  )
  let graphFilterDetail = $derived.by(() => {
    const parts = [
      optionLabel(depthOptions, depthLimit),
      optionLabel(contextOptions, contextDepth),
      optionLabel(layoutOptions, layoutMode),
      graphDirectionDetail,
    ]
    if (hideDone) parts.push('Hide done')
    return parts.join(' · ')
  })
  const graphFilterSections = $derived.by(() => [
    {
      title: 'Depth',
      items: depthOptions.map((option) => ({
        id: `depth-${option.value}`,
        label: option.label,
        active: depthLimit === option.value,
        onSelect: () => setDepthLimit(option.value),
      })),
    },
    {
      title: 'Context',
      items: contextOptions.map((option) => ({
        id: `context-${option.value}`,
        label: option.label,
        active: contextDepth === option.value,
        onSelect: () => setContextDepth(option.value),
      })),
    },
    {
      title: 'Layout',
      items: layoutOptions.map((option) => ({
        id: `layout-${option.value}`,
        label: option.label,
        active: layoutMode === option.value,
        onSelect: () => setLayoutMode(option.value),
      })),
    },
    {
      title: 'Direction',
      items: graphDirectionOptions.map((option) => ({
        id: `direction-${option.value}`,
        label: option.label,
        active:
          graphDirectionOverride === null
            ? option.value === 'follow'
            : graphDirectionOverride === option.value,
        onSelect: () => setGraphDirection(option.value),
      })),
    },
    {
      title: 'Visibility',
      items: [
        {
          id: 'visibility-hide-done',
          label: 'Hide done',
          active: hideDone,
          onSelect: () => {
            hideDone = !hideDone
          },
        },
      ],
    },
  ])
  const graphMinZoom = 0.02
  const fitViewOptions = {
    duration: 0,
    padding: 0.12,
    minZoom: graphMinZoom,
  }

  function isContextDepth(value: unknown): value is KataGraphContextDepth {
    return value === 'all' || value === '1' || value === '2' || value === '3'
  }

  function isDepthLimit(value: unknown): value is KataGraphDepthLimit {
    return value === 'full' || value === '1' || value === '2' || value === '3'
  }

  function isLayoutMode(value: unknown): value is KataGraphLayoutMode {
    return value === 'compact' || value === 'elk'
  }

  function isLayoutDirection(value: unknown): value is KataGraphLayoutDirection {
    return value === 'LR' || value === 'TB'
  }

  function normalizeGraphPreferences(value: unknown): KataGraphPreferences {
    if (!value || typeof value !== 'object') return defaultGraphPreferences
    const candidate = value as Partial<KataGraphPreferences>
    return {
      contextDepth: isContextDepth(candidate.contextDepth)
        ? candidate.contextDepth
        : defaultGraphPreferences.contextDepth,
      depthLimit: isDepthLimit(candidate.depthLimit)
        ? candidate.depthLimit
        : defaultGraphPreferences.depthLimit,
      layoutMode: isLayoutMode(candidate.layoutMode)
        ? candidate.layoutMode
        : defaultGraphPreferences.layoutMode,
      layoutDirection:
        candidate.layoutDirection === null || candidate.layoutDirection === undefined
          ? defaultGraphPreferences.layoutDirection
          : isLayoutDirection(candidate.layoutDirection)
            ? candidate.layoutDirection
            : defaultGraphPreferences.layoutDirection,
    }
  }

  function graphPreferenceStorage(): Storage | null {
    try {
      return globalThis.localStorage ?? null
    } catch {
      return null
    }
  }

  function readGraphPreferences(): KataGraphPreferences {
    try {
      const storage = graphPreferenceStorage()
      if (!storage) return defaultGraphPreferences
      return normalizeGraphPreferences(
        JSON.parse(storage.getItem(graphPreferencesStorageKey) ?? 'null'),
      )
    } catch {
      return defaultGraphPreferences
    }
  }

  function writeGraphPreferences(preferences: KataGraphPreferences): void {
    try {
      const storage = graphPreferenceStorage()
      if (!storage) return
      storage.setItem(graphPreferencesStorageKey, JSON.stringify(preferences))
    } catch {
      // localStorage may be unavailable in private or quota-restricted contexts.
    }
  }

  function optionLabel<T extends string>(
    options: readonly { value: T; label: string }[],
    value: T,
  ): string {
    return options.find((option) => option.value === value)?.label ?? value
  }

  function missingRefKey(ref: {
    uid: string
    side: string
    kind: string
    otherUID: string
  }): string {
    return `${ref.side}:${ref.kind}:${ref.uid}:${ref.otherUID}`
  }

  function graphDistanceByUID(
    sourceUID: string,
    edges: readonly KataReachableGraphEdge[],
  ): ReadonlyMap<string, number> {
    const distanceByUID = new SvelteMap<string, number>([[sourceUID, 0]])
    const queue = [sourceUID]
    while (queue.length > 0) {
      const uid = queue.shift()!
      const distance = distanceByUID.get(uid) ?? 0
      for (const edge of edges) {
        const adjacentUID =
          edge.from_uid === uid ? edge.to_uid : edge.to_uid === uid ? edge.from_uid : null
        if (!adjacentUID || distanceByUID.has(adjacentUID)) continue
        distanceByUID.set(adjacentUID, distance + 1)
        queue.push(adjacentUID)
      }
    }
    return distanceByUID
  }

  function filterSnapshotGraph(
    snapshot: KataReachableGraphResponse,
    sourceUID: string,
    depth: KataGraphDepthLimit,
    shouldHideDone: boolean,
  ): KataReachableGraphResponse {
    const maxDepth = depth === 'full' ? Number.POSITIVE_INFINITY : Number(depth)
    const distanceByUID = graphDistanceByUID(sourceUID, snapshot.edges)
    const nodes = snapshot.nodes.filter((node) => {
      const distance = distanceByUID.get(node.uid)
      if (distance === undefined || distance > maxDepth) return false
      return !shouldHideDone || node.uid === sourceUID || node.closed_reason !== 'done'
    })
    const visibleUIDs = new Set(nodes.map((node) => node.uid))
    return {
      ...snapshot,
      depth,
      hide_done: shouldHideDone,
      nodes,
      edges: snapshot.edges.filter(
        (edge) => visibleUIDs.has(edge.from_uid) && visibleUIDs.has(edge.to_uid),
      ),
      unresolved_refs: snapshot.unresolved_refs.filter((ref) => visibleUIDs.has(ref.other_uid)),
    }
  }

  function selectNodeID(uid: string): void {
    onSelectIssue(uid)
  }

  function selectNode(node: KataGraphNode): void {
    if (!node.data.selectable) return
    selectNodeID(node.id)
  }

  function withNodeActivation(node: KataGraphNode): KataGraphNode {
    return {
      ...node,
      data: {
        ...node.data,
        onSelect: selectNodeID,
      },
    }
  }

  function setDepthLimit(value: string): void {
    depthLimit = value as KataGraphDepthLimit
  }

  function setContextDepth(value: string): void {
    contextDepth = value as KataGraphContextDepth
  }

  function setLayoutMode(value: string): void {
    layoutMode = value as KataGraphLayoutMode
  }

  function setGraphDirection(direction: KataGraphDirectionChoice): void {
    graphDirectionOverride = direction === 'follow' ? null : direction
  }

  function graphLayoutSignature(
    nodes: readonly KataGraphNode[],
    edges: readonly KataGraphEdge[],
  ): string {
    return JSON.stringify({
      nodes: nodes.map((node) => node.id),
      edges: edges.map((edge) => [edge.id, edge.source, edge.target]),
    })
  }

  function elkDirection(direction: KataGraphLayoutDirection): 'RIGHT' | 'DOWN' {
    return direction === 'TB' ? 'DOWN' : 'RIGHT'
  }

  function elkGraph(
    nodes: readonly KataGraphNode[],
    edges: readonly KataGraphEdge[],
    direction: KataGraphLayoutDirection,
  ): ElkNode {
    const nodeIDs = new Set(nodes.map((node) => node.id))
    return {
      id: 'kata-reachable-graph',
      layoutOptions: {
        'elk.direction': elkDirection(direction),
      },
      children: nodes.map((node) => ({
        id: node.id,
        width: node.width ?? 250,
        height: node.height ?? 74,
      })),
      edges: edges
        .filter((edge) => nodeIDs.has(edge.source) && nodeIDs.has(edge.target))
        .map((edge) => ({
          id: edge.id,
          sources: [edge.source],
          targets: [edge.target],
        })),
    }
  }

  function elkPositions(layoutedGraph: ElkNode): ReadonlyMap<string, LayoutPosition> {
    return new Map(
      (layoutedGraph.children ?? []).map((node) => [node.id, { x: node.x ?? 0, y: node.y ?? 0 }]),
    )
  }

  function applyLayoutPositions(
    nodes: readonly KataGraphNode[],
    positions: ReadonlyMap<string, LayoutPosition>,
  ): KataGraphNode[] {
    if (positions.size === 0) return [...nodes]
    return nodes.map((node) => ({
      ...node,
      position: positions.get(node.id) ?? node.position,
    }))
  }

  function layoutBounds(nodes: readonly KataGraphNode[]): LayoutBounds {
    if (nodes.length === 0) return { width: 0, height: 0, aspectRatio: 1 }
    const minX = Math.min(...nodes.map((node) => node.position.x))
    const minY = Math.min(...nodes.map((node) => node.position.y))
    const maxX = Math.max(...nodes.map((node) => node.position.x + (node.width ?? 250)))
    const maxY = Math.max(...nodes.map((node) => node.position.y + (node.height ?? 74)))
    const width = Math.max(0, maxX - minX)
    const height = Math.max(0, maxY - minY)
    return {
      width,
      height,
      aspectRatio: height === 0 ? 1 : width / height,
    }
  }

  function minimapData(node: SvelteFlowNode): Partial<KataGraphNode['data']> {
    return node.data as Partial<KataGraphNode['data']>
  }

  function minimapNodeColor(node: SvelteFlowNode): string {
    const data = minimapData(node)
    if (data.status === 'closed' && data.closedReason === 'done') return 'var(--text-muted)'
    if (data.isDepthContext) return 'var(--text-muted)'
    if (data.status === 'uncached') return 'var(--bg-surface-hover)'
    if (data.isSource || data.isSelected) return 'var(--accent-blue)'
    return 'var(--bg-surface)'
  }

  function minimapNodeStrokeColor(node: SvelteFlowNode): string {
    const data = minimapData(node)
    if (data.isSource || data.isSelected) return 'var(--accent-blue)'
    return 'var(--border-default)'
  }

  $effect(() => {
    writeGraphPreferences({
      contextDepth,
      depthLimit,
      layoutMode,
      layoutDirection: graphDirectionOverride,
    })
  })

  $effect(() => {
    const key = activeLayoutKey
    const nodes = graph.nodes
    const edges = graph.layoutEdges
    const mode = layoutMode
    const direction = effectiveLayoutDirection
    const run = ++layoutRun
    if (mode === 'compact' || nodes.length === 0) {
      layoutedPositions = new Map()
      layoutedKey = key
      return
    }

    recordKataGraphDebugEvent('graph-layout-start', {
      layoutMode: mode,
      layoutDirection: direction,
      nodeCount: nodes.length,
      edgeCount: edges.length,
    })
    elk
      .layout(elkGraph(nodes, edges, direction))
      .then((layoutedGraph) => {
        if (run !== layoutRun) return
        layoutedPositions = elkPositions(layoutedGraph)
        layoutedKey = key
        recordKataGraphDebugEvent('graph-layout-complete', {
          layoutMode: mode,
          layoutDirection: direction,
          nodeCount: nodes.length,
          edgeCount: edges.length,
        })
      })
      .catch((error: unknown) => {
        if (run !== layoutRun) return
        layoutedPositions = new Map()
        layoutedKey = key
        recordKataGraphDebugEvent('graph-layout-error', {
          layoutMode: mode,
          layoutDirection: direction,
          message: error instanceof Error ? error.message : String(error),
        })
      })
  })

  $effect(() => {
    const snapshot = {
      sourceUID: sourceIssue.uid,
      selectedUID,
      hideDone,
      contextDepth,
      depthLimit,
      layoutMode,
      layoutDirection: effectiveLayoutDirection,
      layoutReady,
      nodeIds: flowNodes.map((node) => node.id),
      edges: graph.edges.map((edge) => ({
        id: edge.id,
        source: edge.source,
        target: edge.target,
        kind: typeof edge.data?.kind === 'string' ? edge.data.kind : null,
        isDepthContext: Boolean(edge.data?.isDepthContext),
      })),
      nodePositions: flowNodes.map((node) => ({
        id: node.id,
        x: node.position.x,
        y: node.position.y,
      })),
      disabledNodeIds: flowNodes.filter((node) => !node.data.selectable).map((node) => node.id),
      missingRefKeys: graph.missingRefs.map(missingRefKey),
      nodeCount: flowNodes.length,
      edgeCount: graph.edges.length,
      layoutEdgeCount: graph.layoutEdges.length,
      layoutBounds: layoutBounds(flowNodes),
    }
    setKataGraphDebugGraph(snapshot)
    recordKataGraphDebugEvent('graph-render', snapshot)
  })

  $effect(() => {
    return () => resetKataGraphDebug()
  })
</script>

<section
  class="kata-graph-pane"
  aria-label="Reachable task graph"
  data-layout-direction={effectiveLayoutDirection}
>
  <header class="graph-toolbar">
    <div class="graph-title-row">
      <button type="button" class="toolbar-button" aria-label="Back to task list" onclick={onBack}>
        <ArrowLeftIcon size={14} strokeWidth={1.9} aria-hidden="true" />
        <span>Tasks</span>
      </button>
      <div class="graph-source">
        <strong title={source.qualified_id || source.uid}
          >{source.title || 'Reachable graph'}</strong
        >
      </div>
    </div>
    <div class="graph-control-row" aria-label="Graph controls">
      <div class="graph-filter-menu">
        <FilterDropdown
          label="Graph filters"
          detail={graphFilterDetail}
          title="Graph filters"
          active={graphFilterActive}
          showBadge={false}
          sections={graphFilterSections}
          minWidth="220px"
        />
      </div>
    </div>
  </header>

  {#if graph.nodes.length === 0}
    <p class="graph-empty">No task data is available for this graph.</p>
  {:else}
    <div class="graph-canvas">
      <SvelteFlow
        nodes={interactiveNodes}
        edges={graph.edges}
        {nodeTypes}
        fitView
        {fitViewOptions}
        autoPanOnSelection={false}
        autoPanOnNodeFocus={false}
        defaultMarkerColor={null}
        minZoom={graphMinZoom}
        nodesDraggable={false}
        nodesConnectable={false}
        elementsSelectable={true}
        edgesFocusable={false}
        elevateEdgesOnSelect={false}
        onnodeclick={({ node }) => selectNode(node as KataGraphNode)}
      >
        <Controls />
        <MiniMap nodeColor={minimapNodeColor} nodeStrokeColor={minimapNodeStrokeColor} />
        <Background variant={BackgroundVariant.Dots} gap={14} size={1} />
      </SvelteFlow>
    </div>
  {/if}
</section>

<style>
  .kata-graph-pane {
    --kata-graph-edge-ambient: color-mix(in srgb, var(--text-muted) 58%, var(--bg-primary));
    --kata-graph-edge-context: color-mix(in srgb, var(--border-default) 76%, var(--bg-primary));
    --kata-graph-edge-related: color-mix(in srgb, var(--text-muted) 46%, var(--bg-primary));
    --kata-graph-edge-selected: color-mix(in srgb, var(--text-secondary) 82%, var(--text-primary));
    --kata-graph-edge-selected-blocks: color-mix(
      in srgb,
      var(--accent-amber) 90%,
      var(--text-primary)
    );

    min-width: 0;
    min-height: 0;
    height: 100%;
    display: flex;
    flex-direction: column;
    container-type: inline-size;
    background: var(--bg-primary);
  }

  :global(:root.dark) .kata-graph-pane {
    --kata-graph-edge-ambient: color-mix(in srgb, var(--text-secondary) 88%, var(--bg-primary));
    --kata-graph-edge-context: color-mix(in srgb, var(--text-muted) 45%, var(--bg-primary));
    --kata-graph-edge-related: color-mix(in srgb, var(--text-muted) 74%, var(--bg-primary));
    --kata-graph-edge-selected: var(--text-primary);
    --kata-graph-edge-selected-blocks: var(--accent-amber);
  }

  .graph-toolbar {
    flex: 0 0 auto;
    display: flex;
    flex-direction: column;
    gap: 8px;
    padding: 10px 14px;
    border-bottom: 1px solid var(--border-default);
    background: var(--bg-surface);
  }

  .graph-title-row,
  .graph-control-row {
    display: flex;
    align-items: center;
    gap: 8px;
    min-width: 0;
  }

  .graph-title-row {
    width: 100%;
  }

  .graph-control-row {
    flex-wrap: wrap;
  }

  .toolbar-button {
    box-sizing: border-box;
    min-height: 28px;
    display: inline-flex;
    align-items: center;
    gap: 6px;
    border: 1px solid var(--border-default);
    border-radius: 6px;
    background: var(--bg-primary);
    color: var(--text-secondary);
    padding: 4px 8px;
    font: inherit;
    font-size: var(--font-size-xs);
    cursor: pointer;
  }

  .toolbar-button {
    flex: 0 0 auto;
  }

  .toolbar-button:hover {
    background: var(--bg-hover);
    color: var(--text-primary);
  }

  .graph-filter-menu {
    flex: 0 1 auto;
    min-width: 0;
  }

  .graph-filter-menu :global(.kit-filter-dropdown__btn) {
    min-height: 30px;
    height: 30px;
    max-width: 100%;
    background: var(--bg-primary);
    border-color: var(--border-default);
    color: var(--text-secondary);
  }

  .graph-filter-menu :global(.kit-filter-dropdown__btn:hover:not(:disabled)) {
    background: var(--bg-hover);
    color: var(--text-primary);
  }

  .graph-filter-menu :global(.kit-filter-dropdown__trigger-detail) {
    min-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
    color: var(--text-primary);
    font-weight: 600;
  }

  .graph-filter-menu :global(.kit-filter-dropdown__btn--active) {
    border-color: var(--accent-blue);
  }

  .graph-filter-menu :global(.kit-filter-dropdown__panel) {
    max-height: min(520px, calc(100vh - 24px));
    overflow-y: auto;
  }

  .graph-source {
    flex: 1 1 180px;
    min-width: 0;
    display: flex;
    align-items: center;
  }

  .graph-source strong {
    min-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
    color: var(--text-primary);
    font-size: var(--font-size-sm);
  }

  @container (max-width: 700px) {
    .graph-toolbar {
      padding: 9px 12px;
    }

    .graph-source {
      flex: 1 1 auto;
      min-height: 28px;
    }

    .graph-filter-menu :global(.kit-filter-dropdown__btn) {
      width: 100%;
    }
  }

  .graph-canvas {
    position: relative;
    flex: 1 1 auto;
    min-height: 360px;
    overflow: hidden;
    contain: paint;
  }

  :global(.kata-graph-pane .svelte-flow__controls) {
    border: 1px solid var(--border-default);
    border-radius: 6px;
    box-shadow: var(--shadow-sm);
    overflow: hidden;
  }

  :global(.kata-graph-pane .svelte-flow__controls-button) {
    border: 1px solid var(--border-default);
    border-width: 0 0 1px;
    background: var(--bg-surface-hover);
    color: var(--text-primary);
  }

  :global(.kata-graph-pane .svelte-flow__controls-button:last-child) {
    border-bottom: 0;
  }

  :global(.kata-graph-pane .svelte-flow__controls-button:hover) {
    background: var(--bg-hover);
  }

  :global(.kata-graph-pane .svelte-flow__controls-button svg) {
    fill: currentColor;
  }

  :global(.kata-graph-pane .svelte-flow__minimap) {
    border: 1px solid var(--border-default);
    border-radius: 6px;
    background: var(--bg-primary);
    box-shadow: var(--shadow-sm);
  }

  :global(.kata-graph-pane .svelte-flow__minimap-mask) {
    fill: color-mix(in srgb, var(--bg-primary) 68%, transparent);
    stroke: var(--accent-blue);
  }

  :global(.kata-graph-pane .svelte-flow__minimap-node) {
    fill: var(--bg-hover);
    stroke: var(--border-default);
  }

  :global(.kata-graph-node .svelte-flow__handle) {
    opacity: 0;
    pointer-events: none;
  }

  :global(.kata-graph-pane .svelte-flow__edges) {
    z-index: 1;
  }

  :global(.kata-graph-pane .svelte-flow__nodes) {
    position: relative;
    z-index: 2;
  }

  :global(.kata-graph-edge .svelte-flow__edge-path) {
    stroke-width: 1.8;
  }

  :global(.kata-graph-edge--parent .svelte-flow__edge-path) {
    stroke: var(--kata-graph-edge-ambient);
  }

  :global(.kata-graph-edge--related .svelte-flow__edge-path) {
    stroke: var(--kata-graph-edge-related);
    stroke-dasharray: 6 4;
  }

  :global(.kata-graph-edge--ambient .svelte-flow__edge-path) {
    stroke: var(--kata-graph-edge-ambient);
    stroke-width: 1.4;
  }

  :global(.kata-graph-edge--depth-context) {
    z-index: 0;
  }

  :global(.kata-graph-edge--depth-context .svelte-flow__edge-path) {
    stroke: var(--kata-graph-edge-context);
    stroke-width: 1.2;
  }

  :global(.kata-graph-edge--selected-adjacent .svelte-flow__edge-path) {
    stroke-width: 2.2;
  }

  :global(.kata-graph-edge--selected-adjacent.kata-graph-edge--blocks .svelte-flow__edge-path) {
    stroke: var(--kata-graph-edge-selected-blocks);
  }

  :global(.kata-graph-edge--selected-adjacent.kata-graph-edge--parent .svelte-flow__edge-path) {
    stroke: var(--kata-graph-edge-selected);
  }

  .graph-empty {
    margin: 16px;
    color: var(--text-muted);
    font-size: var(--font-size-sm);
  }
</style>
