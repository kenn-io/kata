import { describe, expect, it } from 'vitest'
import { previewDates } from './rrule'

describe('previewDates', () => {
  it.each([
    ['FREQ=WEEKLY', '2026-11-22', ['2026-11-22', '2026-11-29', '2026-12-06', '2026-12-13']],
    ['FREQ=MONTHLY', '2026-01-31', ['2026-01-31', '2026-03-31', '2026-05-31', '2026-07-31']],
    ['FREQ=YEARLY', '2024-02-29', ['2024-02-29', '2028-02-29', '2032-02-29', '2036-02-29']],
    ['FREQ=DAILY;COUNT=2', '2026-05-15', ['2026-05-15', '2026-05-16']],
    ['FREQ=DAILY;UNTIL=20260517T000000Z', '2026-05-15', ['2026-05-15', '2026-05-16', '2026-05-17']],
    [
      'FREQ=WEEKLY;BYDAY=MO',
      '2026-05-20',
      ['2026-05-25', '2026-06-01', '2026-06-08', '2026-06-15'],
    ],
  ])('previews %s', (rule, start, expected) =>
    expect(previewDates(rule, start, 'UTC')).toEqual(expected),
  )
  it('interprets a date-only UNTIL as UTC in the rule timezone', () => {
    expect(previewDates('FREQ=DAILY;UNTIL=20260517', '2026-05-15', 'America/Los_Angeles')).toEqual([
      '2026-05-15',
      '2026-05-16',
    ])
  })
})
