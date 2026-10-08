import type { KataCommentReplyIntent } from './types'

export function commentRetryIdentity(
  issueUID: string,
  body: string,
  reply?: KataCommentReplyIntent,
): string {
  return JSON.stringify([
    issueUID,
    body,
    reply?.replyTo ?? null,
    reply?.kind ?? null,
    reply?.force ?? false,
  ])
}
