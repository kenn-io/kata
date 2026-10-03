import { captureTelemetryEvent } from '../api/generated'
import { loadSessionCredentials } from '../auth/session'

export interface AppOpenedReporter {
  /** Called on page load and on window focus: arms today's opening if not yet reported. */
  opened(): void
  /** Called when the tab has just obtained serving-daemon session credentials. */
  sessionAvailable(): void
}

/**
 * Reports app_opened at most once per UTC day, only for a load or a focus. The day is marked before
 * dispatch. A 401 hands the opening to the next session, as the app's fetch hands the tab to recovery.
 */
export function createAppOpenedReporter(
  send: typeof captureTelemetryEvent = captureTelemetryEvent,
  now: () => Date = () => new Date(),
  currentSession: () => string | undefined = () => loadSessionCredentials()?.session,
): AppOpenedReporter {
  let reportedDay = ''
  let armedDay = ''
  const today = () => now().toISOString().slice(0, 10)
  const flush = () => {
    if (armedDay !== '' && armedDay !== today()) armedDay = ''
    const session = currentSession()
    if (armedDay === '' || armedDay === reportedDay || session === undefined) return
    const day = armedDay
    reportedDay = day
    armedDay = ''
    send({ event: 'app_opened', properties: { surface: 'web' } })
      .then((response) => {
        if (response.status !== 401 || reportedDay !== day) return
        reportedDay = ''
        if (armedDay === '' && day === today()) armedDay = day
        const next = currentSession()
        if (next !== undefined && next !== session) flush()
      })
      .catch(() => undefined)
  }
  return {
    opened() {
      const day = today()
      if (day === reportedDay) return
      armedDay = day
      flush()
    },
    sessionAvailable() {
      flush()
    },
  }
}
