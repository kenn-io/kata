import { describe, expect, test } from 'vitest'

import { buildKataTaskView } from './view'
import type { KataProjectSummary, KataTaskSummary } from './types'

const today = '2026-05-15'
const fetchedAt = '2026-05-15T16:00:00.000Z'

const projects: KataProjectSummary[] = [
  project('project-inbox', 'Inbox', { role: 'inbox', area: 'Unfiled' }),
  project('project-health', 'Health', { area: 'Personal' }),
  project('project-workspace', 'Work', { area: 'Work' }),
  project('project-later', 'Later', { icon: 'inbox', area: 'Personal' }),
  project('project-named-inbox', 'Inbox', { area: 'Personal' }),
]

function project(
  uid: string,
  name: string,
  metadata: KataProjectSummary['metadata'] = {},
): KataProjectSummary {
  return {
    id: 0,
    uid,
    name,
    metadata,
    open_count: 0,
  }
}

function issue(
  uid: string,
  title: string,
  project_uid: string,
  metadata: KataTaskSummary['metadata'] = {},
  status: KataTaskSummary['status'] = 'open',
  closed_at?: string,
): KataTaskSummary {
  const projectName =
    projects.find((candidate) => candidate.uid === project_uid)?.name ?? project_uid
  return {
    id: Number(uid.replace(/\D/g, '')) || 1,
    uid,
    project_id: 1,
    short_id: uid,
    qualified_id: `${projectName}#${uid}`,
    title,
    status,
    project_uid,
    project_name: projectName,
    metadata,
    revision: 1,
    author: 'user-a',
    created_at: '2026-05-01T12:00:00.000Z',
    updated_at: '2026-05-15T16:00:00.000Z',
    closed_at,
  }
}

describe('kata task view builder', () => {
  test('builds Today from due scheduled dates and overdue unscheduled deadlines', () => {
    const view = buildKataTaskView({
      view: 'today',
      issues: [
        issue('issue-1', 'Deadline slipped', 'project-health', { deadline_on: '2026-05-10' }),
        issue('issue-2', 'Morning review', 'project-workspace', { scheduled_on: '2026-05-15' }),
        issue('issue-3', 'Evening class', 'project-health', {
          scheduled_on: '2026-05-15',
          today_bucket: 'evening',
        }),
        issue('issue-6', 'Future scheduled but overdue', 'project-workspace', {
          scheduled_on: '2026-05-20',
          deadline_on: '2026-05-10',
        }),
        issue('issue-4', 'Future task', 'project-workspace', { scheduled_on: '2026-05-16' }),
        issue(
          'issue-5',
          'Closed task',
          'project-workspace',
          { scheduled_on: '2026-05-15' },
          'closed',
        ),
      ],
      projects,
      today,
      fetched_at: fetchedAt,
    })

    expect(view).toEqual({
      view: 'today',
      fetched_at: fetchedAt,
      groups: [
        {
          id: 'overdue',
          title: 'Overdue',
          issues: [
            expect.objectContaining({ title: 'Deadline slipped' }),
            expect.objectContaining({ title: 'Future scheduled but overdue' }),
          ],
        },
        {
          id: 'today',
          title: 'Today',
          issues: [expect.objectContaining({ title: 'Morning review' })],
        },
        {
          id: 'evening',
          title: 'This evening',
          issues: [expect.objectContaining({ title: 'Evening class' })],
        },
      ],
    })
  })

  test('sorts visible issues by priority before title', () => {
    const view = buildKataTaskView({
      view: 'today',
      issues: [
        {
          ...issue('issue-low', 'Alpha low', 'project-health', { scheduled_on: today }),
          priority: 2,
        },
        {
          ...issue('issue-high', 'Zulu high', 'project-health', { scheduled_on: today }),
          priority: 0,
        },
        issue('issue-none', 'Middle none', 'project-health', { scheduled_on: today }),
      ],
      projects,
      today,
      fetched_at: fetchedAt,
    })

    expect(view.groups[0]!.issues.map((item) => item.title)).toEqual([
      'Zulu high',
      'Alpha low',
      'Middle none',
    ])
  })

  test('builds Upcoming from future scheduled open issues grouped by date', () => {
    const view = buildKataTaskView({
      view: 'upcoming',
      issues: [
        issue('issue-2', 'Next week', 'project-workspace', { scheduled_on: '2026-05-22' }),
        issue('issue-1', 'Tomorrow', 'project-health', { scheduled_on: '2026-05-16' }),
        issue('issue-3', 'Today', 'project-workspace', { scheduled_on: '2026-05-15' }),
      ],
      projects,
      today,
      fetched_at: fetchedAt,
    })

    expect(
      view.groups.map((group) => [group.id, group.title, group.issues.map((item) => item.title)]),
    ).toEqual([
      ['2026-05-16', '2026-05-16', ['Tomorrow']],
      ['2026-05-22', '2026-05-22', ['Next week']],
    ])
  })

  test('builds Inbox only from task inbox projects', () => {
    const view = buildKataTaskView({
      view: 'inbox',
      issues: [
        issue('issue-1', 'Inbox capture', 'project-inbox'),
        issue('issue-2', 'Icon-only project', 'project-later'),
        issue('issue-4', 'Generic project named Inbox', 'project-named-inbox'),
        issue('issue-3', 'Regular work', 'project-workspace'),
      ],
      projects,
      today,
      fetched_at: fetchedAt,
    })

    expect(view.groups).toHaveLength(1)
    expect(view.groups[0]).toMatchObject({ id: 'inbox', title: 'Inbox' })
    expect(view.groups[0]!.issues.map((item) => item.title)).toEqual(['Inbox capture'])
  })

  test('builds All Open from every open issue grouped by project', () => {
    const view = buildKataTaskView({
      view: 'all',
      issues: [
        issue('issue-1', 'Free work', 'project-workspace'),
        issue('issue-2', 'Inbox capture', 'project-inbox'),
        issue('issue-3', 'Scheduled work', 'project-workspace', { scheduled_on: '2026-05-20' }),
        issue('issue-4', 'Free health', 'project-health'),
        issue('issue-5', 'Closed', 'project-workspace', {}, 'closed', '2026-05-14T09:00:00.000Z'),
      ],
      projects,
      today,
      fetched_at: fetchedAt,
    })

    expect(
      view.groups.map((group) => [group.id, group.title, group.issues.map((item) => item.title)]),
    ).toEqual([
      ['project-health', 'Health', ['Free health']],
      ['project-inbox', 'Inbox', ['Inbox capture']],
      ['project-workspace', 'Work', ['Free work', 'Scheduled work']],
    ])
  })

  test('builds Deadlines from open issues with deadlines, pinning Overdue and Today', () => {
    const view = buildKataTaskView({
      view: 'deadlines',
      issues: [
        issue('issue-1', 'Pay rent', 'project-workspace', { deadline_on: '2026-05-10' }),
        issue('issue-2', 'Sign contract', 'project-workspace', { deadline_on: '2026-05-15' }),
        issue('issue-3', 'File taxes', 'project-health', { deadline_on: '2026-05-20' }),
        issue('issue-4', 'Renew domain', 'project-health', { deadline_on: '2026-05-22' }),
        issue('issue-5', 'No deadline', 'project-workspace'),
        issue(
          'issue-6',
          'Closed deadline',
          'project-workspace',
          { deadline_on: '2026-05-10' },
          'closed',
          '2026-05-09T00:00:00.000Z',
        ),
      ],
      projects,
      today,
      fetched_at: fetchedAt,
    })

    expect(
      view.groups.map((group) => [group.id, group.title, group.issues.map((item) => item.title)]),
    ).toEqual([
      ['overdue', 'Overdue', ['Pay rent']],
      ['today', 'Today', ['Sign contract']],
      ['2026-05-20', '2026-05-20', ['File taxes']],
      ['2026-05-22', '2026-05-22', ['Renew domain']],
    ])
  })

  test('builds Logbook from closed issues grouped by closed_at day', () => {
    const view = buildKataTaskView({
      view: 'logbook',
      issues: [
        issue(
          'issue-2',
          'Done later',
          'project-workspace',
          {},
          'closed',
          '2026-05-15T15:00:00.000Z',
        ),
        issue(
          'issue-1',
          'Done earlier',
          'project-health',
          {},
          'closed',
          '2026-05-14T09:00:00.000Z',
        ),
        issue('issue-3', 'Still open', 'project-workspace'),
      ],
      projects,
      today,
      fetched_at: fetchedAt,
    })

    expect(
      view.groups.map((group) => [group.id, group.title, group.issues.map((item) => item.title)]),
    ).toEqual([
      ['2026-05-15', '2026-05-15', ['Done later']],
      ['2026-05-14', '2026-05-14', ['Done earlier']],
    ])
  })
})
