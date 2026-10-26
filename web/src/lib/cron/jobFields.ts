import { parseTree } from 'jsonc-parser'
import { buildCronJobDraft, type CronDefinitionDraft } from './jobDraft'
import { parseCronDocument, rawCronValue } from './document'

export interface JobField {
  key: string
  label: string
  kind: 'text' | 'textarea' | 'choice' | 'bool' | 'readonly'
  choices?: readonly { value: string; label: string }[]
  readOnlyReason?: string
}
export interface JobFieldContext {
  persisted: boolean
  projectUID: string
  workflows: readonly { uid: string; name: string; deleted: boolean }[]
}
const preserved = 'Preserved; edit the definition with kata cron job.'
const modes = ['prompt', 'workflow', 'notify', 'recurring']
const triggers = ['manual', 'cron', 'interval', 'once', 'issue-scheduled', 'issue-deadline']
const durationKeys = ['grace', 'timeout', 'deadline', 'lead']
function native(draft: CronDefinitionDraft) {
  return (
    parseTree(rawCronValue(draft.original, ['action', 'kind']) ?? 'null')?.value === 'notify' &&
    parseTree(rawCronValue(draft.original, ['issue', 'kind']) ?? 'null')?.value === 'per-run'
  )
}
export function jobFields(draft: CronDefinitionDraft, context: JobFieldContext): JobField[] {
  const out: JobField[] = [],
    v = draft.values,
    recurring = v.mode === 'recurring',
    execute = v.mode === 'prompt' || v.mode === 'workflow'
  const add = (
    key: string,
    label: string,
    kind: JobField['kind'] = 'text',
    choices?: readonly string[],
    reason?: string,
  ) => {
    out.push({
      key,
      label,
      kind,
      ...(choices ? { choices: choices.map((value) => ({ value, label: value })) } : {}),
      ...(reason ? { readOnlyReason: reason } : {}),
    })
  }
  add('name', 'Name')
  if (!recurring) {
    add('description', 'Description', 'readonly', undefined, preserved)
    add('tags', 'Tags', 'readonly', undefined, preserved)
  }
  add(
    'mode',
    'Action',
    context.persisted && native(draft) ? 'readonly' : 'choice',
    modes,
    'Native recurring ownership is immutable.',
  )
  if (v.mode === 'prompt') add('prompt', 'Prompt', 'textarea')
  if (v.mode === 'workflow') {
    const workflows = context.workflows
      .filter((w) => !w.deleted)
      .toSorted((a, b) => a.name.localeCompare(b.name) || a.uid.localeCompare(b.uid))
    out.push({
      key: 'workflow',
      label: 'Workflow',
      kind: 'choice',
      choices: workflows.map((w) => ({ value: w.uid, label: w.name })),
    })
    add('input', 'Input', 'readonly', undefined, preserved)
  }
  if (!execute) {
    add('recipient', 'Recipient')
    if (recurring) {
      add(
        'parent',
        'Parent issue',
        context.persisted && native(draft) ? 'readonly' : 'text',
        undefined,
        'Native parent binding is immutable.',
      )
      add('title', 'Child title')
      add('body', 'Child body', 'textarea')
    }
    add('message', 'Message', 'textarea')
  }
  add('trigger', 'Schedule', 'choice', recurring ? ['cron', 'interval'] : triggers)
  if (['cron', 'interval', 'once'].includes(v.trigger ?? ''))
    add(
      'schedule',
      v.trigger === 'cron' ? 'Cron' : v.trigger === 'interval' ? 'Interval' : 'Run at',
    )
  if (v.trigger === 'issue-scheduled' || v.trigger === 'issue-deadline')
    add('trigger_issue', 'Issue reference')
  if (v.trigger === 'issue-deadline') add('lead', 'Lead')
  add('timezone', 'Timezone')
  add('catchup', 'Catchup', 'choice', ['latest', 'all', 'skip'])
  add('grace', 'Grace')
  add(
    'overlap',
    'Overlap',
    recurring ? 'readonly' : 'choice',
    ['forbid', 'allow'],
    recurring ? 'Recurring jobs require allow.' : undefined,
  )
  if (execute) {
    add('timeout', 'Timeout')
    add('checkout', 'Checkout')
    add('issue_kind', 'Issue', 'choice', ['per-run', 'existing'])
    if (v.issue_kind === 'per-run') {
      add('title', 'Issue title')
      add('body', 'Issue body', 'textarea')
      add('deadline', 'Deadline offset')
    } else add('issue', 'Existing issue')
    add('secret_refs', 'Secret refs')
  } else if (!recurring) add('issue', 'Existing issue')
  add('other_options', 'Other options', 'readonly', undefined, preserved)
  if (v.scheduled_offset !== undefined)
    add('scheduled_offset', 'Scheduled offset', 'readonly', undefined, 'Preserved existing offset.')
  add('enabled', 'Enabled', 'bool')
  return out
}

function seconds(text: string, signed: boolean): string {
  const trimmed = text.trim()
  let value: bigint
  if (/^[+-]?\d+$/.test(trimmed)) value = BigInt(trimmed)
  else {
    const sign = trimmed.startsWith('-') ? -1n : 1n
    const body = trimmed.replace(/^[+-]/, '')
    const parts = [...body.matchAll(/(\d+(?:\.\d*)?|\.\d+)(h|ms|m|s|us|µs|μs|ns)/g)]
    if (!parts.length || parts.map((p) => p[0]).join('') !== body)
      throw new Error('Enter whole seconds or a duration such as 2h30m.')
    const units: Record<string, bigint> = {
      h: 3600000000000n,
      m: 60000000000n,
      s: 1000000000n,
      ms: 1000000n,
      us: 1000n,
      µs: 1000n,
      μs: 1000n,
      ns: 1n,
    }
    let total = 0n
    for (const p of parts) {
      const [integer = '', fraction = ''] = p[1]!.split('.')
      const denominator = 10n ** BigInt(fraction.length)
      const numerator = BigInt((integer || '0') + fraction) * units[p[2]!]!
      if (numerator % denominator !== 0n) throw new Error('Enter an exact whole-second duration.')
      total += numerator / denominator
    }
    if (total % 1000000000n !== 0n) throw new Error('Enter an exact whole-second duration.')
    value = (sign * total) / 1000000000n
  }
  if (value > 9223372036n || value < (signed ? -9223372036n : 0n))
    throw new Error(
      'Duration must fit Go seconds (maximum 9223372036); only deadline offsets may be negative.',
    )
  return value.toString()
}
function zoneFormat(zone: string) {
  return new Intl.DateTimeFormat('en-CA', {
    timeZone: zone || 'UTC',
    year: 'numeric',
    month: '2-digit',
    day: '2-digit',
    hour: '2-digit',
    minute: '2-digit',
    second: '2-digit',
    hourCycle: 'h23',
  })
}
function wall(format: Intl.DateTimeFormat, instant: number) {
  const parts = Object.fromEntries(format.formatToParts(instant).map((p) => [p.type, p.value]))
  return `${parts.year}-${parts.month}-${parts.day}T${parts.hour}:${parts.minute}:${parts.second}`
}
function once(text: string, zone: string, original?: string): string {
  const format = zoneFormat(zone)
  // An unchanged stored instant is kept verbatim, including subsecond precision.
  if (text === original) return text
  if (/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d{1,9})?(Z|[+-]\d{2}:\d{2})$/.test(text)) {
    const date = text.slice(0, 10),
      clock = text.slice(11, 19)
    if (
      !Number.isFinite(Date.parse(text)) ||
      new Date(`${date}T${clock}Z`).toISOString().slice(0, 19) !== `${date}T${clock}` ||
      /[+-](?:2[4-9]|[3-9]\d):|[+-]\d\d:[6-9]\d$/.test(text)
    )
      throw new Error('Enter a valid RFC3339 instant.')
    return text
  }
  const local = text
    .trim()
    .replace(' ', 'T')
    .replace(/^(\d{4}-\d{2}-\d{2}T\d{2}:\d{2})$/, '$1:00')
  if (!/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}$/.test(local))
    throw new Error('Enter RFC3339 or YYYY-MM-DD HH:mm in the selected timezone.')
  const nominal = Date.parse(local + 'Z')
  if (!Number.isFinite(nominal) || new Date(nominal).toISOString().slice(0, 19) !== local)
    throw new Error('Enter a valid calendar date and time.')
  // Sample offsets on both sides of a transition, then verify candidates against
  // the full wall clock. This handles fractional-hour and date-line transitions.
  const offsets = new Set<number>()
  for (const hours of [-48, -24, -12, 0, 12, 24, 48]) {
    const instant = nominal + hours * 3600000
    offsets.add(Date.parse(wall(format, instant) + 'Z') - instant)
  }
  const candidates = [...offsets]
    .map((offset) => nominal - offset)
    .filter((instant) => wall(format, instant) === local)
  if (candidates.length !== 1)
    throw new Error(
      candidates.length
        ? 'Ambiguous local time; supply an explicit RFC3339 offset.'
        : 'This local time does not exist; supply an explicit RFC3339 offset.',
    )
  return new Date(candidates[0]!).toISOString().replace('.000Z', 'Z')
}
const portable = (value: string) =>
  value.length > 0 &&
  new TextEncoder().encode(value).length <= 256 &&
  value !== '.' &&
  value !== '..' &&
  /^[\p{L}\p{Nd}_.-]+$/u.test(value)
function secrets(text: string): string {
  // Existing JSON is already a portable reference map, never an opaque options value.
  if (text.trim().startsWith('{')) {
    parseCronDocument(text)
    const root = parseTree(text)
    if (
      !root ||
      root.type !== 'object' ||
      (root.children ?? []).some(
        (p) =>
          !portable(p.children![0]!.value as string) ||
          p.children![1]?.type !== 'string' ||
          !portable(p.children![1]!.value as string),
      )
    )
      throw new Error('Use portable VAR=name pairs.')
    const keys = (root.children ?? []).map((p) => p.children![0]!.value as string)
    if (new Set(keys).size !== keys.length) throw new Error('Duplicate secret variable.')
    return text
  }
  const refs: Record<string, string> = Object.create(null) as Record<string, string>
  for (const pair of text.split(',')) {
    const parts = pair.trim().split('='),
      key = parts[0]?.trim() ?? '',
      value = parts[1]?.trim() ?? ''
    if (parts.length !== 2 || !portable(key) || !portable(value) || Object.hasOwn(refs, key))
      throw new Error('Use unique portable VAR=name pairs.')
    refs[key] = value
  }
  return JSON.stringify(refs)
}
function canonical(draft: CronDefinitionDraft, key: string, text: string): string {
  if (durationKeys.includes(key)) return text.trim() ? seconds(text, key === 'deadline') : ''
  if (key === 'schedule' && draft.values.trigger === 'interval') {
    const value = seconds(text, false)
    if (BigInt(value) < (draft.values.mode === 'recurring' ? 60n : 1n))
      throw new Error('Interval must be positive; recurring requires at least 60 seconds.')
    return value
  }
  if (key === 'schedule' && draft.values.trigger === 'once')
    return once(
      text,
      draft.values.timezone ?? '',
      buildCronJobDraft(
        {
          uid: draft.uid,
          name: draft.name,
          definitionEventUID: draft.definitionEventUID,
          document: draft.original,
        },
        draft.projectID,
      ).values.schedule,
    )
  if (key === 'timezone') {
    zoneFormat(text)
    return text.trim()
  }
  if (key === 'secret_refs') return text.trim() ? secrets(text) : ''
  return text
}
export function setJobField(
  draft: CronDefinitionDraft,
  key: string,
  text: string,
): CronDefinitionDraft {
  const fields = jobFields(draft, {
    persisted: !!draft.definitionEventUID,
    projectUID: '',
    workflows: [],
  })
  const field = fields.find((f) => f.key === key)
  // Workflow choices are supplied by the editor's project catalog. Hidden fields
  // cannot be set through this entry point.
  if (!field || field.kind === 'readonly') throw new Error(`${key}: read-only or unavailable`)
  if (
    field.kind === 'choice' &&
    key !== 'workflow' &&
    !field.choices?.some((c) => c.value === text)
  )
    throw new Error(`${key}: choose an available value`)
  if (key === 'enabled' && !['true', 'false'].includes(text))
    throw new Error('enabled: expected true or false')
  const next = { ...draft, values: { ...draft.values }, dirty: new Set(draft.dirty) }
  let value = text
  try {
    value = canonical(draft, key, text)
  } catch {
    /* Keep rejected input visible until corrected. */
  }
  if (value === draft.values[key]) return draft
  if (key === 'trigger') {
    next.values[`schedule:${draft.values.trigger}`] = draft.values.schedule ?? ''
    next.values.schedule = next.values[`schedule:${text}`] ?? ''
    next.dirty.add('schedule')
  }
  next.values[key] = value
  next.dirty.add(key)
  if (key === 'mode') {
    // Notify and recurring imply their own issue policy; an execution job gets
    // back the policy it had before the detour.
    const executes = (mode: string | undefined) => mode === 'prompt' || mode === 'workflow'
    if (executes(draft.values.mode) && !executes(text))
      next.values['issue_kind:execute'] = draft.values.issue_kind ?? 'per-run'
    if (text === 'recurring') {
      next.values['overlap:ordinary'] = draft.values.overlap ?? 'forbid'
      next.values.issue_kind = 'per-run'
      next.values.overlap = 'allow'
      if (!['cron', 'interval'].includes(next.values.trigger ?? '')) {
        // Keep the schedule for its trigger, as a trigger change does.
        next.values[`schedule:${next.values.trigger}`] = next.values.schedule ?? ''
        next.values.trigger = 'cron'
        next.values.schedule = next.values['schedule:cron'] ?? ''
        next.dirty.add('trigger')
        next.dirty.add('schedule')
      }
    }
    if (draft.values.mode === 'recurring' && text !== 'recurring')
      next.values.overlap = draft.values['overlap:ordinary'] ?? 'forbid'
    if (text === 'notify') next.values.issue_kind = 'existing'
    if (executes(text) && !executes(draft.values.mode))
      next.values.issue_kind = draft.values['issue_kind:execute'] ?? 'per-run'
  }
  if (
    key === 'issue' &&
    next.values.mode === 'notify' &&
    value.trim() &&
    next.values.issue_kind !== 'existing'
  ) {
    next.values.issue_kind = 'existing'
    next.dirty.add('issue_kind')
  }
  // Selectors such as timezone decide how other edited text normalizes. Text the
  // new selectors accept is stored in its canonical form, so it serializes as
  // validated.
  for (const field of jobFields(next, {
    persisted: !!next.definitionEventUID,
    projectUID: '',
    workflows: [],
  })) {
    if (field.key === key || field.kind !== 'text' || !next.dirty.has(field.key)) continue
    try {
      next.values[field.key] = canonical(next, field.key, next.values[field.key] ?? '')
    } catch {
      /* Keep rejected input visible until corrected. */
    }
  }
  return next
}
export function validateJobDraft(draft: CronDefinitionDraft): Record<string, string> {
  const errors: Record<string, string> = {},
    v = draft.values
  for (const field of jobFields(draft, {
    persisted: !!draft.definitionEventUID,
    projectUID: '',
    workflows: [],
  })) {
    if (field.kind === 'readonly') continue
    try {
      canonical(draft, field.key, v[field.key] ?? '')
    } catch (error) {
      errors[field.key] = error instanceof Error ? error.message : 'Invalid value'
    }
  }
  if (!v.name?.trim()) errors.name = 'Name is required.'
  if (v.mode === 'prompt' && !v.prompt?.trim()) errors.prompt = 'Prompt is required.'
  if (v.mode === 'workflow' && !v.workflow) errors.workflow = 'Select an available workflow.'
  if (v.trigger === 'cron' && !v.schedule?.trim()) errors.schedule = 'Cron expression is required.'
  if (
    v.issue_kind === 'existing' &&
    rawCronValue(draft.original, ['issue', 'scheduled_offset_seconds']) !== undefined &&
    rawCronValue(draft.original, ['issue', 'scheduled_offset_seconds']) !== 'null'
  )
    errors[v.mode === 'notify' ? 'mode' : 'issue_kind'] =
      'Cannot preserve the existing scheduled offset with an existing-issue policy. Create a new job.'
  if (v.mode === 'recurring' && !native(draft)) {
    const options = rawCronValue(draft.original, ['options']),
      node = options ? parseTree(options) : undefined
    if (node && (node.type !== 'object' || !!node.children?.length))
      errors.mode =
        'Cannot preserve adapter options when converting to native recurring. Create a new recurring job.'
    if (
      (rawCronValue(draft.original, ['issue', 'scheduled_offset_seconds']) !== undefined &&
        rawCronValue(draft.original, ['issue', 'scheduled_offset_seconds']) !== 'null') ||
      (rawCronValue(draft.original, ['issue', 'deadline_offset_seconds']) !== undefined &&
        rawCronValue(draft.original, ['issue', 'deadline_offset_seconds']) !== 'null' &&
        !(draft.dirty.has('deadline') && v.deadline === ''))
    )
      errors.mode =
        'Cannot preserve existing offsets when converting to native recurring. Create a new recurring job.'
  }
  if (v.mode === 'recurring' && !v.title?.trim()) errors.title = 'Child title is required.'
  if (v.mode === 'recurring' && !v.parent?.trim()) errors.parent = 'Parent issue is required.'
  if (
    v.mode === 'recurring' &&
    v.catchup === 'skip' &&
    (!v.grace || !/^\d+$/.test(v.grace) || BigInt(v.grace) === 0n)
  )
    errors.grace = 'Skip requires positive grace.'
  if (v.mode === 'notify' || v.mode === 'recurring') {
    if (
      !v.recipient?.trim() ||
      /\s/.test(v.recipient) ||
      v.recipient.split('/').some((p) => !p) ||
      v.recipient.split('/').length > 2
    )
      errors.recipient = 'Enter an exact recipient or current-owner-or-author.'
    if (v.mode === 'notify' && v.recipient !== 'current-owner-or-author' && !v.message?.trim())
      errors.message = 'Custom notifications require a message.'
  }
  const issueDateTrigger = ['issue-scheduled', 'issue-deadline'].includes(v.trigger ?? '')
  const rawIssue = rawCronValue(draft.original, ['issue'])
  const explicitIssuePolicy =
    v.issue_kind !== undefined ||
    !!v.issue?.trim() ||
    (rawIssue !== undefined && rawIssue !== 'null')
  if (v.mode === 'notify' && explicitIssuePolicy && v.issue_kind !== 'existing')
    errors.issue = 'Notifications require an existing-issue policy.'
  else if (
    (v.issue_kind === 'existing' ||
      (v.mode === 'notify' && (!issueDateTrigger || explicitIssuePolicy))) &&
    !v.issue?.trim()
  )
    errors.issue = 'Existing issue is required.'
  if (['issue-scheduled', 'issue-deadline'].includes(v.trigger ?? '') && !v.trigger_issue?.trim())
    errors.trigger_issue = 'Issue reference is required.'
  if (v.trigger === 'issue-deadline' && ['prompt', 'workflow'].includes(v.mode ?? ''))
    errors.trigger = 'Deadline actions are notification-only.'
  if (v.checkout && !portable(v.checkout)) errors.checkout = 'Checkout must be a portable key.'
  return errors
}
