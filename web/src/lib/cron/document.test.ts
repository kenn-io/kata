import { expect, test } from 'vitest'
import * as hegel from '@hegeldev/hegel'
import * as gs from '@hegeldev/hegel/generators'
import { findNodeAtLocation, parseTree, type Node } from 'jsonc-parser'
import { parseCronDocument, patchCronValue, rawCronValue, sameCronDefinition } from './document'

// Independent AST slices catch accidental JSON.parse/stringify rounding and prototype lookup.
function slice(source: string, path: (string | number)[]) {
  const node = findNodeAtLocation(parseTree(source)!, path)
  return node && source.slice(node.offset, node.offset + node.length)
}
const opaque =
  '{"big":9007199254740993,"exponent":1e40,"negativeZero":-0,"null":null,"empty":{},"array":[],"text":"\\u0061","__proto__":{"constructor":3}}'
test('edits only the requested source span and preserves hostile opaque keys', () => {
  const source = ` { "action":{"prompt":"old"},"options":${opaque} } `
  const original = parseCronDocument(source)
  const changed = patchCronValue(original, ['action', 'prompt'], '"new"')
  expect(changed.source).toBe(source.replace('"old"', '"new"'))
  expect(rawCronValue(changed, ['options'])).toBe(opaque)
  expect(rawCronValue(changed, ['options', '__proto__', 'constructor'])).toBe('3')
  expect(original.source).toBe(source)
})
test('inserts and deletes paths while distinguishing absent from null', () => {
  let doc = parseCronDocument(`{"options":${opaque}}`)
  doc = patchCronValue(doc, ['action', 'prompt'], '"new"')
  doc = patchCronValue(doc, ['options', 'new'], '9007199254740993')
  doc = patchCronValue(doc, ['options', 'null'], undefined)
  expect(rawCronValue(doc, ['action', 'prompt'])).toBe('"new"')
  expect(rawCronValue(doc, ['options', 'new'])).toBe('9007199254740993')
  expect(rawCronValue(doc, ['options', 'big'])).toBe('9007199254740993')
  expect(rawCronValue(doc, ['options', 'null'])).toBeUndefined()
  expect(rawCronValue(parseCronDocument('{"x":null}'), ['x'])).toBe('null')
})
for (const bad of [
  '',
  '{}{}',
  '{/*x*/"a":1}',
  '{"a":1,}',
  '{"a":1,"a":2}',
  '{"a":1,"\\u0061":2}',
  '[1,]',
  '{"x":[{"x":1,"x":2}]}',
  'NaN',
  '{',
]) {
  test(`rejects non-strict or duplicate JSON: ${bad}`, () =>
    expect(() => parseCronDocument(bad)).toThrow())
  test(`rejects invalid replacement: ${bad}`, () =>
    expect(() => patchCronValue(parseCronDocument('{}'), ['x'], bad)).toThrow())
}
test('compares exact decimal meaning without expanding exponents', () => {
  const doc = (number: string) => parseCronDocument(`{"options":{"number":${number}}}`)
  expect(sameCronDefinition('job', doc('9007199254740993'), doc('9007199254740992'))).toBe(false)
  expect(sameCronDefinition('job', doc('1e999999999999999999'), doc('10e999999999999999998'))).toBe(
    true,
  )
  expect(sameCronDefinition('workflow', doc('1e40'), doc('100e38'))).toBe(true)
  expect(sameCronDefinition('job', doc('-0'), doc('0'))).toBe(true)
})
test('normalizes native optional omissions and object order only', () => {
  expect(
    sameCronDefinition(
      'job',
      parseCronDocument(
        '{"trigger":{"kind":"manual","lead_seconds":0},"timeout_seconds":0,"checkout_key":""}',
      ),
      parseCronDocument('{"trigger":{"kind":"manual"}}'),
    ),
  ).toBe(true)
  expect(
    sameCronDefinition(
      'workflow',
      parseCronDocument('{"steps":[{"key":"a","after":[],"retries":0}],"about":""}'),
      parseCronDocument('{"steps":[{"key":"a"}]}'),
    ),
  ).toBe(true)
  expect(
    sameCronDefinition(
      'job',
      parseCronDocument('{"options":{"x":null}}'),
      parseCronDocument('{"options":{}}'),
    ),
  ).toBe(false)
  expect(
    sameCronDefinition('job', parseCronDocument('{"options":null}'), parseCronDocument('{}')),
  ).toBe(false)
  expect(
    sameCronDefinition(
      'job',
      parseCronDocument('{"options":{"a":1,"b":2}}'),
      parseCronDocument('{"options":{"b":2,"a":1}}'),
    ),
  ).toBe(true)
})

test.each([
  ['job', '{"options":{}}', '{}'],
  ['workflow', '{"options":{},"steps":[]}', '{"steps":[]}'],
  ['workflow', '{"steps":[{"options":{}}]}', '{"steps":[{}]}'],
] as const)('native empty options compare equal at %s boundary: %s', (kind, sent, stored) => {
  expect(sameCronDefinition(kind, parseCronDocument(sent), parseCronDocument(stored))).toBe(true)
  expect(sameCronDefinition(kind, parseCronDocument(stored), parseCronDocument(sent))).toBe(true)
})
test.each([
  ['job', '{"options":{"nested":{}}}', '{"options":{}}'],
  ['job', '{"options":{"nested":{"options":{}}}}', '{"options":{"nested":{}}}'],
  ['workflow', '{"steps":[{"options":{"nested":{}}}]}', '{"steps":[{"options":{}}]}'],
] as const)('native empty options never erase opaque fields in %s: %s', (kind, a, b) => {
  expect(sameCronDefinition(kind, parseCronDocument(a), parseCronDocument(b))).toBe(false)
})

// Draw full integer/text domains; bound recursive materialization rather than filtering values.
function nested(seed: number, text: string, depth = 0): string {
  const scalar = [
    JSON.stringify(text),
    String(seed),
    'null',
    'false',
    '9007199254740993',
    '1e40',
    '-0',
  ][Math.abs(seed % 7)]!
  if (depth >= 4 || seed % 3 === 0) return scalar
  return seed % 2
    ? `[${scalar},${nested(Math.trunc(seed / 3), text, depth + 1)}]`
    : `{"__proto__":${scalar},"nested":${nested(Math.trunc(seed / 3), text, depth + 1)}}`
}
// Enumerate editable nested containers using the independent parser, never implementation helpers.
function containers(
  node: Node,
  path: (string | number)[] = [],
): { node: Node; path: (string | number)[] }[] {
  const out: { node: Node; path: (string | number)[] }[] = []
  if (node.type === 'object' || node.type === 'array') {
    if (path.length) out.push({ node, path })
    for (const [index, child] of (node.children ?? []).entries()) {
      const key = node.type === 'object' ? (child.children![0]!.value as string) : index
      out.push(...containers(node.type === 'object' ? child.children![1]! : child, [...path, key]))
    }
  }
  return out
}
for (const operation of ['replace', 'insert', 'delete'] as const) {
  test(
    `Hegel nested ${operation} preserves independent sibling AST spans and original source`,
    () =>
      hegel.test(
        (tc) => {
          const seed = tc.draw(gs.integers()),
            text = tc.draw(gs.text({ codec: 'utf-8' }))
          const source = `{ "root": {${JSON.stringify('key:' + text)}:${nested(seed, text)},"unchanged":${nested(~seed, text)}}, "outside":${nested(Math.trunc(seed / 3), text)} }`
          const ast = parseTree(source)!
          const candidates = containers(ast)
          const selected = candidates[Math.abs(tc.draw(gs.integers()) % candidates.length)]!
          const children = selected.node.children ?? []
          const index = Math.abs(tc.draw(gs.integers()) % children.length)
          const keys = children.map((child, i) =>
            selected.node.type === 'object' ? (child.children![0]!.value as string) : i,
          )
          let key: string | number = keys[index]!
          if (operation === 'insert') {
            if (selected.node.type === 'array') key = children.length
            else {
              key = 'insert:' + tc.draw(gs.text({ codec: 'utf-8' }))
              while (keys.includes(key)) key += '_'
            }
          }
          const replacement =
            operation === 'delete'
              ? undefined
              : nested(tc.draw(gs.integers()), tc.draw(gs.text({ codec: 'utf-8' })))
          const original = parseCronDocument(source)
          const edited = patchCronValue(original, [...selected.path, key], replacement)
          const parsed = parseTree(edited.source)!
          // Every sibling at the edit level retains its exact original source span.
          for (const [siblingIndex, siblingKey] of keys.entries()) {
            if (operation !== 'insert' && siblingKey === key) continue
            const newKey =
              operation === 'delete' && typeof siblingKey === 'number' && siblingIndex > index
                ? siblingKey - 1
                : siblingKey
            expect(slice(edited.source, [...selected.path, newKey])).toBe(
              slice(source, [...selected.path, siblingKey]),
            )
          }
          // Ancestor siblings, including complete opaque subtrees, are untouched too.
          for (let depth = 0; depth < selected.path.length; depth++) {
            const prefix = selected.path.slice(0, depth)
            const ancestor = findNodeAtLocation(ast, prefix)!
            for (const [i, child] of (ancestor.children ?? []).entries()) {
              const sibling = ancestor.type === 'object' ? (child.children![0]!.value as string) : i
              if (sibling !== selected.path[depth])
                expect(slice(edited.source, [...prefix, sibling])).toBe(
                  slice(source, [...prefix, sibling]),
                )
            }
          }
          expect(findNodeAtLocation(parsed, selected.path)?.children?.length).toBe(
            children.length + (operation === 'insert' ? 1 : operation === 'delete' ? -1 : 0),
          )
          if (replacement !== undefined)
            expect(slice(edited.source, [...selected.path, key])).toBe(replacement)
          expect(original.source).toBe(source)
        },
        { testCases: 2000 },
      ),
    60000,
  )
}
test('Hegel opaque numeric literals survive sibling insertion exactly', () =>
  hegel.test(
    (tc) => {
      const coefficient = tc.draw(gs.bigIntegers()).toString(),
        exponent = tc.draw(gs.integers())
      const literal = `${coefficient}e${exponent}`
      const result = patchCronValue(
        parseCronDocument(`{"options":{"n":${literal}}}`),
        ['name'],
        '"new"',
      )
      expect(slice(result.source, ['options', 'n'])).toBe(literal)
    },
    { testCases: 2000 },
  ))
test('array replacement, append and deletion preserve raw sibling numbers', () => {
  const original = parseCronDocument('[9007199254740993,{"x":1e40},-0]')
  const replaced = patchCronValue(original, [1, 'x'], '2e40')
  expect(replaced.source).toBe('[9007199254740993,{"x":2e40},-0]')
  const appended = patchCronValue(replaced, [3], '{"__proto__":9007199254740993}')
  expect(rawCronValue(appended, [3, '__proto__'])).toBe('9007199254740993')
  const removed = patchCronValue(appended, [1], undefined)
  expect(rawCronValue(removed, [0])).toBe('9007199254740993')
  expect(rawCronValue(removed, [1])).toBe('-0')
})
test('refuses sparse array insertion instead of silently using another index', () => {
  expect(() => patchCronValue(parseCronDocument('[1]'), [3], '2')).toThrow(/index/i)
})
test('Hegel malformed and duplicate generated text must be rejected', () =>
  hegel.test(
    (tc) => {
      const key = JSON.stringify(tc.draw(gs.text({ codec: 'utf-8' })))
      expect(() => parseCronDocument(`{${key}:1,${key}:2}`)).toThrow(/duplicate/i)
    },
    { testCases: 2000 },
  ))

test('root edits keep raw numeric text and root deletion is refused', () => {
  expect(patchCronValue(parseCronDocument('{}'), [], '9007199254740993').source).toBe(
    '9007199254740993',
  )
  expect(() => patchCronValue(parseCronDocument('{}'), [], undefined)).toThrow()
})
test('opaque names resembling native defaults remain significant', () => {
  expect(
    sameCronDefinition(
      'job',
      parseCronDocument('{"options":{"timeout_seconds":0}}'),
      parseCronDocument('{"options":{}}'),
    ),
  ).toBe(false)
  expect(
    sameCronDefinition(
      'workflow',
      parseCronDocument('{"options":{"about":""}}'),
      parseCronDocument('{"options":{}}'),
    ),
  ).toBe(false)
})

test('native null secret refs compare equal to their omitted readback', () => {
  expect(
    sameCronDefinition('job', parseCronDocument('{"secret_refs":null}'), parseCronDocument('{}')),
  ).toBe(true)
})
test('native null workflow dependencies compare equal to their omitted readback', () => {
  expect(
    sameCronDefinition(
      'workflow',
      parseCronDocument('{"steps":[{"key":"a","after":null}]}'),
      parseCronDocument('{"steps":[{"key":"a"}]}'),
    ),
  ).toBe(true)
})
test('native optional scalar nulls compare equal to decoded zero omissions', () => {
  expect(
    sameCronDefinition(
      'job',
      parseCronDocument(
        '{"checkout_key":null,"timeout_seconds":null,"trigger":{"timezone":null,"lead_seconds":null},"action":{"prompt":null},"issue":{"body":null}}',
      ),
      parseCronDocument('{"trigger":{},"action":{},"issue":{}}'),
    ),
  ).toBe(true)
  expect(
    sameCronDefinition(
      'workflow',
      parseCronDocument(
        '{"about":null,"input":null,"steps":[{"command":null,"prompt":null,"retries":null}]}',
      ),
      parseCronDocument('{"steps":[{}]}'),
    ),
  ).toBe(true)
})
test('native null optional offsets omit while explicit zero offsets remain significant', () => {
  expect(
    sameCronDefinition(
      'job',
      parseCronDocument(
        '{"issue":{"scheduled_offset_seconds":null,"deadline_offset_seconds":null}}',
      ),
      parseCronDocument('{"issue":{}}'),
    ),
  ).toBe(true)
  expect(
    sameCronDefinition(
      'job',
      parseCronDocument('{"issue":{"deadline_offset_seconds":0}}'),
      parseCronDocument('{"issue":{}}'),
    ),
  ).toBe(false)
})
test('opaque null native-looking fields remain distinct from missing', () => {
  for (const kind of ['job', 'workflow'] as const) {
    for (const key of [
      'secret_refs',
      'after',
      'checkout_key',
      'timeout_seconds',
      'scheduled_offset_seconds',
    ]) {
      expect(
        sameCronDefinition(
          kind,
          parseCronDocument(`{"options":{${JSON.stringify(key)}:null}}`),
          parseCronDocument('{"options":{}}'),
        ),
      ).toBe(false)
    }
    expect(
      sameCronDefinition(kind, parseCronDocument('{"options":null}'), parseCronDocument('{}')),
    ).toBe(false)
  }
})

test('retains shrunk nested array final-element deletion regression', () => {
  // Hegel: seed=0, text='', containerChoice=1, childChoice=1.
  const source = '{"root":{"key:":"","unchanged":[-1,""]},"outside":""}'
  const edited = patchCronValue(parseCronDocument(source), ['root', 'unchanged', 1], undefined)
  expect(edited.source).toBe('{"root":{"key:":"","unchanged":[-1]},"outside":""}')
})

for (const [index, expected] of [
  [0, '[1e40,-0]'],
  [1, '[9007199254740993,-0]'],
  [2, '[9007199254740993,1e40]'],
] as const) {
  test(`array deletion at index ${index} preserves exact remaining numeric siblings`, () => {
    const original = parseCronDocument('[9007199254740993,1e40,-0]')
    expect(patchCronValue(original, [index], undefined).source).toBe(expected)
    expect(original.source).toBe('[9007199254740993,1e40,-0]')
  })
}
for (const [key, expected] of [
  ['__proto__', '{"x":1e40,"constructor":-0}'],
  ['x', '{"__proto__":9007199254740993,"constructor":-0}'],
  ['constructor', '{"__proto__":9007199254740993,"x":1e40}'],
] as const) {
  test(`object deletion of ${key} preserves exact remaining numeric siblings`, () => {
    const original = parseCronDocument('{"__proto__":9007199254740993,"x":1e40,"constructor":-0}')
    expect(patchCronValue(original, [key], undefined).source).toBe(expected)
    expect(original.source).toBe('{"__proto__":9007199254740993,"x":1e40,"constructor":-0}')
  })
}
