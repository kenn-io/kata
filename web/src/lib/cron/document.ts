import {
  applyEdits,
  findNodeAtLocation,
  modify,
  parseTree,
  type Node,
  type ParseError,
} from 'jsonc-parser'

export interface RawCronDocument {
  source: string
}
type Path = readonly (string | number)[]

function tree(source: string): Node {
  const errors: ParseError[] = []
  const root = parseTree(source, errors, {
    disallowComments: true,
    allowTrailingComma: false,
    allowEmptyContent: false,
  })
  if (!root || errors.length)
    throw new Error('Cron definition must contain exactly one strict JSON value')
  function unique(node: Node) {
    if (node.type === 'object') {
      const keys = new Set<string>()
      for (const property of node.children ?? []) {
        const key = property.children![0]!.value as string
        if (keys.has(key)) throw new Error('Cron definition contains a duplicate JSON key')
        keys.add(key)
      }
    }
    for (const child of node.children ?? []) unique(child)
  }
  unique(root)
  return root
}
export function parseCronDocument(source: string): RawCronDocument {
  tree(source)
  return { source }
}
export function rawCronValue(doc: RawCronDocument, path: Path): string | undefined {
  const node = findNodeAtLocation(tree(doc.source), [...path])
  return node && doc.source.slice(node.offset, node.offset + node.length)
}
export function patchCronValue(
  doc: RawCronDocument,
  path: Path,
  replacement: string | undefined,
): RawCronDocument {
  const root = tree(doc.source)
  if (replacement !== undefined) tree(replacement)
  for (const segment of path)
    if (typeof segment === 'number' && (!Number.isSafeInteger(segment) || segment < 0))
      throw new Error('Invalid cron array index')
  let parent: Node | undefined = root
  for (const segment of path) {
    if (typeof segment === 'number') {
      if ((parent && parent.type !== 'array') || segment > (parent?.children?.length ?? 0))
        throw new Error('Invalid cron array index')
    } else if (parent && parent.type !== 'object')
      throw new Error('Cannot edit a property of a non-object cron value')
    parent = parent && findNodeAtLocation(parent, [segment])
  }
  const existing = findNodeAtLocation(root, [...path])
  if (existing && replacement !== undefined) {
    return parseCronDocument(
      doc.source.slice(0, existing.offset) +
        replacement +
        doc.source.slice(existing.offset + existing.length),
    )
  }
  if (replacement === undefined && !existing) return { source: doc.source }
  if (!path.length && replacement === undefined)
    throw new Error('Cannot delete the cron definition root')
  // Insert a harmless placeholder; replace its AST span with raw text after the edit.
  // No original value is converted to a JS object or Number.
  const edits = modify(doc.source, [...path], replacement === undefined ? undefined : null, {})
  if (replacement === undefined && existing?.parent?.type === 'array') {
    const siblings = existing.parent.children ?? []
    if (siblings.length > 1 && siblings.at(-1) === existing) {
      // jsonc-parser 3.3.1 ends this deletion one character before the final value.
      // Keep its comma/start boundary, but use the parsed value's complete end.
      for (const edit of edits) edit.length = existing.offset + existing.length - edit.offset
    }
  }
  const edited = parseCronDocument(applyEdits(doc.source, edits))
  return replacement === undefined ? edited : patchCronValue(edited, path, replacement)
}

// Decimal coefficient/exponent canonicalization never expands an exponent or rounds a number.
function decimal(source: string): string {
  const match = /^(-?)(\d+)(?:\.(\d+))?(?:[eE]([+-]?\d+))?$/.exec(source)!
  let digits = (match[2]! + (match[3] ?? '')).replace(/^0+/, '')
  if (!digits) return '0'
  let exponent = BigInt(match[4] ?? '0') - BigInt((match[3] ?? '').length)
  const zeros = /0+$/.exec(digits)?.[0].length ?? 0
  digits = digits.slice(0, digits.length - zeros)
  exponent += BigInt(zeros)
  return `${match[1]}${digits}e${exponent}`
}

// Native omitempty/omitzero fields only. Opaque options never inherit these rules.
const optionalStrings = new Set([
  'job.checkout_key',
  'trigger.timezone',
  'trigger.cron',
  'trigger.at',
  'trigger.issue_uid',
  'action.prompt',
  'action.workflow_uid',
  'action.recipient',
  'action.message',
  'issue.parent_uid',
  'issue.uid',
  'issue.title',
  'issue.body',
  'workflow.about',
  'workflow.input',
  'step.command',
  'step.prompt',
])
const optionalNulls = new Set([
  'job.issue',
  'job.secret_refs',
  'step.after',
  'issue.scheduled_offset_seconds',
  'issue.deadline_offset_seconds',
])
const optionalNumbers = new Set([
  'job.grace_seconds',
  'job.timeout_seconds',
  'trigger.interval_seconds',
  'trigger.lead_seconds',
  'step.retries',
])
const optionalObjects = new Set([
  'job.secret_refs',
  'job.options',
  'workflow.options',
  'step.options',
])
function childContext(context: string, key: string): string {
  if (context === 'job' && ['trigger', 'action', 'issue'].includes(key)) return key
  if (context === 'workflow' && key === 'steps') return 'steps'
  return ''
}
function canonical(doc: RawCronDocument, node: Node, context: string): unknown {
  if (node.type === 'number')
    return ['number', decimal(doc.source.slice(node.offset, node.offset + node.length))]
  if (node.type === 'array')
    return [
      'array',
      (node.children ?? []).map((child) =>
        canonical(doc, child, context === 'steps' ? 'step' : ''),
      ),
    ]
  if (node.type !== 'object') return [node.type, node.value]
  const entries: [string, unknown][] = []
  for (const property of node.children ?? []) {
    const [keyNode, value] = property.children!,
      key = keyNode!.value as string
    const field = `${context}.${key}`
    // Native optional scalars decode null to zero; optional containers/pointers decode
    // null to nil. Their native omitempty/omitzero tags remove those values on readback.
    if (
      value!.type === 'null' &&
      (optionalStrings.has(field) || optionalNumbers.has(field) || optionalNulls.has(field))
    )
      continue
    if (optionalStrings.has(field) && value!.type === 'string' && value!.value === '') continue
    if (
      optionalNumbers.has(field) &&
      value!.type === 'number' &&
      decimal(doc.source.slice(value!.offset, value!.offset + value!.length)) === '0'
    )
      continue
    if (field === 'step.after' && value!.type === 'array' && !value!.children?.length) continue
    if (optionalObjects.has(field) && value!.type === 'object' && !value!.children?.length) continue
    entries.push([key, canonical(doc, value!, childContext(context, key))])
  }
  entries.sort(([a], [b]) => (a < b ? -1 : a > b ? 1 : 0))
  return ['object', entries]
}
export function sameCronDefinition(
  kind: 'job' | 'workflow',
  a: RawCronDocument,
  b: RawCronDocument,
): boolean {
  return (
    JSON.stringify(canonical(a, tree(a.source), kind)) ===
    JSON.stringify(canonical(b, tree(b.source), kind))
  )
}
