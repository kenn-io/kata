import type { KataTaskStatusFilter, KataTaskSummary } from './types'

export function kataTaskStatusMatchesFilter(
  issue: Pick<KataTaskSummary, 'uid' | 'status'>,
  filter: KataTaskStatusFilter,
  readyIssueUIDs?: ReadonlySet<string>,
): boolean {
  if (filter === 'all') return true
  if (filter === 'ready') return readyIssueUIDs?.has(issue.uid) === true
  return issue.status === filter
}
