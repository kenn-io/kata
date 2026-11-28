// "Needs attention": open issues where an agent or a person is waiting on a
// human. Every signal is already in the snapshot: agents' work.attention
// metadata, `kata notify` requests stored as notify.* metadata, and the
// needs-review label the default agent contract asks agents to leave.
//
// Reply reports share the notify.* slots but only tell an author that someone
// answered their comment, so they are agents talking to each other, not a
// request for a human.

import type { KataTaskGroup, KataTaskSummary } from './types'

export type AttentionKind = 'stuck' | 'needs-human' | 'request' | 'needs-review'

export interface AttentionReason {
  kind: AttentionKind
  text: string
  /** The notify.* key holding a request, so handling it can clear exactly that slot. */
  key?: string
}

/** Whose attention the browser is reading. */
export interface AttentionViewer {
  /** The actor the browser writes as; requests it sent ask nothing of it. */
  actor: string
  /**
   * The exact inbox address to read, like `kata inbox --for`. Without one (the
   * owner-local browser, which has no account) every recipient's requests count.
   */
  inbox?: string | undefined
}

export const needsReviewLabel = 'needs-review'

/** The actor the browser writes as when the daemon does not derive one. */
export const webActor = 'kata-web'

/**
 * The browser's viewer. An account (identity mode) reads exactly that inbox;
 * the owner-local browser has no account and reads every inbox, as the daemon
 * owner supervising the agents on this machine.
 */
export function attentionViewer(account: string | undefined): AttentionViewer {
  return account ? { actor: account, inbox: account } : { actor: webActor }
}

// Most urgent first; groups and reasons follow this order.
const attentionOrder: readonly AttentionKind[] = ['stuck', 'needs-human', 'request', 'needs-review']

const groupTitles: Record<AttentionKind, string> = {
  stuck: 'Stuck',
  'needs-human': 'Needs a human',
  request: 'Requests',
  'needs-review': 'Needs review',
}

export const attentionBadgeLabels: Record<AttentionKind, string> = {
  stuck: 'Stuck',
  'needs-human': 'Needs you',
  request: 'Request',
  'needs-review': 'Review',
}

const notifyPrefix = 'notify.'

// The kinds `kata comment --reply/--confirm/--refute/--supersede` report.
const replyReportKinds = new Set(['reply', 'confirm', 'refute', 'supersede'])

/** Why an open issue needs attention, most urgent first; empty when it does not. */
export function attentionReasons(
  issue: KataTaskSummary,
  viewer?: AttentionViewer,
): AttentionReason[] {
  if (issue.status !== 'open') return []
  const reasons: AttentionReason[] = []
  const metadata = issue.metadata
  const attention = metadata['work.attention']
  const message = textValue(metadata['work.attention_msg'])
  if (attention === 'stuck') {
    reasons.push({ kind: 'stuck', text: message ? `Stuck: ${message}` : 'Stuck' })
  } else if (attention === 'needs-human') {
    reasons.push({
      kind: 'needs-human',
      text: message ? `Needs a human: ${message}` : 'Needs a human',
    })
  }
  for (const key of Object.keys(metadata).sort()) {
    if (!key.startsWith(notifyPrefix)) continue
    const request = notifyRequest(key.slice(notifyPrefix.length), metadata[key], viewer)
    if (request) reasons.push({ kind: 'request', key, text: request })
  }
  if (issue.labels?.includes(needsReviewLabel)) {
    reasons.push({ kind: 'needs-review', text: 'Needs review' })
  }
  return reasons
}

export function hasAttention(issue: KataTaskSummary, viewer?: AttentionViewer): boolean {
  return attentionReasons(issue, viewer).length > 0
}

/** The notify.* keys of the requests the viewer is being asked to handle. */
export function requestKeys(issue: KataTaskSummary, viewer?: AttentionViewer): string[] {
  return attentionReasons(issue, viewer).flatMap((reason) => (reason.key ? [reason.key] : []))
}

/** Groups open issues by their most urgent reason, in urgency order. */
export function buildAttentionGroups(
  issues: readonly KataTaskSummary[],
  viewer?: AttentionViewer,
): KataTaskGroup[] {
  const groups = new Map<AttentionKind, KataTaskSummary[]>()
  for (const issue of issues) {
    const first = attentionReasons(issue, viewer)[0]
    if (!first) continue
    const group = groups.get(first.kind) ?? []
    group.push(issue)
    groups.set(first.kind, group)
  }
  return attentionOrder
    .filter((kind) => groups.has(kind))
    .map((kind) => ({
      id: `attention-${kind}`,
      title: groupTitles[kind],
      issues: [...groups.get(kind)!].sort(byPriorityThenTitle),
    }))
}

function notifyRequest(
  encodedRecipient: string,
  value: unknown,
  viewer: AttentionViewer | undefined,
): string | undefined {
  const recipient = decodeRecipient(encodedRecipient)
  if (!recipient || typeof value !== 'object' || value === null || Array.isArray(value)) {
    return undefined
  }
  if (viewer?.inbox !== undefined && recipient !== viewer.inbox) return undefined
  const fields = value as Record<string, unknown>
  const kind = textValue(fields.kind)
  if (kind && replyReportKinds.has(kind)) return undefined
  const message = textValue(fields.message)
  if (!message) return undefined
  const from = textValue(fields.from)
  const teammate = textValue(fields.teammate)
  // The browser sends as its actor with no teammate; a teammate sharing the
  // account is someone else asking.
  if (viewer && from === viewer.actor && !teammate) return undefined
  const sender = from ? (teammate ? `${from}/${teammate}` : from) : undefined
  return `Request to ${recipient}${sender ? ` from ${sender}` : ''}: ${message}`
}

// Recipients are stored as unpadded base64url of the UTF-8 handle.
function decodeRecipient(encoded: string): string | undefined {
  if (!/^[A-Za-z0-9_-]+$/.test(encoded)) return undefined
  try {
    const base64 = encoded.replace(/-/g, '+').replace(/_/g, '/')
    const padded = base64 + '='.repeat((4 - (base64.length % 4)) % 4)
    const bytes = Uint8Array.from(atob(padded), (char) => char.charCodeAt(0))
    const handle = new TextDecoder('utf-8', { fatal: true }).decode(bytes).trim()
    return handle || undefined
  } catch {
    return undefined
  }
}

function textValue(value: unknown): string | undefined {
  return typeof value === 'string' && value.trim() ? value.trim() : undefined
}

function byPriorityThenTitle(a: KataTaskSummary, b: KataTaskSummary): number {
  const ap = a.priority ?? Number.MAX_SAFE_INTEGER
  const bp = b.priority ?? Number.MAX_SAFE_INTEGER
  if (ap !== bp) return ap - bp
  return a.title.localeCompare(b.title) || a.uid.localeCompare(b.uid)
}
