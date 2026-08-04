import type { KataTaskSummary } from './types'

export type KataTaskSortKey = 'priority' | 'title' | 'updated' | 'owner' | 'id'
export type KataTaskSortDirection = 'asc' | 'desc'

export interface KataTaskSort {
  key: KataTaskSortKey
  direction: KataTaskSortDirection
}

export const KATA_TASK_NATURAL_DIRECTION: Record<KataTaskSortKey, KataTaskSortDirection> = {
  priority: 'asc',
  title: 'asc',
  updated: 'desc',
  owner: 'asc',
  id: 'asc',
}

export const DEFAULT_KATA_TASK_SORT: KataTaskSort = {
  key: 'updated',
  direction: KATA_TASK_NATURAL_DIRECTION.updated,
}

export function toggleKataTaskSort(current: KataTaskSort, key: KataTaskSortKey): KataTaskSort {
  if (current.key === key) {
    return { key, direction: current.direction === 'asc' ? 'desc' : 'asc' }
  }
  return { key, direction: KATA_TASK_NATURAL_DIRECTION[key] }
}

export function sortKataTasks(
  issues: readonly KataTaskSummary[],
  sort: KataTaskSort,
): KataTaskSummary[] {
  return [...issues].sort((a, b) => compareKataTasks(a, b, sort))
}

export function compareKataTasks(
  a: KataTaskSummary,
  b: KataTaskSummary,
  sort: KataTaskSort,
): number {
  const missing = compareMissing(a, b, sort.key)
  if (missing !== 0) return missing
  const primary = compareKey(a, b, sort.key)
  const ordered = sort.direction === 'asc' ? primary : -primary
  if (ordered !== 0) return ordered
  if (sort.key !== 'updated') {
    const updated = compareKey(a, b, 'updated')
    if (updated !== 0) return -updated
  }
  return a.uid.localeCompare(b.uid)
}

function compareMissing(a: KataTaskSummary, b: KataTaskSummary, key: KataTaskSortKey): number {
  if (key === 'priority') {
    const am = a.priority === undefined
    const bm = b.priority === undefined
    if (am && bm) return 0
    if (am) return 1
    if (bm) return -1
  } else if (key === 'owner') {
    const am = !a.owner
    const bm = !b.owner
    if (am && bm) return 0
    if (am) return 1
    if (bm) return -1
  }
  return 0
}

function compareKey(a: KataTaskSummary, b: KataTaskSummary, key: KataTaskSortKey): number {
  switch (key) {
    case 'priority':
      return (a.priority ?? 0) - (b.priority ?? 0)
    case 'title':
      return a.title.localeCompare(b.title, undefined, { sensitivity: 'base' })
    case 'updated': {
      const at = Date.parse(a.updated_at)
      const bt = Date.parse(b.updated_at)
      const av = Number.isFinite(at) ? at : Number.NEGATIVE_INFINITY
      const bv = Number.isFinite(bt) ? bt : Number.NEGATIVE_INFINITY
      if (av === bv) return 0
      return av < bv ? -1 : 1
    }
    case 'owner':
      return (a.owner ?? '').localeCompare(b.owner ?? '', undefined, { sensitivity: 'base' })
    case 'id':
      return a.qualified_id.localeCompare(b.qualified_id, undefined, { numeric: true })
  }
}
