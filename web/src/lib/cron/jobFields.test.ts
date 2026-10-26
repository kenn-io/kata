import { expect, test } from 'vitest'
import * as hegel from '@hegeldev/hegel'
import * as gs from '@hegeldev/hegel/generators'
import { buildCronJobDraft, serializeCronJobDraft } from './jobDraft'
import { parseCronDocument, rawCronValue } from './document'
import { jobFields, setJobField, validateJobDraft } from './jobFields'
const context = {
  persisted: false,
  projectUID: 'p1',
  workflows: [
    { uid: 'w1', name: 'Example', deleted: false },
    { uid: 'w2', name: 'Deleted', deleted: true },
  ],
}
const draft = () => buildCronJobDraft(undefined, 1)
test('TUI field policy covers action and schedule variants', () => {
  for (const mode of ['prompt', 'workflow', 'notify', 'recurring']) {
    const d = setJobField(draft(), 'mode', mode),
      fields = new Map(jobFields(d, context).map((f) => [f.key, f]))
    expect(fields.get('other_options')?.kind).toBe('readonly')
    if (mode !== 'recurring')
      for (const key of ['description', 'tags']) expect(fields.get(key)?.kind).toBe('readonly')
    if (mode === 'workflow') {
      expect(fields.get('input')?.kind).toBe('readonly')
      expect(fields.get('workflow')?.choices?.map((c) => c.value)).toEqual(['w1'])
    }
    if (mode === 'recurring') expect(fields.get('overlap')?.kind).toBe('readonly')
  }
  for (const trigger of [
    'manual',
    'cron',
    'interval',
    'once',
    'issue-scheduled',
    'issue-deadline',
  ]) {
    const fields = jobFields(setJobField(draft(), 'trigger', trigger), context)
    expect(fields.some((f) => f.key === 'schedule')).toBe(
      ['cron', 'interval', 'once'].includes(trigger),
    )
    expect(fields.some((f) => f.key === 'lead')).toBe(trigger === 'issue-deadline')
  }
})
test('exact Go duration boundaries and recurring minimum', () => {
  for (const value of ['9223372037', '9007199254740993', '1.5s', '1e3', '-1'])
    expect(validateJobDraft(setJobField(draft(), 'timeout', value)).timeout).toBeTruthy()
  expect(setJobField(draft(), 'timeout', '2h30m').values.timeout).toBe('9000')
  expect(validateJobDraft(setJobField(draft(), 'timeout', '9223372036')).timeout).toBeUndefined()
  expect(setJobField(draft(), 'deadline', '-1h').values.deadline).toBe('-3600')
  const d = setJobField(setJobField(draft(), 'mode', 'recurring'), 'trigger', 'interval')
  expect(validateJobDraft(setJobField(d, 'schedule', '59')).schedule).toBeTruthy()
  expect(validateJobDraft(setJobField(d, 'schedule', '1m')).schedule).toBeUndefined()
})
const durationDraft = (key: string) =>
  key === 'schedule'
    ? setJobField(draft(), 'trigger', 'interval')
    : key === 'lead'
      ? setJobField(setJobField(draft(), 'mode', 'notify'), 'trigger', 'issue-deadline')
      : draft()
test.each(['timeout', 'grace', 'deadline', 'lead', 'schedule'])(
  'duration conversion for %s matches independent integer multiplication',
  (key) =>
    hegel.test(
      (tc) => {
        const n = tc.draw(
          gs.integers({
            minValue: key === 'deadline' ? -153722867 : key === 'schedule' ? 1 : 0,
            maxValue: 153722867,
          }),
        )
        expect(setJobField(durationDraft(key), key, `${n}m`).values[key]).toBe(
          (BigInt(n) * 60n).toString(),
        )
      },
      { testCases: 300 },
    ),
)
test.each(['timeout', 'grace', 'deadline', 'lead', 'schedule'])(
  'seconds beyond duration bound are rejected for %s',
  (key) =>
    hegel.test(
      (tc) => {
        const n = tc.draw(gs.integers({ minValue: 1, maxValue: Number.MAX_SAFE_INTEGER }))
        expect(
          validateJobDraft(
            setJobField(durationDraft(key), key, (9223372036n + BigInt(n)).toString()),
          )[key],
        ).toBeTruthy()
      },
      { testCases: 300 },
    ),
)
test('once wall time honors IANA timezone and rejects DST gaps and ambiguous edits', () => {
  const d = setJobField(setJobField(draft(), 'trigger', 'once'), 'timezone', 'America/New_York')
  expect(setJobField(d, 'schedule', '2026-01-15 09:00').values.schedule).toBe(
    '2026-01-15T14:00:00Z',
  )
  for (const value of ['2026-03-08 02:30', '2026-11-01 01:30', '2026-02-30 09:00'])
    expect(validateJobDraft(setJobField(d, 'schedule', value)).schedule).toBeTruthy()
  expect(setJobField(d, 'schedule', '2026-11-01T01:30:00-04:00').values.schedule).toBe(
    '2026-11-01T01:30:00-04:00',
  )
  expect(validateJobDraft(setJobField(d, 'timezone', 'Invalid/Zone')).timezone).toBeTruthy()
})
const source =
  '{"version":1,"kind":"job","enabled":true,"trigger":{"kind":"cron","cron":"0 7 * * *"},"action":{"kind":"execute","workflow_uid":"w1"},"issue":{"kind":"per-run","title":"Result","scheduled_offset_seconds":-23},"overlap":"forbid","catchup":"all","options":{"input":{"n":9007199254740993},"adapter":{"n":9007199254740993},"tags":["tag"],"description":"about"}}'
const existing = (raw = source) =>
  buildCronJobDraft(
    { uid: 'job', name: 'Example', definitionEventUID: 'rev', document: parseCronDocument(raw) },
    1,
  )
test('mode/trigger transitions clear incompatible fields and preserve hidden raw fields', () => {
  const d = setJobField(
      setJobField(setJobField(existing(), 'mode', 'prompt'), 'prompt', 'New prompt'),
      'trigger',
      'manual',
    ),
    out = serializeCronJobDraft(d)
  expect(rawCronValue(out, ['action', 'workflow_uid'])).toBeUndefined()
  expect(rawCronValue(out, ['action', 'prompt'])).toBe('"New prompt"')
  expect(rawCronValue(out, ['trigger', 'cron'])).toBeUndefined()
  for (const path of [['options'], ['issue', 'scheduled_offset_seconds']])
    expect(rawCronValue(out, path)).toBe(rawCronValue(parseCronDocument(source), path))
  for (const key of [
    'description',
    'tags',
    'input',
    'scheduled_offset',
    'other_options',
    'profile',
    'run_parent',
    '__proto__',
  ])
    expect(() => setJobField(existing(), key, 'changed')).toThrow()
})
test('persisted recurring mode, parent and overlap are immutable', () => {
  const d = existing(
    '{"version":1,"kind":"job","enabled":true,"trigger":{"kind":"interval","interval_seconds":60},"action":{"kind":"notify","recipient":"owner"},"issue":{"kind":"per-run","parent_uid":"parent","title":"Child"},"overlap":"allow","catchup":"all"}',
  )
  for (const key of ['mode', 'parent', 'overlap']) {
    expect(jobFields(d, { ...context, persisted: true }).find((f) => f.key === key)?.kind).toBe(
      'readonly',
    )
    expect(() => setJobField(d, key, 'changed')).toThrow()
  }
  expect(serializeCronJobDraft(setJobField(d, 'name', 'Changed')).source).toBe(d.original.source)
})
test('secret refs accept only unique portable name pairs', () => {
  expect(
    setJobField(draft(), 'secret_refs', 'TOKEN=service-token, USER=service-user').values
      .secret_refs,
  ).toBe('{"TOKEN":"service-token","USER":"service-user"}')
  for (const text of [
    'TOKEN=../secret',
    'TOKEN=x,TOKEN=y',
    '=x',
    'TOKEN=secret value',
    'TOKEN=⅓',
    `TOKEN=${'猫'.repeat(100)}`,
  ])
    expect(validateJobDraft(setJobField(draft(), 'secret_refs', text)).secret_refs).toBeTruthy()
})
test('Go-style fractional durations are accepted only when their total is whole seconds', () => {
  for (const [text, expected] of [
    ['1.5m', '90'],
    ['1000ms', '1'],
    ['1.25h30m', '6300'],
  ])
    expect(setJobField(draft(), 'timeout', text!).values.timeout).toBe(expected)
  for (const text of ['1ms', '0.5s', '-1ms'])
    expect(validateJobDraft(setJobField(draft(), 'timeout', text)).timeout).toBeTruthy()
})
test('switching modes after editing fields never revives incompatible values', () => {
  let d = setJobField(setJobField(draft(), 'prompt', 'Previously edited'), 'mode', 'workflow')
  d = setJobField(d, 'workflow', 'workflow-uid')
  const out = serializeCronJobDraft(d)
  expect(rawCronValue(out, ['action', 'prompt'])).toBeUndefined()
  expect(rawCronValue(out, ['action', 'workflow_uid'])).toBe('"workflow-uid"')
  d = setJobField(
    setJobField(setJobField(d, 'trigger', 'interval'), 'schedule', '5m'),
    'trigger',
    'manual',
  )
  expect(rawCronValue(serializeCronJobDraft(d), ['trigger', 'interval_seconds'])).toBeUndefined()
})
test('blank per-run title uses the job name when creating or explicitly clearing title', () => {
  let d = setJobField(setJobField(draft(), 'name', 'Example title'), 'prompt', 'Do work')
  expect(rawCronValue(serializeCronJobDraft(d), ['issue', 'title'])).toBe('"Example title"')
  d = setJobField(existing(), 'title', '')
  expect(rawCronValue(serializeCronJobDraft(d), ['issue', 'title'])).toBe('"Example"')
})
test('recurring conversion refuses to discard preserved adapter options', () => {
  const d = setJobField(existing(), 'mode', 'recurring')
  expect(validateJobDraft(d).mode).toBeTruthy()
  expect(() => serializeCronJobDraft(d)).toThrow(/preserv|options/i)
})
test('once instant is byte-preserved across unrelated edits and timezone changes', () => {
  const raw = source.replace(
    '"kind":"cron","cron":"0 7 * * *"',
    '"kind":"once","at":"2026-11-01T01:30:00.123456789-04:00"',
  )
  const d = setJobField(existing(raw), 'timezone', 'America/New_York')
  expect(rawCronValue(serializeCronJobDraft(d), ['trigger', 'at'])).toBe(
    '"2026-11-01T01:30:00.123456789-04:00"',
  )
})
test('issue-policy changes refuse to remove a hidden existing scheduled offset', () => {
  for (const d of [
    setJobField(existing(), 'issue_kind', 'existing'),
    setJobField(existing(), 'mode', 'notify'),
  ]) {
    expect(Object.values(validateJobDraft(d)).join(' ')).toMatch(/preserv.*scheduled offset/i)
    expect(() => serializeCronJobDraft(d)).toThrow(/preserv.*scheduled offset/i)
  }
})
test('nullable optional durations remain editable without rewriting their raw nulls', () => {
  const raw = source
    .replace('"scheduled_offset_seconds":-23', '"scheduled_offset_seconds":null')
    .replace('"options":', '"timeout_seconds":null,"options":')
  const d = setJobField(existing(raw), 'issue_kind', 'existing')
  expect(validateJobDraft(d).timeout).toBeUndefined()
  expect(validateJobDraft(d).issue_kind).toBeUndefined()
  const out = serializeCronJobDraft(d)
  expect(rawCronValue(out, ['timeout_seconds'])).toBe('null')
  expect(rawCronValue(out, ['issue', 'scheduled_offset_seconds'])).toBe('null')
})
test('lossless conversion to recurring preserves empty options and nullable offsets', () => {
  const raw = source.slice(0, source.indexOf(',"options":')) + ',"options":{}}'
  const clean = raw.replace('"scheduled_offset_seconds":-23', '"scheduled_offset_seconds":null')
  let d = setJobField(existing(clean), 'mode', 'recurring')
  d = setJobField(d, 'parent', '01ARZ3NDEKTSV4RRFFQ69G5FAV')
  d = setJobField(d, 'recipient', 'current-owner-or-author')
  expect(validateJobDraft(d)).toEqual({})
  const out = serializeCronJobDraft(d)
  expect(rawCronValue(out, ['options'])).toBe('{}')
  expect(rawCronValue(out, ['issue', 'scheduled_offset_seconds'])).toBe('null')
})

test('visible booleans, policies, checkout and duration controls accept and reject their domains', () => {
  for (const [key, values] of [
    ['enabled', ['true', 'false']],
    ['catchup', ['latest', 'all', 'skip']],
    ['overlap', ['allow', 'forbid']],
    ['issue_kind', ['existing', 'per-run']],
  ] as const) {
    for (const value of values) expect(setJobField(draft(), key, value).values[key]).toBe(value)
    expect(() => setJobField(draft(), key, 'unsupported')).toThrow()
  }
  expect(setJobField(draft(), 'checkout', 'workspace-main').values.checkout).toBe('workspace-main')
  expect(validateJobDraft(setJobField(draft(), 'checkout', '../workspace')).checkout).toBeTruthy()
  for (const key of ['grace', 'timeout', 'lead']) {
    expect(validateJobDraft(setJobField(durationDraft(key), key, '-1'))[key]).toBeTruthy()
  }
})
test('empty notification strings on an execute action do not change its form mode', () => {
  const raw = source.replace(
    '"workflow_uid":"w1"',
    '"workflow_uid":"w1","recipient":"","message":""',
  )
  const d = existing(raw)
  expect(d.values.mode).toBe('workflow')
  expect(serializeCronJobDraft(setJobField(d, 'name', 'Renamed')).source).toBe(raw)
})

test.each([false, true])(
  'recurring roundtrip restores ordinary overlap edits (persisted %s)',
  (persisted) => {
    for (const overlap of ['forbid', 'allow']) {
      let d = persisted ? existing() : draft()
      d = setJobField(d, 'overlap', overlap)
      d = setJobField(
        setJobField(d, 'mode', 'recurring'),
        'mode',
        persisted ? 'workflow' : 'prompt',
      )
      expect(d.values.overlap).toBe(overlap)
      expect(rawCronValue(serializeCronJobDraft(d), ['overlap'])).toBe(JSON.stringify(overlap))
    }
  },
)
test('signed deadline exact lower boundary and next rejected second', () => {
  const accepted = setJobField(draft(), 'deadline', '-9223372036')
  expect(validateJobDraft(accepted).deadline).toBeUndefined()
  expect(rawCronValue(serializeCronJobDraft(accepted), ['issue', 'deadline_offset_seconds'])).toBe(
    '-9223372036',
  )
  expect(validateJobDraft(setJobField(draft(), 'deadline', '-9223372037')).deadline).toBeTruthy()
})
test('generated negative deadline overflow is rejected exactly', () =>
  hegel.test(
    (tc) => {
      const excess = tc.draw(gs.integers({ minValue: 1, maxValue: Number.MAX_SAFE_INTEGER }))
      const text = (-9223372036n - BigInt(excess)).toString()
      const d = setJobField(draft(), 'deadline', text)
      expect(validateJobDraft(d).deadline).toBeTruthy()
      expect(() => serializeCronJobDraft(d)).toThrow(/represent|seconds/i)
    },
    { testCases: 300 },
  ))
const nativeTriggerNotify =
  '{"version":1,"kind":"job","trigger":{"kind":"issue-deadline","issue_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAV","lead_seconds":1800},"action":{"kind":"notify","recipient":"operator/worker","message":"Due soon"},"overlap":"forbid","catchup":"latest"}'
test.each(['issue-scheduled', 'issue-deadline'])(
  'native %s notify accepts its trigger target without an issue policy',
  (trigger) => {
    const raw = nativeTriggerNotify
      .replace('issue-deadline', trigger)
      .replace(trigger === 'issue-scheduled' ? ',"lead_seconds":1800' : 'NEVER', '')
    const d = existing(raw)
    expect(validateJobDraft(d)).toEqual({})
    expect(
      rawCronValue(serializeCronJobDraft(setJobField(d, 'name', 'Renamed')), ['issue']),
    ).toBeUndefined()
  },
)
test('notifications with other triggers still require an existing issue target', () => {
  for (const trigger of ['manual', 'cron', 'interval', 'once'])
    expect(
      validateJobDraft(setJobField(existing(nativeTriggerNotify), 'trigger', trigger)).issue,
    ).toBeTruthy()
})
test('an explicitly added notification issue creates and validates an existing policy', () => {
  const d = setJobField(existing(nativeTriggerNotify), 'issue', '01ARZ3NDEKTSV4RRFFQ69G5FAV')
  expect(validateJobDraft(d)).toEqual({})
  expect(rawCronValue(serializeCronJobDraft(d), ['issue', 'kind'])).toBe('"existing"')
  const invalid = existing(
    nativeTriggerNotify.replace(
      '"overlap":',
      '"issue":{"kind":"future","uid":"01ARZ3NDEKTSV4RRFFQ69G5FAV"},"overlap":',
    ),
  )
  expect(validateJobDraft(invalid).issue).toBeTruthy()
})
test('ordinary notify triggers accept an explicitly supplied valid existing issue policy', () => {
  for (const trigger of ['manual', 'cron', 'interval', 'once']) {
    let d = setJobField(existing(nativeTriggerNotify), 'trigger', trigger)
    if (trigger === 'cron') d = setJobField(d, 'schedule', '0 9 * * *')
    if (trigger === 'interval') d = setJobField(d, 'schedule', '1m')
    if (trigger === 'once') d = setJobField(d, 'schedule', '2026-10-25T09:00:00Z')
    d = setJobField(d, 'issue', '01ARZ3NDEKTSV4RRFFQ69G5FAV')
    expect(validateJobDraft(d)).toEqual({})
    expect(rawCronValue(serializeCronJobDraft(d), ['issue', 'kind'])).toBe('"existing"')
  }
})
test('changing the timezone normalizes a once time it made valid', () => {
  let d = setJobField(draft(), 'trigger', 'once')
  d = setJobField(d, 'timezone', 'America/New_York')
  d = setJobField(d, 'schedule', '2026-11-01 01:30')
  expect(validateJobDraft(d).schedule).toMatch(/Ambiguous/)
  d = setJobField(d, 'timezone', 'UTC')
  expect(validateJobDraft(d).schedule).toBeUndefined()
  expect(d.values.schedule).toBe('2026-11-01T01:30:00Z')
  expect(rawCronValue(serializeCronJobDraft(d), ['trigger', 'at'])).toBe('"2026-11-01T01:30:00Z"')
})
test.each(['notify', 'recurring'])(
  'an execution job keeps its existing-issue policy across a %s detour',
  (detour) => {
    const uid = '01ARZ3NDEKTSV4RRFFQ69G5FAV'
    let d = buildCronJobDraft(
      {
        uid: 'job',
        name: 'Example job',
        definitionEventUID: 'event',
        document: parseCronDocument(
          `{"version":1,"kind":"job","enabled":true,"trigger":{"kind":"cron","cron":"0 * * * *"},"action":{"kind":"execute","prompt":"Inspect"},"issue":{"kind":"existing","uid":"${uid}"},"overlap":"forbid","catchup":"all","options":{}}`,
        ),
      },
      1,
    )
    d = setJobField(setJobField(d, 'mode', detour), 'mode', 'prompt')
    expect(d.values.issue_kind).toBe('existing')
    const out = serializeCronJobDraft(d)
    expect(rawCronValue(out, ['issue', 'kind'])).toBe('"existing"')
    expect(rawCronValue(out, ['issue', 'uid'])).toBe(`"${uid}"`)
    expect(rawCronValue(out, ['issue', 'title'])).toBeUndefined()
  },
)
