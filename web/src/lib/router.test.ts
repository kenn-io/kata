import { describe, expect, it } from 'vitest'

import { parseRoute, serializeRoute, screenNameForRoute } from './router'

const issueUID = '01HZNQ7VFPK1XGD8R5MABCD4EX'

describe('canonical Kata routes', () => {
  it.each([
    ['issue=example%23abc4', { issue: 'example#abc4' }],
    ['scope=example&issue=abc4', { scope: 'example', issue: 'abc4' }],
    [`scope=${issueUID}&issue=abc4`, { scope: issueUID, issue: 'abc4' }],
    [`scope=example&issue=${issueUID}`, { scope: 'example', issue: issueUID }],
    ['scope=%20example%20&issue=abc4', { scope: ' example ', issue: 'abc4' }],
    [`issue=${issueUID}%23abc4`, { issue: `${issueUID}#abc4` }],
  ])('preserves link values in %s', (query, link) => {
    const route = parseRoute(new URL(`https://daemon.example/kata?${query}&graph=1&label=ready`))
    expect(route).toMatchObject({ kind: 'kata', link, graph: true })
    if (route.kind !== 'kata') throw new Error('Expected a Kata route')
    expect(route.link).toEqual(link)
    expect(
      parseRoute(new URL(serializeRoute({ ...route, issueUID }), 'https://daemon.example')),
    ).toEqual(route)
  })
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

  it('parses and serializes the Delegated view', () => {
    const route = parseRoute(new URL('https://daemon.example/kata?view=delegated'))

    expect(route).toMatchObject({ kind: 'kata', view: 'delegated' })
    expect(serializeRoute(route)).toBe('/kata?view=delegated')
  })

  it('parses and serializes the Needs attention view', () => {
    const route = parseRoute(new URL('https://daemon.example/kata?view=attention'))

    expect(route).toMatchObject({ kind: 'kata', view: 'attention' })
    expect(serializeRoute(route)).toBe('/kata?view=attention')
  })

  it('parses and serializes the Scheduled view', () => {
    const route = parseRoute(new URL('https://daemon.example/kata?view=scheduled'))

    expect(route).toMatchObject({ kind: 'kata', view: 'scheduled' })
    expect(serializeRoute(route)).toBe('/kata?view=scheduled')
  })

  it.each(['credentials', 'settings'])(
    'parses %s as a daemon route without carrying issue workspace state',
    (view) => {
      const route = parseRoute(
        new URL(`https://daemon.example/kata?view=${view}&issue=${issueUID}&label=ready`),
      )
      expect(route).toEqual({
        kind: 'kata',
        view,
        graph: false,
        filters: { status: [], owner: [], label: [], relationship: [] },
      })
      expect(serializeRoute(route)).toBe(`/kata?view=${view}`)
    },
  )

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

describe('board layout', () => {
  const projectUID = '01HZNQ7VFPK1XGD8R5MABCD4EW'

  it('round-trips the board layout beside scope, issue, and filters', () => {
    const route = parseRoute(
      new URL(
        `https://daemon.example/kata?layout=board&scope=${projectUID}&issue=${issueUID}&owner=alex`,
      ),
    )

    expect(route).toMatchObject({ kind: 'kata', board: true, projectUID, issueUID, graph: false })
    expect(serializeRoute(route)).toBe(
      `/kata?scope=${projectUID}&issue=${issueUID}&layout=board&owner=alex`,
    )
  })

  it('leaves the list layout out of the URL', () => {
    const route = parseRoute(new URL(`https://daemon.example/kata?scope=${projectUID}`))

    expect(route).not.toHaveProperty('board')
    expect(serializeRoute(route)).toBe(`/kata?scope=${projectUID}`)
  })

  it('ignores layout values other than board', () => {
    expect(parseRoute(new URL('https://daemon.example/kata?layout=grid'))).not.toHaveProperty(
      'board',
    )
  })

  it('never combines the board with the reachable graph', () => {
    const route = parseRoute(
      new URL(`https://daemon.example/kata?layout=board&issue=${issueUID}&graph=1`),
    )

    expect(route).toMatchObject({ board: true, graph: false })
  })
})

describe('screen names', () => {
  it.each([
    ['', 'inbox'],
    ['?view=all-open', 'issues'],
    ['?view=today', 'today'],
    ['?view=delegated', 'delegated'],
    ['?view=scheduled', 'scheduled'],
    ['?view=logbook', 'logbook'],
    ['?view=attention', 'attention'],
    [`?scope=${issueUID}`, 'issues'],
    [`?issue=${issueUID}`, 'issue'],
    [`?issue=${issueUID}&graph=1`, 'graph'],
    ['?layout=board', 'board'],
    [`?scope=${issueUID}&layout=board`, 'board'],
    [`?layout=board&issue=${issueUID}`, 'issue'],
    [`?view=credentials&issue=${issueUID}`, 'credentials'],
    [`?view=settings&issue=${issueUID}`, 'settings'],
    ['?view=unknown', undefined],
  ])('maps %s to %s', (query, screen) => {
    expect(screenNameForRoute(parseRoute(new URL(`https://daemon.example/kata${query}`)))).toBe(
      screen,
    )
  })
})
