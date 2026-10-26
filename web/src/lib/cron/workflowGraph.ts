import type { Node, Edge } from '@xyflow/svelte'
import { validateWorkflowDraft, type WorkflowDraft, type WorkflowProblem } from './workflowDraft'

export function workflowGraph(draft: WorkflowDraft): {
  nodes: Node[]
  edges: Edge[]
  problems: WorkflowProblem[]
} {
  const problems = validateWorkflowDraft(draft)
  const invalid = new Set(problems.flatMap((p) => (p.editorID ? [p.editorID] : [])))
  const byKey = new Map(draft.steps.map((s) => [s.key, s.editorID]))
  const nodes: Node[] = draft.steps.map((s, i) => ({
    id: s.editorID,
    position: { x: i * 240, y: 0 },
    data: { label: s.key || '(unnamed)', invalid: invalid.has(s.editorID) },
    ...(invalid.has(s.editorID)
      ? { style: 'border-color:var(--accent-red);color:var(--accent-red)' }
      : {}),
  }))
  const edges: Edge[] = []
  for (const step of draft.steps)
    for (const key of new Set(step.after)) {
      const source = byKey.get(key) ?? `missing:${key}`
      if (!byKey.has(key) && !nodes.some((n) => n.id === source))
        nodes.push({
          id: source,
          position: { x: 0, y: 120 },
          data: { label: `Missing: ${key}`, invalid: true },
          style: 'border-color:var(--accent-red);color:var(--accent-red)',
        })
      edges.push({
        id: `${source}:${step.editorID}`,
        source,
        target: step.editorID,
        style:
          invalid.has(step.editorID) || !byKey.has(key)
            ? 'stroke:var(--accent-red)'
            : 'stroke:var(--accent-green)',
      })
    }
  return { nodes, edges, problems }
}
