import { findNodeAtLocation, parseTree, type Node } from 'jsonc-parser'
import { createULID } from '../ulid'
import type { CronDefinitionRecord } from './client'
import type { CronDefinitionDraft } from './jobDraft'
import { parseCronDocument, patchCronValue, rawCronValue, type RawCronDocument } from './document'

export interface WorkflowStepDraft {
  editorID: string
  key: string
  kind: 'command' | 'prompt'
  command: string
  prompt: string
  after: string[]
  retries: string
  options: RawCronDocument
  original?: RawCronDocument
  dirty: Set<string>
}
export interface WorkflowDraft extends CronDefinitionDraft {
  kind: 'workflow'
  about: string
  input: string
  options: RawCronDocument
  steps: WorkflowStepDraft[]
}
export interface WorkflowProblem {
  editorID?: string
  field: string
  code: 'duplicate' | 'missing' | 'later' | 'cycle' | 'invalid'
  message: string
}
type WorkflowField = 'name' | 'about' | 'input' | 'options'
type StepField = 'key' | 'kind' | 'command' | 'prompt' | 'after' | 'retries' | 'options'
const workflowKeys = new Set(['version', 'about', 'input', 'options', 'steps'])
const stepKeys = new Set(['key', 'kind', 'command', 'prompt', 'after', 'retries', 'options'])
const forbiddenOptions = new Set([
  'token',
  'api_token',
  'api_key',
  'password',
  'secret',
  'credentials',
  'authorization',
  'cwd',
  'path',
  'checkout_path',
  'local_path',
])
const slice = (doc: RawCronDocument, node: Node) =>
  doc.source.slice(node.offset, node.offset + node.length)
function text(doc: RawCronDocument, field: string, fallback = ''): string {
  const raw = rawCronValue(doc, [field])
  if (raw === undefined || raw === 'null') return fallback
  const node = parseTree(raw)!
  if (node.type !== 'string') throw new Error(`${field}: expected text`)
  return node.value as string
}
function stepsFromDocument(doc: RawCronDocument): WorkflowStepDraft[] {
  const array = findNodeAtLocation(parseTree(doc.source)!, ['steps'])
  if (array?.type !== 'array') throw new Error('steps: expected an ordered array')
  return (array.children ?? []).map((node) => {
    if (node.type !== 'object') throw new Error('step: expected an object')
    const original = parseCronDocument(slice(doc, node))
    const after = rawCronValue(original, ['after']),
      dependencies = after ? parseTree(after)! : undefined
    if (
      dependencies &&
      dependencies.type !== 'null' &&
      (dependencies.type !== 'array' || dependencies.children?.some((n) => n.type !== 'string'))
    )
      throw new Error('after: expected a list of step keys')
    return {
      editorID: createULID(),
      key: text(original, 'key'),
      kind: text(original, 'kind') as WorkflowStepDraft['kind'],
      command: text(original, 'command'),
      prompt: text(original, 'prompt'),
      after:
        dependencies?.type === 'array'
          ? (dependencies.children ?? []).map((n) => n.value as string)
          : [],
      retries:
        rawCronValue(original, ['retries']) === 'null'
          ? '0'
          : (rawCronValue(original, ['retries']) ?? '0'),
      options: { source: rawCronValue(original, ['options']) ?? '{}' },
      original,
      dirty: new Set(),
    }
  })
}
export function buildWorkflowDraft(
  record: CronDefinitionRecord | undefined,
  projectID: number,
): WorkflowDraft {
  const original = record?.document ?? parseCronDocument('{"version":1,"steps":[]}')
  return {
    kind: 'workflow',
    projectID,
    uid: record?.uid ?? createULID(),
    name: record?.name ?? '',
    definitionEventUID: record?.definitionEventUID ?? '',
    original,
    values: { name: record?.name ?? '' },
    dirty: new Set(),
    about: text(original, 'about'),
    input: text(original, 'input'),
    options: { source: rawCronValue(original, ['options']) ?? '{}' },
    steps: stepsFromDocument(original),
  }
}
export function setWorkflowField(
  draft: WorkflowDraft,
  field: WorkflowField,
  value: string,
): WorkflowDraft {
  const next = { ...draft, dirty: new Set([...draft.dirty, field]) }
  if (field === 'name') return { ...next, name: value, values: { ...next.values, name: value } }
  return field === 'options' ? { ...next, options: { source: value } } : { ...next, [field]: value }
}
function stepIndex(draft: WorkflowDraft, editorID: string): number {
  const index = draft.steps.findIndex((s) => s.editorID === editorID)
  if (index === -1) throw new Error('Step no longer exists')
  return index
}
function portableKey(key: string): boolean {
  return (
    !!key &&
    key !== '.' &&
    key !== '..' &&
    new TextEncoder().encode(key).length <= 256 &&
    /^[\p{L}\p{Nd}_.-]+$/u.test(key)
  )
}
function referenceDirty(step: WorkflowStepDraft): Set<string> {
  return new Set([
    ...step.dirty,
    ...(!step.dirty.has('after') ? ['after:references'] : []),
    'after',
  ])
}
export function renameWorkflowStep(
  draft: WorkflowDraft,
  editorID: string,
  newKey: string,
): WorkflowDraft {
  const index = stepIndex(draft, editorID),
    oldKey = draft.steps[index]!.key
  if (!portableKey(newKey))
    throw new Error('Key must be a portable name of at most 256 UTF-8 bytes.')
  if (draft.steps.some((s, i) => i !== index && s.key === newKey))
    throw new Error('Step keys must be unique.')
  if (oldKey === newKey) return draft
  return {
    ...draft,
    steps: draft.steps.map((s, i) => {
      const renamed = i === index,
        references = s.after.includes(oldKey)
      if (!renamed && !references) return s
      return {
        ...s,
        key: renamed ? newKey : s.key,
        after: references ? s.after.map((key) => (key === oldKey ? newKey : key)) : s.after,
        dirty: new Set([
          ...(references ? referenceDirty(s) : s.dirty),
          ...(renamed ? ['key'] : []),
        ]),
      }
    }),
  }
}
export function setWorkflowStepField(
  draft: WorkflowDraft,
  editorID: string,
  field: StepField,
  value: string | string[],
): WorkflowDraft {
  if (field === 'key') return renameWorkflowStep(draft, editorID, String(value))
  const index = stepIndex(draft, editorID),
    step = draft.steps[index]!
  if (field === 'kind' && value !== 'command' && value !== 'prompt')
    throw new Error('Unknown step kind')
  const changed = { ...step, dirty: new Set([...step.dirty, field]) }
  if (field === 'options') changed.options = { source: String(value) }
  else if (field === 'after') {
    changed.after = Array.isArray(value)
      ? [...value]
      : value
          .split(',')
          .map((s) => s.trim())
          .filter(Boolean)
    changed.dirty.delete('after:references')
  } else if (field === 'kind') {
    changed.kind = value as WorkflowStepDraft['kind']
    const inactive = value === 'command' ? 'prompt' : 'command'
    changed[inactive] = ''
    changed.dirty.add(inactive)
  } else changed[field] = String(value)
  return { ...draft, steps: draft.steps.map((s, i) => (i === index ? changed : s)) }
}
export function addWorkflowStep(
  draft: WorkflowDraft,
  kind: WorkflowStepDraft['kind'],
): WorkflowDraft {
  let ordinal = draft.steps.length + 1
  while (draft.steps.some((s) => s.key === `step${ordinal}`)) ordinal++
  const step: WorkflowStepDraft = {
    editorID: createULID(),
    key: `step${ordinal}`,
    kind,
    command: '',
    prompt: '',
    after: [],
    retries: '0',
    options: { source: '{}' },
    dirty: new Set(['key', 'kind', kind, 'retries', 'options']),
  }
  return { ...draft, dirty: new Set([...draft.dirty, 'steps']), steps: [...draft.steps, step] }
}
export function workflowDependents(draft: WorkflowDraft, editorID: string): string[] {
  const key = draft.steps[stepIndex(draft, editorID)]!.key
  return draft.steps.filter((s) => s.after.includes(key)).map((s) => s.key)
}
export function deleteWorkflowStep(
  draft: WorkflowDraft,
  editorID: string,
  confirmRemoveReferences: boolean,
): WorkflowDraft {
  const key = draft.steps[stepIndex(draft, editorID)]!.key
  if (workflowDependents(draft, editorID).length && !confirmRemoveReferences)
    throw new Error('Confirm removal of direct dependent references before deleting this step.')
  return {
    ...draft,
    dirty: new Set([...draft.dirty, 'steps']),
    steps: draft.steps
      .filter((s) => s.editorID !== editorID)
      .map((s) =>
        s.after.includes(key)
          ? { ...s, after: s.after.filter((ref) => ref !== key), dirty: referenceDirty(s) }
          : s,
      ),
  }
}
function validateOptions(doc: RawCronDocument): string | undefined {
  try {
    const parsed = parseCronDocument(doc.source),
      root = parseTree(parsed.source)!
    if (root.type !== 'object') return 'Options must be a JSON object.'
    const pending = [root]
    while (pending.length) {
      const node = pending.pop()!
      if (
        node.type === 'property' &&
        forbiddenOptions.has(String(node.children![0]!.value).toLowerCase())
      )
        return 'Options cannot contain local credentials or paths.'
      pending.push(...(node.children ?? []))
    }
  } catch (error) {
    return error instanceof Error ? error.message : 'Options must contain strict JSON.'
  }
  return undefined
}
function assertPreservable(doc: RawCronDocument, keys: Set<string>, label: string) {
  const root = parseTree(doc.source)!
  if (root.type !== 'object') throw new Error(`Cannot preserve unsupported ${label} value`)
  for (const property of root.children ?? []) {
    const key = property.children![0]!.value as string
    if (!keys.has(key)) throw new Error(`Cannot preserve unsupported ${label} field ${key}`)
  }
}
export function validateWorkflowDraft(draft: WorkflowDraft): WorkflowProblem[] {
  const problems: WorkflowProblem[] = []
  const add = (field: string, code: WorkflowProblem['code'], message: string, editorID?: string) =>
    problems.push({ field, code, message, ...(editorID ? { editorID } : {}) })
  if (!draft.values.name?.trim()) add('name', 'missing', 'Enter a workflow name.')
  if (!draft.steps.length) add('steps', 'missing', 'A workflow requires at least one step.')
  try {
    assertPreservable(draft.original, workflowKeys, 'workflow')
    if (rawCronValue(draft.original, ['version']) !== '1')
      add('version', 'invalid', 'Unsupported workflow version.')
  } catch (error) {
    add('definition', 'invalid', (error as Error).message)
  }
  const optionsError = validateOptions(draft.options)
  if (optionsError) add('options', 'invalid', optionsError)
  const byKey = new Map<string, number[]>()
  draft.steps.forEach((step, i) => byKey.set(step.key, [...(byKey.get(step.key) ?? []), i]))
  draft.steps.forEach((step, i) => {
    const id = step.editorID
    if (step.original)
      try {
        assertPreservable(step.original, stepKeys, 'step')
      } catch (error) {
        add('definition', 'invalid', (error as Error).message, id)
      }
    if (!portableKey(step.key))
      add('key', 'invalid', 'Key must be a portable name of at most 256 UTF-8 bytes.', id)
    if (byKey.get(step.key)!.length > 1)
      add('key', 'duplicate', `Duplicate step key: ${step.key}.`, id)
    if (!/^(?:0|[1-9]\d*)$/.test(step.retries) || BigInt(step.retries) > 100n)
      add('retries', 'invalid', 'Retries must be an integer from 0 through 100.', id)
    if (step.kind !== 'command' && step.kind !== 'prompt')
      add('kind', 'invalid', 'Select command or prompt.', id)
    else {
      if (!step[step.kind].trim()) add(step.kind, 'missing', `Enter a ${step.kind}.`, id)
      const inactive = step.kind === 'command' ? 'prompt' : 'command'
      if (step[inactive])
        add(inactive, 'invalid', `${step.kind} steps cannot carry ${inactive}.`, id)
    }
    const error = validateOptions(step.options)
    if (error) add('options', 'invalid', error, id)
    const seen = new Set<string>()
    for (const key of step.after) {
      if (seen.has(key)) add('after', 'duplicate', `Repeated dependency: ${key}.`, id)
      seen.add(key)
      const indexes = byKey.get(key)
      if (!indexes) add('after', 'missing', `Missing dependency: ${key}.`, id)
      else if (key === step.key) add('after', 'invalid', 'A step cannot depend on itself.', id)
      else if (indexes.some((index) => index >= i))
        add('after', 'later', `Dependency ${key} must name an earlier step.`, id)
    }
  })
  // Iterative DFS groups strongly connected steps without recursion per workflow step.
  const visited = new Map<number, { order: number; low: number; active: boolean }>()
  const members: number[] = []
  const cyclic = new Set<number>()
  for (let start = 0; start < draft.steps.length; start++) {
    if (visited.has(start)) continue
    const stack: { index: number; next: number; targets: number[] }[] = []
    const enter = (index: number) => {
      const order = visited.size
      visited.set(index, { order, low: order, active: true })
      members.push(index)
      stack.push({
        index,
        next: 0,
        targets: draft.steps[index]!.after.flatMap((key) => byKey.get(key) ?? []),
      })
    }
    enter(start)
    while (stack.length) {
      const frame = stack.at(-1)!
      const mark = visited.get(frame.index)!
      if (frame.next === frame.targets.length) {
        stack.pop()
        const parent = stack.at(-1)
        if (parent) {
          const parentMark = visited.get(parent.index)!
          parentMark.low = Math.min(parentMark.low, mark.low)
        }
        if (mark.low === mark.order) {
          const component: number[] = []
          let member: number
          do {
            member = members.pop()!
            visited.get(member)!.active = false
            component.push(member)
          } while (member !== frame.index)
          if (component.length > 1 || frame.targets.includes(frame.index))
            for (const index of component) cyclic.add(index)
        }
        continue
      }
      const target = frame.targets[frame.next++]!
      const targetMark = visited.get(target)
      if (!targetMark) enter(target)
      else if (targetMark.active) mark.low = Math.min(mark.low, targetMark.order)
    }
  }
  for (const index of cyclic)
    add('after', 'cycle', 'Dependency cycle includes this step.', draft.steps[index]!.editorID)
  return problems
}
export function serializeWorkflowDraft(draft: WorkflowDraft): RawCronDocument {
  const problems = validateWorkflowDraft(draft)
  if (problems.length) throw new Error(problems.map((p) => `${p.field}: ${p.message}`).join(' '))
  let doc = draft.original
  for (const field of ['about', 'input', 'options'] as const)
    if (draft.dirty.has(field))
      doc = patchCronValue(
        doc,
        [field],
        field === 'options'
          ? parseCronDocument(draft.options.source).source
          : JSON.stringify(draft[field]),
      )
  if (draft.dirty.has('steps') || draft.steps.some((s) => s.dirty.size)) {
    const steps = draft.steps.map((step) => {
      let raw = step.original ?? parseCronDocument('{}')
      for (const field of step.dirty) {
        if (field === 'after:references') continue
        if (!stepKeys.has(field)) throw new Error(`Unsupported step field: ${field}`)
        const value =
          field === 'options'
            ? parseCronDocument(step.options.source).source
            : field === 'retries'
              ? step.retries
              : (field === 'command' || field === 'prompt') && !step[field]
                ? undefined
                : JSON.stringify(step[field as Exclude<StepField, 'options'>])
        raw = patchCronValue(raw, [field], value)
      }
      return raw.source
    })
    doc = patchCronValue(doc, ['steps'], `[${steps.join(',')}]`)
  }
  if (new TextEncoder().encode(doc.source).length > 256 * 1024)
    throw new Error('Workflow definition exceeds the 256 KiB size limit.')
  return doc
}

/** Match persisted steps by their original keys only. Never infer a rename from array position. */
export function rebaseWorkflowDraft(
  intended: WorkflowDraft,
  record: CronDefinitionRecord,
): WorkflowDraft {
  if (record.deletedAt) throw new Error('The newer workflow was deleted.')
  let rebased = buildWorkflowDraft(record, intended.projectID)
  const base = stepsFromDocument(intended.original),
    originals = new Map(base.map((s) => [s.key, s]))
  for (const step of base)
    if (rebased.steps.filter((s) => s.key === step.key).length !== 1)
      throw new Error(
        `Ambiguous step correspondence: ${step.key} was renamed or deleted. Reopen the workflow and compare the retained draft.`,
      )
  if (new Set(rebased.steps.map((s) => s.key)).size !== rebased.steps.length)
    throw new Error('Ambiguous duplicate step keys in newer workflow.')
  const localByOriginalKey = new Map(
    intended.steps.filter((s) => s.original).map((s) => [text(s.original!, 'key'), s]),
  )
  // Keep editor identities across the rebase and preserve newer steps in their own order.
  rebased = {
    ...rebased,
    steps: rebased.steps.map((s) => ({
      ...s,
      editorID: localByOriginalKey.get(s.key)?.editorID ?? s.editorID,
    })),
  }
  for (const original of base) {
    const local = localByOriginalKey.get(original.key),
      remote = rebased.steps.find((s) => s.key === original.key)!
    if (!local) {
      const knownDependents = new Set(
        base.filter((s) => s.after.includes(original.key)).map((s) => s.key),
      )
      if (rebased.steps.some((s) => s.after.includes(original.key) && !knownDependents.has(s.key)))
        throw new Error(
          `Newer steps depend on deleted step ${original.key}. Review and confirm those references before rebasing.`,
        )
      rebased = deleteWorkflowStep(rebased, remote.editorID, true)
    }
  }
  // Resolve renames simultaneously after deletions, so deliberately reused keys
  // cannot collide temporarily or rewrite another step's already-renamed references.
  const renames = new Map(
    base.flatMap((s) => {
      const local = localByOriginalKey.get(s.key)
      return local && local.key !== s.key ? [[s.key, local.key] as const] : []
    }),
  )
  rebased = {
    ...rebased,
    steps: rebased.steps.map((s) => {
      const key = renames.get(s.key) ?? s.key,
        references = s.after.some((ref) => renames.has(ref))
      return key === s.key && !references
        ? s
        : {
            ...s,
            key,
            after: references ? s.after.map((ref) => renames.get(ref) ?? ref) : s.after,
            dirty: new Set([
              ...(references ? referenceDirty(s) : s.dirty),
              ...(key !== s.key ? ['key'] : []),
            ]),
          }
    }),
  }
  if (new Set(rebased.steps.map((s) => s.key)).size !== rebased.steps.length)
    throw new Error(
      'A renamed step conflicts with a newer step key. Compare the retained draft before rebasing.',
    )
  for (const local of intended.steps) {
    if (!local.original) {
      if (rebased.steps.some((s) => s.key === local.key))
        throw new Error(`Ambiguous new step key: ${local.key}.`)
      rebased = {
        ...rebased,
        steps: [...rebased.steps, local],
        dirty: new Set([...rebased.dirty, 'steps']),
      }
      continue
    }
    if (!originals.has(text(local.original, 'key')))
      throw new Error('Ambiguous original step identity.')
    for (const field of [...local.dirty].sort((a, b) =>
      a === 'kind' ? -1 : b === 'kind' ? 1 : 0,
    )) {
      if (
        field === 'key' ||
        field === 'after:references' ||
        (field === 'after' && local.dirty.has('after:references'))
      )
        continue
      const value =
        field === 'options' ? local.options.source : local[field as Exclude<StepField, 'options'>]
      rebased = setWorkflowStepField(rebased, local.editorID, field as StepField, value)
    }
  }
  for (const field of intended.dirty)
    if (field !== 'steps')
      rebased = setWorkflowField(
        rebased,
        field as WorkflowField,
        field === 'name'
          ? (intended.values.name ?? '')
          : field === 'options'
            ? intended.options.source
            : intended[field as 'about' | 'input'],
      )
  return rebased
}
