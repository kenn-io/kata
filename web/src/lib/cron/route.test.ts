import { expect, test } from 'vitest'
import { parseRoute, serializeRoute, screenNameForRoute } from '../router'
import { parseCronRoute, writeCronRoute } from './route'
const uid = '01ARZ3NDEKTSV4RRFFQ69G5FAV'
test('canonical cron route strips issue filters and restores selection', () => {
  const route = parseRoute(
    new URL(
      `https://daemon.example/?view=cron&status=open&cron_tab=runs&cron_project=${uid}&cron_status=failed&cron_status=bad&cron_group=project&cron_kind=run&cron_scope=${uid}&cron_uid=${uid}`,
    ),
    '/',
  )
  expect(route.kind).toBe('kata')
  expect(screenNameForRoute(route)).toBe('cron')
  if (route.kind === 'kata') {
    expect(route.cron?.projectUIDs).toEqual([uid])
    expect(route.cron?.selection).toEqual({ kind: 'run', projectUID: uid, uid })
  }
  expect(serializeRoute(route, '/')).not.toContain('status=open')
  expect(parseRoute(new URL('https://daemon.example' + serializeRoute(route, '/')), '/')).toEqual(
    route,
  )
})
test('normalizes duplicates and rejects invalid selection', () => {
  const value = parseCronRoute(
    new URLSearchParams(
      'cron_tag=z&cron_tag=a&cron_tag=z&cron_project=no&cron_kind=run&cron_scope=no&cron_uid=no',
    ),
  )
  expect(value.tags).toEqual(['a', 'z'])
  expect(value.projectUIDs).toEqual([])
  expect(value.selection).toBeUndefined()
  const q = new URLSearchParams()
  writeCronRoute(q, value)
  expect(parseCronRoute(q)).toEqual(value)
})

import * as hegel from '@hegeldev/hegel'
import * as gs from '@hegeldev/hegel/generators'
test('Unicode tags and valid UIDs round-trip canonically', () =>
  hegel.test(
    (tc) => {
      const tags = tc.draw(gs.arrays(gs.text({ maxSize: 40, codec: 'utf-8' }), { maxSize: 10 }))
      const project = tc.draw(gs.fromRegex('[0-7][0-9A-HJKMNP-TV-Z]{25}'))
      const scope = tc.draw(gs.fromRegex('[0-7][0-9A-HJKMNP-TV-Z]{25}'))
      const entity = tc.draw(gs.fromRegex('[0-7][0-9A-HJKMNP-TV-Z]{25}'))
      const kind = tc.draw(gs.sampledFrom(['job', 'run', 'workflow'] as const))
      const q = new URLSearchParams()
      for (const tag of tags) q.append('cron_tag', tag)
      q.set('cron_project', project)
      q.set('cron_tab', tc.draw(gs.sampledFrom(['jobs', 'runs', 'workflows'])))
      q.set('cron_group', tc.draw(gs.booleans()) ? 'project' : '')
      q.set('cron_kind', kind)
      q.set('cron_scope', scope)
      q.set('cron_uid', entity)
      const state = parseCronRoute(q),
        encoded = new URLSearchParams()
      expect(state.projectUIDs).toEqual([project])
      expect(state.selection).toEqual({ kind, projectUID: scope, uid: entity })
      writeCronRoute(encoded, state)
      expect(parseCronRoute(new URLSearchParams(encoded.toString()))).toEqual(state)
    },
    { testCases: 2000 },
  ))
test('filter order and duplicate occurrences do not affect canonical routes', () =>
  hegel.test(
    (tc) => {
      const tags = tc.draw(gs.arrays(gs.text({ maxSize: 30, codec: 'utf-8' }), { maxSize: 10 }))
      const first = new URLSearchParams(),
        second = new URLSearchParams()
      for (const tag of tags) first.append('cron_tag', tag)
      for (const tag of [...tags, ...tags].reverse()) second.append('cron_tag', tag)
      expect(parseCronRoute(first)).toEqual(parseCronRoute(second))
    },
    { testCases: 2000 },
  ))

test('malformed UID shapes cannot select or scope a cron record', () =>
  hegel.test(
    (tc) => {
      const malformed = '!' + tc.draw(gs.text({ maxSize: 80, codec: 'utf-8' }))
      const q = new URLSearchParams({
        cron_kind: 'run',
        cron_scope: malformed,
        cron_uid: malformed,
        cron_project: malformed,
      })
      expect(parseCronRoute(q).projectUIDs).toEqual([])
      expect(parseCronRoute(q).selection).toBeUndefined()
    },
    { testCases: 2000 },
  ))

test('never_ran is a job status and cannot filter run history', () => {
  expect(
    parseCronRoute(new URLSearchParams('cron_tab=runs&cron_status=never_ran')).statuses,
  ).toEqual([])
  expect(parseCronRoute(new URLSearchParams('cron_status=never_ran')).statuses).toEqual([
    'never_ran',
  ])
})
