<script lang="ts">
  import { Handle, Position, type NodeProps } from '@xyflow/svelte'

  import type { KataGraphNode, KataGraphNodeData } from '../lib/graph/layout'
  import { markColor } from '../lib/marks'

  let {
    id,
    data,
    selected = false,
    sourcePosition,
    targetPosition,
  }: NodeProps<KataGraphNode> & { data: KataGraphNodeData } = $props()

  let resolvedSourcePosition = $derived(
    sourcePosition ?? (data.layoutDirection === 'TB' ? Position.Bottom : Position.Right),
  )
  let resolvedTargetPosition = $derived(
    targetPosition ?? (data.layoutDirection === 'TB' ? Position.Top : Position.Left),
  )
  let tone = $derived.by(() => {
    if (data.status === 'uncached') return 'uncached'
    if (data.status === 'closed' && data.closedReason === 'done') return 'done'
    if (data.status === 'closed') return 'closed'
    return 'open'
  })
  let metaLabel = $derived(
    data.projectLabel ? `${data.projectLabel} ${data.idLabel}` : data.idLabel,
  )

  function activateFromKeyboard(event: KeyboardEvent): void {
    if (event.key !== 'Enter' && event.key !== ' ') return
    event.preventDefault()
    ;(event.currentTarget as HTMLButtonElement).click()
  }

  function activateNode(event: MouseEvent): void {
    event.stopPropagation()
    if (!data.selectable) return
    data.onSelect?.(id)
  }
</script>

<button
  type="button"
  class={[
    'graph-task-node',
    `graph-task-node--${tone}`,
    data.isSource ? 'graph-task-node--source' : '',
    data.isSource || selected ? 'graph-task-node--selected' : '',
    data.isDepthContext ? 'graph-task-node--depth-context' : '',
  ]}
  aria-label={data.accessibleLabel}
  title={data.qualifiedLabel}
  aria-current={data.isSource ? 'true' : undefined}
  aria-pressed={data.isSource || selected}
  disabled={!data.selectable}
  onclick={activateNode}
  onkeydown={activateFromKeyboard}
>
  <span class="node-title-row">
    <span
      class={[
        'status-mark',
        tone === 'done' ? 'status-mark--done' : '',
        tone === 'closed' ? 'status-mark--closed' : '',
        tone === 'uncached' ? 'status-mark--uncached' : '',
      ]}
      aria-hidden="true"
    ></span>
    <span class="node-title" title={data.title}>{data.title}</span>
    {#if data.priorityLabel}
      <span class="priority-marker" data-priority={data.priorityLabel}>{data.priorityLabel}</span>
    {/if}
  </span>
  <span class="node-meta-row" title={metaLabel}>
    {#if data.projectName}<span
        class="project-mark"
        style:background={markColor(data.projectName)}
        aria-hidden="true"
      ></span>{/if}
    {#if data.projectLabel}<span class="node-project">{data.projectLabel}</span>{/if}
    <span class="node-id">{data.idLabel}</span>
  </span>
  <Handle
    class="graph-task-handle"
    type="target"
    position={resolvedTargetPosition}
    isConnectable={false}
    aria-hidden="true"
  />
  <Handle
    class="graph-task-handle"
    type="source"
    position={resolvedSourcePosition}
    isConnectable={false}
    aria-hidden="true"
  />
</button>

<style>
  /* A list row as a card: the status circle, the title, and the priority
     over the project's mark and the ID. The lines carry the relation, so the
     card takes no colored edge. */
  .graph-task-node {
    width: 100%;
    height: 100%;
    display: flex;
    flex-direction: column;
    justify-content: center;
    gap: var(--space-1);
    box-sizing: border-box;
    border: 1px solid var(--border-default);
    border-radius: 6px;
    background: var(--bg-surface);
    color: var(--text-primary);
    padding: 9px 12px;
    box-shadow: var(--shadow-sm);
    text-align: left;
    font: inherit;
    cursor: pointer;
  }

  .graph-task-node:hover:not(:disabled) {
    background: var(--bg-surface-hover);
  }

  .graph-task-node:disabled {
    cursor: default;
  }

  .graph-task-node--source {
    border-color: var(--accent-blue);
  }

  /* The task open beside the graph: a blue tint inside a blue ring. */
  .graph-task-node--selected,
  .graph-task-node--selected:hover:not(:disabled) {
    border-color: var(--accent-blue);
    background: color-mix(in srgb, var(--accent-blue) 14%, var(--bg-surface));
    box-shadow:
      0 0 0 1px var(--accent-blue),
      var(--shadow-sm);
  }

  .graph-task-node--closed .node-title,
  .graph-task-node--done .node-title {
    color: var(--text-secondary);
  }

  .graph-task-node--done {
    opacity: 0.62;
  }

  .graph-task-node--uncached {
    border-style: dashed;
    box-shadow: none;
    color: var(--text-muted);
  }

  .graph-task-node--depth-context {
    opacity: 0.36;
  }

  .graph-task-node--depth-context:hover,
  .graph-task-node--depth-context:focus-visible {
    opacity: 0.58;
  }

  .node-title-row,
  .node-meta-row {
    min-width: 0;
    display: flex;
    align-items: center;
    gap: var(--space-2);
  }

  .node-title {
    flex: 1;
    min-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
    font-size: var(--font-size-md);
    line-height: 20px;
  }

  /* The task list's status circle: hollow while open, filled once closed. */
  .status-mark {
    flex: 0 0 14px;
    width: 14px;
    height: 14px;
    box-sizing: border-box;
    border: 1.5px solid var(--text-muted);
    border-radius: 50%;
  }

  .status-mark--done {
    border-color: var(--accent-green);
    background: var(--accent-green);
  }

  .status-mark--closed {
    background: var(--text-muted);
  }

  .status-mark--uncached {
    border-style: dashed;
  }

  /* The project's mark and the ID line up under the title, past the status
     circle; a long project name cuts at its end and the ID never cuts. */
  .node-meta-row {
    padding-left: calc(14px + var(--space-2));
    color: var(--text-secondary);
    font-size: var(--font-size-sm);
    line-height: 16px;
    white-space: nowrap;
  }

  .project-mark {
    flex: 0 0 10px;
    width: 10px;
    height: 10px;
    border-radius: 3px;
  }

  .node-project {
    min-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
  }

  .node-id {
    flex: 0 0 auto;
    font-family: var(--font-mono);
  }

  .priority-marker {
    flex: 0 0 auto;
    border-radius: var(--radius-sm);
    background: var(--bg-inset);
    color: var(--text-secondary);
    padding: 3px 5px;
    font-size: var(--font-size-xs);
    font-weight: var(--font-weight-semibold);
    line-height: 1;
  }

  .priority-marker[data-priority='P0'] {
    background: color-mix(in srgb, var(--accent-red) 14%, transparent);
    color: var(--accent-red);
  }

  .priority-marker[data-priority='P1'] {
    background: color-mix(in srgb, var(--accent-amber) 14%, transparent);
    color: var(--accent-amber);
  }

  :global(.graph-task-handle) {
    width: 1px;
    height: 1px;
    border: 0;
    background: transparent;
    opacity: 0;
    pointer-events: none;
  }
</style>
