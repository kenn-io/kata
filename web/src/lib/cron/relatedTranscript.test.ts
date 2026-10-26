import { expect, test } from 'vitest'
import * as hegel from '@hegeldev/hegel'
import * as gs from '@hegeldev/hegel/generators'
import type { KataTaskEvent } from '../kata/types'
import { relatedCronTranscriptURL } from './relatedTranscript'

const projectUID = '01J00000000000000000000002'
const issueUID = '01J00000000000000000000001'
const safe = 'https://agentsview.example/sessions/abc'

function closed(
  eventID: number,
  transcript: unknown,
  overrides: Partial<KataTaskEvent> = {},
): KataTaskEvent {
  return {
    event_id: eventID,
    event_uid: `event-${eventID}`,
    origin_instance_uid: 'instance',
    type: 'issue.closed',
    project_id: 7,
    project_uid: projectUID,
    project_name: 'example-project',
    issue_uid: issueUID,
    actor: 'worker',
    created_at: '2026-10-25T10:00:00Z',
    payload: { reason: 'done', transcript },
    ...overrides,
  } as KataTaskEvent
}

test('AgentsViewCron returns nothing while disabled or unrelated to an issue', () => {
  const events = [closed(1, { url: safe })]
  expect(relatedCronTranscriptURL(false, [projectUID], issueUID, events)).toBeUndefined()
  expect(relatedCronTranscriptURL(true, [projectUID], undefined, events)).toBeUndefined()
  expect(relatedCronTranscriptURL(true, [projectUID], issueUID, events)).toBe(safe)
})

test('AgentsViewCron matches only the exact project and subject issue close', () => {
  const related = closed(
    9,
    { url: 'https://agentsview.example/sessions/related' },
    {
      issue_uid: '01J00000000000000000000009',
      related_issue_uid: issueUID,
    },
  )
  const otherProject = closed(
    8,
    { url: 'https://agentsview.example/sessions/project' },
    {
      project_uid: '01J00000000000000000000008',
    },
  )
  const comment = closed(
    7,
    { url: 'https://agentsview.example/sessions/comment' },
    {
      type: 'issue.commented',
    },
  )
  expect(
    relatedCronTranscriptURL(true, [projectUID], issueUID, [related, otherProject, comment]),
  ).toBeUndefined()
  expect(
    relatedCronTranscriptURL(true, [projectUID], issueUID, [
      related,
      otherProject,
      comment,
      closed(2, { url: safe }),
    ]),
  ).toBe(safe)
})

test('AgentsViewCron follows a moved issue into its current project', () => {
  const currentProject = '01J00000000000000000000003'
  const before = closed(4, { url: 'https://agentsview.example/sessions/before' })
  const after = closed(
    6,
    { url: 'https://agentsview.example/sessions/after' },
    {
      project_uid: currentProject,
    },
  )
  expect(relatedCronTranscriptURL(true, [projectUID], issueUID, [after, before])).toBe(
    'https://agentsview.example/sessions/before',
  )
  expect(
    relatedCronTranscriptURL(true, [projectUID, currentProject], issueUID, [after, before]),
  ).toBe('https://agentsview.example/sessions/after')
  expect(relatedCronTranscriptURL(true, [], issueUID, [after, before])).toBeUndefined()
})

test('AgentsViewCron chooses the greatest event ID with a safe locator', () => {
  const events = [
    closed(5, { url: 'https://agentsview.example/sessions/five' }),
    closed(11, { url: 'https://agentsview.example/sessions/eleven' }),
    closed(13, { url: 'javascript:alert(1)' }),
    closed(3, { url: 'https://agentsview.example/sessions/three' }),
  ]
  expect(relatedCronTranscriptURL(true, [projectUID], issueUID, events)).toBe(
    'https://agentsview.example/sessions/eleven',
  )
})

test.each([
  ['missing payload', undefined],
  ['string transcript', 'https://agentsview.example/sessions/abc'],
  ['array transcript', [{ url: safe }]],
  ['null transcript', null],
  ['numeric url', { url: 42 }],
  ['missing url', { agent: 'codex', session_id: '00000000-0000-4000-8000-000000000001' }],
])('AgentsViewCron ignores malformed transcript: %s', (_name, transcript) => {
  const event =
    transcript === undefined ? closed(1, undefined, { payload: undefined }) : closed(1, transcript)
  expect(relatedCronTranscriptURL(true, [projectUID], issueUID, [event])).toBeUndefined()
})

test.each([
  'agentsview.example/sessions/abc',
  '/sessions/abc',
  '//agentsview.example/sessions/abc',
  'ftp://agentsview.example/sessions/abc',
  'javascript:alert(1)',
  'data:text/html,hello',
  'http:/agentsview.example/sessions/abc',
  'https://',
  'https://user:secret@agentsview.example/sessions/abc',
  'https://user@agentsview.example/sessions/abc',
  'https://@agentsview.example/sessions/abc',
  'https://agentsview.example/sessions/abc?token=secret',
  'https://agentsview.example/sessions/abc?',
  'https://agentsview.example/sessions/abc#top',
  'https://agentsview.example/sessions/abc#',
  'https://agentsview.example/sessions/a\u0000c',
  'https://agentsview.example/sessions/a\u007fc',
  'https://agentsview.example/sessions/a c',
  'https://agentsview.example/sessions/abc\n',
  ' https://agentsview.example/sessions/abc',
  'https://agentsview.example/sessions/a\u00a0c',
  'https://agentsview.example\\sessions\\abc',
])('AgentsViewCron rejects unsafe locator %j', (url) => {
  expect(
    relatedCronTranscriptURL(true, [projectUID], issueUID, [closed(1, { url })]),
  ).toBeUndefined()
})

test.each([
  'https://agentsview.example/sessions/abc',
  'http://127.0.0.1:8080/sessions/abc',
  'https://AgentsView.Example/Base%2Fpath/sessions/Codex:Session%3A42',
  'https://agentsview.example/base/sessions/provider/opaque~id_42',
])('AgentsViewCron preserves accepted locator bytes %j', (url) => {
  expect(relatedCronTranscriptURL(true, [projectUID], issueUID, [closed(1, { url })])).toBe(url)
})

test('AgentsViewCron accepts URL-only and non-native recorded transcripts', () => {
  for (const transcript of [
    { url: safe },
    { agent: 'future-agent', session_id: 'not-a-uuid', url: safe },
    { agent: 'codex', session_id: '00000000-0000-4000-8000-000000000001', url: safe },
  ])
    expect(relatedCronTranscriptURL(true, [projectUID], issueUID, [closed(1, transcript)])).toBe(
      safe,
    )
})

test('AgentsViewCron selection is order independent and never returns an unsafe locator', () =>
  hegel.test(
    (tc) => {
      const count = tc.draw(gs.integers({ minValue: 0, maxValue: 8 }))
      const events: KataTaskEvent[] = []
      for (let index = 0; index < count; index++) {
        const id = tc.draw(gs.integers({ minValue: 1, maxValue: 1_000_000 }))
        // Event IDs are unique within one daemon's history.
        if (events.some((event) => event.event_id === id)) continue
        const url = tc.draw(
          gs.sampledFrom([
            `https://agentsview.example/sessions/${id}`,
            `https://agentsview.example/sessions/${id}?q`,
            `ftp://agentsview.example/${id}`,
            tc.draw(gs.text({ codec: 'utf-8' })),
          ]),
        )
        events.push(closed(id, { url }, tc.draw(gs.booleans()) ? {} : { issue_uid: 'other-issue' }))
      }
      const result = relatedCronTranscriptURL(true, [projectUID], issueUID, events)
      const reversed = relatedCronTranscriptURL(true, [projectUID], issueUID, [...events].reverse())
      expect(reversed).toBe(result)
      if (result === undefined) return
      expect(result).toMatch(/^https?:\/\/[^/?#@\s]+/)
      expect(result).not.toMatch(/[?#\s\\]/)
      const chosen = events.filter(
        (event) =>
          event.issue_uid === issueUID &&
          (event.payload?.transcript as { url: string }).url === result,
      )
      expect(chosen.length).toBeGreaterThan(0)
    },
    { testCases: 500 },
  ))
