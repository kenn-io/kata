import { expect, test } from 'vitest'
import { parseCronDocument, rawCronValue } from './document'
import { buildCronJobDraft, serializeCronJobDraft } from './jobDraft'
import { setJobField } from './jobFields'
const source =
  '{"version":1,"kind":"job","enabled":true,"trigger":{"kind":"manual"},"action":{"kind":"execute","prompt":"old"},"issue":{"kind":"per-run","title":"Result"},"overlap":"forbid","catchup":"all","options":{"adapter":{"big":9007199254740993,"exp":1e40,"zero":-0,"nil":null,"empty":{},"list":[],"escaped":"\\u0061","__proto__":{}}}}'
const record = () => ({
  uid: '01ARZ3NDEKTSV4RRFFQ69G5FAV',
  name: 'Example job',
  definitionEventUID: 'revision',
  document: parseCronDocument(source),
})
test('name edits leave the definition text exact', () => {
  const draft = buildCronJobDraft(record(), 2)
  draft.values.name = 'Renamed'
  draft.dirty.add('name')
  expect(serializeCronJobDraft(draft).source).toBe(source)
  expect(draft.definitionEventUID).toBe('revision')
})
test('prompt edits retain every unknown options value', () => {
  const draft = buildCronJobDraft(record(), 2)
  draft.values.prompt = 'Changed'
  draft.dirty.add('prompt')
  expect(serializeCronJobDraft(draft).source).toBe(source.replace('"old"', '"Changed"'))
})
test('creates allocate one stable UID per draft', () => {
  const draft = buildCronJobDraft(undefined, 2),
    uid = draft.uid
  serializeCronJobDraft(draft)
  serializeCronJobDraft(draft)
  expect(draft.uid).toMatch(/^[0-7][0-9A-HJKMNP-TV-Z]{25}$/)
  expect(draft.uid).toBe(uid)
  expect(buildCronJobDraft(undefined, 2).uid).not.toBe(uid)
})
test('rejects unrepresentable dirty numeric fields', () => {
  const draft = buildCronJobDraft(record(), 2)
  for (const value of ['9007199254740993', '1e40', '1.5', 'NaN']) {
    draft.values.timeout = value
    draft.dirty.add('timeout')
    expect(() => serializeCronJobDraft(draft)).toThrow(/timeout|represent|integer/i)
  }
})
test('refuses unknown typed fields visibly instead of stripping them', () => {
  for (const extra of ['"future":1,', '"trigger":{"kind":"manual","future":1},']) {
    const raw = extra.startsWith('"trigger"')
      ? source.replace('"trigger":{"kind":"manual"},', extra)
      : '{' + extra + source.slice(1)
    expect(() =>
      serializeCronJobDraft(
        buildCronJobDraft({ ...record(), document: parseCronDocument(raw) }, 2),
      ),
    ).toThrow(/preserv/i)
  }
})
test('missing optional values remain missing; untouched null remains null', () => {
  const draft = buildCronJobDraft(
    {
      ...record(),
      document: parseCronDocument(source.replace('"options":', '"timeout_seconds":0,"options":')),
    },
    2,
  )
  const output = serializeCronJobDraft(draft)
  expect(rawCronValue(output, ['checkout_key'])).toBeUndefined()
  expect(rawCronValue(output, ['options', 'adapter', 'nil'])).toBe('null')
  expect(rawCronValue(output, ['timeout_seconds'])).toBe('0')
})

test('a dormant deadline offset stays out of a recurring job and returns with its action', () => {
  let d = setJobField(buildCronJobDraft(undefined, 1), 'deadline', '-1h')
  d = setJobField(d, 'mode', 'recurring')
  expect(
    rawCronValue(serializeCronJobDraft(d), ['issue', 'deadline_offset_seconds']),
  ).toBeUndefined()
  expect(d.values.deadline).toBe('-3600')
  d = setJobField(d, 'mode', 'prompt')
  expect(rawCronValue(serializeCronJobDraft(d), ['issue', 'deadline_offset_seconds'])).toBe('-3600')
})

test('clearing a stored deadline offset before switching to recurring removes it', () => {
  const document = parseCronDocument(
    '{"version":1,"kind":"job","enabled":true,"trigger":{"kind":"cron","cron":"0 * * * *"},"action":{"kind":"execute","prompt":"Inspect"},"issue":{"kind":"per-run","title":"Result","deadline_offset_seconds":60},"overlap":"forbid","catchup":"all","options":{}}',
  )
  let d = buildCronJobDraft(
    { uid: 'job', name: 'Example job', definitionEventUID: 'event', document },
    1,
  )
  d = setJobField(d, 'deadline', '')
  d = setJobField(d, 'mode', 'recurring')
  expect(
    rawCronValue(serializeCronJobDraft(d), ['issue', 'deadline_offset_seconds']),
  ).toBeUndefined()
})
