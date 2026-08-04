// Canonical advanced-only RRULE fixtures. Each is valid (rrule-go would
// accept it) but uses parts the Common form does not model. Used by
// parser tests and by the editor's mode-toggle tests to lock in
// byte-for-byte preservation of `raw`.
export const ADVANCED_LAST_WEEKDAY = 'FREQ=MONTHLY;BYDAY=MO,TU,WE,TH,FR;BYSETPOS=-1'
export const ADVANCED_MIXED_MONTHLY = 'FREQ=MONTHLY;BYMONTHDAY=15;BYDAY=FR'
export const ADVANCED_TIME_OF_DAY = 'FREQ=DAILY;BYHOUR=9;BYMINUTE=0'

export const ADVANCED_FIXTURES = [
  ADVANCED_LAST_WEEKDAY,
  ADVANCED_MIXED_MONTHLY,
  ADVANCED_TIME_OF_DAY,
] as const
