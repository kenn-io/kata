import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/svelte'
import { afterEach, describe, expect, test, vi } from 'vitest'

import type { UISnapshot } from '../lib/state/snapshot'
import AppShell from './AppShell.svelte'

describe('AppShell', () => {
  afterEach(() => {
    cleanup()
    vi.unstubAllGlobals()
    vi.useRealTimers()
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

  test('retains repeated route filters when another control changes', async () => {
    const onNavigate = vi.fn()
    render(AppShell, {
      props: {
        route: {
          kind: 'kata',
          view: 'all-open',
          graph: false,
          filters: {
            status: ['open', 'ready'],
            owner: ['user-a', 'user-b'],
            label: ['backend', 'frontend'],
            relationship: ['blocks'],
          },
        },
        snapshot: snapshot(),
        loading: false,
        ...mutationProps(),
        onNavigate,
        onCreateProject: vi.fn(async () => ({ changed: true })),
      },
    })

    await fireEvent.input(screen.getByLabelText('Search tasks'), { target: { value: 'example' } })

    await waitFor(() =>
      expect(onNavigate).toHaveBeenCalledWith({
        kind: 'kata',
        view: 'all-open',
        graph: false,
        filters: {
          status: ['open', 'ready'],
          owner: ['user-a', 'user-b'],
          label: ['backend', 'frontend'],
          relationship: ['blocks'],
          text: 'example',
        },
      }),
    )
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
    accepted.catalog![0]!.project.metadata.role = 'inbox'
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

    const create = screen.getByRole('button', { name: 'New task' }) as HTMLButtonElement
    expect(create.disabled).toBe(false)
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
    expect(screen.getByRole('status', { name: 'Stale Kata data' })).not.toBeNull()
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

    let create = screen.getByRole('button', { name: 'New project' }) as HTMLButtonElement
    expect(create.disabled).toBe(true)

    await view.rerender({ canMutate: true, mutationPending: true })
    create = screen.getByRole('button', { name: 'New project' }) as HTMLButtonElement
    expect(create.disabled).toBe(true)

    await view.rerender({ mutationPending: false })
    create = screen.getByRole('button', { name: 'New project' }) as HTMLButtonElement
    expect(create.disabled).toBe(false)
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

    expect(within(view.container).getByRole('status').textContent).toContain(
      'unavailable from the current authority',
    )

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

    expect(within(view.container).getByRole('status').textContent).toContain('archived')
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
    contract_version: '1',
    cursor: 12,
    capabilities: { writable: true, updates: 'sse', actor_policy: 'identity' },
    origin: 'https://daemon.example',
    origin_stable: true,
    catalog: [
      {
        project: {
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
