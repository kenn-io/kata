export const preferencesStorageKey = 'kata.preferences.v1'

export interface Preferences {
  theme: 'system' | 'light' | 'dark'
  columns: string[]
  splitDirection: 'horizontal' | 'vertical'
  splitSize: number
  collapsedGroups: string[]
  /** Board cards show one line each. */
  boardCompact: boolean
  /** Board column IDs (such as `done` or `label:review`) shown as narrow strips. */
  collapsedBoardColumns: string[]
  /** Flash board cards that someone else moved or added. */
  boardHighlightRemote: boolean
}

export const defaultPreferences: Preferences = {
  theme: 'system',
  columns: ['status', 'title'],
  splitDirection: 'horizontal',
  splitSize: 480,
  collapsedGroups: [],
  boardCompact: false,
  collapsedBoardColumns: [],
  boardHighlightRemote: true,
}

export function loadPreferences(storage: Storage = localStorage): Preferences {
  const raw = storage.getItem(preferencesStorageKey)
  if (!raw) return cloneDefaults()
  try {
    const value = JSON.parse(raw) as Partial<Preferences>
    return {
      theme: value.theme === 'light' || value.theme === 'dark' ? value.theme : 'system',
      columns: stringArray(value.columns, defaultPreferences.columns),
      splitDirection: value.splitDirection === 'horizontal' ? 'horizontal' : 'vertical',
      splitSize:
        typeof value.splitSize === 'number' && value.splitSize >= 160 && value.splitSize <= 4000
          ? value.splitSize
          : defaultPreferences.splitSize,
      collapsedGroups: stringArray(value.collapsedGroups, []),
      boardCompact: value.boardCompact === true,
      collapsedBoardColumns: stringArray(value.collapsedBoardColumns, []),
      boardHighlightRemote: value.boardHighlightRemote !== false,
    }
  } catch {
    return cloneDefaults()
  }
}

export function savePreferences(value: Preferences, storage?: Storage): void
export function savePreferences(
  value: Partial<Preferences> & Record<string, unknown>,
  storage?: Storage,
): void
export function savePreferences(
  value: Partial<Preferences>,
  storage: Storage = localStorage,
): void {
  const current = { ...cloneDefaults(), ...value }
  const allowed: Preferences = {
    theme:
      current.theme === 'light' || current.theme === 'dark' || current.theme === 'system'
        ? current.theme
        : 'system',
    columns: stringArray(current.columns, defaultPreferences.columns),
    splitDirection: current.splitDirection === 'horizontal' ? 'horizontal' : 'vertical',
    splitSize:
      typeof current.splitSize === 'number' && current.splitSize >= 160 && current.splitSize <= 4000
        ? current.splitSize
        : defaultPreferences.splitSize,
    collapsedGroups: stringArray(current.collapsedGroups, []),
    boardCompact: current.boardCompact === true,
    collapsedBoardColumns: stringArray(current.collapsedBoardColumns, []),
    boardHighlightRemote: current.boardHighlightRemote !== false,
  }
  storage.setItem(preferencesStorageKey, JSON.stringify(allowed))
}

export function originStabilityWarning(originStable: boolean): string | undefined {
  if (originStable) return undefined
  return 'This daemon is using a temporary browser origin; bookmarks and preferences may not persist.'
}

function cloneDefaults(): Preferences {
  return {
    ...defaultPreferences,
    columns: [...defaultPreferences.columns],
    collapsedGroups: [...defaultPreferences.collapsedGroups],
    collapsedBoardColumns: [...defaultPreferences.collapsedBoardColumns],
  }
}

function stringArray(value: unknown, fallback: readonly string[]): string[] {
  if (!Array.isArray(value) || !value.every((item) => typeof item === 'string')) {
    return [...fallback]
  }
  return [...new Set(value)].sort()
}
