// Parse/serialize RRULE strings into a Common/Advanced/Invalid tri-state.
// The classification rule of thumb:
//   Invalid  = Kata (rrule-go) would reject the string
//   Advanced = Kata accepts but the Common form cannot represent it
//   Common   = Kata accepts and every part is form-representable
// The frontend MUST NOT reject any RRULE the server accepts.

export type Weekday = 'MO' | 'TU' | 'WE' | 'TH' | 'FR' | 'SA' | 'SU'

export const WEEKDAYS: readonly Weekday[] = ['MO', 'TU', 'WE', 'TH', 'FR', 'SA', 'SU']

// Ordinal positions for the "nth weekday of the month" pattern: -1
// means "last", 1..5 means "first..fifth". 0 is intentionally excluded
// — the RFC rejects 0MO, and the formatter relies on this to render
// "the 1st Monday" without a defensive fallback for ORDINAL[0].
export type Ordinal = -1 | 1 | 2 | 3 | 4 | 5

export type CommonRule = {
  freq: 'DAILY' | 'WEEKLY' | 'MONTHLY' | 'YEARLY'
  interval: number
  byDay?: Weekday[]
  byMonth?: number
  dayInMonth?:
    | { kind: 'dayOfMonth'; day: number }
    | { kind: 'nthWeekday'; ordinal: Ordinal; weekday: Weekday }
  end?: { kind: 'never' } | { kind: 'until'; date: string } | { kind: 'count'; count: number }
}

export type ParseResult =
  | { kind: 'common'; rrule: CommonRule }
  | { kind: 'advanced'; raw: string; reason?: string }
  | { kind: 'invalid'; raw: string; error: string }

// Keys that rrule-go recognizes. Anything outside this set causes
// rrule.StrToROption to error, so we classify as Invalid.
const KNOWN_KEYS = new Set([
  'FREQ',
  'INTERVAL',
  'COUNT',
  'UNTIL',
  'BYDAY',
  'BYMONTHDAY',
  'BYMONTH',
  'BYYEARDAY',
  'BYWEEKNO',
  'BYHOUR',
  'BYMINUTE',
  'BYSECOND',
  'BYSETPOS',
  'WKST',
])

const COMMON_FREQS = new Set(['DAILY', 'WEEKLY', 'MONTHLY', 'YEARLY'])
const SUBDAILY_FREQS = new Set(['HOURLY', 'MINUTELY', 'SECONDLY'])
const ALL_FREQS = new Set([...COMMON_FREQS, ...SUBDAILY_FREQS])

// Keys that rrule-go accepts but the Common form cannot represent.
// Per spec §2.4, any rule that includes one of these is Advanced even
// if FREQ is in COMMON_FREQS — but only if the value is well-formed.
// Malformed values (e.g. BYHOUR=abc, WKST=XX) are Invalid because
// rrule-go would reject the rule outright.
const ADVANCED_ONLY_KEYS = [
  'BYHOUR',
  'BYMINUTE',
  'BYSECOND',
  'BYSETPOS',
  'BYWEEKNO',
  'BYYEARDAY',
  'WKST',
] as const

type AdvancedOnlyKey = (typeof ADVANCED_ONLY_KEYS)[number]

export function parseRRule(input: string): ParseResult {
  const partsOrErr = splitSegments(input)
  if (!partsOrErr.ok) return partsOrErr.error
  const parts = partsOrErr.parts

  const freqStr = parts.get('FREQ')
  if (freqStr === undefined || freqStr === '') {
    return invalid(input, 'missing FREQ')
  }
  if (!ALL_FREQS.has(freqStr)) {
    return invalid(input, `unknown FREQ value ${freqStr}`)
  }

  const intervalRaw = parts.get('INTERVAL')
  let interval = 1
  if (intervalRaw !== undefined) {
    const parsed = parseIntStrict(intervalRaw)
    if (parsed === null || parsed < 1) {
      return invalid(input, 'INTERVAL must be a positive integer')
    }
    interval = parsed
  }

  const countRaw = parts.get('COUNT')
  if (countRaw !== undefined) {
    const n = parseIntStrict(countRaw)
    if (n === null || n < 1) return invalid(input, 'COUNT must be a positive integer')
  }

  const untilRaw = parts.get('UNTIL')
  if (untilRaw !== undefined && !isWellFormedUntil(untilRaw)) {
    return invalid(input, `malformed UNTIL ${untilRaw}`)
  }

  const byDayRaw = parts.get('BYDAY')
  if (byDayRaw !== undefined && !isWellFormedByDay(byDayRaw)) {
    return invalid(input, `malformed BYDAY ${byDayRaw}`)
  }

  const byMonthDayRaw = parts.get('BYMONTHDAY')
  if (byMonthDayRaw !== undefined && !isWellFormedByMonthDay(byMonthDayRaw)) {
    return invalid(input, `malformed BYMONTHDAY ${byMonthDayRaw}`)
  }

  const byMonthRaw = parts.get('BYMONTH')
  if (byMonthRaw !== undefined && !isWellFormedByMonth(byMonthRaw)) {
    return invalid(input, `malformed BYMONTH ${byMonthRaw}`)
  }

  // Validate every advanced-only key present, regardless of FREQ. A
  // single malformed value (e.g. BYMINUTE=99) invalidates the rule
  // even when an earlier key was well-formed, and the check must run
  // for sub-daily FREQs too so e.g. FREQ=HOURLY;BYHOUR=abc doesn't
  // silently reach the server.
  let firstAdvancedOnly: AdvancedOnlyKey | null = null
  for (const key of ADVANCED_ONLY_KEYS) {
    const value = parts.get(key)
    if (value === undefined) continue
    if (!isWellFormedAdvancedOnly(key, value)) {
      return invalid(input, `malformed ${key}`)
    }
    if (firstAdvancedOnly === null) firstAdvancedOnly = key
  }

  if (!COMMON_FREQS.has(freqStr)) {
    return { kind: 'advanced', raw: input }
  }

  if (firstAdvancedOnly !== null) {
    return { kind: 'advanced', raw: input, reason: `contains ${firstAdvancedOnly}` }
  }

  const common: CommonRule = {
    freq: freqStr as CommonRule['freq'],
    interval,
  }

  const endResult = applyEndCondition(common, countRaw, untilRaw, input)
  if (endResult !== null) return endResult

  // Any combination of BYMONTHDAY and BYDAY is too rich for the form.
  if (byDayRaw !== undefined && byMonthDayRaw !== undefined) {
    return { kind: 'advanced', raw: input, reason: 'BYDAY combined with BYMONTHDAY' }
  }

  return classifyByFreq(common, { byDayRaw, byMonthDayRaw, byMonthRaw, raw: input })
}

type FreqInputs = {
  byDayRaw: string | undefined
  byMonthDayRaw: string | undefined
  byMonthRaw: string | undefined
  raw: string
}

// Dispatches to the per-FREQ classifier. The classifier either mutates
// `common` and returns null (caller wraps as Common) or returns a
// terminal ParseResult (Advanced or Invalid).
function classifyByFreq(common: CommonRule, in_: FreqInputs): ParseResult {
  let result: ParseResult | null
  switch (common.freq) {
    case 'DAILY':
      result = classifyDaily(in_)
      break
    case 'WEEKLY':
      result = classifyWeekly(common, in_)
      break
    case 'MONTHLY':
      result = classifyMonthly(common, in_)
      break
    case 'YEARLY':
      result = classifyYearly(common, in_)
      break
  }
  return result ?? { kind: 'common', rrule: common }
}

// DAILY accepts only FREQ + INTERVAL + end. Anything else is Advanced.
function classifyDaily(in_: FreqInputs): ParseResult | null {
  if (
    in_.byDayRaw !== undefined ||
    in_.byMonthDayRaw !== undefined ||
    in_.byMonthRaw !== undefined
  ) {
    return { kind: 'advanced', raw: in_.raw }
  }
  return null
}

function classifyWeekly(common: CommonRule, in_: FreqInputs): ParseResult | null {
  if (in_.byMonthDayRaw !== undefined || in_.byMonthRaw !== undefined) {
    return { kind: 'advanced', raw: in_.raw }
  }
  if (in_.byDayRaw !== undefined) {
    const days = parseByDayPlain(in_.byDayRaw)
    if (days === null) {
      return { kind: 'advanced', raw: in_.raw, reason: 'WEEKLY BYDAY uses ordinals' }
    }
    common.byDay = sortWeekdays(days)
  }
  return null
}

function classifyMonthly(common: CommonRule, in_: FreqInputs): ParseResult | null {
  if (in_.byMonthRaw !== undefined) {
    return { kind: 'advanced', raw: in_.raw, reason: 'BYMONTH on non-YEARLY FREQ' }
  }
  if (in_.byMonthDayRaw !== undefined) {
    return applyByMonthDay(common, in_.byMonthDayRaw, in_.raw)
  }
  if (in_.byDayRaw !== undefined) {
    return applyNthWeekday(common, in_.byDayRaw, in_.raw, 'MONTHLY BYDAY without ordinal')
  }
  return null
}

function classifyYearly(common: CommonRule, in_: FreqInputs): ParseResult | null {
  if (in_.byMonthRaw === undefined) {
    // YEARLY without BYMONTH but with day-in-month parts isn't a
    // shape the form models ("the 15th of every month each year").
    if (in_.byMonthDayRaw !== undefined || in_.byDayRaw !== undefined) {
      return { kind: 'advanced', raw: in_.raw, reason: 'YEARLY without BYMONTH' }
    }
    return null
  }
  const month = parseIntStrict(in_.byMonthRaw)
  if (month === null || month < 1 || month > 12) {
    return { kind: 'advanced', raw: in_.raw, reason: 'BYMONTH out of range' }
  }
  common.byMonth = month
  if (in_.byMonthDayRaw !== undefined) {
    // The Yearly form has no last-day affordance (only Monthly does),
    // so a single BYMONTHDAY=-1 must round-trip through Advanced.
    // Without this, loading FREQ=YEARLY;BYMONTH=3;BYMONTHDAY=-1 into
    // the editor and saving would silently rewrite the rule to
    // BYMONTHDAY=1. Parse the value here (instead of comparing the
    // raw string) so equivalent forms like "-01" or " -1 " are
    // classified the same way.
    const tokens = in_.byMonthDayRaw.split(',')
    if (tokens.length === 1) {
      const parsed = parseIntStrict(tokens[0] ?? '')
      if (parsed === -1) {
        return {
          kind: 'advanced',
          raw: in_.raw,
          reason: 'YEARLY BYMONTHDAY=-1 outside Common-mode range',
        }
      }
    }
    return applyByMonthDay(common, in_.byMonthDayRaw, in_.raw)
  }
  if (in_.byDayRaw !== undefined) {
    return applyNthWeekday(common, in_.byDayRaw, in_.raw, 'YEARLY BYDAY without ordinal')
  }
  return null
}

// applyByMonthDay validates the value, mutates `common`, and returns
// null when the rule fits Common form. Per RFC 5545 §3.3.10 BYMONTHDAY
// is a comma-separated list of integers in 1..31 or -31..-1; everything
// else (non-integer, 0, |day|>31, empty token) is malformed and
// rrule-go rejects it. Single-value rules where day∈{-1} ∪ [1..31] map
// to Common; in-range values the form can't model (-2..-31) and any
// multi-value list classify as Advanced.
function applyByMonthDay(
  common: CommonRule,
  byMonthDayRaw: string,
  raw: string,
): ParseResult | null {
  const tokens = byMonthDayRaw.split(',')
  const days: number[] = []
  for (const token of tokens) {
    if (token === '') return invalid(raw, 'BYMONTHDAY empty token')
    const day = parseIntStrict(token)
    if (day === null) return invalid(raw, `malformed BYMONTHDAY ${token}`)
    if (day === 0) return invalid(raw, 'BYMONTHDAY=0 is not valid')
    if (day < -31 || day > 31) return invalid(raw, `BYMONTHDAY ${day} out of range`)
    days.push(day)
  }
  if (days.length !== 1) {
    return { kind: 'advanced', raw, reason: 'BYMONTHDAY list' }
  }
  const day = days[0]!
  if (day < 0 && day !== -1) {
    return { kind: 'advanced', raw, reason: 'BYMONTHDAY outside Common-mode range' }
  }
  common.dayInMonth = { kind: 'dayOfMonth', day }
  return null
}

function applyNthWeekday(
  common: CommonRule,
  byDayRaw: string,
  raw: string,
  reason: string,
): ParseResult | null {
  const ord = parseByDayOrdinal(byDayRaw)
  if (ord === null) return { kind: 'advanced', raw, reason }
  common.dayInMonth = { kind: 'nthWeekday', ordinal: ord.ordinal, weekday: ord.weekday }
  return null
}

// applyEndCondition mutates `common.end` from COUNT/UNTIL. Returns a
// terminal ParseResult only when the input isn't representable in
// Common form (both present, or UNTIL with time-of-day); otherwise null.
function applyEndCondition(
  common: CommonRule,
  countRaw: string | undefined,
  untilRaw: string | undefined,
  raw: string,
): ParseResult | null {
  if (countRaw !== undefined && untilRaw !== undefined) {
    return { kind: 'advanced', raw, reason: 'RRULE has both COUNT and UNTIL' }
  }
  if (countRaw !== undefined) {
    common.end = { kind: 'count', count: parseInt(countRaw, 10) }
    return null
  }
  if (untilRaw !== undefined) {
    if (untilRaw.length === 8) {
      common.end = {
        kind: 'until',
        date: `${untilRaw.slice(0, 4)}-${untilRaw.slice(4, 6)}-${untilRaw.slice(6, 8)}`,
      }
      return null
    }
    // UNTIL with time-of-day is valid but not Common (spec §2.4).
    return { kind: 'advanced', raw, reason: 'UNTIL includes time-of-day' }
  }
  return null
}

type SplitResult = { ok: true; parts: Map<string, string> } | { ok: false; error: ParseResult }

function splitSegments(input: string): SplitResult {
  const parts = new Map<string, string>()
  for (const segment of input.split(';')) {
    if (segment === '') return { ok: false, error: invalid(input, 'empty segment') }
    const eq = segment.indexOf('=')
    if (eq < 0) return { ok: false, error: invalid(input, `malformed segment "${segment}"`) }
    const key = segment.slice(0, eq)
    const value = segment.slice(eq + 1)
    if (parts.has(key)) return { ok: false, error: invalid(input, `duplicate key ${key}`) }
    if (!KNOWN_KEYS.has(key)) return { ok: false, error: invalid(input, `unknown property ${key}`) }
    parts.set(key, value)
  }
  return { ok: true, parts }
}

function invalid(input: string, error: string): ParseResult {
  return { kind: 'invalid', raw: input, error }
}

function parseIntStrict(s: string): number | null {
  if (!/^-?\d+$/.test(s)) return null
  return parseInt(s, 10)
}

// Accepts YYYYMMDD or YYYYMMDDTHHMMSS[Z]. Rrule-go is strict about this
// shape; dashed dates are rejected. The calendar values must also be
// real dates/times, so impossible months, days, and clock values do not
// reach the daemon as supposedly accepted rules.
function isWellFormedUntil(s: string): boolean {
  const dateOnly = /^(\d{4})(\d{2})(\d{2})$/.exec(s)
  if (dateOnly) {
    return isRealDate(Number(dateOnly[1]), Number(dateOnly[2]), Number(dateOnly[3]))
  }
  const dateTime = /^(\d{4})(\d{2})(\d{2})T(\d{2})(\d{2})(\d{2})Z?$/.exec(s)
  if (dateTime) {
    const hour = Number(dateTime[4])
    const minute = Number(dateTime[5])
    const second = Number(dateTime[6])
    return (
      isRealDate(Number(dateTime[1]), Number(dateTime[2]), Number(dateTime[3])) &&
      hour >= 0 &&
      hour <= 23 &&
      minute >= 0 &&
      minute <= 59 &&
      second >= 0 &&
      second <= 59
    )
  }
  return false
}

function isRealDate(year: number, month: number, day: number): boolean {
  if (month < 1 || month > 12 || day < 1 || day > 31) return false
  const date = new Date(Date.UTC(year, month - 1, day))
  return (
    date.getUTCFullYear() === year && date.getUTCMonth() === month - 1 && date.getUTCDate() === day
  )
}

// isWellFormedAdvancedOnly validates the value of an ADVANCED_ONLY key
// against RFC 5545's ranges. A well-formed value classifies the rule as
// Advanced; a malformed value (which rrule-go would reject) classifies
// it as Invalid. Empty values are always malformed.
function isWellFormedAdvancedOnly(key: AdvancedOnlyKey, value: string): boolean {
  if (value === '') return false
  switch (key) {
    case 'BYHOUR':
      return isIntListInRange(value, 0, 23, { nonZero: false })
    case 'BYMINUTE':
      return isIntListInRange(value, 0, 59, { nonZero: false })
    case 'BYSECOND':
      // BYSECOND allows 60 for leap seconds per RFC 5545 §3.3.10.
      return isIntListInRange(value, 0, 60, { nonZero: false })
    case 'BYSETPOS':
      return isIntListInRange(value, -366, 366, { nonZero: true })
    case 'BYWEEKNO':
      return isIntListInRange(value, -53, 53, { nonZero: true })
    case 'BYYEARDAY':
      return isIntListInRange(value, -366, 366, { nonZero: true })
    case 'WKST':
      return (WEEKDAYS as readonly string[]).includes(value)
  }
}

// isIntListInRange parses a comma-separated list of integers and
// validates each falls within [min, max]. When `nonZero` is true, 0
// is rejected (e.g. BYSETPOS=0 is invalid). Rejects empty tokens.
function isIntListInRange(
  s: string,
  min: number,
  max: number,
  { nonZero }: { nonZero: boolean },
): boolean {
  const tokens = s.split(',')
  for (const t of tokens) {
    if (t === '') return false
    const n = parseIntStrict(t)
    if (n === null) return false
    if (n < min || n > max) return false
    if (nonZero && n === 0) return false
  }
  return true
}

function isWellFormedByMonthDay(s: string): boolean {
  return isIntListInRange(s, -31, 31, { nonZero: true })
}

function isWellFormedByMonth(s: string): boolean {
  return isIntListInRange(s, 1, 12, { nonZero: false })
}

// Accepts comma-separated weekday tokens, optionally prefixed with a
// signed non-zero integer (e.g. "MO", "1MO", "-1FR"). Rejects unknown
// weekday codes and ordinal 0 so the parser flags them as Invalid —
// rrule-go rejects "0MO" per RFC 5545.
function isWellFormedByDay(s: string): boolean {
  const tokens = s.split(',')
  if (tokens.length === 0) return false
  for (const t of tokens) {
    const m = /^(-?\d+)?([A-Z]{2})$/.exec(t)
    if (!m) return false
    if (m[1] !== undefined && parseInt(m[1], 10) === 0) return false
    if (!(WEEKDAYS as readonly string[]).includes(m[2]!)) return false
  }
  return true
}

// parseByDayPlain returns the list when BYDAY contains only un-prefixed
// weekday tokens; returns null if any token has an ordinal prefix.
function parseByDayPlain(s: string): Weekday[] | null {
  const out: Weekday[] = []
  for (const t of s.split(',')) {
    const m = /^([A-Z]{2})$/.exec(t)
    if (!m) return null
    if (!(WEEKDAYS as readonly string[]).includes(m[1]!)) return null
    out.push(m[1] as Weekday)
  }
  return out
}

// parseByDayOrdinal returns the single (ordinal, weekday) pair when
// BYDAY is exactly one ordinal-prefixed token. Returns null otherwise
// (no prefix, multiple tokens, etc).
function parseByDayOrdinal(s: string): { ordinal: Ordinal; weekday: Weekday } | null {
  if (s.includes(',')) return null
  const m = /^(-?\d+)([A-Z]{2})$/.exec(s)
  if (!m) return null
  const ordinal = parseInt(m[1]!, 10)
  if (ordinal === 0 || ordinal < -1 || ordinal > 5) {
    // Spec §2.3: ordinal ∈ {-1, 1..5}. Other ordinals (e.g. -2MO) are
    // valid RFC but outside Common-mode controls.
    return null
  }
  if (!(WEEKDAYS as readonly string[]).includes(m[2]!)) return null
  // The range check above is the runtime source of truth; `parseInt`
  // returns `number`, so we cast to the literal union here.
  return { ordinal: ordinal as Ordinal, weekday: m[2] as Weekday }
}

function sortWeekdays(days: Weekday[]): Weekday[] {
  const order = new Map(WEEKDAYS.map((w, i) => [w, i]))
  // `?? 0` is dead-defensive — every Weekday is keyed in `order`.
  return [...days].sort((a, b) => (order.get(a) ?? 0) - (order.get(b) ?? 0))
}

// serializeRRule emits the canonical Common-mode form described in
// spec §2.5. Parts order: FREQ;INTERVAL[;BYMONTH][;BYMONTHDAY|BYDAY]
// [;UNTIL|COUNT]. INTERVAL is always emitted (even =1) so diff logic
// can compare strings without re-parsing. Weekdays are sorted.
export function serializeRRule(rule: CommonRule): string {
  const parts: string[] = [`FREQ=${rule.freq}`, `INTERVAL=${rule.interval}`]

  if (rule.byMonth !== undefined) {
    parts.push(`BYMONTH=${rule.byMonth}`)
  }

  if (rule.dayInMonth !== undefined) {
    if (rule.dayInMonth.kind === 'dayOfMonth') {
      parts.push(`BYMONTHDAY=${rule.dayInMonth.day}`)
    } else {
      parts.push(`BYDAY=${rule.dayInMonth.ordinal}${rule.dayInMonth.weekday}`)
    }
  } else if (rule.byDay && rule.byDay.length > 0) {
    parts.push(`BYDAY=${sortWeekdays(rule.byDay).join(',')}`)
  }

  if (rule.end) {
    if (rule.end.kind === 'count') {
      parts.push(`COUNT=${rule.end.count}`)
    } else if (rule.end.kind === 'until') {
      parts.push(`UNTIL=${rule.end.date.split('-').join('')}`)
    }
    // "never" emits nothing.
  }

  return parts.join(';')
}

export const WEEKDAY_LABEL: Record<Weekday, string> = {
  MO: 'Mon',
  TU: 'Tue',
  WE: 'Wed',
  TH: 'Thu',
  FR: 'Fri',
  SA: 'Sat',
  SU: 'Sun',
}

export const WEEKDAY_LONG: Record<Weekday, string> = {
  MO: 'Monday',
  TU: 'Tuesday',
  WE: 'Wednesday',
  TH: 'Thursday',
  FR: 'Friday',
  SA: 'Saturday',
  SU: 'Sunday',
}

export const MONTH_LONG = [
  'January',
  'February',
  'March',
  'April',
  'May',
  'June',
  'July',
  'August',
  'September',
  'October',
  'November',
  'December',
]

const ORDINAL = ['', '1st', '2nd', '3rd', '4th', '5th']

// formatOrdinal turns the typed Ordinal into its display string. The
// CommonRule.dayInMonth type excludes 0, so ORDINAL[ordinal] is always
// a non-empty string for ordinal >= 1; -1 renders as "last".
function formatOrdinal(ordinal: Ordinal): string {
  return ordinal === -1 ? 'last' : ORDINAL[ordinal]!
}

// formatRRule produces the user-facing one-line summary shown in
// RecurrencePanel and the editor's compact preview. See spec §2.7.
export function formatRRule(input: string): string {
  const parsed = parseRRule(input)
  if (parsed.kind === 'invalid') return 'Invalid recurrence'
  if (parsed.kind === 'advanced') {
    const raw = parsed.raw
    if (raw.length <= 40) return `Advanced: ${raw}`
    return `Advanced: ${raw.slice(0, 40)}…`
  }
  return formatCommon(parsed.rrule)
}

function formatCommon(r: CommonRule): string {
  const head = formatHead(r)
  const tail = formatTail(r)
  return tail ? `${head}${tail}` : head
}

function formatHead(r: CommonRule): string {
  switch (r.freq) {
    case 'DAILY':
      return r.interval === 1 ? 'Daily' : `Every ${r.interval} days`
    case 'WEEKLY': {
      const cadence = r.interval === 1 ? 'Weekly' : `Every ${r.interval} weeks`
      if (r.byDay && r.byDay.length > 0) {
        const names = r.byDay.map((d) => WEEKDAY_LABEL[d]).join(', ')
        return `${cadence} on ${names}`
      }
      return cadence
    }
    case 'MONTHLY': {
      const cadence = r.interval === 1 ? 'Monthly' : `Every ${r.interval} months`
      if (r.dayInMonth?.kind === 'dayOfMonth') {
        return r.dayInMonth.day === -1
          ? `${cadence} on the last day`
          : `${cadence} on day ${r.dayInMonth.day}`
      }
      if (r.dayInMonth?.kind === 'nthWeekday') {
        return `${cadence} on the ${formatOrdinal(r.dayInMonth.ordinal)} ${WEEKDAY_LONG[r.dayInMonth.weekday]}`
      }
      return cadence
    }
    case 'YEARLY': {
      const cadence = r.interval === 1 ? 'Yearly' : `Every ${r.interval} years`
      if (r.byMonth === undefined) return cadence
      // Parser validates 1..12 (classifyYearly); byMonth stays `number`
      // for ergonomics in editor form code, so the `!` is the type-side
      // counterpart to that runtime invariant.
      const month = MONTH_LONG[r.byMonth - 1]!
      if (r.dayInMonth?.kind === 'dayOfMonth') {
        return r.dayInMonth.day === -1
          ? `${cadence} on the last day of ${month}`
          : `${cadence} on ${month} ${r.dayInMonth.day}`
      }
      if (r.dayInMonth?.kind === 'nthWeekday') {
        return `${cadence} on the ${formatOrdinal(r.dayInMonth.ordinal)} ${WEEKDAY_LONG[r.dayInMonth.weekday]} of ${month}`
      }
      return `${cadence} in ${month}`
    }
  }
}

function formatTail(r: CommonRule): string {
  if (!r.end || r.end.kind === 'never') return ''
  if (r.end.kind === 'count') {
    const noun = r.end.count === 1 ? 'time' : 'times'
    return `, ${r.end.count} ${noun}`
  }
  return ` until ${r.end.date}`
}
