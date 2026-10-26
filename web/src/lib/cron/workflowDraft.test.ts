import { expect, test } from 'vitest'
import * as hegel from '@hegeldev/hegel'
import * as gs from '@hegeldev/hegel/generators'
import { parseCronDocument, rawCronValue } from './document'
import {
  buildWorkflowDraft,
  setWorkflowField,
  setWorkflowStepField,
  renameWorkflowStep,
  workflowDependents,
  deleteWorkflowStep,
  addWorkflowStep,
  validateWorkflowDraft,
  serializeWorkflowDraft,
  rebaseWorkflowDraft,
} from './workflowDraft'
import { workflowGraph } from './workflowGraph'

const opaque = '{"big":9007199254740993,"exp":1e40,"zero":-0,"nil":null,"text":"\\u0061"}'
const source = ` {"version":1,"about":"Before","input":"Input","options":${opaque},"steps":[{"key":"first","kind":"command","command":"echo first","options":${opaque}},{"key":"second","kind":"prompt","prompt":"Continue","after":["first"],"retries":0},{"key":"third","kind":"command","command":"echo third","after":["first","second"]}]} `
const record = (text = source) => ({
  uid: 'workflow1',
  name: 'Example',
  definitionEventUID: 'event1',
  document: parseCronDocument(text),
})
const draft = () => buildWorkflowDraft(record(), 1)
test('untouched and name-only edits preserve the complete original source', () => {
  const original = draft()
  expect(serializeWorkflowDraft(original).source).toBe(source)
  const renamed = setWorkflowField(original, 'name', 'Renamed')
  expect(renamed.values.name).toBe('Renamed')
  expect(serializeWorkflowDraft(renamed).source).toBe(source)
  expect(renamed.uid).toBe(original.uid)
})
test('rename rewrites all incoming references, preserving editor identities and opaque fields', () => {
  const original = draft(),
    first = original.steps[0]!
  const changed = renameWorkflowStep(original, first.editorID, 'renamed')
  expect(changed.steps.map((s) => s.editorID)).toEqual(original.steps.map((s) => s.editorID))
  expect(changed.steps.map((s) => s.after)).toEqual([[], ['renamed'], ['renamed', 'second']])
  expect(rawCronValue(serializeWorkflowDraft(changed), ['steps', 0, 'options'])).toBe(opaque)
  expect(original.steps[1]!.after).toEqual(['first'])
  expect(changed.original.source).toBe(source)
})
test.each(['second', '', '.', '..', 'bad/key', 'has space', 'x'.repeat(257)])(
  'invalid rename %s is atomic',
  (key) => {
    const original = draft()
    expect(() => renameWorkflowStep(original, original.steps[0]!.editorID, key)).toThrow()
    expect(serializeWorkflowDraft(original).source).toBe(source)
  },
)
test('portable keys accept native Unicode letters and digits within the UTF-8 byte limit', () => {
  const original = draft()
  expect(renameWorkflowStep(original, original.steps[0]!.editorID, 'étape_１').steps[0]!.key).toBe(
    'étape_１',
  )
  expect(() => renameWorkflowStep(original, original.steps[0]!.editorID, 'é'.repeat(129))).toThrow()
})
test('delete requires confirmation for direct dependents and removes only those references', () => {
  const original = draft(),
    id = original.steps[0]!.editorID
  expect(workflowDependents(original, id)).toEqual(['second', 'third'])
  expect(() => deleteWorkflowStep(original, id, false)).toThrow(/confirm/i)
  expect(serializeWorkflowDraft(original).source).toBe(source)
  const changed = deleteWorkflowStep(original, id, true)
  expect(changed.steps.map((s) => [s.key, s.after])).toEqual([
    ['second', []],
    ['third', ['second']],
  ])
  expect(rawCronValue(serializeWorkflowDraft(changed), ['options'])).toBe(opaque)
})
test('kind switch clears only the inapplicable field as an explicit dirty edit', () => {
  const original = draft(),
    id = original.steps[0]!.editorID
  const changed = setWorkflowStepField(original, id, 'kind', 'prompt')
  expect(changed.steps[0]!.command).toBe('')
  expect(changed.steps[0]!.dirty.has('command')).toBe(true)
  const ready = setWorkflowStepField(changed, id, 'prompt', 'New prompt')
  expect(rawCronValue(serializeWorkflowDraft(ready), ['steps', 0, 'command'])).toBeUndefined()
  expect(rawCronValue(serializeWorkflowDraft(ready), ['steps', 0, 'prompt'])).toBe('"New prompt"')
  expect(original.steps[0]!.command).toBe('echo first')
})
test.each(['-1', '101', '1.5', '1e2', '', '9007199254740993'])(
  'invalid retry %s remains visible and prevents save',
  (retries) => {
    const original = draft(),
      changed = setWorkflowStepField(original, original.steps[0]!.editorID, 'retries', retries)
    expect(changed.steps[0]!.retries).toBe(retries)
    expect(validateWorkflowDraft(changed)).toContainEqual(
      expect.objectContaining({ field: 'retries', code: 'invalid' }),
    )
    expect(() => serializeWorkflowDraft(changed)).toThrow()
  },
)
test.each(['0', '100'])('accepts retry boundary %s', (retries) => {
  const original = draft(),
    changed = setWorkflowStepField(original, original.steps[0]!.editorID, 'retries', retries)
  expect(validateWorkflowDraft(changed)).toEqual([])
  expect(rawCronValue(serializeWorkflowDraft(changed), ['steps', 0, 'retries'])).toBe(retries)
})
test('untouched native null retry values display as zero and preserve their raw source', () => {
  const original = buildWorkflowDraft(record(source.replace('"retries":0', '"retries":null')), 1)
  expect(original.steps[1]!.retries).toBe('0')
  expect(serializeWorkflowDraft(original).source).toBe(
    source.replace('"retries":0', '"retries":null'),
  )
})
test('new steps have distinct stable ULIDs and at least one step is required', () => {
  const empty = buildWorkflowDraft(undefined, 2)
  expect(validateWorkflowDraft(empty)).toContainEqual(
    expect.objectContaining({ field: 'steps', code: 'missing' }),
  )
  const one = addWorkflowStep(empty, 'command'),
    two = addWorkflowStep(one, 'prompt')
  expect(two.steps.map((s) => s.kind)).toEqual(['command', 'prompt'])
  expect(two.steps[0]!.editorID).toMatch(/^[0-7][0-9A-HJKMNP-TV-Z]{25}$/)
  expect(two.steps[0]!.editorID).not.toBe(two.steps[1]!.editorID)
})
test.each([
  '[]',
  'null',
  '{"nested":{"API_KEY":"value"}}',
  '{"list":[{"path":"value"}]}',
  '{',
  '{"a":1,"a":2}',
])('options %s reports a field error without losing the entered text', (options) => {
  const changed = setWorkflowField(draft(), 'options', options)
  expect(changed.options.source).toBe(options)
  expect(validateWorkflowDraft(changed)).toContainEqual(
    expect.objectContaining({ field: 'options', code: 'invalid' }),
  )
  expect(() => serializeWorkflowDraft(changed)).toThrow()
})
test('opaque option edits preserve exact raw numbers at both workflow and step level', () => {
  let changed = setWorkflowField(draft(), 'options', '{"new":9007199254740993}')
  changed = setWorkflowStepField(changed, changed.steps[1]!.editorID, 'options', '{"new":1e400}')
  const output = serializeWorkflowDraft(changed)
  expect(rawCronValue(output, ['options'])).toBe('{"new":9007199254740993}')
  expect(rawCronValue(output, ['steps', 1, 'options'])).toBe('{"new":1e400}')
  expect(rawCronValue(output, ['steps', 0, 'options'])).toBe(opaque)
})
test('unknown typed fields refuse save visibly instead of losing values', () => {
  for (const text of [
    source.replace('"version":1', '"version":1,"future":1'),
    source.replace('"key":"first"', '"key":"first","future":1'),
  ])
    expect(() => serializeWorkflowDraft(buildWorkflowDraft(record(text), 1))).toThrow(/preserv/i)
})
test('rebase keeps newer untouched raw values and local explicit fields through renames and additions', () => {
  const original = draft()
  let intended = renameWorkflowStep(original, original.steps[0]!.editorID, 'renamed')
  intended = setWorkflowField(intended, 'about', 'My about')
  intended = setWorkflowStepField(intended, original.steps[1]!.editorID, 'prompt', 'My prompt')
  const newer = {
    ...record(
      source
        .replace('"Input"', '"New input"')
        .replace('9007199254740993', '9007199254740995')
        .replace('"echo third"', '"echo remote"'),
    ),
    definitionEventUID: 'event2',
    name: 'Remote name',
  }
  const rebased = rebaseWorkflowDraft(intended, newer)
  const output = serializeWorkflowDraft(rebased)
  expect(rebased.values.name).toBe('Remote name')
  expect(rebased.definitionEventUID).toBe('event2')
  expect(rawCronValue(output, ['input'])).toBe('"New input"')
  expect(rawCronValue(output, ['about'])).toBe('"My about"')
  expect(rawCronValue(output, ['options', 'big'])).toBe('9007199254740995')
  expect(rawCronValue(output, ['steps', 1, 'prompt'])).toBe('"My prompt"')
  expect(rawCronValue(output, ['steps', 2, 'command'])).toBe('"echo remote"')
  expect(rebased.steps.map((s) => s.editorID)).toEqual(original.steps.map((s) => s.editorID))
})
test('rebase visibly refuses ambiguous concurrent step rename or deletion', () => {
  const original = draft(),
    intended = setWorkflowField(original, 'about', 'My about')
  expect(() =>
    rebaseWorkflowDraft(intended, record(source.replaceAll('"first"', '"remote"'))),
  ).toThrow(/ambig|correspond|renamed|deleted/i)
  const newer = record(
    '{"version":1,"steps":[{"key":"third","kind":"command","command":"echo third"}]}',
  )
  expect(() => rebaseWorkflowDraft(intended, newer)).toThrow(/ambig|correspond|renamed|deleted/i)
})
test('rename rebase preserves newly added remote dependencies unless runs-after was explicitly edited', () => {
  const original = draft()
  const renamed = renameWorkflowStep(original, original.steps[0]!.editorID, 'renamed')
  const newer = record(source.replace('"after":["first","second"]', '"after":["first"]'))
  const rebased = rebaseWorkflowDraft(renamed, newer)
  expect(rebased.steps[2]!.after).toEqual(['renamed'])
  const explicit = setWorkflowStepField(renamed, original.steps[2]!.editorID, 'after', ['second'])
  expect(rebaseWorkflowDraft(explicit, newer).steps[2]!.after).toEqual(['second'])
})
test('deletion rebase refuses newly introduced direct dependent references', () => {
  const original = draft(),
    intended = deleteWorkflowStep(original, original.steps[0]!.editorID, true)
  const newer = record(
    source.replace(
      /\]\}\s*$/,
      ',{"key":"remote","kind":"command","command":"echo remote","after":["first"]}]}',
    ),
  )
  expect(() => rebaseWorkflowDraft(intended, newer)).toThrow(/depend.*deleted|references/i)
})
test('rebase preserves newer additions and appends explicit new local steps without losing order or raw values', () => {
  const original = draft()
  let intended = addWorkflowStep(original, 'command')
  const id = intended.steps.at(-1)!.editorID
  intended = setWorkflowStepField(intended, id, 'command', 'echo local')
  intended = setWorkflowStepField(intended, id, 'after', ['third'])
  const remote = `{"key":"remote","kind":"command","command":"echo remote","options":${opaque}}`
  const rebased = rebaseWorkflowDraft(
    intended,
    record(source.replace('"steps":[', `"steps":[${remote},`)),
  )
  expect(rebased.steps.map((s) => s.key)).toEqual(['remote', 'first', 'second', 'third', 'step4'])
  expect(rebased.steps.at(-1)!.editorID).toBe(id)
  expect(rawCronValue(serializeWorkflowDraft(rebased), ['steps', 0, 'options'])).toBe(opaque)
  expect(rawCronValue(serializeWorkflowDraft(rebased), ['steps', 4, 'after'])).toBe('["third"]')
})
test('rebase applies a confirmed local deletion before reusing that key in a local rename', () => {
  const original = draft()
  let intended = deleteWorkflowStep(original, original.steps[1]!.editorID, true)
  intended = renameWorkflowStep(intended, original.steps[0]!.editorID, 'second')
  const rebased = rebaseWorkflowDraft(intended, { ...record(), definitionEventUID: 'event2' })
  expect(rebased.steps.map((s) => [s.key, s.after])).toEqual([
    ['second', []],
    ['third', ['second']],
  ])
  expect(rawCronValue(serializeWorkflowDraft(rebased), ['steps', 0, 'options'])).toBe(opaque)
})
test('500 earlier-step chain validates without changing order', () => {
  const steps = Array.from({ length: 500 }, (_, i) => ({
    key: `step${i}`,
    kind: 'command',
    command: 'echo ok',
    ...(i ? { after: [`step${i - 1}`] } : {}),
  }))
  const large = buildWorkflowDraft(record(JSON.stringify({ version: 1, steps })), 1)
  expect(validateWorkflowDraft(large)).toEqual([])
  expect(large.steps.map((s) => s.key)).toEqual(steps.map((s) => s.key))
})

function localDependencyDraft(edges: number[][]) {
  let value = buildWorkflowDraft(undefined, 1)
  for (let i = 0; i < edges.length; i++) {
    value = addWorkflowStep(value, 'command')
    const id = value.steps[i]!.editorID
    value = renameWorkflowStep(value, id, `s${i}`)
    value = setWorkflowStepField(value, id, 'command', 'echo inspection')
  }
  for (const [i, targets] of edges.entries())
    value = setWorkflowStepField(
      value,
      value.steps[i]!.editorID,
      'after',
      targets.map((j) => `s${j}`),
    )
  return value
}
test('overlapping cycle membership includes every participant and excludes a one-way dependent', () => {
  const value = localDependencyDraft([[1, 2], [0], [1], [0]])
  const actual = validateWorkflowDraft(value)
    .filter((problem) => problem.code === 'cycle')
    .map((problem) => problem.editorID)
    .sort()
  expect(actual).toEqual(
    value.steps
      .slice(0, 3)
      .map((step) => step.editorID)
      .sort(),
  )
})
test(
  'Hegel cycle membership equals independent transitive closure for every local step',
  () =>
    hegel.test(
      (tc) => {
        const count = 1 + Math.abs(tc.draw(gs.integers()) % 8)
        const edges = Array.from({ length: count }, () =>
          Array.from({ length: count }, (_, j) => j).filter(() => tc.draw(gs.booleans())),
        )
        const value = localDependencyDraft(edges)
        // A nonempty path back to the same vertex proves membership, independently of DFS colors.
        const reaches = edges.map((targets) =>
          Array.from({ length: count }, (_, j) => targets.includes(j)),
        )
        for (let via = 0; via < count; via++)
          for (let from = 0; from < count; from++)
            for (let to = 0; to < count; to++)
              reaches[from]![to] ||= reaches[from]![via]! && reaches[via]![to]!
        const expected = value.steps
          .filter((_, i) => reaches[i]![i])
          .map((step) => step.editorID)
          .sort()
        const actual = validateWorkflowDraft(value)
          .filter((problem) => problem.code === 'cycle')
          .map((problem) => problem.editorID)
          .sort()
        expect(actual).toEqual(expected)
      },
      { testCases: 2000 },
    ),
  30_000,
)

test(
  'Hegel rename preserves dependency connectivity by editor identity',
  () =>
    hegel.test(
      (tc) => {
        const seed = tc.draw(gs.integers()),
          count = 1 + Math.abs(seed % 14)
        const steps = Array.from({ length: count }, (_, i) => ({
          key: `s${i}`,
          kind: i % 2 ? 'prompt' : 'command',
          ...(i % 2 ? { prompt: 'work' } : { command: 'echo ok' }),
          after: Array.from({ length: i }, (_, j) => j)
            .filter(() => tc.draw(gs.booleans()))
            .map((j) => `s${j}`),
        }))
        const original = buildWorkflowDraft(record(JSON.stringify({ version: 1, steps })), 1),
          selected = Math.abs(tc.draw(gs.integers()) % count)
        const changed = renameWorkflowStep(
          original,
          original.steps[selected]!.editorID,
          `new${selected}`,
        )
        const expected = steps.flatMap((s, target) =>
          s.after.map((key) => [
            original.steps[Number(key.slice(1))]!.editorID,
            original.steps[target]!.editorID,
          ]),
        )
        expect(workflowGraph(changed).edges.map((e) => [e.source, e.target])).toEqual(expected)
      },
      { testCases: 2000 },
    ),
  30_000,
)

test(
  'Hegel confirmed deletion preserves unrelated order, raw options and numbers',
  () =>
    hegel.test(
      (tc) => {
        const count = 1 + Math.abs(tc.draw(gs.integers()) % 12),
          selected = Math.abs(tc.draw(gs.integers()) % count)
        const rawOptions = `{"number":${tc.draw(gs.bigIntegers())},"exp":1e${tc.draw(gs.integers())},"text":${JSON.stringify(tc.draw(gs.text({ codec: 'utf-8' })))}}`
        const rows = Array.from(
          { length: count },
          (_, i) =>
            `{"key":"s${i}","kind":"command","command":"echo ok","options":${rawOptions},"after":${JSON.stringify(
              Array.from({ length: i }, (_, j) => j)
                .filter(() => tc.draw(gs.booleans()))
                .map((j) => `s${j}`),
            )}}`,
        )
        const original = buildWorkflowDraft(record(`{"version":1,"steps":[${rows.join(',')}]}`), 1),
          changed = deleteWorkflowStep(original, original.steps[selected]!.editorID, true)
        expect(changed.steps.map((s) => [s.key, s.after])).toEqual(
          original.steps
            .filter((_, i) => i !== selected)
            .map((s) => [s.key, s.after.filter((key) => key !== `s${selected}`)]),
        )
        if (changed.steps.length) {
          const output = serializeWorkflowDraft(changed)
          for (let i = 0; i < changed.steps.length; i++)
            expect(rawCronValue(output, ['steps', i, 'options'])).toBe(rawOptions)
        }
      },
      { testCases: 2000 },
    ),
  30_000,
)
