// Go strings.Fields uses Unicode White_Space, which differs from JS trim/\s.
export function commentEvidenceLength(body: string): number {
  const normalized = body
    .split(/\p{White_Space}+/u)
    .filter(Boolean)
    .join(' ')
  return Array.from(normalized).length
}

export function trimCommentEvidence(body: string): string {
  return body.replace(/^\p{White_Space}+|\p{White_Space}+$/gu, '')
}
