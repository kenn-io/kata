import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/svelte'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import type { KataReachableGraphResponse, KataTaskSummary } from '../lib/kata/types'
import { task } from '../test/task'
import IssueGraph from './IssueGraph.svelte'

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
    window.localStorage.removeItem('kata.graph.preferences.v1')
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

  it('renders task titles, status, priority, project marks, and IDs', () => {
    const root = task({ uid: 'issue-root', short_id: 'root', title: 'Root issue', priority: 0 })
    const done = task({
      uid: 'issue-done',
      short_id: 'done',
      title: 'Done issue',
      status: 'closed',
      closed_reason: 'done',
    })
    render(IssueGraph, {
      props: {
        graph: {
          ...graphResponse(root, [root, done]),
          edges: [{ from_uid: root.uid, to_uid: done.uid, kind: 'parent' as const, layout: true }],
        },
        sourceIssue: root,
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
    const rootNode = graphNodeButtonWithText('Root issue')
    expect(rootNode.querySelector('.status-mark--done')).toBeNull()
    expect(rootNode.querySelector('.project-mark')).toBeTruthy()
    expect(rootNode.querySelector('.node-id')?.textContent).toBe('root')
    expect(graphNodeButtonWithText('Done issue').querySelector('.status-mark--done')).toBeTruthy()
  })

  it('keys the three line styles', () => {
    const root = task({ uid: 'issue-root', short_id: 'root' })
    render(IssueGraph, {
      props: {
        graph: graphResponse(root, [root]),
        sourceIssue: root,
        onBack: () => {},
        onSelectIssue: () => {},
      },
    })

    const key = screen.getByRole('group', { name: 'Line key' })
    expect(
      [...key.querySelectorAll('.graph-key-row')].map((row) => row.textContent?.trim()),
    ).toEqual(['Blocks', 'Parent → child', 'Related'])
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
        onBack: () => {},
        onSelectIssue: () => {},
      },
    })

    await waitFor(() => expect(screen.getAllByText('Closed issue').length).toBeGreaterThan(0))
    const trigger = screen.getByRole('button', { name: /Graph filters/ })
    expect(trigger.textContent?.trim()).toBe('Graph filters')
    await fireEvent.click(trigger)
    await fireEvent.click(screen.getByRole('button', { name: 'Hide closed' }))
    await waitFor(() => expect(screen.queryAllByText('Closed issue')).toEqual([]))
    await fireEvent.click(screen.getByRole('button', { name: 'Hide closed' }))
    await waitFor(() => expect(screen.getAllByText('Closed issue').length).toBeGreaterThan(0))
    expect(trigger.textContent?.trim()).toBe('Graph filters')
  })

  it('selects cached nodes and returns to the list', async () => {
    const root = task({ uid: 'issue-root', short_id: 'root', title: 'Root issue' })
    const onSelectIssue = vi.fn()
    const onBack = vi.fn()
    render(IssueGraph, {
      props: {
        graph: graphResponse(root, [root]),
        sourceIssue: root,
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
