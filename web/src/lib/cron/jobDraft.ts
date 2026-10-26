import { createULID } from '../ulid'
import type { CronDefinitionRecord } from './client'
import { parseCronDocument, patchCronValue, rawCronValue, type RawCronDocument } from './document'
import { parseTree, type Node } from 'jsonc-parser'

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
  if (draft.dirty.has('mode')) {
    const execute = values.mode === 'prompt' || values.mode === 'workflow'
    set(['action', 'kind'], execute ? 'execute' : 'notify')
    for (const key of execute ? ['recipient', 'message'] : ['prompt', 'workflow_uid'])
      remove(['action', key])
    if (values.mode === 'prompt') remove(['action', 'workflow_uid'])
    if (values.mode === 'workflow') remove(['action', 'prompt'])
    if (!execute) {
      remove(['checkout_key'])
      remove(['timeout_seconds'])
      remove(['secret_refs'])
      if (values.mode === 'recurring') {
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
        set(['overlap'], 'allow')
      }
    }
  }
  if (
    values.issue_kind === 'existing' &&
    rawCronValue(draft.original, ['issue', 'scheduled_offset_seconds']) !== undefined &&
    rawCronValue(draft.original, ['issue', 'scheduled_offset_seconds']) !== 'null'
  )
    throw new Error('Cannot preserve the existing scheduled offset with an existing-issue policy')
  // An absent issue policy stays absent; a mode change alone never adds one.
  const issueKind = values.issue_kind
  if ((draft.dirty.has('mode') || draft.dirty.has('issue_kind')) && issueKind === undefined)
    remove(['issue'])
  else if (draft.dirty.has('mode') || draft.dirty.has('issue_kind')) {
    set(['issue', 'kind'], issueKind!)
    if (values.issue_kind === 'existing')
      for (const key of ['parent_uid', 'title', 'body', 'deadline_offset_seconds'])
        remove(['issue', key])
    else remove(['issue', 'uid'])
    if (values.mode !== 'recurring') remove(['issue', 'parent_uid'])
    if (values.mode !== 'recurring' && values.issue_kind === 'per-run' && !values.title?.trim())
      set(['issue', 'title'], values.name ?? '')
  }
  if (draft.dirty.has('trigger')) {
    const active =
      values.trigger === 'cron'
        ? 'cron'
        : values.trigger === 'interval'
          ? 'interval_seconds'
          : values.trigger === 'once'
            ? 'at'
            : ''
    for (const key of ['cron', 'interval_seconds', 'at'])
      if (key !== active) remove(['trigger', key])
    if (!['issue-scheduled', 'issue-deadline'].includes(values.trigger ?? ''))
      remove(['trigger', 'issue_uid'])
    if (values.trigger !== 'issue-deadline') remove(['trigger', 'lead_seconds'])
  }
  for (const key of draft.dirty) {
    if (key === 'name' || key === 'mode') continue
    // Drafts remember dormant input while switching modes. Only active fields
    // may be written back, otherwise an old dirty field revives invalid state.
    if (
      (key === 'prompt' && values.mode !== 'prompt') ||
      (key === 'workflow' && values.mode !== 'workflow')
    )
      continue
    if (
      ['recipient', 'message'].includes(key) &&
      ['prompt', 'workflow'].includes(values.mode ?? '')
    )
      continue
    if (
      ['timeout', 'checkout', 'secret_refs'].includes(key) &&
      !['prompt', 'workflow'].includes(values.mode ?? '')
    )
      continue
    if (key === 'parent' && values.mode !== 'recurring') continue
    if (key === 'issue' && values.issue_kind !== 'existing') continue
    if (['title', 'body', 'deadline'].includes(key) && values.issue_kind === 'existing') continue
    // Native recurring jobs carry no deadline offset; only clearing one is written.
    if (key === 'deadline' && values.mode === 'recurring' && values.deadline) continue
    if (
      key === 'trigger_issue' &&
      !['issue-scheduled', 'issue-deadline'].includes(values.trigger ?? '')
    )
      continue
    if (key === 'lead' && values.trigger !== 'issue-deadline') continue
    if (key === 'schedule' && !ownPath(schedules, values.trigger ?? '')) continue
    const path =
      key === 'schedule'
        ? ownPath(schedules, draft.values.trigger ?? '')
        : ownPath(cronJobFieldPaths, key)
    if (!path) throw new Error(`Unsupported cron field: ${key}`)
    const text = draft.values[key] ?? ''
    let replacement: string | undefined
    if (secondsFields.has(key) || (key === 'schedule' && draft.values.trigger === 'interval'))
      replacement = text === '' ? undefined : exactSeconds(key, text)
    else if (key === 'enabled') {
      if (text !== 'true' && text !== 'false') throw new Error('enabled: expected true or false')
      replacement = text
    } else if (key === 'tags' || key === 'secret_refs') {
      replacement = text === '' ? undefined : parseCronDocument(text).source
    } else replacement = text === '' ? undefined : JSON.stringify(text)
    doc = patchCronValue(doc, path, replacement)
  }
  if (
    values.issue_kind === 'per-run' &&
    ['prompt', 'workflow'].includes(values.mode ?? '') &&
    !values.title?.trim() &&
    (!draft.definitionEventUID ||
      ['title', 'mode', 'issue_kind'].some((key) => draft.dirty.has(key)))
  )
    set(['issue', 'title'], values.name ?? '')
  return doc
}
