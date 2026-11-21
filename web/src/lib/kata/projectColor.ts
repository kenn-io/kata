import { hashColor } from '@kenn-io/kit-ui'

const projectMarkPalette = [
  'var(--accent-green)',
  'var(--accent-purple)',
  'var(--accent-teal)',
  'var(--status-waiting)',
  'var(--text-muted)',
  'color-mix(in srgb, var(--accent-blue) 50%, var(--accent-purple))',
  'var(--accent-blue)',
  'color-mix(in srgb, var(--accent-teal) 55%, var(--text-primary))',
]

export function projectMarkColor(name: string): string {
  return hashColor(name, projectMarkPalette)
}
