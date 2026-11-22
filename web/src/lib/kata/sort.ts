import type { KataTaskSummary } from './types'
import { deadlineDate } from './view'

export type KataTaskSortKey = 'priority' | 'title' | 'updated' | 'owner' | 'id' | 'due'
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
  due: 'asc',
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
  childrenOf?: (issue: KataTaskSummary) => readonly KataTaskSummary[],
): KataTaskSummary[] {
  const dueDate = dueDateLookup(childrenOf)
  return [...issues].sort((a, b) => compareWithDueDate(a, b, sort, dueDate))
}

function dueDateLookup(childrenOf?: (issue: KataTaskSummary) => readonly KataTaskSummary[]) {
  const dates = new Map<KataTaskSummary, string | undefined>()
  return (issue: KataTaskSummary): string | undefined => {
    if (dates.has(issue)) return dates.get(issue)
    const seen = new Set<string>()
    const visit = (item: KataTaskSummary): string | undefined => {
      if (seen.has(item.uid)) return undefined
      seen.add(item.uid)
      let earliest = deadlineDate(item)
      for (const child of childrenOf?.(item) ?? []) {
        const date = visit(child)
        if (date && (!earliest || date < earliest)) earliest = date
      }
      return earliest
    }
    const date = visit(issue)
    dates.set(issue, date)
    return date
  }
}

export function compareKataTasks(
  a: KataTaskSummary,
  b: KataTaskSummary,
  sort: KataTaskSort,
  childrenOf?: (issue: KataTaskSummary) => readonly KataTaskSummary[],
): number {
  return compareWithDueDate(a, b, sort, dueDateLookup(childrenOf))
}

function compareWithDueDate(
  a: KataTaskSummary,
  b: KataTaskSummary,
  sort: KataTaskSort,
  dueDate: (issue: KataTaskSummary) => string | undefined,
): number {
  const missing = compareMissing(a, b, sort.key, dueDate)
  if (missing !== 0) return missing
  const primary =
    sort.key === 'due'
      ? (dueDate(a) ?? '').localeCompare(dueDate(b) ?? '')
      : compareKey(a, b, sort.key)
  const ordered = sort.direction === 'asc' ? primary : -primary
  if (ordered !== 0) return ordered
  if (sort.key !== 'updated') {
    const updated = compareKey(a, b, 'updated')
    if (updated !== 0) return -updated
  }
  return a.uid.localeCompare(b.uid)
}

function compareMissing(
  a: KataTaskSummary,
  b: KataTaskSummary,
  key: KataTaskSortKey,
  dueDate: (issue: KataTaskSummary) => string | undefined,
): number {
  if (key === 'priority') {
    const am = a.priority === undefined
    const bm = b.priority === undefined
    if (am && bm) return 0
    if (am) return 1
    if (bm) return -1
  } else if (key === 'owner' || key === 'due') {
    const am = key === 'owner' ? !a.owner : !dueDate(a)
    const bm = key === 'owner' ? !b.owner : !dueDate(b)
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
    case 'due':
      return (deadlineDate(a) ?? '').localeCompare(deadlineDate(b) ?? '')
  }
}
