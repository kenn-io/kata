import { describe, expect, it } from 'vitest'
import { commentEvidenceLength, trimCommentEvidence } from './commentEvidence'

describe('comment evidence normalization', () => {
  it('matches the server Unicode White_Space character floor', () => {
    expect(commentEvidenceLength('é'.repeat(40))).toBe(40)
    expect(commentEvidenceLength('é'.repeat(39) + '\u0085')).toBe(39)
    expect(commentEvidenceLength('\u0085' + 'é'.repeat(39))).toBe(39)
    expect(commentEvidenceLength('é'.repeat(39) + '\uFEFF')).toBe(40)
    expect(commentEvidenceLength(' a\t\n\u0085b ')).toBe(3)
    expect(trimCommentEvidence('\u0085' + 'é'.repeat(39) + '\uFEFF')).toBe(
      'é'.repeat(39) + '\uFEFF',
    )
  })
})
