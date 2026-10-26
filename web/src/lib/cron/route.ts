import { runStatuses, type CronRouteState } from './types'
export const defaultCronRoute = (): CronRouteState => ({
  tab: 'jobs',
  projectUIDs: [],
  statuses: [],
  tags: [],
  groupByProject: false,
})
const uid = (v: string) => /^[0-7][0-9A-HJKMNP-TV-Z]{25}$/.test(v)
const unique = (v: string[]) => [...new Set(v.map((x) => x.trim()).filter(Boolean))].sort()
export function parseCronRoute(q: URLSearchParams): CronRouteState {
  const state = defaultCronRoute()
  const tab = q.get('cron_tab')
  if (tab === 'runs' || tab === 'workflows') state.tab = tab
  state.projectUIDs = unique(q.getAll('cron_project')).filter(uid)
  state.statuses = unique(q.getAll('cron_status')).filter((s) =>
    [...runStatuses, ...(state.tab === 'jobs' ? ['never_ran'] : [])].includes(s),
  )
  state.tags = unique(q.getAll('cron_tag'))
  state.groupByProject = q.get('cron_group') === 'project'
  const kind = q.get('cron_kind'),
    projectUID = q.get('cron_scope') ?? '',
    id = q.get('cron_uid') ?? ''
  if ((kind === 'job' || kind === 'run' || kind === 'workflow') && uid(projectUID) && uid(id))
    state.selection = { kind, projectUID, uid: id }
  return state
}
export function writeCronRoute(q: URLSearchParams, state: CronRouteState): void {
  if (state.tab !== 'jobs') q.set('cron_tab', state.tab)
  for (const [name, values] of [
    ['cron_project', state.projectUIDs],
    ['cron_status', state.statuses],
    ['cron_tag', state.tags],
  ] as const)
    for (const value of unique(values)) q.append(name, value)
  if (state.groupByProject) q.set('cron_group', 'project')
  if (state.selection) {
    q.set('cron_kind', state.selection.kind)
    q.set('cron_scope', state.selection.projectUID)
    q.set('cron_uid', state.selection.uid)
  }
}
