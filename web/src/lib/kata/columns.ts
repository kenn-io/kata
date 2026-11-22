import type { KataOpenBlocker } from './types'

export const KATA_TASK_COLUMNS_STORAGE_KEY = 'kata:issue-columns/v2'
export const KATA_TASK_COLUMN_WIDTHS_STORAGE_KEY = 'kata:column-widths/v1'

export const KATA_OPTIONAL_TASK_COLUMNS = [
  { id: 'id', label: 'ID' },
  { id: 'project', label: 'Project' },
  { id: 'due', label: 'Due' },
  { id: 'updated', label: 'Updated' },
  { id: 'priority', label: 'Priority' },
  { id: 'owner', label: 'Owner' },
  { id: 'blockedBy', label: 'Blocked by' },
  { id: 'tags', label: 'Labels' },
] as const

export type KataOptionalTaskColumn = (typeof KATA_OPTIONAL_TASK_COLUMNS)[number]['id']
export type KataTaskColumnVisibility = Record<KataOptionalTaskColumn, boolean>
export type KataTaskColumnWidths = Partial<
  Record<KataOptionalTaskColumn | 'date' | 'closed', number>
>

const BLOCKED_BY_VISIBLE_IDS = 3

export interface BlockedBySummary {
  /** Up to three IDs, comma separated. */
  ids: string
  /** "+N" for blockers beyond the first three, else empty. */
  more: string
  /** Every open blocker with its qualified ID, for the hover title. */
  title: string
}

function projectOf(qualifiedID: string): string {
  return qualifiedID.slice(0, qualifiedID.lastIndexOf('#'))
}

/** Blockers from the row's own project show the short ID; others stay qualified. */
export function summarizeBlockedBy(
  blockers: readonly KataOpenBlocker[] | undefined,
  rowQualifiedID: string,
): BlockedBySummary {
  const all = blockers ?? []
  const rowProject = projectOf(rowQualifiedID)
  const shown = all
    .slice(0, BLOCKED_BY_VISIBLE_IDS)
    .map((blocker) =>
      projectOf(blocker.qualified_id) === rowProject ? blocker.short_id : blocker.qualified_id,
    )
  const more = all.length - BLOCKED_BY_VISIBLE_IDS
  return {
    ids: shown.join(', '),
    more: more > 0 ? `+${more}` : '',
    title: all.map((blocker) => blocker.qualified_id).join(', '),
  }
}

type ColumnStorage = Pick<Storage, 'getItem' | 'setItem'>

const knownColumns = new Set<string>(KATA_OPTIONAL_TASK_COLUMNS.map((column) => column.id))
const widthColumns = new Set([...knownColumns, 'date', 'closed'])

export function defaultKataTaskColumnVisibility(): KataTaskColumnVisibility {
  return {
    id: false,
    project: true,
    updated: true,
    priority: true,
    due: true,
    owner: true,
    blockedBy: false,
    tags: true,
  }
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
    const current = storage.getItem(KATA_TASK_COLUMNS_STORAGE_KEY)
    const raw = current ?? storage.getItem('kata:issue-columns/v1')
    if (raw === null) return defaultKataTaskColumnVisibility()
    const parsed: unknown = JSON.parse(raw)
    if (!Array.isArray(parsed) || !parsed.every((value) => typeof value === 'string')) {
      return defaultKataTaskColumnVisibility()
    }
    const visible = new Set(parsed.filter((value) => knownColumns.has(value)))
    if (current === null) {
      visible.add('project')
    }
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

export function clampColumnWidth(width: number): number {
  return Math.round(Math.max(48, Math.min(480, width)))
}

export function loadKataTaskColumnWidths(
  storage: ColumnStorage | null = browserStorage(),
): KataTaskColumnWidths {
  if (!storage) return {}
  try {
    const raw = storage.getItem(KATA_TASK_COLUMN_WIDTHS_STORAGE_KEY)
    if (raw === null) return {}
    const parsed: unknown = JSON.parse(raw)
    if (parsed === null || typeof parsed !== 'object' || Array.isArray(parsed)) return {}
    return Object.fromEntries(
      Object.entries(parsed).flatMap(([key, value]) =>
        widthColumns.has(key) && typeof value === 'number' && Number.isFinite(value)
          ? [[key, clampColumnWidth(value)]]
          : [],
      ),
    ) as KataTaskColumnWidths
  } catch {
    return {}
  }
}

export function persistKataTaskColumnWidths(
  widths: KataTaskColumnWidths,
  storage: ColumnStorage | null = browserStorage(),
): void {
  if (!storage) return
  try {
    storage.setItem(KATA_TASK_COLUMN_WIDTHS_STORAGE_KEY, JSON.stringify(widths))
  } catch {
    // Browser storage is best-effort. Keep the in-memory preference usable.
  }
}
