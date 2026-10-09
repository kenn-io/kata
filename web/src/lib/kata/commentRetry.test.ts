import { expect, test } from 'vitest'
import { commentRetryIdentity } from './commentRetry'

test('comment retry identity distinguishes each canonical reply field', () => {
  for (const body of ['Answer', 'é'.repeat(40), '\u0000', 'target|answer']) {
    const reply = { replyTo: 'target-comment', kind: 'reply' as const, force: false }
    const original = commentRetryIdentity('source-issue', body, reply)
    expect(commentRetryIdentity('source-issue', body, { ...reply })).toBe(original)
    expect(commentRetryIdentity('other-issue', body, reply)).not.toBe(original)
    expect(commentRetryIdentity('source-issue', body + 'x', reply)).not.toBe(original)
    expect(
      commentRetryIdentity('source-issue', body, { ...reply, replyTo: 'other-target' }),
    ).not.toBe(original)
    expect(commentRetryIdentity('source-issue', body, { ...reply, kind: 'refute' })).not.toBe(
      original,
    )
    expect(commentRetryIdentity('source-issue', body, { ...reply, force: true })).not.toBe(original)
  }
})
