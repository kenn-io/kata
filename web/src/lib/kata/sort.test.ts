import { describe, expect, test } from 'vitest'

import type { KataTaskSummary } from './types'
import {
  DEFAULT_KATA_TASK_SORT,
  sortKataTasks,
  toggleKataTaskSort,
  type KataTaskSort,
} from './sort'

function task(overrides: Partial<KataTaskSummary>): KataTaskSummary {
  return {
    id: 1,
    uid: 'task-a',
    project_id: 1,
    short_id: 'a',
    qualified_id: 'Roadmap#a',
    title: 'Alpha',
    status: 'open',
    project_uid: 'project-roadmap',
    project_name: 'Roadmap',
    metadata: {},
    revision: 1,
    author: 'user-a',
    created_at: '2026-05-01T00:00:00Z',
    updated_at: '2026-05-01T00:00:00Z',
    ...overrides,
  }
}

describe('kata task sorting', () => {
  test('defaults to recently updated first and toggles the active key', () => {
    expect(DEFAULT_KATA_TASK_SORT).toEqual({ key: 'updated', direction: 'desc' })
    expect(toggleKataTaskSort(DEFAULT_KATA_TASK_SORT, 'updated')).toEqual({
      key: 'updated',
      direction: 'asc',
    })
    expect(toggleKataTaskSort(DEFAULT_KATA_TASK_SORT, 'priority')).toEqual({
      key: 'priority',
      direction: 'asc',
    })
  })

  test('keeps missing priority and owner values last in either direction', () => {
    const unassigned = task({ uid: 'unassigned', priority: undefined, owner: undefined })
    const ownedLow = task({ uid: 'owned-low', priority: 3, owner: 'zephyr' })
    const ownedHigh = task({ uid: 'owned-high', priority: 0, owner: 'ada' })

    expect(
      sortKataTasks([unassigned, ownedLow, ownedHigh], { key: 'priority', direction: 'asc' }).map(
        (issue) => issue.uid,
      ),
    ).toEqual(['owned-high', 'owned-low', 'unassigned'])
    expect(
      sortKataTasks([unassigned, ownedLow, ownedHigh], { key: 'priority', direction: 'desc' }).map(
        (issue) => issue.uid,
      ),
    ).toEqual(['owned-low', 'owned-high', 'unassigned'])

    const ownerSort: KataTaskSort = { key: 'owner', direction: 'desc' }
    expect(
      sortKataTasks([unassigned, ownedLow, ownedHigh], ownerSort).map((issue) => issue.uid),
    ).toEqual(['owned-low', 'owned-high', 'unassigned'])
  })

  test('sorts updated timestamps by parsed time and falls back to uid ties', () => {
    const oldUTC = task({ uid: 'b', updated_at: '2026-05-02T10:00:00Z' })
    const sameInstantOffset = task({ uid: 'a', updated_at: '2026-05-02T05:00:00-05:00' })
    const newest = task({ uid: 'c', updated_at: '2026-05-03T09:00:00Z' })

    expect(
      sortKataTasks([oldUTC, newest, sameInstantOffset], { key: 'updated', direction: 'desc' }).map(
        (issue) => issue.uid,
      ),
    ).toEqual(['c', 'a', 'b'])
  })

  test('sorts titles case-insensitively with recency as a tiebreaker', () => {
    const older = task({ uid: 'older', title: 'Launch', updated_at: '2026-05-01T00:00:00Z' })
    const newer = task({ uid: 'newer', title: 'launch', updated_at: '2026-05-03T00:00:00Z' })
    const alpha = task({ uid: 'alpha', title: 'Alpha', updated_at: '2026-05-02T00:00:00Z' })

    expect(
      sortKataTasks([older, alpha, newer], { key: 'title', direction: 'asc' }).map(
        (issue) => issue.uid,
      ),
    ).toEqual(['alpha', 'newer', 'older'])
  })
})
