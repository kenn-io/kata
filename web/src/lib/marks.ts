// A project's or server's mark takes one of eight hues, picked by a hash of
// its name so the same name gets the same color in every view. Red and amber
// are left out: they mean overdue, high priority, and warnings.
const markHues = [
  'var(--accent-green)',
  'var(--accent-purple)',
  'var(--accent-teal)',
  'var(--status-waiting)',
  'var(--text-muted)',
  'color-mix(in srgb, var(--accent-blue) 50%, var(--accent-purple))',
  'var(--accent-blue)',
  'color-mix(in srgb, var(--accent-teal) 55%, var(--text-primary))',
]

export function markColor(name: string): string {
  let hash = 0
  for (const char of name) hash = (hash * 31 + (char.codePointAt(0) ?? 0)) >>> 0
  return markHues[hash % markHues.length]!
}
