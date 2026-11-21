export function localDateString(date = new Date()): string {
  const year = date.getFullYear()
  const month = String(date.getMonth() + 1).padStart(2, '0')
  const day = String(date.getDate()).padStart(2, '0')
  return `${year}-${month}-${day}`
}

const MONTHS_SHORT = [
  'Jan',
  'Feb',
  'Mar',
  'Apr',
  'May',
  'Jun',
  'Jul',
  'Aug',
  'Sep',
  'Oct',
  'Nov',
  'Dec',
]

/**
 * Compact relative-time label suitable for a dense table column.
 * Mirrors the Linear/Things shorthand: "now", "5m", "3h", "2d" for
 * recent events; calendar month+day for older dates this year; month+
 * 2-digit year for prior years. Returns "-" if the input is empty or
 * unparseable so the column never collapses on missing data.
 */
/**
 * Compact absolute date label for table cells holding deadlines or
 * scheduled dates. Always uses calendar form ("May 1", "Dec 1 '24")
 * rather than relative units - useful when the absolute date is what
 * the user actually cares about. Returns "" for empty input.
 */
export function shortDate(iso: string | undefined, now = new Date()): string {
  if (!iso) return ''
  const head = iso.slice(0, 10)
  // Strict YYYY-MM-DD with sane month/day ranges so malformed metadata
  // like "2026-13-01" renders the raw string instead of "undefined 1".
  if (!/^\d{4}-\d{2}-\d{2}$/.test(head)) return iso
  const [year, month, day] = head.split('-').map((p) => Number(p)) as [number, number, number]
  if (!year || month < 1 || month > 12 || day < 1 || day > 31) return iso
  const monthDay = `${MONTHS_SHORT[month - 1]} ${day}`
  if (year === now.getFullYear()) return monthDay
  return `${monthDay} '${String(year).slice(-2)}`
}

export function relativeTime(iso: string | undefined, now = new Date()): string {
  if (!iso) return '-'
  const ts = Date.parse(iso)
  if (Number.isNaN(ts)) return '-'
  const then = new Date(ts)
  const seconds = Math.round((now.getTime() - ts) / 1000)
  if (seconds < 45) return 'now'
  const minutes = Math.round(seconds / 60)
  if (minutes < 60) return `${minutes}m`
  const hours = Math.round(minutes / 60)
  if (hours < 24) return `${hours}h`
  const days = Math.round(hours / 24)
  if (days < 7) return `${days}d`
  const monthDay = `${MONTHS_SHORT[then.getMonth()]} ${then.getDate()}`
  if (then.getFullYear() === now.getFullYear()) return monthDay
  return `${monthDay} '${String(then.getFullYear()).slice(-2)}`
}
