import { expect, test } from 'vitest'
import * as hegel from '@hegeldev/hegel'
import * as gs from '@hegeldev/hegel/generators'
import { parseCronDocument, rawCronValue } from './document'
import {
  buildCronJobDraft,
  cronJobFieldPaths,
  serializeCronJobDraft,
  supportedJobFields,
  type CronDefinitionDraft,
} from './jobDraft'
import { jobFields, setJobField, validateJobDraft } from './jobFields'

// Hidden fields left behind by Action, Schedule or Issue changes must never
// reach the request or block saving, and untouched original fields survive.
const uid = '01ARZ3NDEKTSV4RRFFQ69G5FAV'
const originals: (string | undefined)[] = [
  undefined,
  `{"version":1,"kind":"job","enabled":true,"trigger":{"kind":"cron","cron":"0 7 * * *","timezone":"UTC"},"action":{"kind":"execute","prompt":"Inspect"},"issue":{"kind":"per-run","title":"Result","body":"Body","deadline_offset_seconds":3600},"overlap":"forbid","catchup":"all","grace_seconds":30,"timeout_seconds":600,"checkout_key":"main","secret_refs":{"TOKEN":"vault-token"},"options":{"tags":["tag"]}}`,
  `{"version":1,"kind":"job","enabled":true,"trigger":{"kind":"interval","interval_seconds":120},"action":{"kind":"execute","workflow_uid":"${uid}"},"issue":{"kind":"existing","uid":"${uid}"},"overlap":"allow","catchup":"latest","timeout_seconds":60}`,
  `{"version":1,"kind":"job","enabled":true,"trigger":{"kind":"manual"},"action":{"kind":"notify","recipient":"current-owner-or-author","message":"Check"},"issue":{"kind":"existing","uid":"${uid}"},"overlap":"forbid","catchup":"all","timeout_seconds":60,"secret_refs":{"TOKEN":"vault-token"}}`,
  `{"version":1,"kind":"job","enabled":true,"trigger":{"kind":"issue-deadline","issue_uid":"${uid}","lead_seconds":600},"action":{"kind":"notify","recipient":"current-owner-or-author"},"overlap":"forbid","catchup":"all"}`,
  `{"version":1,"kind":"job","enabled":true,"trigger":{"kind":"cron","cron":"0 9 * * 1"},"action":{"kind":"notify","recipient":"current-owner-or-author"},"issue":{"kind":"per-run","parent_uid":"${uid}","title":"Weekly"},"overlap":"allow","catchup":"latest","options":{}}`,
]
const samples: Record<string, string[]> = {
  name: ['Edited job'],
  prompt: ['Edited prompt', ''],
  workflow: [uid],
  recipient: ['current-owner-or-author', 'team/reviewer'],
  message: ['Edited message', ''],
  parent: [uid],
  title: ['Edited title', ''],
  body: ['Edited body'],
  deadline: ['-1h', 'not a duration'],
  timeout: ['90', '9223372037'],
  grace: ['15'],
  checkout: ['workspace-main', '../outside'],
  secret_refs: ['API=vault-api', 'not pairs'],
  issue: [uid, ''],
  trigger_issue: [uid],
  lead: ['1h'],
  timezone: ['America/New_York', 'Not/AZone'],
  schedule: ['0 * * * *', '5m', '2026-11-02T06:30:00Z'],
  enabled: ['false'],
}
const choices = (draft: CronDefinitionDraft) =>
  jobFields(draft, { persisted: !!draft.definitionEventUID, projectUID: 'p1', workflows: [] })
function draftFor(source: string | undefined) {
  return buildCronJobDraft(
    source
      ? {
          uid,
          name: 'Example job',
          definitionEventUID: 'event',
          document: parseCronDocument(source),
        }
      : undefined,
    1,
  )
}
const zero = (value: unknown) =>
  value === undefined ||
  value === null ||
  value === '' ||
  value === 0 ||
  (typeof value === 'object' && Object.keys(value as object).length === 0)

test('Hegel action, schedule and issue sequences send only fields the final job supports', () =>
  hegel.test(
    (tc) => {
      const source = tc.draw(gs.sampledFrom(originals))
      let d = draftFor(source)
      const touched = new Set<string>()
      for (let step = tc.draw(gs.integers({ minValue: 0, maxValue: 8 })); step > 0; step--) {
        const fields = choices(d).filter((field) => field.kind !== 'readonly')
        const field = fields[tc.draw(gs.integers({ minValue: 0, maxValue: fields.length - 1 }))]!
        const options =
          field.kind === 'choice'
            ? (field.choices ?? []).map((choice) => choice.value)
            : field.kind === 'bool'
              ? ['true', 'false']
              : (samples[field.key] ?? ['Edited'])
        if (!options.length) continue
        const value = tc.draw(gs.sampledFrom(options))
        try {
          d = setJobField(d, field.key, value)
          touched.add(field.key)
        } catch {
          /* The form refuses this value; the draft is unchanged. */
        }
      }
      const visible = new Set(
        choices(d)
          .filter((field) => field.kind !== 'readonly' || field.key === 'mode')
          .map((field) => field.key),
      )
      const errors = validateJobDraft(d)
      for (const key of Object.keys(errors)) expect(visible).toContain(key)
      if (Object.keys(errors).length) return
      let out
      try {
        out = serializeCronJobDraft(d)
      } catch (error) {
        // Refusals that keep data the form cannot show are reported, never stripped.
        expect(String(error)).toMatch(/preserve|immutable/)
        return
      }
      const job = JSON.parse(out.source) as {
        trigger: Record<string, unknown>
        action: Record<string, unknown>
        issue?: Record<string, unknown>
        [key: string]: unknown
      }
      const kind = job.action.kind
      const issue = job.issue
      if (issue) expect(['existing', 'per-run']).toContain(issue.kind)
      if (kind === 'execute') {
        expect(zero(job.action.recipient) && zero(job.action.message)).toBe(true)
        expect(issue).toBeDefined()
        expect(issue!.parent_uid).toBeUndefined()
      } else {
        expect(zero(job.action.prompt) && zero(job.action.workflow_uid)).toBe(true)
        expect(zero(job.checkout_key)).toBe(true)
      }
      if (issue?.kind === 'existing') {
        expect(Object.keys(issue).sort()).toEqual(['kind', 'uid'])
      }
      if (kind === 'notify' && issue?.kind === 'per-run') {
        expect(zero(job.timeout_seconds) && zero(job.secret_refs)).toBe(true)
        expect(job.overlap).toBe('allow')
        expect(issue.deadline_offset_seconds ?? null).toBeNull()
      }
      if (issue?.kind === 'per-run' && kind === 'execute') expect(issue.uid).toBeUndefined()
      const trigger = job.trigger
      for (const [schedule, key] of [
        ['cron', 'cron'],
        ['interval', 'interval_seconds'],
        ['once', 'at'],
      ] as const)
        if (trigger.kind !== schedule) expect(trigger[key]).toBeUndefined()
      if (!['issue-scheduled', 'issue-deadline'].includes(trigger.kind as string))
        expect(trigger.issue_uid).toBeUndefined()
      if (trigger.kind !== 'issue-deadline') expect(trigger.lead_seconds).toBeUndefined()
      // Untouched original values survive whenever the final job supports them.
      if (!source) return
      const original = parseCronDocument(source)
      const supported = supportedJobFields(d.values)
      for (const [key, path] of Object.entries(cronJobFieldPaths)) {
        if (touched.has(key) || ['issue_kind', 'trigger', 'overlap', 'title'].includes(key))
          continue
        const before = rawCronValue(original, path)
        if (before === undefined || !supported.has(key)) continue
        expect(rawCronValue(out, path), key).toBe(before)
      }
    },
    { testCases: 2000 },
  ))
