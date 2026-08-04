export const preferencesStorageKey = 'kata.preferences.v1'

export interface Preferences {
  theme: 'system' | 'light' | 'dark'
  columns: string[]
  splitDirection: 'horizontal' | 'vertical'
  splitSize: number
  collapsedGroups: string[]
}

export const defaultPreferences: Preferences = {
  theme: 'system',
  columns: ['status', 'title'],
  splitDirection: 'vertical',
  splitSize: 420,
  collapsedGroups: [],
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
  }
}

function stringArray(value: unknown, fallback: readonly string[]): string[] {
  if (!Array.isArray(value) || !value.every((item) => typeof item === 'string')) {
    return [...fallback]
  }
  return [...new Set(value)].sort()
}
