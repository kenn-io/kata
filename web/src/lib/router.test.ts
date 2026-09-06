import { describe, expect, it } from 'vitest'

import { parseRoute, serializeRoute } from './router'

const issueUID = '01HZNQ7VFPK1XGD8R5MABCD4EX'

describe('canonical Kata routes', () => {
  it('keeps Forge view, scope, and issue state independent', () => {
    const projectUID = '01HZNQ7VFPK1XGD8R5MABCD4EW'
    const route = parseRoute(
      new URL(
        `https://daemon.example/kata?view=all-open&scope=${projectUID}&issue=${issueUID}&label=ready`,
      ),
    )

    expect(route).toMatchObject({
      kind: 'kata',
      view: 'all-open',
      projectUID,
      issueUID,
      graph: false,
    })
    expect(serializeRoute(route)).toBe(
      `/kata?view=all-open&scope=${projectUID}&issue=${issueUID}&label=ready`,
    )
  })

  it('parses only the canonical route families', () => {
    expect(parseRoute(new URL(`https://daemon.example/kata?view=today&label=ready`))).toMatchObject(
      {
        kind: 'kata',
        view: 'today',
      },
    )
    expect(parseRoute(new URL(`https://daemon.example/kata?scope=${issueUID}`))).toMatchObject({
      kind: 'kata',
      projectUID: issueUID,
    })
    expect(
      parseRoute(new URL(`https://daemon.example/kata?issue=${issueUID}&graph=1`)),
    ).toMatchObject({
      kind: 'kata',
      issueUID,
      graph: true,
    })
  })

  it('keeps invalid issue UIDs routed and gives short refs one search action', () => {
    expect(parseRoute(new URL('https://daemon.example/kata?issue=abc4'))).toEqual({
      kind: 'route-error',
      path: '/kata?issue=abc4',
      reason: 'issue_uid',
      searchRef: 'abc4',
    })
    expect(
      parseRoute(new URL('https://daemon.example/kata?issue=not-a-valid-full-uid-value')),
    ).toEqual({
      kind: 'route-error',
      path: '/kata?issue=not-a-valid-full-uid-value',
      reason: 'issue_uid',
    })
  })

  it('serializes shareable filters deterministically', () => {
    const route = parseRoute(
      new URL(
        `https://daemon.example/kata?scope=${issueUID}&label=urgent&status=open&label=backend&owner=user-a`,
      ),
    )
    expect(serializeRoute(route)).toBe(
      `/kata?scope=${issueUID}&label=backend&label=urgent&owner=user-a&status=open`,
    )
  })

  it('parses and serializes a browser application mounted below a path', () => {
    const routePath = '/tools/tasks/'
    const route = parseRoute(
      new URL(`https://daemon.example${routePath}?view=today&label=ready`),
      routePath,
    )

    expect(route).toMatchObject({ kind: 'kata', view: 'today' })
    expect(serializeRoute(route, routePath)).toBe('/tools/tasks/?view=today&label=ready')
  })
})
