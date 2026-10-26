import type { KataTaskEvent } from '../kata/types'

// A recorded locator is clickable only as an absolute HTTP(S) URL with a host
// and no userinfo, query, fragment, control, whitespace or backslash.
function safeLocator(value: unknown): string | undefined {
  if (typeof value !== 'string') return undefined
  // eslint-disable-next-line no-control-regex
  if (/[\s\u0000-\u001f\u007f-\u009f?#\\]/u.test(value)) return undefined
  const authority = /^https?:\/\/([^/]+)/i.exec(value)?.[1]
  if (!authority || authority.includes('@')) return undefined
  try {
    const url = new URL(value)
    if (url.protocol !== 'http:' && url.protocol !== 'https:') return undefined
    if (!url.hostname || url.username || url.password) return undefined
  } catch {
    return undefined
  }
  return value
}

/**
 * relatedCronTranscriptURL returns the recorded `issue.closed`
 * `payload.transcript.url` of a cron run's subject issue, verbatim. Only close
 * events for that exact issue in one of the given projects qualify: the run's
 * project and, for a moved issue, its current one. Events keep the project the
 * issue had when they were recorded. The greatest event ID with a safe locator
 * wins. The link is related issue context, not run attribution.
 */
export function relatedCronTranscriptURL(
  enabled: boolean,
  projectUIDs: readonly string[],
  issueUID: string | undefined,
  events: readonly KataTaskEvent[],
): string | undefined {
  if (!enabled || !issueUID) return undefined
  let best: { id: number; url: string } | undefined
  for (const event of events) {
    if (
      event.type !== 'issue.closed' ||
      !projectUIDs.includes(event.project_uid) ||
      event.issue_uid !== issueUID
    )
      continue
    const transcript = event.payload?.transcript
    if (!transcript || typeof transcript !== 'object' || Array.isArray(transcript)) continue
    const url = safeLocator((transcript as Record<string, unknown>).url)
    if (url && (!best || event.event_id > best.id)) best = { id: event.event_id, url }
  }
  return best?.url
}
