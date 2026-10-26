<script lang="ts">
  import ELK from 'elkjs/lib/elk.bundled.js'
  import { SvelteFlow, Background, Controls, type Node } from '@xyflow/svelte'
  import '@xyflow/svelte/dist/style.css'
  import type { WorkflowDraft } from '../../lib/cron/workflowDraft'
  import { workflowGraph } from '../../lib/cron/workflowGraph'
  let { draft }: { draft: WorkflowDraft } = $props()
  const graph = $derived(workflowGraph(draft))
  const elk = new ELK({
    defaultLayoutOptions: { 'elk.algorithm': 'layered', 'elk.direction': 'RIGHT' },
  })
  let generation = 0
  let layout = $state.raw<{ graph: typeof graph; nodes: Node[]; error?: string } | undefined>(
    undefined,
  )
  const nodes = $derived(layout?.graph === graph ? layout.nodes : graph.nodes)
  $effect(() => {
    const current = graph,
      run = ++generation
    void elk
      .layout({
        id: 'workflow',
        children: current.nodes.map((n) => ({ id: n.id, width: 200, height: 60 })),
        edges: current.edges.map((e) => ({ id: e.id, sources: [e.source], targets: [e.target] })),
      })
      .then((result) => {
        if (run !== generation) return
        const positions = new Map(
          (result.children ?? []).map((n) => [n.id, { x: n.x ?? 0, y: n.y ?? 0 }]),
        )
        layout = {
          graph: current,
          nodes: current.nodes.map((n) => ({ ...n, position: positions.get(n.id) ?? n.position })),
        }
      })
      .catch(() => {
        if (run === generation)
          layout = {
            graph: current,
            nodes: current.nodes,
            error:
              'Layout unavailable. The ordered step list and dependency diagnostics remain available.',
          }
      })
    return () => {
      generation++
    }
  })
</script>

<section class="workflow-graph" aria-label="Workflow dependency graph">
  <h4>Dependency graph</h4>
  {#if layout?.graph === graph && layout.error}<p role="status">{layout.error}</p>{/if}
  {#each graph.problems.filter((p) => p.field === 'after' || p.field === 'key') as problem, i (`${problem.editorID}:${problem.code}:${i}`)}
    <p role="alert">
      {draft.steps.find((s) => s.editorID === problem.editorID)?.key}: {problem.message}
    </p>
  {/each}
  <div class="canvas">
    <SvelteFlow
      {nodes}
      edges={graph.edges}
      fitView
      nodesDraggable={false}
      nodesConnectable={false}
      elementsSelectable={false}
      edgesFocusable={false}
    >
      <Background /><Controls showLock={false} />
    </SvelteFlow>
  </div>
</section>

<style>
  .workflow-graph {
    min-width: 0;
  }
  h4 {
    margin: 0 0 var(--space-2);
  }
  .canvas {
    height: 260px;
    border: 1px solid var(--border-default);
    border-radius: var(--radius-sm);
    overflow: hidden;
    background: var(--bg-primary);
  }
  p {
    font-size: var(--font-size-sm);
  }
  [role='alert'] {
    color: var(--accent-red);
  }
  :global(.workflow-graph .svelte-flow__node-default) {
    background: var(--bg-surface);
    color: var(--text-primary);
    border-color: var(--border-default);
  }
</style>
