// Go's JSON timestamps have variable fractional widths. Keep nanoseconds so
// distinct API instants do not become ties after JavaScript millisecond parsing.
function nanoseconds(value: string): bigint | undefined {
  const parts = /^(\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2})(?:\.(\d{1,9}))?(Z|[+-]\d{2}:\d{2})$/.exec(
    value,
  )
  if (!parts) return undefined
  const milliseconds = Date.parse(parts[1]! + parts[3]!)
  if (!Number.isFinite(milliseconds)) return undefined
  return BigInt(milliseconds) * 1_000_000n + BigInt((parts[2] ?? '').padEnd(9, '0'))
}

export function compareInstants(first: string, second: string): number {
  const a = nanoseconds(first),
    b = nanoseconds(second)
  if (a !== undefined && b !== undefined) return a < b ? -1 : a > b ? 1 : 0
  // Malformed external values retain a deterministic order without breaking a catalog.
  return first < second ? -1 : first > second ? 1 : 0
}
