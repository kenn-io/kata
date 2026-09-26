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
      issues: [issue('dated', { deadline_on: '2026-05-20' }), issue('undated')],
      today: '2026-05-15',
    })

    expect(titles(view)).toEqual(['dated'])
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
