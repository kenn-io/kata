import { expect, test } from 'vitest'
import * as hegel from '@hegeldev/hegel'
import * as gs from '@hegeldev/hegel/generators'
import { parseCronDocument } from './document'
import { buildWorkflowDraft, validateWorkflowDraft } from './workflowDraft'
import { workflowGraph } from './workflowGraph'
const make = (steps: { key: string; after: string[] }[]) =>
  buildWorkflowDraft(
    {
      uid: 'workflow1',
      name: 'Example',
      definitionEventUID: 'event1',
      document: parseCronDocument(
        JSON.stringify({
          version: 1,
          steps: steps.map((s) => ({ ...s, kind: 'command', command: 'echo ok' })),
        }),
      ),
    },
    1,
  )
test('flags duplicate, missing, self, later and cycle dependencies on rows and graph nodes', () => {
  const draft = make([
    { key: 'a', after: ['b', 'missing', 'a'] },
    { key: 'b', after: ['a'] },
    { key: 'b', after: [] },
  ])
  const graph = workflowGraph(draft)
  expect(new Set(graph.problems.map((p) => p.code))).toEqual(
    new Set(['duplicate', 'missing', 'later', 'cycle', 'invalid']),
  )
  expect(graph.nodes.every((n) => n.data.invalid === true)).toBe(true)
  expect(graph.nodes.some((n) => n.id === 'missing:missing')).toBe(true)
  expect(graph.edges.every((e) => e.style?.includes('var(--accent-red)'))).toBe(true)
})
test('duplicate after references are diagnosed and drawn once', () => {
  const graph = workflowGraph(
    make([
      { key: 'a', after: [] },
      { key: 'b', after: ['a', 'a'] },
    ]),
  )
  expect(graph.edges).toHaveLength(1)
  expect(graph.problems).toContainEqual(
    expect.objectContaining({ code: 'duplicate', field: 'after' }),
  )
})
test(
  'Hegel accepts exactly the dependency graphs allowed by an independent DFS/index oracle',
  () =>
    hegel.test(
      (tc) => {
        const count = 1 + Math.abs(tc.draw(gs.integers()) % 12)
        const steps = Array.from({ length: count }, (_, i) => ({
          key: `s${i}`,
          after: Array.from(
            { length: 1 + Math.abs(tc.draw(gs.integers()) % (count + 1)) },
            () => `s${Math.abs(tc.draw(gs.integers()) % (count + 1))}`,
          ),
        }))
        const indexes = new Map(steps.map((s, i) => [s.key, i]))
        const visiting = new Set<string>(),
          visited = new Set<string>()
        function cyclic(key: string): boolean {
          if (visiting.has(key)) return true
          if (visited.has(key)) return false
          visiting.add(key)
          for (const next of steps[indexes.get(key) ?? -1]?.after ?? [])
            if (indexes.has(next) && cyclic(next)) return true
          visiting.delete(key)
          visited.add(key)
          return false
        }
        const cycle = steps.some((s) => cyclic(s.key))
        const valid =
          !cycle &&
          steps.every(
            (s, i) =>
              new Set(s.after).size === s.after.length &&
              s.after.every((key) => indexes.has(key) && indexes.get(key)! < i),
          )
        const draft = make(steps),
          problems = validateWorkflowDraft(draft)
        expect(problems.length === 0).toBe(valid)
        expect(problems.some((p) => p.code === 'cycle')).toBe(cycle)
        const expected = steps.flatMap((s, i) =>
          [...new Set(s.after)].map((key) => [
            indexes.has(key) ? draft.steps[indexes.get(key)!]!.editorID : `missing:${key}`,
            draft.steps[i]!.editorID,
          ]),
        )
        expect(workflowGraph(draft).edges.map((e) => [e.source, e.target])).toEqual(expected)
      },
      { testCases: 2000 },
    ),
  30_000,
)
test(
  'Hegel valid earlier-step graphs retain order and exactly one edge per dependency',
  () =>
    hegel.test(
      (tc) => {
        const count = 1 + Math.abs(tc.draw(gs.integers()) % 16)
        const steps = Array.from({ length: count }, (_, i) => ({
          key: `s${i}`,
          after: Array.from({ length: i }, (_, j) => j)
            .filter(() => tc.draw(gs.booleans()))
            .map((j) => `s${j}`),
        }))
        const draft = make(steps),
          graph = workflowGraph(draft)
        expect(graph.problems).toEqual([])
        expect(graph.nodes.map((n) => n.id)).toEqual(draft.steps.map((s) => s.editorID))
        expect(graph.edges.map((e) => [e.source, e.target])).toEqual(
          steps.flatMap((s, target) =>
            s.after.map((key) => [
              draft.steps[Number(key.slice(1))]!.editorID,
              draft.steps[target]!.editorID,
            ]),
          ),
        )
      },
      { testCases: 2000 },
    ),
  30_000,
)
