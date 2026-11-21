import { describe, expect, test } from 'vitest'

import {
  defaultKataTaskSearchFilters,
  deriveKataAreas,
  projectKataWorkspaceView,
} from './authority'
import type { KataProjectSummary, KataTaskSummary } from './types'

const projects: KataProjectSummary[] = [
  { id: 1, uid: 'project-work', name: 'example-project', metadata: {}, open_count: 2 },
]

function issue(uid: string, metadata: KataTaskSummary['metadata'] = {}): KataTaskSummary {
  return {
    id: 1,
    uid,
    project_id: 1,
    short_id: uid,
    qualified_id: `example-project#${uid}`,
    title: uid,
    status: 'open',
    project_uid: 'project-work',
    project_name: 'example-project',
    metadata,
    revision: 1,
    author: 'user-a',
    created_at: '2026-05-01T12:00:00.000Z',
    updated_at: '2026-05-15T16:00:00.000Z',
  }
}

function titles(view: ReturnType<typeof projectKataWorkspaceView>): string[] {
  return view.groups.flatMap((group) => group.issues.map((item) => item.title))
}

describe('projectKataWorkspaceView', () => {
  test('keeps filtered Scheduled results to dated issues', () => {
    const view = projectKataWorkspaceView({
      view: 'scheduled',
      filters: { ...defaultKataTaskSearchFilters('scheduled'), owner: 'user-a' },
      snapshot: { projects, fetched_at: '2026-05-15T16:00:00.000Z' },
      issues: [
        issue('first-date', { deadline_on: '2026-05-20' }),
        issue('second-date', { scheduled_on: '2026-05-21' }),
        issue('undated'),
      ],
      today: '2026-05-15',
    })

    expect(view.groups.map((group) => [group.title, group.issues.map((item) => item.title)])).toEqual(
      [
        ['Wed, May 20', ['first-date']],
        ['Thu, May 21', ['second-date']],
      ],
    )
  })

  test('keeps Today date groups when the task set is narrowed by search', () => {
    const view = projectKataWorkspaceView({
      view: 'today',
      filters: { ...defaultKataTaskSearchFilters('today'), query: 'alert' },
      snapshot: { projects, fetched_at: '2026-05-15T16:00:00.000Z' },
      issues: [
        issue('late', { deadline_on: '2026-05-14' }),
        issue('today', { deadline_on: '2026-05-15' }),
      ],
      today: '2026-05-15',
    })

    expect(view.groups.map((group) => [group.title, group.issues.map((item) => item.title)])).toEqual(
      [
        ['Overdue', ['late']],
        ['Today', ['today']],
      ],
    )
  })

  test('keeps Delegated handoff groups when narrowed by a relationship filter', () => {
    const view = projectKataWorkspaceView({
      view: 'delegated',
      filters: {
        ...defaultKataTaskSearchFilters('delegated'),
        relationships: ['blocks'],
      },
      snapshot: { projects, fetched_at: '2026-05-15T16:00:00.000Z' },
      issues: [issue('handoff', { teammate: 'reviewer-7' })],
      today: '2026-05-15',
    })

    expect(view.groups.map((group) => [group.title, group.issues.map((item) => item.title)])).toEqual(
      [['reviewer-7 for user-a', ['handoff']]],
    )
  })

  test('keeps Scheduled groups for Ready status', () => {
    const view = projectKataWorkspaceView({
      view: 'scheduled',
      filters: { ...defaultKataTaskSearchFilters('scheduled'), status: 'ready' },
      snapshot: { projects, fetched_at: '2026-05-15T16:00:00.000Z' },
      issues: [issue('ready', { scheduled_on: '2026-05-20' })],
      today: '2026-05-15',
    })

    expect(view.groups.map((group) => [group.title, group.issues.map((item) => item.title)])).toEqual(
      [['Wed, May 20', ['ready']]],
    )
  })

  test.each([
    ['closed', [{ ...issue('closed-task'), status: 'closed' as const }], ['closed-task']],
    [
      'all',
      [issue('open-task'), { ...issue('closed-task'), status: 'closed' as const }],
      ['open-task', 'closed-task'],
    ],
  ] as const)('keeps Results when Today expands to %s tasks', (status, issues, expected) => {
    const view = projectKataWorkspaceView({
      view: 'today',
      filters: { ...defaultKataTaskSearchFilters('today'), status },
      snapshot: { projects, fetched_at: '2026-05-15T16:00:00.000Z' },
      issues,
      today: '2026-05-15',
    })

    expect(view.groups.map((group) => [group.title, group.issues.map((item) => item.title)])).toEqual(
      [['Results', expected]],
    )
  })

  test('holds future start dates out of filtered Inbox results', () => {
    const view = projectKataWorkspaceView({
      view: 'inbox',
      filters: { ...defaultKataTaskSearchFilters('inbox'), owner: 'user-a' },
      snapshot: { projects, fetched_at: '2026-05-15T16:00:00.000Z' },
      issues: [
        issue('now', { scheduled_on: '2026-05-15' }),
        issue('later', { scheduled_on: '2026-05-16' }),
      ],
      today: '2026-05-15',
    })

    expect(titles(view)).toEqual(['now'])
  })
})

describe('deriveKataAreas', () => {
  test('groups projects without an area under Projects, including the capture Inbox', () => {
    const areas = deriveKataAreas([
      {
        id: 1,
        uid: 'p-work',
        name: 'example-workspace',
        metadata: { area: 'Work' },
        open_count: 0,
      },
      { id: 2, uid: 'p-plain', name: 'example-project', metadata: {}, open_count: 0 },
      {
        id: 3,
        uid: 'p-legacy',
        name: 'legacy-project',
        metadata: { area: 'Unfiled' },
        open_count: 0,
      },
      {
        id: 4,
        uid: 'p-inbox',
        name: 'capture-project',
        metadata: { role: 'inbox' },
        open_count: 0,
      },
    ])

    expect(areas.map((area) => [area.name, area.projects.map((project) => project.name)])).toEqual([
      ['Work', ['example-workspace']],
      ['Projects', ['capture-project', 'example-project', 'legacy-project']],
    ])
  })
})
