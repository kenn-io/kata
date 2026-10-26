export function jobTags(options: unknown): string[] {
  if (
    !options ||
    typeof options !== 'object' ||
    !('tags' in options) ||
    !Array.isArray(options.tags) ||
    !options.tags.every((v) => typeof v === 'string')
  )
    return []
  return [...new Set(options.tags.map((v) => v.trim()).filter(Boolean))].sort()
}
export function statusDot(status: string): 'working' | 'idle' | 'unclean' | 'quiet' | 'stale' {
  return (
    ({ running: 'working', succeeded: 'idle', failed: 'unclean', cancelled: 'quiet' } as const)[
      status as 'running'
    ] ?? 'stale'
  )
}
