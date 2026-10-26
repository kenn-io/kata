import { cleanup, render, screen } from '@testing-library/svelte'
import { afterEach, expect, test, vi } from 'vitest'
import type { ElkNode } from 'elkjs'
import CronWorkflowGraph from './CronWorkflowGraph.svelte'
import { buildWorkflowDraft, renameWorkflowStep } from '../../lib/cron/workflowDraft'
import { parseCronDocument } from '../../lib/cron/document'
const pending = vi.hoisted(
  () =>
    [] as { graph: ElkNode; resolve: (value: ElkNode) => void; reject: (error: Error) => void }[],
)
vi.mock('elkjs/lib/elk.bundled.js', () => ({
  default: class {
    layout(graph: ElkNode) {
      return new Promise<ElkNode>((resolve, reject) => pending.push({ graph, resolve, reject }))
    }
  },
}))
afterEach(() => {
  cleanup()
  pending.length = 0
})
const draft = () =>
  buildWorkflowDraft(
    {
      uid: 'workflow1',
      name: 'Example',
      definitionEventUID: 'event1',
      document: parseCronDocument(
        '{"version":1,"steps":[{"key":"first","kind":"command","command":"echo ok"}]}',
      ),
    },
    1,
  )
test('late layouts cannot overwrite the newest draft layout or step identities', async () => {
  const original = draft(),
    id = original.steps[0]!.editorID
  const view = render(CronWorkflowGraph, { draft: original })
  await vi.waitFor(() => expect(pending.length).toBe(1))
  await view.rerender({ draft: renameWorkflowStep(original, id, 'renamed') })
  await vi.waitFor(() => expect(pending.length).toBe(2))
  pending[1]!.resolve({ ...pending[1]!.graph, children: [{ id, x: 80, y: 90 }] })
  await vi.waitFor(() =>
    expect(document.querySelector(`[data-id="${id}"]`)?.getAttribute('style')).toContain(
      '80px, 90px',
    ),
  )
  pending[0]!.resolve({ ...pending[0]!.graph, children: [{ id, x: 1, y: 2 }] })
  await new Promise((r) => setTimeout(r, 0))
  expect(document.querySelector(`[data-id="${id}"]`)?.getAttribute('style')).toContain('80px, 90px')
  expect(screen.getByText('renamed')).toBeTruthy()
  expect(original.steps[0]!.key).toBe('first')
})
test('layout failure leaves the graph diagnostics and original ordered draft available', async () => {
  const original = draft()
  render(CronWorkflowGraph, { draft: original })
  await vi.waitFor(() => expect(pending.length).toBe(1))
  pending[0]!.reject(new Error('layout failed'))
  await screen.findByText(/Layout unavailable/)
  expect(screen.getByText('first')).toBeTruthy()
  expect(original.steps.map((s) => s.key)).toEqual(['first'])
})
