import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/svelte'
import { afterEach, describe, expect, test, vi } from 'vitest'

vi.mock('../lib/kata/projection', async (importOriginal) => {
  const original = await importOriginal<typeof import('../lib/kata/projection')>()
  return { ...original, normalizeKataUISnapshot: vi.fn(original.normalizeKataUISnapshot) }
})

import { setGeneratedFetch } from '../lib/api/client'
import { normalizeKataUISnapshot } from '../lib/kata/projection'
import type { MutationContext, MutationResult } from '../lib/mutations/controller'
import type { ShareableFilters } from '../lib/router'
import type { UISnapshot } from '../lib/state/snapshot'
import AppShell from './AppShell.svelte'

describe('AppShell', () => {
  afterEach(() => {
    cleanup()
    vi.unstubAllGlobals()
    vi.useRealTimers()
  })

  test('advances Updated labels while the task snapshot stays unchanged', async () => {
    vi.useFakeTimers()
    vi.setSystemTime(new Date('2026-08-01T12:00:16Z'))
    const current = snapshot()
    current.collection[0]!.updated_at = '2026-08-01T12:00:16Z'
    current.catalog[0]!.project.metadata = { role: 'inbox' }
    render(AppShell, {
      props: {
        route: {
          kind: 'kata',
          view: 'inbox',
          graph: false,
          filters: { status: [], owner: [], label: [], relationship: [] },
        },
        snapshot: current,
        loading: false,
        ...mutationProps(),
        onNavigate: vi.fn(),
        onCreateProject: vi.fn(async () => ({ changed: true })),
      },
    })
    expect(screen.getByText('now')).not.toBeNull()
    const normalizations = vi.mocked(normalizeKataUISnapshot).mock.calls.length
    expect(normalizations).toBeGreaterThan(0)
    await vi.advanceTimersByTimeAsync(45_000)
    expect(screen.getByText('1m')).not.toBeNull()
    expect(normalizeKataUISnapshot).toHaveBeenCalledTimes(normalizations)
  })

  test('moves a scheduled task into Inbox at midnight without a new snapshot', async () => {
    vi.useFakeTimers()
    vi.setSystemTime(new Date(2026, 7, 1, 23, 59, 59))
    const current = snapshot()
    current.catalog[0]!.project.metadata = { role: 'inbox' }
    current.collection[0]!.metadata = { scheduled_on: '2026-08-02' }
    render(AppShell, {
      props: {
        route: {
          kind: 'kata',
          view: 'inbox',
          graph: false,
          filters: { status: [], owner: [], label: [], relationship: [] },
        },
        snapshot: current,
        loading: false,
        ...mutationProps(),
        onNavigate: vi.fn(),
        onCreateProject: vi.fn(async () => ({ changed: true })),
      },
    })
    expect(screen.queryByRole('button', { name: /Example issue/ })).toBeNull()
    const normalizations = vi.mocked(normalizeKataUISnapshot).mock.calls.length
    expect(normalizations).toBeGreaterThan(0)
    await vi.advanceTimersByTimeAsync(1000)
    expect(screen.getByRole('button', { name: /Example issue/ })).not.toBeNull()
    expect(normalizeKataUISnapshot).toHaveBeenCalledTimes(normalizations)
  })

  test('identifies the current browser hub when a connection needs attention', () => {
    vi.stubGlobal('location', new URL('https://daemon.example/workspace?token=never-display'))
    render(AppShell, {
      props: {
        route: {
          kind: 'kata',
          view: 'all-open',
          graph: false,
          filters: { status: [], owner: [], label: [], relationship: [] },
        },
        snapshot: snapshot(),
        loading: false,
        readOnly: true,
        ...mutationProps(),
        onNavigate: vi.fn(),
        onCreateProject: vi.fn(async () => ({ changed: true })),
      },
    })
    const summary = screen.getByRole('status', { name: 'Active connection' })
    expect(summary.textContent).toContain('Hub: daemon.example')
    expect(summary.textContent).not.toContain('Local daemon')
    expect(summary.textContent).not.toContain('never-display')
  })

  test('offers project sync status from the current project without credential inventory authority', () => {
    setGeneratedFetch(vi.fn(async () => Response.json({ statuses: [] })))
    const current = snapshot()
    render(AppShell, {
      props: {
        route: {
          kind: 'kata',
          projectUID: current.catalog[0]!.project.uid,
          graph: false,
          filters: { status: [], owner: [], label: [], relationship: [] },
        },
        snapshot: current,
        loading: false,
        ...mutationProps(),
        onNavigate: vi.fn(),
        onCreateProject: vi.fn(async () => ({ changed: true })),
      },
    })
    expect(screen.getByRole('region', { name: 'Project sync' })).not.toBeNull()
    expect(screen.queryByRole('button', { name: 'Credentials' })).toBeNull()
  })

  test('keeps project sync readable for a current read-only project member', () => {
    setGeneratedFetch(vi.fn(async () => Response.json({ statuses: [] })))
    const current = snapshot()
    current.capabilities.writable = false
    render(AppShell, {
      props: {
        route: {
          kind: 'kata',
          projectUID: current.catalog[0]!.project.uid,
          graph: false,
          filters: { status: [], owner: [], label: [], relationship: [] },
        },
        snapshot: current,
        loading: false,
        readOnly: true,
        ...mutationProps({ canMutate: false }),
        onNavigate: vi.fn(),
        onCreateProject: vi.fn(async () => ({ changed: true })),
      },
    })
    expect(screen.getByRole('region', { name: 'Project sync' })).not.toBeNull()
  })

  test('explains a read-only credential at each blocked write control', async () => {
    const current = snapshot()
    current.capabilities.writable = false
    render(AppShell, {
      props: {
        route: {
          kind: 'kata',
          view: 'all-open',
          graph: false,
          filters: { status: [], owner: [], label: [], relationship: [] },
        },
        snapshot: current,
        loading: false,
        readOnly: true,
        ...mutationProps({ canMutate: false }),
        onNavigate: vi.fn(),
        onCreateProject: vi.fn(async () => ({ changed: true })),
      },
    })

    expect(screen.getByRole('status', { name: 'Active connection' }).textContent).toContain(
      'Credential: Read-only',
    )
    expect(screen.queryByText('This Kata session is read-only.')).toBeNull()

    const reason = 'This credential is read-only.'
    const newTask = screen.getByRole('button', { name: 'New task', description: reason })
    expect(newTask.getAttribute('aria-disabled')).toBe('true')
    newTask.focus()
    expect(document.activeElement).toBe(newTask)
    expect((await screen.findByRole('tooltip')).textContent?.trim()).toBe(reason)
    await fireEvent.click(newTask)
    expect(screen.queryByRole('textbox', { name: 'Quick capture' })).toBeNull()
    expect(screen.queryByRole('button', { name: /Use example-project as Inbox/ })).toBeNull()

    const newProject = screen.getByRole('button', { name: 'New project', description: reason })
    expect(newProject.getAttribute('aria-disabled')).toBe('true')
    await fireEvent.click(newProject)
    expect(screen.queryByRole('textbox', { name: 'New project name' })).toBeNull()
  })

  test('reports stale data in the connection summary and at blocked write controls', () => {
    render(AppShell, {
      props: {
        route: {
          kind: 'kata',
          view: 'all-open',
          graph: false,
          filters: { status: [], owner: [], label: [], relationship: [] },
        },
        snapshot: snapshot(),
        loading: false,
        stale: true,
        ...mutationProps({ canMutate: false }),
        onNavigate: vi.fn(),
        onCreateProject: vi.fn(async () => ({ changed: true })),
      },
    })

    const summary = screen.getByRole('status', { name: 'Active connection' })
    expect(summary.textContent).toContain('Workspace data may be out of date. Editing is paused.')
    const reason = 'Editing is paused while workspace data may be out of date.'
    expect(
      screen
        .getByRole('button', { name: 'New task', description: reason })
        .getAttribute('aria-disabled'),
    ).toBe('true')
    expect(screen.getByRole('button', { name: 'New project', description: reason })).not.toBeNull()
  })

  test.each(['stale', 'daemonSwitching', 'reconnecting', 'remote', 'expired', 'removed'])(
    'fences project sync display when authority is %s',
    async (reason) => {
      let signal: AbortSignal | undefined
      const fetcher = vi.fn<typeof fetch>(async (_url, init) => {
        signal = init?.signal ?? undefined
        return Response.json({ statuses: [] })
      })
      setGeneratedFetch(fetcher)
      const current = snapshot()
      const props = {
        route: {
          kind: 'kata' as const,
          projectUID: current.catalog[0]!.project.uid,
          graph: false,
          filters: { status: [], owner: [], label: [], relationship: [] },
        },
        snapshot: current,
        loading: false,
        ...mutationProps(),
        onNavigate: vi.fn(),
        onCreateProject: vi.fn(async () => ({ changed: true })),
      }
      const view = render(AppShell, { props })
      await waitFor(() => expect(fetcher).toHaveBeenCalledTimes(1))
      const next = snapshot()
      if (reason === 'expired') next.capabilities.expires_at = '2020-01-01T00:00:00Z'
      if (reason === 'removed') next.catalog = []
      await view.rerender({
        ...props,
        snapshot: next,
        stale: reason === 'stale',
        daemonSwitching: reason === 'daemonSwitching',
        reconnecting: reason === 'reconnecting',
        activeDaemonID: reason === 'remote' ? 'other-hub' : undefined,
        daemons:
          reason === 'remote'
            ? [
                {
                  id: 'other-hub',
                  url: 'https://other.example',
                  default: true,
                  auth: 'token',
                  health: 'connected',
                  local: false,
                },
              ]
            : [],
      })
      expect(screen.queryByRole('region', { name: 'Project sync' })).toBeNull()
      expect(signal?.aborted).toBe(true)
      expect(fetcher).toHaveBeenCalledTimes(1)
    },
  )

  test('keeps project sync in place while the snapshot reloads after navigation', async () => {
    setGeneratedFetch(vi.fn(async () => Response.json({ statuses: [] })))
    const current = snapshot()
    const props = {
      route: {
        kind: 'kata' as const,
        projectUID: current.catalog[0]!.project.uid,
        graph: false,
        filters: { status: [], owner: [], label: [], relationship: [] },
      },
      snapshot: current,
      loading: false,
      ...mutationProps(),
      onNavigate: vi.fn(),
      onCreateProject: vi.fn(async () => ({ changed: true })),
    }
    const view = render(AppShell, { props })
    expect(screen.getByRole('region', { name: 'Project sync' })).not.toBeNull()

    await view.rerender({ ...props, loading: true })
    expect(screen.getByRole('region', { name: 'Project sync' })).not.toBeNull()
  })

  test('keeps a healthy connection hidden during refresh while preserving attention states', async () => {
    const props = {
      route: {
        kind: 'kata' as const,
        view: 'all-open' as const,
        graph: false,
        filters: { status: [], owner: [], label: [], relationship: [] },
      },
      snapshot: snapshot(),
      loading: false,
      ...mutationProps(),
      onNavigate: vi.fn(),
      onCreateProject: vi.fn(async () => ({ changed: true })),
    }
    const view = render(AppShell, { props })
    expect(screen.queryByRole('status', { name: 'Active connection' })).toBeNull()
    await view.rerender({ ...props, loading: true, canMutate: false })
    expect(screen.queryByRole('status', { name: 'Active connection' })).toBeNull()
    expect(screen.getByRole('button', { name: 'New task' }).getAttribute('aria-disabled')).toBe(
      'true',
    )

    for (const [state, label] of [
      ['stale', 'Connection unavailable'],
      ['reconnecting', 'Reconnecting'],
      ['switching', 'Connecting'],
      ['authentication', 'Authentication required'],
      ['readonly', 'Read-only'],
    ] as const) {
      await view.rerender({
        ...props,
        loading: true,
        stale: state === 'stale',
        reconnecting: state === 'reconnecting',
        daemonSwitching: state === 'switching',
        daemonError: state === 'authentication' ? 'Authentication required' : undefined,
        readOnly: state === 'readonly',
      })
      expect(screen.getByRole('status', { name: 'Active connection' }).textContent).toContain(label)
    }
  })

  test('keeps the healthy workspace toolbar free of a connection banner', () => {
    const { container } = render(AppShell, {
      props: {
        route: {
          kind: 'kata',
          view: 'all-open',
          graph: false,
          filters: { status: [], owner: [], label: [], relationship: [] },
        },
        snapshot: snapshot(),
        loading: false,
        ...mutationProps(),
        onNavigate: vi.fn(),
        onCreateProject: vi.fn(async () => ({ changed: true })),
      },
    })

    expect(screen.queryByRole('status', { name: 'Active connection' })).toBeNull()
    const header = container.querySelector('header.kata-header')
    expect(header).not.toBeNull()
    expect(header?.classList.contains('kit-top-bar')).toBe(true)
    expect(within(header as HTMLElement).getByRole('heading', { name: 'Kata' })).not.toBeNull()
    expect(within(header as HTMLElement).getByRole('button', { name: 'New task' })).not.toBeNull()
  })

  test.each([
    ['static_token', 'Shared daemon token'],
    ['bootstrap', 'Bootstrap token'],
    ['insecure_readonly', 'Anonymous (read-only)'],
    ['unauthenticated_private_network', 'Unauthenticated private network'],
    ['trusted_proxy_absent', 'Proxy without account'],
    ['web_local', 'Local connection'],
    ['none', 'Local connection'],
  ])('labels an actorless %s connection as %s', (authKind, label) => {
    const current = snapshot()
    Object.assign(current.capabilities, { auth_kind: authKind })
    render(AppShell, {
      props: {
        route: {
          kind: 'kata',
          view: 'all-open',
          graph: false,
          filters: { status: [], owner: [], label: [], relationship: [] },
        },
        snapshot: current,
        loading: false,
        readOnly: true,
        ...mutationProps(),
        onNavigate: vi.fn(),
        onCreateProject: vi.fn(async () => ({ changed: true })),
      },
    })
    const summary = screen.getByRole('status', { name: 'Active connection' })
    expect(summary.textContent).toContain(`Account: ${label}`)
  })

  test('says a hub that predates account reporting did not report one', () => {
    const current = snapshot()
    current.capabilities.writable = false
    delete current.capabilities.auth_kind
    render(AppShell, {
      props: {
        route: {
          kind: 'kata',
          view: 'all-open',
          graph: false,
          filters: { status: [], owner: [], label: [], relationship: [] },
        },
        snapshot: current,
        loading: false,
        ...mutationProps({ canMutate: false }),
        onNavigate: vi.fn(),
        onCreateProject: vi.fn(async () => ({ changed: true })),
      },
    })
    const summary = screen.getByRole('status', { name: 'Active connection' })
    expect(summary.textContent).toContain('Account: Not reported by hub')
    expect(summary.textContent).not.toContain('No account')
  })

  test('shows only the active hub account and clears identity while switching', async () => {
    const current = snapshot()
    Object.assign(current.capabilities, { account: 'member-one', token_audit_read: false })
    const props = {
      route: {
        kind: 'kata' as const,
        view: 'all-open' as const,
        graph: false,
        filters: { status: [], owner: [], label: [], relationship: [] },
      },
      snapshot: current,
      loading: false,
      readOnly: true,
      ...mutationProps(),
      onNavigate: vi.fn(),
      onCreateProject: vi.fn(async () => ({ changed: true })),
      daemons: [
        {
          id: 'hub-one',
          url: 'https://daemon.example',
          default: true,
          auth: 'token' as const,
          health: 'connected' as const,
        },
      ],
      activeDaemonID: 'hub-one',
    }
    const view = render(AppShell, { props })
    const summary = screen.getByRole('status', { name: 'Active connection' })
    expect(summary.textContent).toContain('hub-one')
    expect(summary.textContent).toContain('member-one')
    expect(summary.textContent).toContain('Read-only')
    expect(screen.queryByRole('button', { name: 'Credentials' })).toBeNull()
    await view.rerender({ ...props, daemonSwitching: true })
    expect(summary.textContent).toContain('Connecting')
    expect(summary.textContent).not.toContain('member-one')
    const next = snapshot()
    Object.assign(next.capabilities, { account: 'member-two', token_audit_read: false })
    await view.rerender({
      ...props,
      snapshot: next,
      activeDaemonID: 'hub-two',
      daemons: [{ ...props.daemons[0]!, id: 'hub-two' }],
      daemonSwitching: false,
    })
    expect(summary.textContent).toContain('hub-two')
    expect(summary.textContent).toContain('member-two')
    expect(summary.textContent).not.toContain('member-one')
    await view.rerender({ ...props, stale: true, reconnecting: true })
    expect(summary.textContent).toContain('Reconnecting')
    expect(summary.textContent).not.toContain('member-one')
  })

  test('active connection expires without requiring credential inventory', async () => {
    vi.useFakeTimers()
    vi.setSystemTime(new Date('2026-01-01T00:00:00Z'))
    const current = snapshot()
    Object.assign(current.capabilities, {
      account: 'expiring-member',
      expires_at: '2026-01-01T00:00:02Z',
      token_audit_read: false,
    })
    const props = {
      route: {
        kind: 'kata' as const,
        view: 'all-open' as const,
        graph: false,
        filters: { status: [], owner: [], label: [], relationship: [] },
      },
      snapshot: current,
      loading: false,
      ...mutationProps(),
      onNavigate: vi.fn(),
      onCreateProject: vi.fn(async () => ({ changed: true })),
    }
    const view = render(AppShell, { props })
    expect(screen.queryByRole('status', { name: 'Active connection' })).toBeNull()
    await vi.advanceTimersByTimeAsync(3000)
    const summary = screen.getByRole('status', { name: 'Active connection' })
    expect(summary.textContent).toContain('Credential expired')
    await view.rerender({ ...props, stale: true, daemonError: 'Authentication required' })
    expect(summary.textContent).toContain('Authentication required')
    expect(summary.textContent).not.toContain('expiring-member')
  })

  test('connects the ported navigation, filters, and collection to canonical routes', async () => {
    const onNavigate = vi.fn()
    render(AppShell, {
      props: {
        route: {
          kind: 'kata',
          view: 'all-open',
          graph: false,
          filters: { status: [], owner: [], label: [], relationship: [] },
        },
        snapshot: snapshot(),
        loading: false,
        ...mutationProps(),
        onNavigate,
        onCreateProject: vi.fn(async () => ({ changed: true })),
      },
    })

    expect(screen.getByRole('button', { name: /Example issue/ })).not.toBeNull()
    expect(
      within(screen.getByRole('region', { name: 'Kata navigation' })).getByRole('button', {
        name: /example-project/,
      }),
    ).not.toBeNull()

    await fireEvent.click(screen.getByRole('button', { name: 'Today' }))
    expect(onNavigate).toHaveBeenCalledWith({
      kind: 'kata',
      view: 'today',
      graph: false,
      filters: { status: [], owner: [], label: [], relationship: [] },
    })

    await fireEvent.input(screen.getByLabelText('Search tasks'), { target: { value: 'example' } })
    await waitFor(() =>
      expect(onNavigate).toHaveBeenCalledWith({
        kind: 'kata',
        view: 'all-open',
        graph: false,
        filters: { status: [], owner: [], label: [], relationship: [], text: 'example' },
      }),
    )

    await fireEvent.click(screen.getByRole('button', { name: 'Open reachable graph' }))
    expect(onNavigate).toHaveBeenCalledWith({
      kind: 'kata',
      view: 'all-open',
      issueUID: '01J00000000000000000000001',
      graph: true,
      filters: { status: [], owner: [], label: [], relationship: [] },
    })

    await fireEvent.click(screen.getByRole('button', { name: /Example issue/ }))
    expect(onNavigate).toHaveBeenCalledWith({
      kind: 'kata',
      view: 'all-open',
      issueUID: '01J00000000000000000000001',
      graph: false,
      filters: { status: [], owner: [], label: [], relationship: [] },
    })
  })

  test.each([
    ['Example issue', false],
    ['Open reachable graph', true],
  ])('keeps a pending search when opening %s', async (button, graph) => {
    const route = {
      kind: 'kata' as const,
      view: 'all-open' as const,
      graph: false,
      filters: { status: [], owner: [], label: [], relationship: [] },
    }
    const onNavigate = vi.fn()
    const props = {
      route,
      snapshot: snapshot(),
      loading: false,
      ...mutationProps(),
      onNavigate,
      onCreateProject: vi.fn(async () => ({ changed: true })),
    }
    const { rerender } = render(AppShell, { props })
    await fireEvent.input(screen.getByLabelText('Search tasks'), { target: { value: 'example' } })
    await rerender({ ...props, requestedRoute: onNavigate.mock.lastCall![0] })
    await fireEvent.click(screen.getByRole('button', { name: new RegExp(button) }))
    expect(onNavigate).toHaveBeenLastCalledWith({
      ...route,
      issueUID: '01J00000000000000000000001',
      graph,
      filters: { ...route.filters, text: 'example' },
    })
  })

  test.each(['Settings', 'Credentials'])('opens a task while %s is pending', async (screenName) => {
    const current = snapshot()
    current.capabilities.token_audit_read = true
    const route = {
      kind: 'kata' as const,
      view: 'all-open' as const,
      graph: false,
      filters: { status: [], owner: [], label: [], relationship: [] },
    }
    const onNavigate = vi.fn()
    const props = {
      route,
      snapshot: current,
      loading: false,
      ...mutationProps(),
      onNavigate,
      onCreateProject: vi.fn(async () => ({ changed: true })),
    }
    const { rerender } = render(AppShell, { props })
    await fireEvent.click(screen.getByRole('button', { name: screenName }))
    await rerender({ ...props, requestedRoute: onNavigate.mock.lastCall![0] })
    await fireEvent.click(screen.getByRole('button', { name: /Example issue/ }))
    expect(onNavigate).toHaveBeenLastCalledWith({
      ...route,
      issueUID: '01J00000000000000000000001',
    })
  })

  const project = '01J00000000000000000000002'
  const issue = '01J00000000000000000000001'
  const repeatedFilters = {
    status: ['open', 'ready'],
    owner: ['user-a', 'user-b'],
    label: ['backend', 'frontend'],
    relationship: ['blocks'],
  }
  test.each([
    ['same project', 'all-open', project, project, issue, true, repeatedFilters],
    ['different project', 'all-open', project, 'other-project', undefined, true, {}],
    ['implicit Inbox', undefined, undefined, undefined, issue, true, {}],
    ['accepted filters', 'all-open', undefined, undefined, undefined, false, repeatedFilters],
  ] as const)(
    'search preserves the %s',
    async (_name, view, projectUID, requestedProjectUID, issueUID, hasRequest, pending) => {
      const onNavigate = vi.fn()
      const requestedView = view ?? 'inbox'
      const filters = {
        status: [],
        owner: [],
        label: [],
        relationship: [],
        ...pending,
      } as ShareableFilters
      const route = {
        kind: 'kata' as const,
        ...(view ? { view } : {}),
        ...(projectUID ? { projectUID } : {}),
        graph: false,
        filters: hasRequest ? { status: [], owner: [], label: [], relationship: [] } : filters,
      }
      render(AppShell, {
        props: {
          route,
          ...(hasRequest
            ? {
                requestedRoute: {
                  ...route,
                  view: requestedView,
                  ...(requestedProjectUID ? { projectUID: requestedProjectUID } : {}),
                  issueUID: issue,
                  filters,
                },
              }
            : {}),
          snapshot: snapshot(),
          loading: false,
          ...mutationProps(),
          onNavigate,
          onCreateProject: vi.fn(async () => ({ changed: true })),
        },
      })
      await fireEvent.input(screen.getByLabelText('Search tasks'), { target: { value: 'example' } })
      expect(onNavigate).toHaveBeenLastCalledWith(
        expect.objectContaining({ filters: { ...filters, text: 'example' } }),
      )
      expect(onNavigate.mock.lastCall?.[0].projectUID).toBe(projectUID)
      expect(onNavigate.mock.lastCall?.[0].issueUID).toBe(issueUID)
      expect(onNavigate.mock.lastCall?.[0].view).toBe(requestedView)
    },
  )

  test('keeps the requested query when an older snapshot arrives', async () => {
    const route = {
      kind: 'kata' as const,
      view: 'all-open' as const,
      graph: false,
      filters: { status: [], owner: [], label: [], relationship: [] },
    }
    const props = {
      route,
      snapshot: snapshot(),
      loading: false,
      ...mutationProps(),
      onNavigate: vi.fn(),
      onCreateProject: vi.fn(async () => ({ changed: true })),
    }
    const { rerender } = render(AppShell, { props })
    const input = screen.getByLabelText('Search tasks') as HTMLInputElement
    await fireEvent.input(input, { target: { value: 'second ' } })
    await fireEvent.input(input, { target: { value: 'second t' } })
    await rerender({
      ...props,
      route: { ...route, filters: { ...route.filters, text: 'second' } },
      requestedRoute: { ...route, filters: { ...route.filters, text: 'second t' } },
    })
    expect(input.value).toBe('second t')
  })

  test('stays in the landing Inbox while selecting, graphing, and narrowing', async () => {
    const onNavigate = vi.fn()
    const landing = {
      kind: 'kata' as const,
      graph: false,
      filters: { status: [], owner: [], label: [], relationship: [] },
    }
    render(AppShell, {
      props: {
        route: landing,
        snapshot: snapshot(),
        loading: false,
        ...mutationProps(),
        onNavigate,
        onCreateProject: vi.fn(async () => ({ changed: true })),
      },
    })

    await fireEvent.click(screen.getByRole('button', { name: /Example issue/ }))
    expect(onNavigate).toHaveBeenLastCalledWith({
      ...landing,
      view: 'inbox',
      issueUID: '01J00000000000000000000001',
    })

    await fireEvent.click(screen.getByRole('button', { name: 'Open reachable graph' }))
    expect(onNavigate).toHaveBeenLastCalledWith({
      ...landing,
      view: 'inbox',
      issueUID: '01J00000000000000000000001',
      graph: true,
    })

    await fireEvent.click(screen.getByRole('button', { name: /Project scope: All projects/i }))
    await fireEvent.mouseDown(screen.getByRole('option', { name: /example-project/ }))
    await waitFor(() =>
      expect(onNavigate).toHaveBeenLastCalledWith({
        ...landing,
        view: 'inbox',
        projectUID: '01J00000000000000000000002',
      }),
    )
  })

  test('identity bootstrap administers access while ordinary issue writes remain disabled', async () => {
    setGeneratedFetch(async () => Response.json({ teams: [] }))
    const onAccessMutation = vi.fn(executeAccessMutation)
    const props = accessProps(onAccessMutation)
    Object.assign(props.snapshot.capabilities, { access_admin: true, writable: false })
    render(AppShell, { props: { ...props, canMutate: false, readOnly: true } })
    expect(screen.getByRole('heading', { name: 'Teams and visibility' })).not.toBeNull()
    await fireEvent.input(screen.getByLabelText('New team name'), {
      target: { value: 'Example team' },
    })
    await waitFor(() =>
      expect(
        (screen.getByRole('button', { name: 'Create team' }) as HTMLButtonElement).disabled,
      ).toBe(false),
    )
    expect(screen.queryByRole('button', { name: 'New task' })).toBeNull()
  })

  test('offers team and visibility management in the authorized credential screen', () => {
    const authorized = snapshot()
    authorized.capabilities.token_audit_read = true
    authorized.capabilities.access_admin = true
    render(AppShell, {
      props: {
        route: {
          kind: 'kata',
          view: 'credentials',
          graph: false,
          filters: { status: [], owner: [], label: [], relationship: [] },
        },
        snapshot: authorized,
        loading: false,
        ...mutationProps(),
        onNavigate: vi.fn(),
        onCreateProject: vi.fn(async () => ({ changed: true })),
      },
    })
    expect(screen.getByRole('heading', { name: 'Teams and visibility' })).not.toBeNull()
  })

  test('creates a team through the existing mutation authority and refreshes the list', async () => {
    const calls: { url: string; init: RequestInit | undefined }[] = []
    let created = false
    setGeneratedFetch(async (input, init) => {
      calls.push({ url: String(input), init })
      if (init?.method === 'POST') {
        created = true
        return Response.json({ team: { uid: 'team-one', name: 'Example team', revision: 1 } })
      }
      return Response.json({
        teams: created ? [{ uid: 'team-one', name: 'Example team', revision: 1 }] : [],
      })
    })
    const onAccessMutation = vi.fn(async (_draft, mutate) => {
      const result = await mutate({ headers: new Headers({ 'X-Test-Authority': 'current' }) })
      return result.status === 200
    })
    const current = snapshot()
    current.capabilities.token_audit_read = true
    current.capabilities.access_admin = true
    render(AppShell, {
      props: {
        route: {
          kind: 'kata',
          view: 'credentials',
          graph: false,
          filters: { status: [], owner: [], label: [], relationship: [] },
        },
        snapshot: current,
        loading: false,
        ...mutationProps(),
        onAccessMutation,
        onNavigate: vi.fn(),
        onCreateProject: vi.fn(async () => ({ changed: true })),
      },
    })
    await fireEvent.input(screen.getByLabelText('New team name'), {
      target: { value: 'Example team' },
    })
    await waitFor(() =>
      expect(
        (screen.getByRole('button', { name: 'Create team' }) as HTMLButtonElement).disabled,
      ).toBe(false),
    )
    await fireEvent.click(screen.getByRole('button', { name: 'Create team' }))
    await waitFor(() => expect(screen.getByRole('option', { name: 'Example team' })).not.toBeNull())
    expect(onAccessMutation).toHaveBeenCalledOnce()
    const write = calls.find((call) => call.init?.method === 'POST')!
    expect(new URL(write.url).pathname).toBe('/api/v1/teams')
    expect(JSON.parse(String(write.init?.body))).toEqual({ name: 'Example team' })
    expect(new Headers(write.init?.headers).get('X-Test-Authority')).toBe('current')
    expect(calls.filter((call) => call.init?.method === 'GET').length).toBeGreaterThanOrEqual(2)
  })

  test.each([false, true])('recovers a teams read failure, exhausted: %s', async (exhausted) => {
    vi.useFakeTimers()
    let reads = 0
    setGeneratedFetch(async () => {
      reads++
      return reads <= (exhausted ? 4 : 1)
        ? new Response('', { status: 503 })
        : Response.json({ teams: [{ uid: 'team-one', name: 'Example team', revision: 1 }] })
    })
    render(AppShell, { props: accessProps(executeAccessMutation) })
    await vi.advanceTimersByTimeAsync(0)
    expect(
      (screen.getByRole('button', { name: 'Refresh teams' }) as HTMLButtonElement).disabled,
    ).toBe(true)
    await vi.advanceTimersByTimeAsync(exhausted ? 7000 : 1000)
    if (exhausted) {
      expect(screen.getByText('Unable to read teams. Refresh to try again.')).not.toBeNull()
      await fireEvent.click(screen.getByRole('button', { name: 'Refresh teams' }))
      await vi.advanceTimersByTimeAsync(0)
    }
    expect(screen.getByRole('option', { name: 'Example team' })).not.toBeNull()
    expect(screen.queryByText('Unable to read teams. Refresh to try again.')).toBeNull()
    expect(reads).toBe(exhausted ? 5 : 2)
  })

  test('manages selected team membership and deletion through owner mutation authority', async () => {
    const writes: { path: string; method: string }[] = []
    let members = ['existing-member']
    let deleted = false
    setGeneratedFetch(async (input, init) => {
      const path = new URL(String(input)).pathname
      if (init?.method !== 'GET') {
        writes.push({ path, method: init!.method! })
        if (path.endsWith('/members/new-member')) members = [...members, 'new-member']
        else if (path.endsWith('/members/existing-member'))
          members = members.filter((member) => member !== 'existing-member')
        else deleted = true
        return Response.json({ changed: true })
      }
      return Response.json(
        path === '/api/v1/teams'
          ? { teams: deleted ? [] : [{ uid: 'team-one', name: 'Example team', revision: 1 }] }
          : { team: { uid: 'team-one', name: 'Example team', revision: 1 }, members },
      )
    })
    vi.stubGlobal('confirm', () => true)
    render(AppShell, { props: accessProps(executeAccessMutation) })
    await screen.findByRole('option', { name: 'Example team' })
    await fireEvent.change(screen.getByLabelText('Team'), { target: { value: 'team-one' } })
    await screen.findByText('existing-member')
    await fireEvent.input(screen.getByLabelText('Member account'), {
      target: { value: 'new-member' },
    })
    await fireEvent.click(screen.getByRole('button', { name: 'Add member' }))
    await screen.findByText('new-member')
    await fireEvent.click(screen.getByRole('button', { name: 'Remove existing-member' }))
    await waitFor(() => expect(screen.queryByText('existing-member')).toBeNull())
    await fireEvent.click(screen.getByRole('button', { name: 'Delete team' }))
    await waitFor(() => expect(screen.queryByRole('option', { name: 'Example team' })).toBeNull())
    expect(writes).toEqual([
      { path: '/api/v1/teams/team-one/members/new-member', method: 'PUT' },
      { path: '/api/v1/teams/team-one/members/existing-member', method: 'DELETE' },
      { path: '/api/v1/teams/team-one', method: 'DELETE' },
    ])
  })

  test('disables team actions during refresh and removes a deleted team', async () => {
    vi.useFakeTimers()
    let deletes = 0
    let refreshFailed = false
    setGeneratedFetch(async (input, init) => {
      const path = new URL(String(input)).pathname
      if (init?.method === 'DELETE') {
        deletes++
        return Response.json({ changed: true })
      }
      if (path === '/api/v1/teams') {
        if (deletes || refreshFailed) return new Response('', { status: 503 })
        return Response.json({ teams: [{ uid: 'team-one', name: 'Example team', revision: 1 }] })
      }
      return Response.json({
        team: { uid: 'team-one', name: 'Example team', revision: 1 },
        members: ['user-a'],
      })
    })
    vi.stubGlobal('confirm', () => true)
    render(AppShell, { props: accessProps(executeAccessMutation) })
    await vi.advanceTimersByTimeAsync(0)
    await fireEvent.change(screen.getByLabelText('Team'), { target: { value: 'team-one' } })
    await vi.advanceTimersByTimeAsync(0)
    refreshFailed = true
    await fireEvent.click(screen.getByRole('button', { name: 'Refresh teams' }))
    await vi.advanceTimersByTimeAsync(0)
    expect(
      (screen.getByRole('button', { name: 'Delete team' }) as HTMLButtonElement).disabled,
    ).toBe(true)
    expect(
      (screen.getByRole('button', { name: 'Remove user-a' }) as HTMLButtonElement).disabled,
    ).toBe(true)
    refreshFailed = false
    await vi.advanceTimersByTimeAsync(1000)
    await fireEvent.click(screen.getByRole('button', { name: 'Delete team' }))
    await vi.advanceTimersByTimeAsync(0)
    expect(deletes).toBe(1)
    expect(screen.queryByRole('button', { name: 'Delete team' })).toBeNull()
    await vi.advanceTimersByTimeAsync(7000)
    expect(screen.queryByRole('option', { name: 'Example team' })).toBeNull()
  })

  test('reloads the selected project policy after deleting one of its teams', async () => {
    const writes: unknown[] = []
    let deleted = false
    let policyReads = 0
    setGeneratedFetch(async (input, init) => {
      const path = new URL(String(input)).pathname
      if (init?.method === 'DELETE' && path === '/api/v1/teams/team-one') {
        deleted = true
        return Response.json({ changed: true })
      }
      if (init?.method === 'PUT') {
        writes.push(JSON.parse(String(init.body)))
        return Response.json({ changed: true })
      }
      if (path === '/api/v1/teams')
        return Response.json({
          teams: deleted ? [] : [{ uid: 'team-one', name: 'Example team', revision: 1 }],
        })
      if (path === '/api/v1/teams/team-one')
        return Response.json({
          team: { uid: 'team-one', name: 'Example team', revision: 1 },
          members: [],
        })
      if (path === '/api/v1/projects/7/access') {
        policyReads++
        return Response.json({
          policy: deleted
            ? { project_uid: 'example-project', visibility: 'teams', team_uids: [], revision: 5 }
            : {
                project_uid: 'example-project',
                visibility: 'teams',
                team_uids: ['team-one'],
                revision: 4,
              },
        })
      }
      throw new Error(`Unexpected request: ${init?.method ?? 'GET'} ${path}`)
    })
    vi.stubGlobal('confirm', () => true)
    render(AppShell, { props: accessProps(executeAccessMutation) })
    await screen.findByRole('option', { name: 'Example team' })
    await fireEvent.change(screen.getByLabelText('Team'), { target: { value: 'team-one' } })
    await fireEvent.change(screen.getByLabelText('Project visibility'), { target: { value: '7' } })
    await waitFor(() =>
      expect((screen.getByLabelText('Visibility') as HTMLSelectElement).disabled).toBe(false),
    )

    await fireEvent.click(screen.getByRole('button', { name: 'Delete team' }))
    await waitFor(() => expect(screen.queryByRole('option', { name: 'Example team' })).toBeNull())
    // Deleting the last team keeps the project restricted to teams; only the
    // policy revision moves, so the refreshed form must not widen visibility.
    await waitFor(() => expect(policyReads).toBe(2))
    const save = screen.getByRole('button', { name: 'Save visibility' })
    await waitFor(() => expect((save as HTMLButtonElement).disabled).toBe(false))
    expect((screen.getByLabelText('Visibility') as HTMLSelectElement).value).toBe('teams')

    await fireEvent.click(save)
    await waitFor(() => expect(writes).toHaveLength(1))
    expect(writes[0]).toEqual({ visibility: 'teams', team_uids: [], revision: 5 })
  })

  test('updates visibility with the loaded policy revision and selected teams', async () => {
    let write: unknown
    setGeneratedFetch(async (input, init) => {
      if (init?.method === 'PUT') {
        write = JSON.parse(String(init.body))
        return Response.json({
          policy: {
            project_uid: '01J00000000000000000000002',
            visibility: 'teams',
            team_uids: ['team-one'],
            revision: 5,
          },
        })
      }
      return Response.json(
        new URL(String(input)).pathname === '/api/v1/teams'
          ? { teams: [{ uid: 'team-one', name: 'Example team', revision: 1 }] }
          : {
              policy: {
                project_uid: '01J00000000000000000000002',
                visibility: 'all',
                team_uids: [],
                revision: 4,
              },
            },
      )
    })
    render(AppShell, { props: accessProps(executeAccessMutation) })
    await screen.findByRole('option', { name: 'Example team' })
    await fireEvent.change(screen.getByLabelText('Project visibility'), { target: { value: '7' } })
    await waitFor(() =>
      expect((screen.getByLabelText('Visibility') as HTMLSelectElement).disabled).toBe(false),
    )
    await fireEvent.change(screen.getByLabelText('Visibility'), { target: { value: 'teams' } })
    await fireEvent.click(screen.getByRole('checkbox', { name: 'Example team' }))
    await fireEvent.click(screen.getByRole('button', { name: 'Save visibility' }))
    await waitFor(() =>
      expect(write).toEqual({ visibility: 'teams', team_uids: ['team-one'], revision: 4 }),
    )
  })

  test('does not refresh an old team scene after authority changes during a write', async () => {
    let reads = 0
    setGeneratedFetch(async () => {
      reads++
      return Response.json({ teams: [] })
    })
    let finish!: (value: boolean) => void
    const onAccessMutation = vi.fn(
      () =>
        new Promise<boolean>((resolve) => {
          finish = resolve
        }),
    )
    const props = accessProps(onAccessMutation)
    const view = render(AppShell, { props })
    await fireEvent.input(screen.getByLabelText('New team name'), {
      target: { value: 'Example team' },
    })
    await waitFor(() =>
      expect(
        (screen.getByRole('button', { name: 'Create team' }) as HTMLButtonElement).disabled,
      ).toBe(false),
    )
    await fireEvent.click(screen.getByRole('button', { name: 'Create team' }))
    await waitFor(() => expect(onAccessMutation).toHaveBeenCalledOnce())
    await view.rerender({ ...props, daemonSwitching: true })
    finish(true)
    await new Promise((resolve) => setTimeout(resolve, 20))
    expect(reads).toBe(1)
    expect(screen.queryByLabelText('New team name')).toBeNull()
  })

  test('retains a visibility draft after a revision conflict instead of silently overwriting it', async () => {
    const writes: unknown[] = []
    setGeneratedFetch(async (input, init) => {
      if (init?.method === 'PUT') {
        writes.push(JSON.parse(String(init.body)))
        return Response.json({ code: 'revision_conflict' }, { status: 409 })
      }
      return Response.json(
        new URL(String(input)).pathname === '/api/v1/teams'
          ? { teams: [{ uid: 'team-one', name: 'Example team', revision: 1 }] }
          : {
              policy: {
                project_uid: '01J00000000000000000000002',
                visibility: 'all',
                team_uids: [],
                revision: 4,
              },
            },
      )
    })
    render(AppShell, { props: accessProps(executeAccessMutation) })
    await screen.findByRole('option', { name: 'Example team' })
    await fireEvent.change(screen.getByLabelText('Project visibility'), { target: { value: '7' } })
    await waitFor(() =>
      expect((screen.getByLabelText('Visibility') as HTMLSelectElement).disabled).toBe(false),
    )
    await fireEvent.change(screen.getByLabelText('Visibility'), { target: { value: 'teams' } })
    await fireEvent.click(screen.getByRole('checkbox', { name: 'Example team' }))
    await fireEvent.click(screen.getByRole('button', { name: 'Save visibility' }))
    await waitFor(() => expect(writes).toHaveLength(1))
    expect((screen.getByLabelText('Visibility') as HTMLSelectElement).value).toBe('teams')
    expect(
      (screen.getByRole('checkbox', { name: 'Example team' }) as HTMLInputElement).checked,
    ).toBe(true)
    expect(writes[0]).toEqual({ visibility: 'teams', team_uids: ['team-one'], revision: 4 })
  })

  test('discards late member and policy reads after switching selections', async () => {
    let finishMembers!: (response: Response) => void
    let finishPolicy!: (response: Response) => void
    const writes: unknown[] = []
    setGeneratedFetch(async (input, init) => {
      const path = new URL(String(input)).pathname
      if (path === '/api/v1/teams/team-one')
        return new Promise<Response>((resolve) => {
          finishMembers = resolve
        })
      if (path === '/api/v1/projects/7/access' && init?.method !== 'PUT')
        return new Promise<Response>((resolve) => {
          finishPolicy = resolve
        })
      if (path === '/api/v1/teams/team-two') return Response.json({ members: ['second-member'] })
      if (path === '/api/v1/projects/8/access' && init?.method === 'PUT') {
        writes.push(JSON.parse(String(init?.body)))
        return Response.json({
          policy: {
            project_uid: '01J00000000000000000000003',
            visibility: 'all',
            team_uids: [],
            revision: 6,
          },
        })
      }
      if (path === '/api/v1/projects/8/access')
        return Response.json({
          policy: {
            project_uid: '01J00000000000000000000003',
            visibility: 'all',
            team_uids: [],
            revision: 5,
          },
        })
      return Response.json({
        teams: [
          { uid: 'team-one', name: 'Example team', revision: 1 },
          { uid: 'team-two', name: 'Second team', revision: 1 },
        ],
      })
    })
    const props = accessProps(executeAccessMutation)
    const first = props.snapshot.catalog[0]!
    props.snapshot.catalog = [
      first,
      {
        ...first,
        project: {
          ...first.project,
          id: 8,
          uid: '01J00000000000000000000003',
          name: 'second-project',
        },
      },
    ]
    render(AppShell, { props })
    await screen.findByRole('option', { name: 'Second team' })
    await fireEvent.change(screen.getByLabelText('Team'), { target: { value: 'team-one' } })
    await fireEvent.change(screen.getByLabelText('Project visibility'), { target: { value: '7' } })
    await waitFor(() => {
      expect(finishMembers).toBeTypeOf('function')
      expect(finishPolicy).toBeTypeOf('function')
    })
    await fireEvent.change(screen.getByLabelText('Team'), { target: { value: 'team-two' } })
    await fireEvent.change(screen.getByLabelText('Project visibility'), { target: { value: '8' } })
    await screen.findByText('second-member')
    await waitFor(() =>
      expect((screen.getByLabelText('Visibility') as HTMLSelectElement).disabled).toBe(false),
    )
    finishMembers(Response.json({ members: ['late-private-member'] }))
    finishPolicy(
      Response.json({
        policy: {
          project_uid: 'old-project',
          visibility: 'teams',
          team_uids: ['late-private-team'],
          revision: 8,
        },
      }),
    )
    await new Promise((resolve) => setTimeout(resolve, 20))
    expect(screen.queryByText('late-private-member')).toBeNull()
    expect(screen.getByText('second-member')).toBeTruthy()
    expect((screen.getByLabelText('Visibility') as HTMLSelectElement).value).toBe('all')
    await fireEvent.click(screen.getByRole('button', { name: 'Save visibility' }))
    await waitFor(() => expect(writes).toHaveLength(1))
    expect(writes[0]).toEqual({ visibility: 'all', team_uids: [], revision: 5 })
  })

  test('preserves access selections with disabled controls during a same-authority snapshot refresh', async () => {
    setGeneratedFetch(async (input) =>
      Response.json(
        new URL(String(input)).pathname === '/api/v1/teams'
          ? { teams: [{ uid: 'team-one', name: 'Example team', revision: 1 }] }
          : { members: ['existing-member'] },
      ),
    )
    const props = accessProps(executeAccessMutation)
    const view = render(AppShell, { props })
    await screen.findByRole('option', { name: 'Example team' })
    await fireEvent.change(screen.getByLabelText('Team'), { target: { value: 'team-one' } })
    await screen.findByText('existing-member')
    await fireEvent.input(screen.getByLabelText('Member account'), {
      target: { value: 'new-member' },
    })
    const addMember = () => screen.getByRole('button', { name: 'Add member' }) as HTMLButtonElement
    expect(addMember().disabled).toBe(false)
    await view.rerender({ ...props, loading: true, canMutate: false })
    expect((screen.getByLabelText('Team') as HTMLSelectElement).value).toBe('team-one')
    expect((screen.getByLabelText('Member account') as HTMLInputElement).value).toBe('new-member')
    expect(addMember().disabled).toBe(true)
    await view.rerender({ ...props, loading: false, canMutate: true })
    expect((screen.getByLabelText('Team') as HTMLSelectElement).value).toBe('team-one')
    expect((screen.getByLabelText('Member account') as HTMLInputElement).value).toBe('new-member')
    expect(addMember().disabled).toBe(false)
    await view.rerender({ ...props, loading: false, canMutate: false, stale: true })
    expect(screen.queryByLabelText('Team')).toBeNull()
  })

  test.each(['nonadmin', 'readonly', 'stale', 'expired', 'switching', 'remote', 'reconnecting'])(
    'withholds access management from %s authority',
    (kind) => {
      const current = snapshot()
      current.capabilities.token_audit_read = kind !== 'nonadmin'
      current.capabilities.access_admin = kind !== 'nonadmin' && kind !== 'readonly'
      if (kind === 'readonly') current.capabilities.writable = false
      if (kind === 'expired') current.capabilities.expires_at = '2000-01-01T00:00:00Z'
      render(AppShell, {
        props: {
          route: {
            kind: 'kata',
            view: 'credentials',
            graph: false,
            filters: { status: [], owner: [], label: [], relationship: [] },
          },
          snapshot: current,
          loading: false,
          ...mutationProps(),
          stale: kind === 'stale',
          daemonSwitching: kind === 'switching',
          reconnecting: kind === 'reconnecting',
          activeDaemonID: kind === 'remote' ? 'remote-hub' : undefined,
          onNavigate: vi.fn(),
          onCreateProject: vi.fn(async () => ({ changed: true })),
        },
      })
      expect(screen.queryByRole('heading', { name: 'Teams and visibility' })).toBeNull()
    },
  )

  test('gates credential audit navigation on the advertised capability', async () => {
    const onNavigate = vi.fn()
    const authorized = snapshot()
    authorized.capabilities.token_audit_read = true
    authorized.capabilities.access_admin = true
    const view = render(AppShell, {
      props: {
        route: {
          kind: 'kata',
          view: 'all-open',
          graph: false,
          filters: { status: [], owner: [], label: [], relationship: [] },
        },
        snapshot: authorized,
        loading: false,
        ...mutationProps(),
        onNavigate,
        onCreateProject: vi.fn(async () => ({ changed: true })),
      },
    })

    await fireEvent.click(screen.getByRole('button', { name: 'Credentials' }))
    expect(onNavigate).toHaveBeenCalledWith({
      kind: 'kata',
      view: 'credentials',
      graph: false,
      filters: { status: [], owner: [], label: [], relationship: [] },
    })

    const unauthorized = snapshot()
    await view.rerender({ snapshot: unauthorized })
    expect(screen.queryByRole('button', { name: 'Credentials' })).toBeNull()
  })

  test('renders the credential ledger as a daemon screen and returns to issue navigation', async () => {
    const onBackFromCredentials = vi.fn()
    const authorized = snapshot()
    authorized.capabilities.token_audit_read = true
    authorized.capabilities.access_admin = true
    render(AppShell, {
      props: {
        route: {
          kind: 'kata',
          view: 'credentials',
          graph: false,
          filters: { status: [], owner: [], label: [], relationship: [] },
        },
        snapshot: authorized,
        loading: false,
        credentialTokens: [
          {
            id: 12,
            name: 'Worker',
            actor: 'agent-a',
            state: 'live',
            created_at: '2026-09-15T09:00:00Z',
            last_used_at: null,
            revoked_at: null,
          },
        ],
        onRefreshCredentials: vi.fn(),
        onBackFromCredentials,
        ...mutationProps(),
        onNavigate: vi.fn(),
        onCreateProject: vi.fn(async () => ({ changed: true })),
      },
    })

    expect(screen.getByRole('heading', { name: 'Credentials' })).not.toBeNull()
    expect(screen.getByText('Worker')).not.toBeNull()
    expect(screen.queryByLabelText('Search tasks')).toBeNull()
    expect(screen.queryByRole('button', { name: 'New task' })).toBeNull()
    expect(screen.getByRole('button', { name: 'Credentials' }).classList).toContain('active')
    expect(screen.getByRole('button', { name: 'Today' }).classList).not.toContain('active')

    await fireEvent.click(screen.getByRole('button', { name: 'Back to issues' }))
    expect(onBackFromCredentials).toHaveBeenCalledOnce()
  })

  test('opens Settings from the gear for a viewer without access administration', async () => {
    const onNavigate = vi.fn()
    const member = snapshot()
    member.capabilities.writable = false
    render(AppShell, {
      props: {
        route: {
          kind: 'kata',
          view: 'all-open',
          graph: false,
          filters: { status: [], owner: [], label: [], relationship: [] },
        },
        snapshot: member,
        loading: false,
        readOnly: true,
        ...mutationProps({ canMutate: false }),
        onNavigate,
        onCreateProject: vi.fn(async () => ({ changed: true })),
      },
    })

    await fireEvent.click(screen.getByRole('button', { name: 'Settings' }))
    expect(onNavigate).toHaveBeenCalledWith({
      kind: 'kata',
      view: 'settings',
      graph: false,
      filters: { status: [], owner: [], label: [], relationship: [] },
    })
  })

  test('renders Settings as a daemon screen for the remote host and returns to issues', async () => {
    const onBackFromCredentials = vi.fn()
    setGeneratedFetch(
      vi.fn(async () =>
        Response.json({
          provider: 'none',
          download_bytes: 290_000_000,
          ollama_available: false,
          rebuild: false,
          can_change: false,
        }),
      ),
    )
    render(AppShell, {
      props: {
        route: {
          kind: 'kata',
          view: 'settings',
          graph: false,
          filters: { status: [], owner: [], label: [], relationship: [] },
        },
        snapshot: snapshot(),
        loading: false,
        daemons: [
          {
            id: 'local',
            url: 'http://127.0.0.1:7777',
            default: true,
            local: true,
            auth: 'none',
            health: 'connected',
          },
          {
            id: 'team',
            url: 'https://kata.example.com',
            default: false,
            auth: 'token',
            health: 'connected',
          },
        ],
        activeDaemonID: 'team',
        canAdministerDaemon: false,
        onBackFromCredentials,
        ...mutationProps(),
        onNavigate: vi.fn(),
        onCreateProject: vi.fn(async () => ({ changed: true })),
      },
    })

    expect(screen.getByRole('heading', { name: 'Settings' })).not.toBeNull()
    expect(await screen.findByText(/kata\.example\.com/)).not.toBeNull()
    expect(screen.queryByRole('button', { name: 'New task' })).toBeNull()
    expect(screen.queryByRole('button', { name: /layout/ })).toBeNull()

    await fireEvent.click(screen.getByRole('button', { name: 'Back to issues' }))
    expect(onBackFromCredentials).toHaveBeenCalledOnce()
  })

  test('drops held keyboard selection when sidebar navigation begins', async () => {
    vi.useFakeTimers()
    const onNavigate = vi.fn()
    const authority = snapshot()
    authority.collection!.push({
      ...authority.collection![0]!,
      id: 2,
      uid: '01J00000000000000000000003',
      short_id: 'b2',
      qualified_id: 'example-project#b2',
      title: 'Second issue',
    })
    render(AppShell, {
      props: {
        route: {
          kind: 'kata',
          view: 'all-open',
          graph: false,
          filters: { status: [], owner: [], label: [], relationship: [] },
        },
        snapshot: authority,
        loading: false,
        ...mutationProps(),
        onNavigate,
        onCreateProject: vi.fn(async () => ({ changed: true })),
      },
    })

    const issue = screen.getByRole('button', { name: /Example issue/ })
    issue.focus()
    await fireEvent.keyDown(issue, { key: 'j', code: 'KeyJ' })
    await fireEvent.click(screen.getByRole('button', { name: 'Today' }))
    await fireEvent.keyUp(window, { key: 'j', code: 'KeyJ' })
    await vi.advanceTimersByTimeAsync(100)

    expect(onNavigate).toHaveBeenCalledTimes(1)
    expect(onNavigate).toHaveBeenCalledWith({
      kind: 'kata',
      view: 'today',
      graph: false,
      filters: { status: [], owner: [], label: [], relationship: [] },
    })
  })

  test('projects the selected project immediately from complete collection authority', () => {
    const complete = snapshot()
    complete.catalog!.push({
      project: {
        active: true,
        id: 8,
        uid: '01J00000000000000000000003',
        name: 'other-project',
        metadata: { area: 'Work' },
        revision: 1,
        created_at: '2026-08-01T09:00:00.000Z',
      },
      stats: { Open: 1, Closed: 0, LastEventAt: '2026-08-01T12:00:00.000Z' },
    })
    complete.collection!.push({
      id: 2,
      uid: '01J00000000000000000000004',
      project_id: 8,
      project_uid: '01J00000000000000000000003',
      project_name: 'other-project',
      short_id: 'b2',
      qualified_id: 'other-project#b2',
      title: 'Other issue',
      body: '',
      status: 'open',
      metadata: {},
      revision: 1,
      author: 'user-a',
      labels: [],
      created_at: '2026-08-01T09:00:00.000Z',
      updated_at: '2026-08-01T12:00:00.000Z',
    })

    render(AppShell, {
      props: {
        route: {
          kind: 'kata',
          projectUID: '01J00000000000000000000002',
          graph: false,
          filters: { status: [], owner: [], label: [], relationship: [] },
        },
        snapshot: complete,
        loading: true,
        ...mutationProps(),
        onNavigate: vi.fn(),
        onCreateProject: vi.fn(async () => ({ changed: true })),
      },
    })

    expect(screen.getByRole('button', { name: /Example issue/ })).not.toBeNull()
    expect(screen.queryByRole('button', { name: /Other issue/ })).toBeNull()
  })

  test('clears project scope when All projects is selected', async () => {
    const onNavigate = vi.fn()
    render(AppShell, {
      props: {
        route: {
          kind: 'kata',
          projectUID: '01J00000000000000000000002',
          graph: false,
          filters: { status: [], owner: [], label: [], relationship: [] },
        },
        snapshot: snapshot(),
        loading: false,
        ...mutationProps(),
        onNavigate,
        onCreateProject: vi.fn(async () => ({ changed: true })),
      },
    })

    await fireEvent.click(screen.getByRole('button', { name: /Project scope: example-project/i }))
    await fireEvent.mouseDown(screen.getByRole('option', { name: 'All projects' }))

    await waitFor(() =>
      expect(onNavigate).toHaveBeenCalledWith({
        kind: 'kata',
        graph: false,
        filters: { status: [], owner: [], label: [], relationship: [] },
      }),
    )
  })

  test('resets grown rows when repeated label filters change', async () => {
    const current = snapshot()
    current.collection = Array.from({ length: 400 }, (_, index) => ({
      ...current.collection[0]!,
      id: index + 1,
      uid: `bulk-${index}`,
      short_id: `b${index}`,
      title: `Bulk ${index}`,
      labels: ['a', 'b', 'c', 'd'],
    }))
    const props = {
      route: {
        kind: 'kata' as const,
        view: 'all-open' as const,
        graph: false,
        filters: { status: [], owner: [], label: ['a', 'b'], relationship: [] },
      },
      snapshot: current,
      loading: false,
      ...mutationProps(),
      onNavigate: vi.fn(),
      onCreateProject: vi.fn(async () => ({ changed: true })),
    }
    const { container, rerender } = render(AppShell, { props })
    expect(container.querySelectorAll('button.row')).toHaveLength(150)
    await fireEvent.keyDown(container.querySelector('button.row')!, { key: 'End' })
    expect(container.querySelectorAll('button.row')).toHaveLength(400)
    await rerender({
      ...props,
      route: { ...props.route, filters: { ...props.route.filters, label: ['a', 'd'] } },
    })
    expect(container.querySelectorAll('button.row')).toHaveLength(150)
  })

  test('renders the routed source graph and returns to the same selected list context', async () => {
    vi.stubGlobal(
      'ResizeObserver',
      class {
        observe(): void {}
        unobserve(): void {}
        disconnect(): void {}
      },
    )
    Object.defineProperty(window, 'matchMedia', {
      configurable: true,
      value: vi.fn(() => ({
        matches: false,
        addEventListener: vi.fn(),
        removeEventListener: vi.fn(),
      })),
    })
    const selected = snapshot()
    selected.selected = {
      state: 'available',
      issue: { ...selected.collection![0]!, body: 'Original body', revision: 3 },
      comments: [],
      labels: [],
      links: [],
      recurrences: [],
      history: [],
    }
    selected.graph = {
      issues: [...selected.collection!],
      links: [],
      edges: [],
      unresolved_refs: [],
    }
    const onNavigate = vi.fn()

    render(AppShell, {
      props: {
        route: {
          kind: 'kata',
          issueUID: '01J00000000000000000000001',
          graph: true,
          filters: { status: [], owner: ['user-a'], label: [], relationship: [] },
        },
        snapshot: selected,
        loading: false,
        ...mutationProps(),
        onNavigate,
        onCreateProject: vi.fn(async () => ({ changed: true })),
      },
    })

    expect(screen.getByRole('region', { name: 'Reachable task graph' })).not.toBeNull()
    await fireEvent.click(screen.getByRole('button', { name: 'Back to task list' }))
    expect(onNavigate).toHaveBeenCalledWith({
      kind: 'kata',
      issueUID: '01J00000000000000000000001',
      graph: false,
      filters: { status: [], owner: ['user-a'], label: [], relationship: [] },
    })
  })

  test('renders accepted selected authority and forwards detail edits', async () => {
    const selected = snapshot()
    selected.selected = {
      state: 'available',
      issue: { ...selected.collection![0]!, body: 'Original body', revision: 3 },
      comments: [],
      labels: [],
      links: [],
      recurrences: [],
      history: [],
    }
    const onEditIssue = vi.fn(async () => true)

    render(AppShell, {
      props: {
        route: {
          kind: 'kata',
          issueUID: '01J00000000000000000000001',
          graph: false,
          filters: { status: [], owner: [], label: [], relationship: [] },
        },
        snapshot: selected,
        loading: false,
        ...mutationProps({ onEditIssue }),
        onNavigate: vi.fn(),
        onCreateProject: vi.fn(async () => ({ changed: true })),
      },
    })

    await fireEvent.click(screen.getByRole('button', { name: 'Edit issue' }))
    expect(screen.getByRole('region', { name: 'Task detail' })).not.toBeNull()
    await fireEvent.click(screen.getByRole('button', { name: 'Edit title' }))
    await fireEvent.input(screen.getByLabelText('Edit title'), {
      target: { value: 'Updated example issue' },
    })
    await fireEvent.keyDown(screen.getByLabelText('Edit title'), { key: 'Enter' })

    expect(onEditIssue).toHaveBeenCalledWith('01J00000000000000000000001', {
      title: 'Updated example issue',
    })
  })

  test('uses the persisted split orientation and exposes an accessible layout toggle', async () => {
    vi.stubGlobal(
      'ResizeObserver',
      class {
        observe(): void {}
        unobserve(): void {}
        disconnect(): void {}
      },
    )
    vi.spyOn(HTMLElement.prototype, 'getBoundingClientRect').mockReturnValue({
      width: 900,
      height: 700,
      x: 0,
      y: 0,
      top: 0,
      right: 900,
      bottom: 700,
      left: 0,
      toJSON: () => ({}),
    })
    const selected = snapshot()
    selected.selected = {
      state: 'available',
      issue: { ...selected.collection![0]!, body: '', revision: 3 },
      comments: [],
      labels: [],
      links: [],
      recurrences: [],
      history: [],
    }
    const onPreferencesChange = vi.fn()
    render(AppShell, {
      props: {
        route: {
          kind: 'kata',
          issueUID: '01J00000000000000000000001',
          graph: false,
          filters: { status: [], owner: [], label: [], relationship: [] },
        },
        snapshot: selected,
        loading: false,
        preferences: {
          theme: 'system',
          columns: ['status', 'title'],
          splitDirection: 'horizontal',
          splitSize: 420,
          collapsedGroups: [],
        },
        onPreferencesChange,
        ...mutationProps(),
        onNavigate: vi.fn(),
        onCreateProject: vi.fn(async () => ({ changed: true })),
      },
    })

    await fireEvent.click(screen.getByRole('button', { name: 'Theme: System' }))
    expect(onPreferencesChange).toHaveBeenCalledWith(
      expect.objectContaining({ theme: 'light', splitDirection: 'horizontal' }),
    )
    onPreferencesChange.mockClear()

    expect(screen.getByRole('separator', { name: 'Resize Kata panes' })).not.toBeNull()
    await fireEvent.click(screen.getByRole('button', { name: 'Switch to stacked layout' }))
    expect(onPreferencesChange).toHaveBeenCalledWith(
      expect.objectContaining({ splitDirection: 'vertical', splitSize: 420 }),
    )
  })

  test('quick-captures a new task through the ported workspace action', async () => {
    const onCreateIssue = vi.fn(async () => {})
    const accepted = snapshot()
    accepted.catalog![0]!.project.metadata = {
      ...(accepted.catalog![0]!.project.metadata ?? {}),
      role: 'inbox',
    }
    const { container } = render(AppShell, {
      props: {
        route: {
          kind: 'kata',
          view: 'inbox',
          graph: false,
          filters: { status: [], owner: [], label: [], relationship: [] },
        },
        snapshot: accepted,
        loading: false,
        ...mutationProps({ onCreateIssue }),
        onNavigate: vi.fn(),
        onCreateProject: vi.fn(async () => ({ changed: true })),
      },
    })

    await fireEvent.click(within(container).getByRole('button', { name: 'New task' }))
    await fireEvent.input(within(container).getByRole('textbox', { name: 'Quick capture' }), {
      target: { value: 'New example task' },
    })
    await fireEvent.keyDown(within(container).getByRole('textbox', { name: 'Quick capture' }), {
      key: 'Enter',
    })

    expect(onCreateIssue).toHaveBeenCalledWith('New example task')
  })

  test('distinguishes empty projects from empty tasks and keeps refused capture closed', async () => {
    const empty = snapshot()
    empty.catalog = []
    empty.collection = []
    const onCreateProject = vi.fn(async () => {
      throw new Error('Project creation refused')
    })
    const view = render(AppShell, {
      props: {
        route: {
          kind: 'kata',
          view: 'all-open',
          graph: false,
          filters: { status: [], owner: [], label: [], relationship: [] },
        },
        snapshot: empty,
        loading: false,
        ...mutationProps(),
        onNavigate: vi.fn(),
        onCreateProject,
      },
    })
    const collection = screen.getByRole('region', { name: 'Issues' })
    expect(within(collection).getByText('No projects yet')).not.toBeNull()
    await fireEvent.click(within(collection).getByRole('button', { name: 'New project' }))
    const input = screen.getByRole('textbox', { name: 'New project name' })
    await fireEvent.click(screen.getByRole('button', { name: 'New task' }))
    await fireEvent.input(input, { target: { value: 'example-project' } })
    await fireEvent.keyDown(input, { key: 'Enter' })
    await waitFor(() => expect(onCreateProject).toHaveBeenCalledWith('example-project', true))
    await expect(onCreateProject.mock.results[0]!.value).rejects.toThrow()
    expect(screen.queryByRole('textbox', { name: 'Quick capture' })).toBeNull()
    const existing = snapshot()
    existing.collection = []
    await view.rerender({ snapshot: existing })
    expect(within(collection).getByText('No tasks')).not.toBeNull()
    expect(screen.queryByText('No projects yet')).toBeNull()
    await view.rerender({ snapshot: empty, loading: true })
    expect(screen.queryByText('No projects yet')).toBeNull()
  })

  test.each(['close', 'fence'])(
    'cancels pending first-project capture after %s',
    async (action) => {
      const empty = snapshot()
      empty.catalog = []
      empty.collection = []
      vi.stubGlobal('matchMedia', (media: string) => ({
        matches: true,
        media,
        addEventListener() {},
        removeEventListener() {},
      }))
      let finish = () => {}
      const onCreateProject = vi.fn(
        () =>
          new Promise<{ changed: boolean }>(
            (resolve) => (finish = () => resolve({ changed: true })),
          ),
      )
      const view = render(AppShell, {
        props: {
          route: {
            kind: 'kata',
            view: 'all-open',
            graph: false,
            filters: { status: [], owner: [], label: [], relationship: [] },
          },
          snapshot: empty,
          loading: false,
          ...mutationProps(),
          onNavigate: vi.fn(),
          onCreateProject,
        },
      })
      await fireEvent.click(screen.getByRole('button', { name: 'New task' }))
      const drawer = screen.getByRole('dialog', { name: 'Kata navigation' })
      const input = within(drawer).getByRole('textbox', { name: 'New project name' })
      await fireEvent.input(input, { target: { value: 'example-project' } })
      await fireEvent.keyDown(input, { key: 'Enter' })
      await waitFor(() => expect(onCreateProject).toHaveBeenCalledWith('example-project', true))
      if (action === 'close')
        await fireEvent.click(within(drawer).getByRole('button', { name: 'Close' }))
      else await view.rerender({ draftFenceGeneration: 1 })
      finish()
      await new Promise((resolve) => setTimeout(resolve, 0))
      if (action === 'fence')
        expect(screen.queryAllByRole('textbox', { name: 'New project name' })).toHaveLength(0)
      expect(screen.queryByRole('textbox', { name: 'Quick capture' })).toBeNull()
    },
  )

  test('designates an Inbox from New task before opening quick capture', async () => {
    const onDesignateInbox = vi.fn(async () => {})
    render(AppShell, {
      props: {
        route: {
          kind: 'kata',
          view: 'all-open',
          graph: false,
          filters: { status: [], owner: [], label: [], relationship: [] },
        },
        snapshot: snapshot(),
        loading: false,
        ...mutationProps({ onDesignateInbox }),
        onNavigate: vi.fn(),
        onCreateProject: vi.fn(async () => ({ changed: true })),
      },
    })

    const create = screen.getByRole('button', { name: 'New task' })
    expect(create.getAttribute('aria-disabled')).toBeNull()
    await fireEvent.click(create)
    await fireEvent.click(screen.getByRole('button', { name: /Use example-project as Inbox/ }))
    expect(onDesignateInbox).toHaveBeenCalledWith('01J00000000000000000000002')
    expect(screen.getByRole('textbox', { name: 'Quick capture' })).not.toBeNull()
  })

  test('renders daemon selection and recovery status inside stable workspace chrome', async () => {
    const onSelectDaemon = vi.fn()
    render(AppShell, {
      props: {
        route: {
          kind: 'kata',
          view: 'all-open',
          graph: false,
          filters: { status: [], owner: [], label: [], relationship: [] },
        },
        snapshot: snapshot(),
        loading: false,
        ...mutationProps(),
        daemons: [
          {
            id: 'example-local',
            url: '',
            default: true,
            auth: 'none',
            health: 'connected',
          },
        ],
        activeDaemonID: 'example-local',
        reconnecting: true,
        stale: true,
        onSelectDaemon,
        onNavigate: vi.fn(),
        onCreateProject: vi.fn(async () => ({ changed: true })),
      },
    })

    expect(screen.getByRole('status', { name: 'Kata daemon status' }).textContent).toContain(
      'Reconnecting…',
    )
    expect(screen.getByRole('status', { name: 'Active connection' }).textContent).toContain(
      'Workspace data may be out of date. Editing is paused.',
    )
  })

  test('passes mutation authority to project creation controls', async () => {
    const view = render(AppShell, {
      props: {
        route: {
          kind: 'kata',
          view: 'all-open',
          graph: false,
          filters: { status: [], owner: [], label: [], relationship: [] },
        },
        snapshot: snapshot(),
        loading: false,
        ...mutationProps({ canMutate: false }),
        onNavigate: vi.fn(),
        onCreateProject: vi.fn(async () => ({ changed: true })),
      },
    })

    const create = () => screen.getByRole('button', { name: 'New project' })
    expect(create().getAttribute('aria-disabled')).toBe('true')

    await view.rerender({ canMutate: true, mutationPending: true })
    expect(create().getAttribute('aria-disabled')).toBe('true')
    expect(
      screen.getByRole('button', {
        name: 'New project',
        description: 'Wait for the current change to finish.',
      }),
    ).not.toBeNull()

    await view.rerender({ mutationPending: false })
    expect(create().getAttribute('aria-disabled')).toBeNull()
    const empty = snapshot()
    empty.catalog = []
    empty.collection = []
    await view.rerender({ snapshot: empty, canMutate: false, stale: true })
    expect(
      within(screen.getByRole('region', { name: 'Issues' }))
        .getByRole('button', {
          name: 'New project',
          description: 'Editing is paused while workspace data may be out of date.',
        })
        .getAttribute('aria-disabled'),
    ).toBe('true')
  })

  test('keeps the canonical issue route while authority is missing or archived', async () => {
    const missing = snapshot()
    missing.selected = {
      state: 'missing',
      comments: [],
      labels: [],
      links: [],
      recurrences: [],
      history: [],
    }
    const route = {
      kind: 'kata' as const,
      issueUID: '01J00000000000000000000001',
      graph: false,
      filters: { status: [], owner: [], label: [], relationship: [] },
    }
    const view = render(AppShell, {
      props: {
        route,
        snapshot: missing,
        loading: false,
        ...mutationProps(),
        onNavigate: vi.fn(),
        onCreateProject: vi.fn(async () => ({ changed: true })),
      },
    })

    expect(
      within(view.container).getByRole('status', { name: 'Selected issue status' }).textContent,
    ).toContain('unavailable from the current authority')

    const archived = snapshot()
    archived.selected = {
      state: 'archived',
      comments: [],
      labels: [],
      links: [],
      recurrences: [],
      history: [],
    }
    await view.rerender({ snapshot: archived })

    expect(
      within(view.container).getByRole('status', { name: 'Selected issue status' }).textContent,
    ).toContain('archived')
  })

  test('honors issue-scoped allowed actions without exposing project-wide controls', async () => {
    const onCreateIssue = vi.fn(async () => {})
    const scoped = snapshot()
    scoped.capabilities.scope = {
      kind: 'issue_subtree',
      project_uid: '01J00000000000000000000002',
      root_issue_uid: '01J00000000000000000000001',
    }
    scoped.capabilities.allowed_actions = [
      'issue.read',
      'issue.create_child',
      'issue.edit',
      'issue.metadata',
      'issue.close',
      'issue.reopen',
    ]
    scoped.selected = {
      state: 'available',
      issue: { ...scoped.collection![0]!, body: 'Scoped body', revision: 3 },
      comments: [],
      labels: [],
      links: [],
      recurrences: [],
      history: [],
    }

    render(AppShell, {
      props: {
        route: {
          kind: 'kata',
          issueUID: '01J00000000000000000000001',
          graph: false,
          filters: { status: [], owner: [], label: [], relationship: [] },
        },
        snapshot: scoped,
        loading: false,
        ...mutationProps({ onCreateIssue }),
        onNavigate: vi.fn(),
        onCreateProject: vi.fn(async () => ({ changed: true })),
      },
    })

    expect(
      screen.getByRole('button', { name: 'New task' }).getAttribute('aria-disabled'),
    ).toBeNull()
    expect(
      screen
        .getByRole('button', {
          name: 'New project',
          description: 'This credential cannot create projects.',
        })
        .getAttribute('aria-disabled'),
    ).toBe('true')
    expect((screen.getByRole('button', { name: 'Edit issue' }) as HTMLButtonElement).disabled).toBe(
      false,
    )

    await fireEvent.click(screen.getByRole('button', { name: 'Edit issue' }))
    await fireEvent.click(screen.getByRole('button', { name: 'More actions' }))

    expect(screen.getByRole('menuitem', { name: 'Add checklist' })).not.toBeNull()
    expect(screen.queryByRole('menuitem', { name: 'Move to another project' })).toBeNull()
    expect(screen.queryByRole('menuitem', { name: 'Create recurrence...' })).toBeNull()
    expect(screen.queryByRole('menuitem', { name: 'Delete issue' })).toBeNull()
    expect(screen.queryByRole('region', { name: 'Recurrences' })).toBeNull()

    await fireEvent.click(screen.getByRole('button', { name: 'New task' }))
    await fireEvent.input(screen.getByRole('textbox', { name: 'Quick capture' }), {
      target: { value: 'Scoped child task' },
    })
    await fireEvent.keyDown(screen.getByRole('textbox', { name: 'Quick capture' }), {
      key: 'Enter',
    })
    expect(onCreateIssue).toHaveBeenCalledWith('Scoped child task', {
      projectID: 7,
      projectUID: '01J00000000000000000000002',
      issueUID: '01J00000000000000000000001',
    })
  })
})

function mutationProps(overrides: Record<string, unknown> = {}) {
  return {
    canMutate: true,
    mutationPending: false,
    mutationMessage: undefined,
    draftResetGeneration: 0,
    ownerOptions: [],
    searchReferences: vi.fn(async () => []),
    onMoveIssue: vi.fn(async () => true),
    onPatchMetadata: vi.fn(async () => true),
    onAddComment: vi.fn(async () => true),
    onEditIssue: vi.fn(async () => true),
    onAssignOwner: vi.fn(async () => true),
    onUnassignOwner: vi.fn(async () => true),
    onSetPriority: vi.fn(async () => true),
    onAddLabel: vi.fn(async () => true),
    onRemoveLabel: vi.fn(),
    onCloseIssue: vi.fn(async () => true),
    onReopenIssue: vi.fn(),
    onDeleteIssue: vi.fn(async () => true),
    onCreateIssue: vi.fn(async () => {}),
    onDesignateInbox: vi.fn(async () => {}),
    onCreateRecurrence: vi.fn(async () => {}),
    onPatchRecurrence: vi.fn(async () => {}),
    onDeleteRecurrence: vi.fn(async () => true),
    ...overrides,
  }
}

function snapshot(): UISnapshot {
  return {
    contract_version: '2',
    cursor: 12,
    capabilities: { writable: true, updates: 'sse', actor_policy: 'identity' },
    origin: 'https://daemon.example',
    origin_stable: true,
    catalog: [
      {
        project: {
          active: true,
          id: 7,
          uid: '01J00000000000000000000002',
          name: 'example-project',
          metadata: { area: 'Personal' },
          revision: 2,
          created_at: '2026-08-01T09:00:00.000Z',
        },
        stats: { Open: 1, Closed: 0, LastEventAt: '2026-08-01T12:00:00.000Z' },
      },
    ],
    collection: [
      {
        id: 1,
        uid: '01J00000000000000000000001',
        project_id: 7,
        project_uid: '01J00000000000000000000002',
        project_name: 'example-project',
        short_id: 'a1',
        qualified_id: 'example-project#a1',
        title: 'Example issue',
        body: '',
        status: 'open',
        metadata: {},
        revision: 1,
        author: 'user-a',
        labels: [],
        created_at: '2026-08-01T09:00:00.000Z',
        updated_at: '2026-08-01T12:00:00.000Z',
      },
    ],
    collection_links: [],
  }
}

function accessProps(
  onAccessMutation: (
    draft: unknown,
    mutate: (context: MutationContext) => Promise<MutationResult>,
  ) => Promise<boolean>,
) {
  const current = snapshot()
  current.capabilities.token_audit_read = true
  current.capabilities.access_admin = true
  return {
    route: {
      kind: 'kata' as const,
      view: 'credentials' as const,
      graph: false,
      filters: { status: [], owner: [], label: [], relationship: [] },
    },
    snapshot: current,
    loading: false,
    ...mutationProps(),
    onAccessMutation,
    onNavigate: vi.fn(),
    onCreateProject: vi.fn(async () => ({ changed: true })),
  }
}

async function executeAccessMutation(
  _draft: unknown,
  mutate: (context: MutationContext) => Promise<MutationResult>,
): Promise<boolean> {
  const response = await mutate({ headers: new Headers(), body: (value) => value })
  return response.status === 200
}

describe('AppShell task selection', () => {
  afterEach(() => {
    cleanup()
  })

  const listRoute = {
    kind: 'kata' as const,
    view: 'all-open' as const,
    graph: false,
    filters: { status: [], owner: [], label: [], relationship: [] },
  }
  const firstUID = '01J00000000000000000000001'
  const secondUID = '01J00000000000000000000003'

  function shownTitle(): string | null {
    return within(screen.getByRole('region', { name: 'Kata issue detail' })).getByRole('heading', {
      level: 2,
    }).textContent
  }

  function twoTaskSnapshot(): UISnapshot {
    const current = snapshot()
    current.collection = [
      ...current.collection!,
      {
        ...current.collection![0]!,
        id: 2,
        uid: secondUID,
        short_id: 'a2',
        qualified_id: 'example-project#a2',
        title: 'Second issue',
        body: 'Second body',
      },
    ]
    current.selected = {
      state: 'available',
      issue: { ...current.collection[0]!, body: 'First body' },
      comments: [
        {
          id: 1,
          uid: '01J00000000000000000000004',
          issue_id: 1,
          author: 'user-a',
          body: 'First comment',
          created_at: '2026-08-01T12:00:00.000Z',
        },
      ],
      labels: [],
      links: [],
      recurrences: [],
      history: [],
    }
    return current
  }

  test('shows a clicked task in the same frame, before its snapshot arrives', async () => {
    const onNavigate = vi.fn()
    const props = {
      route: { ...listRoute, issueUID: firstUID },
      snapshot: twoTaskSnapshot(),
      loading: false,
      ...mutationProps(),
      onNavigate,
      onCreateProject: vi.fn(async () => ({ changed: true })),
    }
    const { rerender } = render(AppShell, { props })
    expect(shownTitle()).toContain('Example issue')

    await fireEvent.click(screen.getByRole('button', { name: /Second issue/ }))
    expect(onNavigate).toHaveBeenLastCalledWith({ ...listRoute, issueUID: secondUID })
    // App keeps the accepted route and snapshot until the new snapshot lands.
    await rerender({ ...props, loading: true, requestedRoute: onNavigate.mock.lastCall![0] })

    expect(shownTitle()).toContain('Second issue')
    expect(await screen.findAllByText('Second body')).not.toHaveLength(0)
    expect(screen.queryAllByText('First comment')).toHaveLength(0)
    expect(screen.queryByText('No comments.')).toBeNull()
    expect(screen.queryByText('No events')).toBeNull()
    expect(screen.getByRole('status', { name: 'Loading comments' })).not.toBeNull()
    expect(screen.getByRole('button', { name: 'Edit issue' })).toHaveProperty('disabled', true)

    const arrived = twoTaskSnapshot()
    arrived.selected = {
      ...arrived.selected!,
      issue: { ...arrived.collection![1]!, body: 'Second body' },
      comments: [{ ...arrived.selected!.comments[0]!, body: 'Second comment' }],
    }
    await rerender({
      ...props,
      route: { ...listRoute, issueUID: secondUID },
      snapshot: arrived,
      requestedRoute: undefined,
    })
    expect(await screen.findAllByText('Second comment')).not.toHaveLength(0)
    expect(screen.queryByRole('status', { name: 'Loading comments' })).toBeNull()
  })

  test('keeps the accepted task when the clicked task is not in the loaded list', async () => {
    const props = {
      route: { ...listRoute, issueUID: firstUID },
      snapshot: twoTaskSnapshot(),
      loading: true,
      ...mutationProps(),
      onNavigate: vi.fn(),
      onCreateProject: vi.fn(async () => ({ changed: true })),
    }
    const { rerender } = render(AppShell, { props })
    await rerender({
      ...props,
      requestedRoute: { ...listRoute, issueUID: '01J00000000000000000000009' },
    })
    expect(shownTitle()).toContain('Example issue')
    expect(await screen.findAllByText('First comment')).not.toHaveLength(0)
  })
})
