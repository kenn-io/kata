<script lang="ts">
  import { Handle, Position, type NodeProps } from '@xyflow/svelte'

  import type { KataGraphNode, KataGraphNodeData } from '../lib/graph/layout'

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
    data.projectLabel ? `${data.projectLabel} / ${data.idLabel}` : data.idLabel,
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
    data.isSelected || selected ? 'graph-task-node--selected' : '',
    data.isDepthContext ? 'graph-task-node--depth-context' : '',
    data.adjacentRelation ? 'graph-task-node--adjacent' : '',
    data.adjacentRelation ? `graph-task-node--relation-${data.adjacentRelation}` : '',
  ]}
  aria-label={data.accessibleLabel}
  title={data.qualifiedLabel}
  aria-current={data.isSource ? 'true' : undefined}
  aria-pressed={data.isSelected || selected}
  disabled={!data.selectable}
  onclick={activateNode}
  onkeydown={activateFromKeyboard}
>
  <div class="node-title-row">
    <strong title={data.title}>{data.title}</strong>
    {#if data.priorityLabel}
      <span class="priority-marker" data-priority={data.priorityLabel}>{data.priorityLabel}</span>
    {/if}
  </div>
  <div class="node-meta-row">
    <span class="node-id" title={metaLabel}>{metaLabel}</span>
  </div>
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
  .graph-task-node {
    width: 100%;
    height: 100%;
    display: flex;
    flex-direction: column;
    justify-content: center;
    gap: var(--space-2);
    border: 1px solid var(--border-default);
    border-radius: 6px;
    background: var(--node-status-bg, var(--bg-surface));
    color: var(--text-primary);
    padding: 9px 10px;
    box-shadow:
      inset 3px 0 0 var(--node-relation-accent, var(--node-status-accent, var(--border-default))),
      var(--shadow-sm);
    text-align: left;
    font: inherit;
    cursor: pointer;
  }

  .graph-task-node:disabled {
    cursor: default;
  }

  .graph-task-node--source {
    border-color: var(--accent-blue);
  }

  .graph-task-node--selected {
    border-color: var(--accent-blue);
    background: color-mix(in srgb, var(--accent-blue) 8%, var(--node-status-bg, var(--bg-surface)));
    box-shadow:
      inset 3px 0 0 var(--node-relation-accent, var(--node-status-accent, var(--border-default))),
      0 0 0 2px color-mix(in srgb, var(--accent-blue) 82%, transparent),
      0 0 0 5px color-mix(in srgb, var(--accent-blue) 18%, transparent),
      var(--shadow-sm);
  }

  .graph-task-node--open {
    --node-status-accent: var(--border-default);
    --node-status-bg: var(--bg-surface);
  }

  .graph-task-node--closed {
    --node-status-accent: var(--text-secondary);
    --node-status-bg: color-mix(in srgb, var(--text-secondary) 8%, var(--bg-primary));
  }

  .graph-task-node--done {
    --node-status-accent: var(--text-muted);
    --node-status-bg: color-mix(in srgb, var(--text-muted) 10%, var(--bg-primary));
    opacity: 0.62;
  }

  .graph-task-node--uncached {
    --node-status-accent: var(--border-default);
    --node-status-bg: var(--bg-surface);
    border-style: dashed;
    color: var(--text-muted);
  }

  .graph-task-node--depth-context {
    opacity: 0.36;
  }

  .graph-task-node--depth-context:hover,
  .graph-task-node--depth-context:focus-visible {
    opacity: 0.58;
  }

  .graph-task-node--relation-blocks {
    --node-relation-accent: var(--accent-blue);
  }

  .graph-task-node--relation-blockedBy {
    --node-relation-accent: var(--accent-red);
  }

  .graph-task-node--relation-child {
    --node-relation-accent: var(--accent-teal);
  }

  .graph-task-node--relation-parent {
    --node-relation-accent: var(--accent-amber);
  }

  .graph-task-node--relation-related {
    --node-relation-accent: var(--accent-purple);
  }

  .node-title-row,
  .node-meta-row {
    min-width: 0;
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 8px;
  }

  .node-title-row strong {
    min-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
    font-size: var(--font-size-sm);
    font-weight: var(--font-weight-semibold);
  }

  .node-meta-row {
    color: var(--text-muted);
    font-family: var(--font-mono);
    font-size: var(--font-size-xs);
    justify-content: flex-start;
  }

  .node-id {
    min-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .priority-marker {
    flex: 0 0 auto;
    border-radius: var(--radius-sm);
    background: var(--bg-inset);
    color: var(--text-secondary);
    padding: 3px 5px;
    font-size: var(--font-size-2xs);
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
