import type { KataRecurrence } from '../../kata/types'

export const recurrence = {
  id: 7,
  uid: 'rec-uid',
  project_id: 1,
  rrule: 'FREQ=WEEKLY;INTERVAL=1;BYDAY=MO',
  dtstart: '2026-05-20',
  timezone: 'America/Chicago',
  template_title: 'Weekly review',
  template_body: 'Body',
  template_owner: 'user-a',
  template_priority: 2,
  template_labels: ['routine'],
  template_metadata: { k: 'v' },
  next_occurrence_key: '2026-05-27',
  author: 'user-a',
  revision: 1,
  created_at: '2026-05-15T12:00:00.000Z',
  updated_at: '2026-05-15T12:00:00.000Z',
} satisfies KataRecurrence
