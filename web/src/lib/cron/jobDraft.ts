import { createULID } from '../ulid'
import type { CronDefinitionRecord } from './client'
import { parseCronDocument, patchCronValue, rawCronValue, type RawCronDocument } from './document'
import { parseTree, type Node } from 'jsonc-parser'
import { jobFields } from './jobFields'

export interface CronDefinitionDraft {
  kind: 'job' | 'workflow'
  projectID: number
  uid: string
  name: string
  original: RawCronDocument
  definitionEventUID: string
  values: Record<string, string>
  dirty: Set<string>
}
export const cronJobFieldPaths: Readonly<Record<string, readonly string[]>> = Object.freeze({
  description: ['options', 'description'],
  tags: ['options', 'tags'],
  input: ['options', 'input'],
  enabled: ['enabled'],
  trigger: ['trigger', 'kind'],
  timezone: ['trigger', 'timezone'],
  prompt: ['action', 'prompt'],
  workflow: ['action', 'workflow_uid'],
  recipient: ['action', 'recipient'],
  message: ['action', 'message'],
  issue_kind: ['issue', 'kind'],
  issue: ['issue', 'uid'],
  parent: ['issue', 'parent_uid'],
  title: ['issue', 'title'],
  body: ['issue', 'body'],
  deadline: ['issue', 'deadline_offset_seconds'],
  scheduled_offset: ['issue', 'scheduled_offset_seconds'],
  overlap: ['overlap'],
  catchup: ['catchup'],
  grace: ['grace_seconds'],
  timeout: ['timeout_seconds'],
  checkout: ['checkout_key'],
  profile: ['options', 'agent'],
  run_parent: ['options', 'run_parent'],
  secret_refs: ['secret_refs'],
  trigger_issue: ['trigger', 'issue_uid'],
  lead: ['trigger', 'lead_seconds'],
})
const schedules: Readonly<Record<string, readonly string[]>> = Object.freeze({
  cron: ['trigger', 'cron'],
  interval: ['trigger', 'interval_seconds'],
  once: ['trigger', 'at'],
})
const secondsFields = new Set(['deadline', 'scheduled_offset', 'grace', 'timeout', 'lead'])
const initial =
  '{"version":1,"kind":"job","enabled":true,"trigger":{"kind":"manual"},"action":{"kind":"execute","prompt":""},"issue":{"kind":"per-run","title":""},"overlap":"forbid","catchup":"all"}'
function ownPath(paths: Readonly<Record<string, readonly string[]>>, key: string) {
  return Object.hasOwn(paths, key) ? paths[key] : undefined
}
function fieldText(doc: RawCronDocument, path: readonly string[]): string | undefined {
  const raw = rawCronValue(doc, path)
  if (raw === undefined) return undefined
  const node = parseTree(raw)!
  if (node.type === 'null') return undefined
  return node.type === 'string' ? (node.value as string) : raw
}
export function buildCronJobDraft(
  record: CronDefinitionRecord | undefined,
  projectID: number,
): CronDefinitionDraft {
  const original = record?.document ?? parseCronDocument(initial)
  const values: Record<string, string> = Object.create(null) as Record<string, string>
  values.name = record?.name ?? ''
  for (const [key, path] of Object.entries(cronJobFieldPaths)) {
    const text = fieldText(original, path)
    if (text !== undefined) values[key] = text
  }
  const schedule = ownPath(schedules, values.trigger ?? '')
  if (schedule) values.schedule = fieldText(original, schedule) ?? ''
  values.mode =
    fieldText(original, ['action', 'kind']) === 'notify'
      ? values.issue_kind === 'per-run'
        ? 'recurring'
        : 'notify'
      : values.workflow
        ? 'workflow'
        : 'prompt'
  return {
    kind: 'job',
    projectID,
    uid: record?.uid ?? createULID(),
    name: record?.name ?? '',
    original,
    definitionEventUID: record?.definitionEventUID ?? '',
    values,
    dirty: new Set(),
  }
}

const typedKeys = new Map<string, Set<string>>([
  [
    '',
    new Set([
      'version',
      'kind',
      'enabled',
      'trigger',
      'action',
      'checkout_key',
      'issue',
      'overlap',
      'catchup',
      'grace_seconds',
      'timeout_seconds',
      'secret_refs',
      'options',
    ]),
  ],
  [
    'trigger',
    new Set(['kind', 'timezone', 'cron', 'interval_seconds', 'at', 'issue_uid', 'lead_seconds']),
  ],
  ['action', new Set(['kind', 'prompt', 'workflow_uid', 'recipient', 'message'])],
  [
    'issue',
    new Set([
      'kind',
      'parent_uid',
      'uid',
      'title',
      'body',
      'scheduled_offset_seconds',
      'deadline_offset_seconds',
    ]),
  ],
])
function assertPreservable(doc: RawCronDocument) {
  const root = parseTree(doc.source)!
  function check(node: Node, context: string) {
    if (node.type !== 'object')
      throw new Error(`Cannot preserve unsupported cron ${context || 'definition'} value`)
    for (const property of node.children ?? []) {
      const [keyNode, value] = property.children!,
        key = keyNode!.value as string
      if (!typedKeys.get(context)!.has(key))
        throw new Error(
          `Cannot preserve unsupported cron field ${context ? context + '.' : ''}${key}`,
        )
      if (context === '' && typedKeys.has(key) && value!.type !== 'null') check(value!, key)
    }
  }
  check(root, '')
}
function exactSeconds(key: string, text: string): string {
  if (!/^-?\d+$/.test(text)) throw new Error(`${key}: enter exact integer seconds`)
  const value = BigInt(text)
  if (
    value < -9223372036n ||
    value > 9223372036n ||
    (key !== 'deadline' && key !== 'scheduled_offset' && value < 0)
  )
    throw new Error(`${key}: seconds cannot be represented safely`)
  return value.toString()
}
/**
 * The definition fields the daemon accepts for an action, trigger and issue
 * policy (see internal/cron/validate.go). Everything else is removed when a
 * job is saved, whatever the draft still remembers.
 */
export function supportedJobFields(values: Readonly<Record<string, string>>): Set<string> {
  const keys = new Set([
    'description',
    'tags',
    'input',
    'profile',
    'run_parent',
    'enabled',
    'trigger',
    'timezone',
    'overlap',
    'catchup',
    'grace',
  ])
  const mode = values.mode,
    policy = values.issue_kind
  if (ownPath(schedules, values.trigger ?? '')) keys.add('schedule')
  if (['issue-scheduled', 'issue-deadline'].includes(values.trigger ?? ''))
    keys.add('trigger_issue')
  if (values.trigger === 'issue-deadline') keys.add('lead')
  if (mode === 'prompt' || mode === 'workflow') {
    keys.add(mode)
    for (const key of ['timeout', 'checkout', 'secret_refs', 'issue_kind']) keys.add(key)
    if (policy === 'existing') keys.add('issue')
    if (policy === 'per-run')
      for (const key of ['title', 'body', 'deadline', 'scheduled_offset']) keys.add(key)
  } else if (mode === 'recurring') {
    for (const key of ['recipient', 'message', 'issue_kind', 'parent', 'title', 'body'])
      keys.add(key)
  } else {
    for (const key of ['recipient', 'message', 'timeout', 'secret_refs']) keys.add(key)
    if (policy !== undefined) keys.add('issue_kind')
    if (policy === 'existing') keys.add('issue')
  }
  return keys
}
function zeroValue(raw: string | undefined): boolean {
  return raw === undefined || ['null', '""', '0', '{}', '[]'].includes(raw.replace(/\s/g, ''))
}
export function serializeCronJobDraft(draft: CronDefinitionDraft): RawCronDocument {
  if (draft.kind !== 'job') throw new Error('Expected a job draft')
  // The server rejects unknown typed fields. Refuse visibly instead of stripping them.
  assertPreservable(draft.original)
  let doc = draft.original
  const values = draft.values
  const originallyRecurring =
    fieldText(draft.original, ['action', 'kind']) === 'notify' &&
    fieldText(draft.original, ['issue', 'kind']) === 'per-run'
  if (draft.definitionEventUID && originallyRecurring) {
    for (const key of ['mode', 'parent', 'overlap']) {
      if (draft.dirty.has(key)) throw new Error(`${key}: native recurring ownership is immutable`)
    }
  }
  const remove = (path: readonly string[]) => {
    doc = patchCronValue(doc, path, undefined)
  }
  const set = (path: readonly string[], value: string) => {
    doc = patchCronValue(doc, path, JSON.stringify(value))
  }
  const execute = values.mode === 'prompt' || values.mode === 'workflow'
  if (values.mode === 'recurring' && !originallyRecurring) {
    const rawOptions = rawCronValue(draft.original, ['options']),
      options = rawOptions ? parseTree(rawOptions) : undefined
    if (options && (options.type !== 'object' || !!options.children?.length))
      throw new Error('Cannot preserve adapter options in native recurring mode')
    if (
      (rawCronValue(draft.original, ['issue', 'scheduled_offset_seconds']) !== undefined &&
        rawCronValue(draft.original, ['issue', 'scheduled_offset_seconds']) !== 'null') ||
      (rawCronValue(draft.original, ['issue', 'deadline_offset_seconds']) !== undefined &&
        rawCronValue(draft.original, ['issue', 'deadline_offset_seconds']) !== 'null' &&
        !(draft.dirty.has('deadline') && values.deadline === ''))
    )
      throw new Error('Cannot preserve offsets in native recurring mode')
  }
  if (
    values.issue_kind === 'existing' &&
    rawCronValue(draft.original, ['issue', 'scheduled_offset_seconds']) !== undefined &&
    rawCronValue(draft.original, ['issue', 'scheduled_offset_seconds']) !== 'null'
  )
    throw new Error('Cannot preserve the existing scheduled offset with an existing-issue policy')
  // One rule for every field: the final action, trigger and issue policy decide
  // which fields the request may carry. An edit is written only while its field
  // is visible; an untouched field keeps its original value; anything the final
  // state does not support is removed, so dormant edits never leak.
  const supported = supportedJobFields(values)
  const visible = new Set(
    jobFields(draft, { persisted: !!draft.definitionEventUID, projectUID: '', workflows: [] })
      .filter((field) => field.kind !== 'readonly')
      .map((field) => field.key),
  )
  if (draft.dirty.has('mode')) set(['action', 'kind'], execute ? 'execute' : 'notify')
  if (values.issue_kind === undefined) remove(['issue'])
  else if (draft.dirty.has('mode') || draft.dirty.has('issue_kind'))
    set(['issue', 'kind'], values.issue_kind)
  for (const [kind, path] of Object.entries(schedules)) if (kind !== values.trigger) remove(path)
  for (const [key, fieldPath] of Object.entries(cronJobFieldPaths)) {
    if (key === 'issue_kind' || key === 'trigger') continue
    const path = key === 'schedule' ? undefined : fieldPath
    if (!path) continue
    if (!supported.has(key)) {
      // The daemon decodes null and empty values as absent. A null stays byte
      // for byte, and so does an empty value while the action is unchanged;
      // anything else is removed.
      const raw = rawCronValue(doc, path)
      if (raw !== 'null' && !(zeroValue(raw) && !draft.dirty.has('mode'))) remove(path)
      continue
    }
    if (draft.dirty.has(key) && visible.has(key)) doc = patchCronValue(doc, path, encode(key))
  }
  if (draft.dirty.has('trigger')) set(['trigger', 'kind'], values.trigger ?? '')
  const schedulePath = ownPath(schedules, values.trigger ?? '')
  if (schedulePath && draft.dirty.has('schedule') && visible.has('schedule'))
    doc = patchCronValue(doc, schedulePath, encode('schedule'))
  if (values.mode === 'recurring' && rawCronValue(doc, ['overlap']) !== '"allow"')
    set(['overlap'], 'allow')
  const title = fieldText(doc, ['issue', 'title'])
  if (
    values.issue_kind === 'per-run' &&
    execute &&
    !title?.trim() &&
    (!draft.definitionEventUID ||
      ['title', 'mode', 'issue_kind'].some((key) => draft.dirty.has(key)))
  )
    set(['issue', 'title'], values.name ?? '')
  return doc
  function encode(key: string): string | undefined {
    const text = values[key] ?? ''
    if (secondsFields.has(key) || (key === 'schedule' && values.trigger === 'interval'))
      return text === '' ? undefined : exactSeconds(key, text)
    if (key === 'enabled') {
      if (text !== 'true' && text !== 'false') throw new Error('enabled: expected true or false')
      return text
    }
    if (key === 'tags' || key === 'secret_refs')
      return text === '' ? undefined : parseCronDocument(text).source
    return text === '' ? undefined : JSON.stringify(text)
  }
}
