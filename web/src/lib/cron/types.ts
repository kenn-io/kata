export type { CronRun, CronJob, CronWorkflow, ShowIssueResponseBody } from '../api/generated'
export const runStatuses = ['running', 'succeeded', 'failed', 'unknown', 'cancelled'] as const
export interface CronProject {
  id: number
  uid: string
  name: string
}
export interface CronSelection {
  kind: 'job' | 'run' | 'workflow'
  projectUID: string
  uid: string
}
export interface CronRouteState {
  tab: 'jobs' | 'runs' | 'workflows'
  projectUIDs: string[]
  statuses: string[]
  tags: string[]
  groupByProject: boolean
  selection?: CronSelection | undefined
}
export interface CronSummary {
  failed_runs_24h: number
  window_start: string
  window_end: string
}
