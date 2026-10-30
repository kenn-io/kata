import type { KataOpenBlocker } from './types'

export const KATA_TASK_COLUMNS_STORAGE_KEY = 'kata:issue-columns/v1'

export const KATA_OPTIONAL_TASK_COLUMNS = [
  { id: 'updated', label: 'Updated' },
  { id: 'priority', label: 'Priority' },
  { id: 'due', label: 'Due' },
  { id: 'owner', label: 'Owner' },
  { id: 'blockedBy', label: 'Blocked by' },
  { id: 'tags', label: 'Tags' },
] as const

export type KataOptionalTaskColumn = (typeof KATA_OPTIONAL_TASK_COLUMNS)[number]['id']
export type KataTaskColumnVisibility = Record<KataOptionalTaskColumn, boolean>

const BLOCKED_BY_VISIBLE_IDS = 3

export interface BlockedBySummary {
  /** Cell text: up to three IDs, then " +N" for the rest. */
  text: string
  /** Every open blocker, for the hover title. */
  title: string
}

export function summarizeBlockedBy(
  blockers: readonly KataOpenBlocker[] | undefined,
  qualified: boolean,
): BlockedBySummary {
  const ids = (blockers ?? []).map((blocker) =>
    qualified ? blocker.qualified_id : blocker.short_id,
  )
  const more = ids.length - BLOCKED_BY_VISIBLE_IDS
  const shown = ids.slice(0, BLOCKED_BY_VISIBLE_IDS).join(', ')
  return { text: more > 0 ? `${shown} +${more}` : shown, title: ids.join(', ') }
}

type ColumnStorage = Pick<Storage, 'getItem' | 'setItem'>

const knownColumns = new Set<string>(KATA_OPTIONAL_TASK_COLUMNS.map((column) => column.id))

export function defaultKataTaskColumnVisibility(): KataTaskColumnVisibility {
  return { updated: true, priority: true, due: true, owner: true, blockedBy: false, tags: true }
}

function browserStorage(): ColumnStorage | null {
  if (typeof window === 'undefined') return null
  try {
    return window.localStorage
  } catch {
    return null
  }
}

export function loadKataTaskColumnVisibility(
  storage: ColumnStorage | null = browserStorage(),
): KataTaskColumnVisibility {
  if (!storage) return defaultKataTaskColumnVisibility()
  try {
    const raw = storage.getItem(KATA_TASK_COLUMNS_STORAGE_KEY)
    if (raw === null) return defaultKataTaskColumnVisibility()
    const parsed: unknown = JSON.parse(raw)
    if (!Array.isArray(parsed) || !parsed.every((value) => typeof value === 'string')) {
      return defaultKataTaskColumnVisibility()
    }
    const visible = new Set(parsed.filter((value) => knownColumns.has(value)))
    return Object.fromEntries(
      KATA_OPTIONAL_TASK_COLUMNS.map((column) => [column.id, visible.has(column.id)]),
    ) as KataTaskColumnVisibility
  } catch {
    return defaultKataTaskColumnVisibility()
  }
}

export function persistKataTaskColumnVisibility(
  visibility: KataTaskColumnVisibility,
  storage: ColumnStorage | null = browserStorage(),
): void {
  if (!storage) return
  try {
    const visible = KATA_OPTIONAL_TASK_COLUMNS.filter((column) => visibility[column.id]).map(
      (column) => column.id,
    )
    storage.setItem(KATA_TASK_COLUMNS_STORAGE_KEY, JSON.stringify(visible))
  } catch {
    // Browser storage is best-effort. Keep the in-memory preference usable.
  }
}
