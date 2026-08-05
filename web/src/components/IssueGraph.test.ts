import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/svelte'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import type { KataReachableGraphResponse, KataTaskSummary } from '../lib/kata/types'
import IssueGraph from './IssueGraph.svelte'

function task(overrides: Partial<KataTaskSummary> = {}): KataTaskSummary {
  const shortID = overrides.short_id ?? 'root'
  return {
    id: overrides.id ?? 1,
    uid: overrides.uid ?? 'issue-root',
    project_id: overrides.project_id ?? 7,
    project_uid: overrides.project_uid ?? 'project-example',
    project_name: overrides.project_name ?? 'example-project',
    short_id: shortID,
    qualified_id: overrides.qualified_id ?? `example-project#${shortID}`,
    title: overrides.title ?? 'Root issue',
    status: overrides.status ?? 'open',
    metadata: overrides.metadata ?? {},
    revision: overrides.revision ?? 1,
    author: overrides.author ?? 'user-a',
    priority: overrides.priority,
    closed_reason: overrides.closed_reason,
    created_at: overrides.created_at ?? '2026-08-01T12:00:00Z',
    updated_at: overrides.updated_at ?? '2026-08-01T12:00:00Z',
  }
}

function graphResponse(
  source: KataTaskSummary,
  nodes: KataTaskSummary[],
): KataReachableGraphResponse {
  return {
    source_uid: source.uid,
    depth: 'full',
    hide_done: false,
    nodes,
    edges: [],
    unresolved_refs: [],
    fetched_at: '2026-08-01T12:00:00Z',
  }
}

function graphNodeButtonWithText(text: string): HTMLButtonElement {
  const button = screen
    .getAllByText(text)
    .find((element) => element.closest('.svelte-flow__node'))
    ?.closest('.svelte-flow__node')
    ?.querySelector<HTMLButtonElement>('button.graph-task-node')
  expect(button).toBeTruthy()
  return button!
}

describe('IssueGraph', () => {
  beforeEach(() => {
    Object.defineProperty(window, 'matchMedia', {
      configurable: true,
      writable: true,
      value: vi.fn((query: string) => ({
        matches: false,
        media: query,
        onchange: null,
        addEventListener: vi.fn(),
        removeEventListener: vi.fn(),
        addListener: vi.fn(),
        removeListener: vi.fn(),
        dispatchEvent: vi.fn(),
      })),
    })
    class TestResizeObserver {
      observe = vi.fn()
      unobserve = vi.fn()
      disconnect = vi.fn()
    }
    vi.stubGlobal('ResizeObserver', TestResizeObserver)
  })

  afterEach(() => {
    cleanup()
    vi.restoreAllMocks()
  })

  it('keeps the snapshot graph rooted while selecting another graph node', async () => {
    const root = task({ uid: 'issue-root', short_id: 'root', title: 'Root issue' })
    const child = task({ uid: 'issue-child', short_id: 'child', title: 'Child issue' })
    const graph = {
      ...graphResponse(root, [root, child]),
      edges: [{ from_uid: root.uid, to_uid: child.uid, kind: 'parent' as const, layout: true }],
    }
    const onSelectIssue = vi.fn()
    const view = render(IssueGraph, {
      props: {
        graph,
        sourceIssue: root,
        selectedUID: root.uid,
        onBack: () => {},
        onSelectIssue,
      },
    })

    await fireEvent.click(graphNodeButtonWithText('Child issue'))
    expect(onSelectIssue).toHaveBeenCalledWith(child.uid)

    await view.rerender({
      graph,
      sourceIssue: root,
      selectedUID: child.uid,
      onBack: () => {},
      onSelectIssue,
    })

    expect(screen.getAllByText('Root issue').length).toBeGreaterThan(0)
    expect(screen.getByRole('button', { name: /Source task, Root issue/ })).toBeTruthy()
    expect(screen.getByRole('button', { name: /selected, Child issue/ })).toBeTruthy()
  })

  it('renders snapshot graph node titles and priority markers', () => {
    const root = task({ uid: 'issue-root', short_id: 'root', title: 'Root issue', priority: 0 })
    render(IssueGraph, {
      props: {
        graph: graphResponse(root, [root]),
        sourceIssue: root,
        selectedUID: root.uid,
        onBack: () => {},
        onSelectIssue: () => {},
      },
    })

    expect(screen.getByRole('region', { name: 'Reachable task graph' })).toBeTruthy()
    expect(screen.getAllByText('Root issue').length).toBeGreaterThan(0)
    expect(screen.getByText('P0')).toBeTruthy()
    expect(
      screen.getByRole('button', {
        name: /Source task, selected, Root issue, example-project#root, open/,
      }),
    ).toBeTruthy()
  })

  it('hides closed non-source issues regardless of closure reason', async () => {
    const root = task({ uid: 'issue-root', short_id: 'root' })
    const closed = task({
      uid: 'issue-closed',
      short_id: 'closed',
      title: 'Closed issue',
      status: 'closed',
      closed_reason: 'wontfix',
    })
    render(IssueGraph, {
      props: {
        graph: {
          ...graphResponse(root, [root, closed]),
          edges: [{ from_uid: root.uid, to_uid: closed.uid, kind: 'related', layout: false }],
        },
        sourceIssue: root,
        selectedUID: root.uid,
        onBack: () => {},
        onSelectIssue: () => {},
      },
    })

    await waitFor(() => expect(screen.getAllByText('Closed issue').length).toBeGreaterThan(0))
    await fireEvent.click(screen.getByRole('button', { name: /Graph filters/ }))
    await fireEvent.click(screen.getByRole('button', { name: 'Hide done' }))
    await waitFor(() => expect(screen.queryAllByText('Closed issue')).toEqual([]))
  })

  it('removes descendants reachable only through a hidden closed issue', async () => {
    const root = task({ uid: 'issue-root', short_id: 'root' })
    const closed = task({
      uid: 'issue-closed',
      short_id: 'closed',
      title: 'Closed intermediate',
      status: 'closed',
      closed_reason: 'duplicate',
    })
    const descendant = task({
      uid: 'issue-descendant',
      short_id: 'descendant',
      title: 'Open descendant',
    })
    render(IssueGraph, {
      props: {
        graph: {
          ...graphResponse(root, [root, closed, descendant]),
          edges: [
            { from_uid: root.uid, to_uid: closed.uid, kind: 'related', layout: false },
            { from_uid: closed.uid, to_uid: descendant.uid, kind: 'blocks', layout: true },
          ],
        },
        sourceIssue: root,
        selectedUID: root.uid,
        onBack: () => {},
        onSelectIssue: () => {},
      },
    })

    await waitFor(() => expect(screen.getAllByText('Open descendant').length).toBeGreaterThan(0))
    await fireEvent.click(screen.getByRole('button', { name: /Graph filters/ }))
    await fireEvent.click(screen.getByRole('button', { name: 'Hide done' }))
    await waitFor(() => expect(screen.queryAllByText('Closed intermediate')).toEqual([]))
    expect(screen.queryAllByText('Open descendant')).toEqual([])
  })

  it('excludes disconnected nodes at full depth', async () => {
    const root = task({ uid: 'issue-root', short_id: 'root', title: 'Root issue' })
    const child = task({ uid: 'issue-child', short_id: 'child', title: 'Child issue' })
    const disconnected = task({
      uid: 'issue-disconnected',
      short_id: 'elsewhere',
      title: 'Disconnected issue',
    })
    render(IssueGraph, {
      props: {
        graph: {
          ...graphResponse(root, [root, child, disconnected]),
          edges: [{ from_uid: root.uid, to_uid: child.uid, kind: 'parent', layout: true }],
        },
        sourceIssue: root,
        selectedUID: root.uid,
        onBack: () => {},
        onSelectIssue: () => {},
      },
    })

    await waitFor(() => expect(screen.getAllByText('Child issue').length).toBeGreaterThan(0))
    expect(screen.queryAllByText('Disconnected issue')).toEqual([])
  })

  it('selects cached nodes and returns to the list', async () => {
    const root = task({ uid: 'issue-root', short_id: 'root', title: 'Root issue' })
    const onSelectIssue = vi.fn()
    const onBack = vi.fn()
    render(IssueGraph, {
      props: {
        graph: graphResponse(root, [root]),
        sourceIssue: root,
        selectedUID: null,
        onBack,
        onSelectIssue,
      },
    })

    await waitFor(() => expect(graphNodeButtonWithText('Root issue')).toBeTruthy())
    await fireEvent.click(graphNodeButtonWithText('Root issue'))
    expect(onSelectIssue).toHaveBeenCalledWith('issue-root')

    onSelectIssue.mockClear()
    await fireEvent.keyDown(graphNodeButtonWithText('Root issue'), { key: 'Enter' })
    expect(onSelectIssue).toHaveBeenCalledWith('issue-root')

    onSelectIssue.mockClear()
    await fireEvent.keyDown(graphNodeButtonWithText('Root issue'), { key: ' ' })
    expect(onSelectIssue).toHaveBeenCalledTimes(1)
    expect(onSelectIssue).toHaveBeenCalledWith('issue-root')
    await fireEvent.click(screen.getByRole('button', { name: 'Back to task list' }))
    expect(onBack).toHaveBeenCalled()
  })
})
