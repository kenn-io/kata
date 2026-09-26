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

  test('builds Scheduled from start dates and deadlines, one row per issue', () => {
    const view = buildKataTaskView({
      view: 'scheduled',
      issues: [
        issue('issue-1', 'Pay rent', 'project-workspace', { deadline_on: '2026-05-10' }),
        issue('issue-2', 'Started last week', 'project-health', { scheduled_on: '2026-05-08' }),
        issue('issue-3', 'Sign contract', 'project-workspace', { deadline_on: '2026-05-15' }),
        issue('issue-4', 'Tomorrow', 'project-health', { scheduled_on: '2026-05-16' }),
        issue('issue-5', 'Due before start', 'project-workspace', {
          scheduled_on: '2026-05-25',
          deadline_on: '2026-05-20',
        }),
        issue('issue-6', 'Start then due', 'project-workspace', {
          scheduled_on: '2026-05-20',
          deadline_on: '2026-05-30',
        }),
        issue('issue-7', 'Undated', 'project-workspace'),
        issue(
          'issue-8',
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
      ['today', 'Today', ['Sign contract', 'Started last week']],
      ['2026-05-16', '2026-05-16', ['Tomorrow']],
      ['2026-05-20', '2026-05-20', ['Due before start', 'Start then due']],
    ])
  })

  test('orders Overdue and Today by deadline so the most urgent work leads', () => {
    const view = buildKataTaskView({
      view: 'scheduled',
      issues: [
        issue('issue-1', 'A due yesterday', 'project-workspace', { deadline_on: '2026-05-14' }),
        issue('issue-2', 'B due last week', 'project-workspace', { deadline_on: '2026-05-08' }),
        issue('issue-3', 'A started earlier', 'project-health', { scheduled_on: '2026-05-01' }),
        issue('issue-4', 'B started, due later', 'project-health', {
          scheduled_on: '2026-05-02',
          deadline_on: '2026-05-30',
        }),
        issue('issue-5', 'C due today', 'project-workspace', { deadline_on: '2026-05-15' }),
      ],
      projects,
      today,
      fetched_at: fetchedAt,
    })

    expect(view.groups.map((group) => [group.id, group.issues.map((item) => item.title)])).toEqual([
      ['overdue', ['B due last week', 'A due yesterday']],
      ['today', ['C due today', 'B started, due later', 'A started earlier']],
    ])
  })

  test('builds Delegated from open teammate issues grouped by author and teammate', () => {
    const view = buildKataTaskView({
      view: 'delegated',
      issues: [
        {
          ...issue('issue-3', 'Second task', 'project-workspace', { teammate: 'reviewer-7' }),
          author: 'coordinator',
          priority: 2,
        },
        {
          ...issue('issue-1', 'First task', 'project-health', { teammate: 'reviewer-7' }),
          author: 'coordinator',
          priority: 0,
        },
        {
          ...issue('issue-2', 'Another pair', 'project-health', { teammate: 'agent-2' }),
          author: 'another-author',
        },
        issue('issue-4', 'Unassigned', 'project-workspace'),
        issue('issue-5', 'Invalid teammate', 'project-workspace', { teammate: 'reviewer/7' }),
        issue('issue-6', 'Non-string teammate', 'project-workspace', { teammate: 7 }),
        issue(
          'issue-7',
          'Closed delegated task',
          'project-workspace',
          { teammate: 'reviewer-7' },
          'closed',
        ),
      ],
      projects,
      today,
      fetched_at: fetchedAt,
    })

    expect(
      view.groups.map((group) => [group.id, group.title, group.issues.map((item) => item.title)]),
    ).toEqual([
      ['another-author/agent-2', 'another-author/agent-2', ['Another pair']],
      ['coordinator/reviewer-7', 'coordinator/reviewer-7', ['First task', 'Second task']],
    ])
  })

  test('uses the server-projected browser date for timed schedules', () => {
    const previousBrowserDay = {
      ...issue('issue-1', 'Previous browser day', 'project-health', {
        scheduled_on: '2026-09-01T00:30:00Z',
      }),
      scheduled_on_date: '2026-08-31',
    }
    const nextBrowserDay = {
      ...issue('issue-2', 'Next browser day', 'project-workspace', {
        scheduled_on: '2026-09-01T07:30:00Z',
      }),
      scheduled_on_date: '2026-09-01',
    }

    const todayView = buildKataTaskView({
      view: 'today',
      issues: [previousBrowserDay, nextBrowserDay],
      projects,
      today: '2026-08-31',
      fetched_at: fetchedAt,
    })
    expect(todayView.groups.flatMap((group) => group.issues.map((item) => item.title))).toEqual([
      'Previous browser day',
    ])

    const scheduledView = buildKataTaskView({
      view: 'scheduled',
      issues: [previousBrowserDay, nextBrowserDay],
      projects,
      today: '2026-08-31',
      fetched_at: fetchedAt,
    })
    expect(scheduledView.groups.map((group) => [group.id, group.issues[0]?.title])).toEqual([
      ['today', 'Previous browser day'],
      ['2026-09-01', 'Next browser day'],
    ])
  })

  test('uses the server-projected browser date for timed deadlines', () => {
    const previousBrowserDay = {
      ...issue('issue-1', 'Previous browser deadline', 'project-health', {
        deadline_on: '2026-09-01T00:30:00Z',
      }),
      deadline_on_date: '2026-08-31',
    }
    const nextBrowserDay = {
      ...issue('issue-2', 'Next browser deadline', 'project-workspace', {
        deadline_on: '2026-09-01T07:30:00Z',
      }),
      deadline_on_date: '2026-09-01',
    }

    const todayView = buildKataTaskView({
      view: 'today',
      issues: [previousBrowserDay, nextBrowserDay],
      projects,
      today: '2026-08-31',
      fetched_at: fetchedAt,
    })
    expect(todayView.groups.flatMap((group) => group.issues.map((item) => item.title))).toEqual([
      'Previous browser deadline',
    ])

    const scheduledView = buildKataTaskView({
      view: 'scheduled',
      issues: [previousBrowserDay, nextBrowserDay],
      projects,
      today: '2026-08-31',
      fetched_at: fetchedAt,
    })
    expect(scheduledView.groups.map((group) => [group.id, group.issues[0]?.title])).toEqual([
      ['today', 'Previous browser deadline'],
      ['2026-09-01', 'Next browser deadline'],
    ])
  })

  test('builds Inbox from open issues across projects, holding future start dates', () => {
    const view = buildKataTaskView({
      view: 'inbox',
      issues: [
        issue('issue-1', 'Inbox capture', 'project-inbox'),
        issue('issue-2', 'Icon-only project', 'project-later'),
        issue('issue-3', 'Regular work', 'project-workspace'),
        issue('issue-4', 'Starts today', 'project-workspace', { scheduled_on: '2026-05-15' }),
        issue('issue-5', 'Starts tomorrow', 'project-workspace', { scheduled_on: '2026-05-16' }),
        issue('issue-6', 'Due next week', 'project-health', { deadline_on: '2026-05-22' }),
        issue('issue-7', 'Closed', 'project-workspace', {}, 'closed', '2026-05-14T09:00:00.000Z'),
      ],
      projects,
      today,
      fetched_at: fetchedAt,
    })

    expect(view.groups).toHaveLength(1)
    expect(view.groups[0]).toMatchObject({ id: 'inbox', title: 'Inbox' })
    expect(view.groups[0]!.issues.map((item) => item.title).sort()).toEqual([
      'Due next week',
      'Icon-only project',
      'Inbox capture',
      'Regular work',
      'Starts today',
    ])
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
