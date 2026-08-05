import {
  cleanup,
  fireEvent,
  render as renderComponent,
  screen,
  waitFor,
  within,
} from '@testing-library/svelte'
import type { ComponentProps } from 'svelte'
import { afterEach, describe, expect, it, vi } from 'vitest'

import type { KataTaskSummary } from '../lib/kata/types'
import type { KataCurrentView } from '../lib/kata/authority'
import IssueCollection from './IssueCollection.svelte'
import { KATA_TASK_COLUMNS_STORAGE_KEY } from '../lib/kata/columns'

interface IssueListRenderOptions {
  props: {
    currentView: KataCurrentView
    issueCatalog?: readonly KataTaskSummary[] | undefined
    [key: string]: unknown
  }
}

function render(_component: typeof IssueCollection, options: IssueListRenderOptions) {
  const issueCatalog =
    options.props.issueCatalog ?? options.props.currentView.groups.flatMap((group) => group.issues)
  return renderComponent(IssueCollection, {
    props: { ...options.props, issueCatalog } as ComponentProps<typeof IssueCollection>,
  })
}

const baseIssues: KataTaskSummary[] = [
  task({
    id: 101,
    uid: 'issue-pay-rent',
    project_id: 2,
    project_uid: 'project-example',
    short_id: 'pay-rent',
    qualified_id: 'example-project#pay-rent',
    title: 'Review example project',
    project_name: 'example-project',
    owner: 'user-a',
    priority: 0,
    labels: ['home', 'monthly'],
    updated_at: '2026-05-14T08:00:00Z',
    metadata: { deadline_on: '2026-05-15' },
  }),
  task({
    id: 102,
    uid: 'issue-review-example',
    project_id: 3,
    project_uid: 'project-workspace',
    short_id: 'review-example',
    qualified_id: 'example-workspace#review-example',
    title: 'Prepare summary',
    project_name: 'example-workspace',
    owner: 'user-a',
    priority: 3,
    updated_at: '2026-05-16T08:00:00Z',
  }),
]

const currentView: KataCurrentView = {
  name: 'today',
  fetched_at: '2026-05-16T10:00:00Z',
  groups: [
    {
      id: 'overdue',
      title: 'Overdue',
      issues: [baseIssues[0]!],
    },
    {
      id: 'today',
      title: 'Today',
      issues: [baseIssues[1]!],
    },
  ],
}

describe('IssueCollection', () => {
  afterEach(() => {
    cleanup()
    window.localStorage.clear()
    vi.restoreAllMocks()
    vi.useRealTimers()
  })

  it('expands snapshot-bounded descendants without loading task detail', async () => {
    const parent = task({
      uid: 'issue-parent',
      short_id: 'parent',
      qualified_id: 'example-project#parent',
      title: 'Parent task',
      child_counts: { open: 1, total: 1 },
    })
    const child = task({
      uid: 'issue-child',
      short_id: 'child',
      qualified_id: 'example-project#child',
      title: 'Child task',
      parent_short_id: parent.short_id,
      child_counts: { open: 1, total: 1 },
    })
    const grandchild = task({
      uid: 'issue-grandchild',
      short_id: 'grandchild',
      qualified_id: 'example-project#grandchild',
      title: 'Grandchild task',
      parent_short_id: child.short_id,
    })

    render(IssueCollection, {
      props: {
        currentView: viewWithIssues([parent]),
        issueCatalog: [parent, child, grandchild],
        selectedIssueUID: null,
        loading: false,
        onSelect: () => {},
      },
    })

    const parentRow = screen.getByRole('button', { name: /Parent task/ })
    await fireEvent.keyDown(parentRow, { key: 'ArrowRight' })
    const childRow = await screen.findByRole('button', { name: /Child task/ })
    await fireEvent.keyDown(childRow, { key: 'ArrowRight' })

    expect(await screen.findByRole('button', { name: /Grandchild task/ })).toBeTruthy()
  })

  it('renders the heading, table columns, and the selected row metadata', () => {
    render(IssueCollection, {
      props: {
        currentView,
        selectedIssueUID: 'issue-pay-rent',
        loading: false,
        onSelect: () => {},
      },
    })

    expect(screen.getByRole('heading', { name: 'Today' })).toBeTruthy()
    expect(screen.getByRole('button', { name: /Sort by Priority/ })).toBeTruthy()
    expect(screen.getByRole('button', { name: /Sort by Updated/ })).toBeTruthy()
    expect(screen.getByRole('button', { name: /Sort by Title/ })).toBeTruthy()

    const row = screen.getByRole('button', {
      name: (name) =>
        name.includes('Review example project') &&
        name.includes('example-project#pay-rent') &&
        name.includes('project: example-project') &&
        name.includes('owner: user-a') &&
        name.includes('priority: 0') &&
        name.includes('home · monthly'),
    })
    expect(row.getAttribute('aria-current')).toBe('true')
    expect(row.classList.contains('selected')).toBe(true)
    expect(within(row).getByText('Review example project')).toBeTruthy()
    expect(within(row).getByText('example-project#pay-rent')).toBeTruthy()
    expect(within(row).getByText('P0')).toBeTruthy()
    expect(within(row).getByText('home · monthly')).toBeTruthy()
    expect(within(row).getByText('user-a')).toBeTruthy()
  })

  it('renders only authoritative ready rows even though other tasks are open', () => {
    const closed = task({
      ...baseIssues[1]!,
      id: 103,
      uid: 'issue-closed',
      short_id: 'closed',
      qualified_id: 'example-workspace#closed',
      title: 'Closed task',
      status: 'closed',
    })
    render(IssueCollection, {
      props: {
        currentView: {
          ...currentView,
          groups: [{ id: 'ready', title: 'Ready', issues: [...baseIssues, closed] }],
        },
        selectedIssueUID: null,
        loading: false,
        statusFilter: 'ready',
        readyIssueUIDs: new Set([baseIssues[0]!.uid]),
        onSelect: () => {},
      },
    })

    expect(screen.getByRole('button', { name: /Review example project/ })).toBeTruthy()
    expect(screen.queryByRole('button', { name: /Prepare summary/ })).toBeNull()
    expect(screen.queryByRole('button', { name: /Closed task/ })).toBeNull()
  })

  it('opens a graph from a row action without selecting the task', async () => {
    const onSelect = vi.fn()
    const onOpenGraph = vi.fn()
    render(IssueCollection, {
      props: {
        currentView,
        selectedIssueUID: null,
        loading: false,
        onSelect,
        onOpenGraph,
      },
    })

    const row = screen.getByRole('button', { name: /Review example project/ })
    const frame = row.parentElement
    expect(frame).toBeTruthy()
    await fireEvent.click(within(frame!).getByRole('button', { name: 'Open reachable graph' }))

    expect(onOpenGraph).toHaveBeenCalledWith(baseIssues[0])
    expect(onSelect).not.toHaveBeenCalled()
  })

  it('keeps snapshot loading out of the visual layout', () => {
    render(IssueCollection, {
      props: {
        currentView,
        selectedIssueUID: null,
        loading: true,
        onSelect: () => {},
      },
    })

    const loading = screen.getByText('Loading snapshot')
    expect(loading.classList.contains('kit-sr-only')).toBe(true)
    expect(screen.queryByText('Updating')).toBeNull()
  })

  it('keeps the header in the scrolling table and places Updated third', () => {
    const { container } = render(IssueCollection, {
      props: {
        currentView,
        selectedIssueUID: null,
        loading: false,
        onSelect: () => {},
      },
    })

    const tableBody = container.querySelector('.table-body')
    const tableHeader = container.querySelector('.table-header')
    expect(tableBody?.contains(tableHeader)).toBe(true)

    const labels = Array.from(tableHeader?.querySelectorAll('.col') ?? []).map((el) =>
      el.textContent?.trim(),
    )
    expect(labels.slice(0, 3)).toEqual(['ID', 'Title', 'Updated'])
  })

  it('hides an optional column while keeping ID and Title visible', async () => {
    const { container } = render(IssueCollection, {
      props: {
        currentView,
        selectedIssueUID: null,
        loading: false,
        onSelect: () => {},
      },
    })

    const header = container.querySelector<HTMLElement>('.table-header')!
    const row = screen.getByText('Review example project').closest('button')
    const table = container.querySelector<HTMLElement>('.table')
    expect(row).not.toBeNull()
    expect(table).not.toBeNull()
    expect(table!.style.getPropertyValue('--table-cols-wide')).toContain('minmax(96px, 200px)')

    await fireEvent.click(screen.getByRole('button', { name: 'Columns' }))
    await fireEvent.click(screen.getByRole('checkbox', { name: 'Tags' }))

    expect(within(header).getByText('ID')).toBeTruthy()
    expect(within(header).getByText('Title')).toBeTruthy()
    expect(within(header).queryByText('Tags')).toBeNull()
    expect(within(row!).queryByText('home · monthly')).toBeNull()
    expect(table!.style.getPropertyValue('--table-cols-wide')).not.toContain('minmax(96px, 200px)')
    expect(JSON.parse(localStorage.getItem(KATA_TASK_COLUMNS_STORAGE_KEY)!)).toEqual([
      'updated',
      'priority',
      'due',
      'owner',
    ])
  })

  it('restores hidden columns after remount and Show all resets the preference', async () => {
    const first = render(IssueCollection, {
      props: {
        currentView,
        selectedIssueUID: null,
        loading: false,
        onSelect: () => {},
      },
    })

    await fireEvent.click(screen.getByRole('button', { name: 'Columns' }))
    for (const name of ['Updated', 'Priority', 'Due', 'Owner', 'Tags']) {
      await fireEvent.click(screen.getByRole('checkbox', { name }))
    }
    first.unmount()

    const second = render(IssueCollection, {
      props: {
        currentView,
        selectedIssueUID: null,
        loading: false,
        onSelect: () => {},
      },
    })

    const header = second.container.querySelector<HTMLElement>('.table-header')!
    expect(within(header).queryByText('Updated')).toBeNull()
    expect(within(header).queryByText('Priority')).toBeNull()
    expect(within(header).queryByText('Due')).toBeNull()
    expect(within(header).queryByText('Owner')).toBeNull()
    expect(within(header).queryByText('Tags')).toBeNull()

    await fireEvent.click(screen.getByRole('button', { name: 'Columns' }))
    await fireEvent.click(screen.getByRole('button', { name: 'Show all' }))

    expect(within(header).getByText('Priority')).toBeTruthy()
    expect(within(header).getByText('Due')).toBeTruthy()
    expect(within(header).getByText('Owner')).toBeTruthy()
    expect(within(header).getByText('Tags')).toBeTruthy()
    expect(JSON.parse(localStorage.getItem(KATA_TASK_COLUMNS_STORAGE_KEY)!)).toEqual([
      'updated',
      'priority',
      'due',
      'owner',
      'tags',
    ])
  })

  it('reconciles a restored sort whose optional column is disabled', () => {
    localStorage.setItem(
      KATA_TASK_COLUMNS_STORAGE_KEY,
      JSON.stringify(['updated', 'priority', 'due', 'tags']),
    )
    localStorage.setItem('kata:issue-sort/v1', JSON.stringify({ key: 'owner', direction: 'desc' }))

    render(IssueCollection, {
      props: {
        currentView,
        selectedIssueUID: null,
        loading: false,
        onSelect: () => {},
      },
    })

    expect(screen.queryByRole('button', { name: /Sort by Owner/ })).toBeNull()
    expect(
      screen
        .getByRole('button', { name: 'Sort by Title, currently ascending' })
        .getAttribute('aria-pressed'),
    ).toBe('true')
    expect(JSON.parse(localStorage.getItem('kata:issue-sort/v1')!)).toEqual({
      key: 'title',
      direction: 'asc',
    })
  })

  it('keeps column toggles usable when localStorage writes fail', async () => {
    vi.spyOn(Storage.prototype, 'setItem').mockImplementation(() => {
      throw new Error('quota')
    })
    const { container } = render(IssueCollection, {
      props: {
        currentView,
        selectedIssueUID: null,
        loading: false,
        onSelect: () => {},
      },
    })

    await fireEvent.click(screen.getByRole('button', { name: 'Columns' }))
    await fireEvent.click(screen.getByRole('checkbox', { name: 'Owner' }))

    expect(
      within(container.querySelector<HTMLElement>('.table-header')!).queryByText('Owner'),
    ).toBeNull()
  })

  it('falls back to all columns for malformed saved data', () => {
    localStorage.setItem(KATA_TASK_COLUMNS_STORAGE_KEY, JSON.stringify({ visible: ['updated'] }))
    const { container } = render(IssueCollection, {
      props: {
        currentView,
        selectedIssueUID: null,
        loading: false,
        onSelect: () => {},
      },
    })

    const header = container.querySelector<HTMLElement>('.table-header')!
    for (const name of ['Updated', 'Priority', 'Due', 'Owner', 'Tags']) {
      expect(within(header).getByText(name)).toBeTruthy()
    }
  })

  it('resets an invisible active sort to Title ascending', async () => {
    render(IssueCollection, {
      props: {
        currentView,
        selectedIssueUID: null,
        loading: false,
        onSelect: () => {},
      },
    })

    await fireEvent.click(screen.getByRole('button', { name: 'Sort by Priority' }))
    expect(
      screen
        .getByRole('button', { name: 'Sort by Priority, currently ascending' })
        .getAttribute('aria-pressed'),
    ).toBe('true')

    await fireEvent.click(screen.getByRole('button', { name: 'Columns' }))
    await fireEvent.click(screen.getByRole('checkbox', { name: 'Priority' }))

    expect(screen.queryByRole('button', { name: /Sort by Priority/ })).toBeNull()
    expect(
      screen
        .getByRole('button', { name: 'Sort by Title, currently ascending' })
        .getAttribute('aria-pressed'),
    ).toBe('true')
    expect(JSON.parse(localStorage.getItem('kata:issue-sort/v1')!)).toEqual({
      key: 'title',
      direction: 'asc',
    })
  })

  it('defaults flat lists to recently updated first', () => {
    render(IssueCollection, {
      props: {
        currentView: viewWithIssues(baseIssues),
        selectedIssueUID: null,
        loading: false,
        onSelect: () => {},
      },
    })

    expect(visibleRowTitles()).toEqual(['Prepare summary', 'Review example project'])
  })

  it('clicking the Priority column header reorders rows by priority', async () => {
    render(IssueCollection, {
      props: {
        currentView: viewWithIssues(baseIssues),
        selectedIssueUID: null,
        loading: false,
        onSelect: () => {},
      },
    })

    expect(visibleRowTitles()).toEqual(['Prepare summary', 'Review example project'])

    await fireEvent.click(screen.getByRole('button', { name: /Sort by Priority/ }))

    expect(visibleRowTitles()).toEqual(['Review example project', 'Prepare summary'])

    await fireEvent.click(screen.getByRole('button', { name: /Sort by Priority/ }))

    expect(visibleRowTitles()).toEqual(['Prepare summary', 'Review example project'])
  })

  it('clicking the Updated column header flips the default recency order', async () => {
    render(IssueCollection, {
      props: {
        currentView,
        selectedIssueUID: null,
        loading: false,
        onSelect: () => {},
      },
    })

    await fireEvent.click(screen.getByRole('button', { name: /Sort by Updated/ }))

    expect(visibleRowTitles()).toEqual(['Review example project', 'Prepare summary'])
  })

  it('keeps grouped headings when sorting inside visible groups', async () => {
    render(IssueCollection, {
      props: {
        currentView,
        selectedIssueUID: null,
        loading: false,
        onSelect: () => {},
      },
    })

    await fireEvent.click(screen.getByRole('button', { name: /Sort by Priority/ }))

    expect(screen.getByRole('heading', { level: 3, name: /^Overdue\s+1$/ })).toBeTruthy()
    expect(screen.getByRole('heading', { level: 3, name: /^Today\s+1$/ })).toBeTruthy()

    await fireEvent.click(
      screen.getByRole('button', { name: /Sort by Priority, currently ascending/ }),
    )

    expect(screen.getByRole('heading', { level: 3, name: /^Overdue\s+1$/ })).toBeTruthy()
    expect(screen.getByRole('heading', { level: 3, name: /^Today\s+1$/ })).toBeTruthy()
  })

  it('hides child tasks from top-level rows and expands them on demand', async () => {
    const parent = task({
      uid: 'issue-parent',
      short_id: 'parent',
      qualified_id: 'example-project#parent',
      title: 'Parent task',
      child_counts: { open: 1, total: 1 },
    })
    const child = task({
      uid: 'issue-child',
      short_id: 'child',
      qualified_id: 'example-project#child',
      title: 'Child task',
      parent_short_id: parent.short_id,
    })
    const selected: string[] = []

    render(IssueCollection, {
      props: {
        currentView: viewWithIssues([parent, child]),
        issueCatalog: [parent, child],
        selectedIssueUID: null,
        loading: false,
        onSelect: (issue: KataTaskSummary) => selected.push(issue.uid),
      },
    })

    expect(screen.getByText('Parent task')).toBeTruthy()
    expect(screen.queryByText('Child task')).toBeNull()
    expect(screen.getByText('2 tasks')).toBeTruthy()

    const parentRow = screen.getByRole('button', { name: /Parent task/ })
    await fireEvent.keyDown(parentRow, { key: 'ArrowRight' })

    const childRow = await screen.findByRole('button', { name: /Child task/ })
    expect(childRow).toBeTruthy()
    expect(parentRow.getAttribute('aria-expanded')).toBe('true')
    expect(screen.getByText('2 tasks')).toBeTruthy()
    parentRow.focus()
    await fireEvent.keyDown(parentRow, { key: 'j' })
    await fireEvent.keyUp(childRow, { key: 'j' })
    expect(document.activeElement).toBe(childRow)
    await waitFor(() => {
      expect(selected[selected.length - 1]).toBe('issue-child')
    })

    await fireEvent.keyDown(parentRow, { key: 'ArrowLeft' })
    await waitFor(() => {
      expect(parentRow.getAttribute('aria-expanded')).toBe('false')
    })
    expect(screen.queryByRole('button', { name: /Child task/ })).toBeNull()
  })

  it('does not show an expanded child again as a flat row', async () => {
    const parent = task({
      uid: 'issue-parent',
      short_id: 'parent',
      qualified_id: 'example-project#parent',
      title: 'Parent task',
      child_counts: { open: 1, total: 1 },
    })
    const child = task({
      uid: 'issue-child',
      short_id: 'child',
      qualified_id: 'example-project#child',
      title: 'Child task',
      parent_short_id: parent.short_id,
    })
    render(IssueCollection, {
      props: {
        currentView: viewWithIssues([parent, child]),
        issueCatalog: [parent, child],
        selectedIssueUID: null,
        loading: false,
        onSelect: () => {},
      },
    })

    expect(screen.queryByRole('button', { name: /Child task/ })).toBeNull()

    const parentRow = screen.getByRole('button', { name: /Parent task/ })
    await fireEvent.keyDown(parentRow, { key: 'ArrowRight' })

    await waitFor(() => {
      expect(screen.getAllByRole('button', { name: /Child task/ })).toHaveLength(1)
    })
    expect(
      screen.getByRole('button', { name: /Child task/ }).classList.contains('row--child'),
    ).toBe(true)
  })

  it('uses the actual parent identity for cross-project hierarchy', async () => {
    const parent = task({
      uid: 'issue-cross-project-parent',
      project_id: 2,
      project_uid: 'project-example',
      project_name: 'example-project',
      short_id: 'parent',
      qualified_id: 'example-project#parent',
      title: 'Cross-project parent',
      child_counts: { open: 1, total: 1 },
    })
    const child = task({
      uid: 'issue-cross-project-child',
      project_id: 3,
      project_uid: 'project-workspace',
      project_name: 'example-workspace',
      short_id: 'child',
      qualified_id: 'example-workspace#child',
      title: 'Cross-project child',
      parent: { uid: parent.uid, short_id: parent.short_id },
      parent_short_id: parent.short_id,
    })
    render(IssueCollection, {
      props: {
        currentView: viewWithIssues([parent, child]),
        issueCatalog: [parent, child],
        selectedIssueUID: null,
        loading: false,
        onSelect: () => {},
      },
    })

    expect(screen.queryByRole('button', { name: /Cross-project child/ })).toBeNull()
    await fireEvent.keyDown(screen.getByRole('button', { name: /Cross-project parent/ }), {
      key: 'ArrowRight',
    })
    expect(await screen.findAllByRole('button', { name: /Cross-project child/ })).toHaveLength(1)
  })

  it('renders a matched child as a top-level row when its parent is absent', async () => {
    // A search or filter can surface a child whose parent is not in the
    // result set. The child has a parent_short_id, but with no visible
    // ancestor to fold into it must still render as its own row instead of
    // being dropped — otherwise the header counts it while the list shows
    // "No tasks".
    const child = task({
      uid: 'issue-child',
      short_id: 'child',
      qualified_id: 'example-project#child',
      title: 'Child task',
      parent_short_id: 'parent',
    })

    render(IssueCollection, {
      props: {
        currentView: viewWithIssues([child]),
        selectedIssueUID: null,
        loading: false,
        onSelect: () => {},
      },
    })

    expect(screen.getByRole('button', { name: /Child task/ })).toBeTruthy()
    expect(screen.queryByText('No tasks')).toBeNull()
    expect(screen.getByText('1 task')).toBeTruthy()
  })

  it('expands nested child rows beyond one level', async () => {
    const parent = task({
      uid: 'issue-parent',
      short_id: 'parent',
      qualified_id: 'example-project#parent',
      title: 'Parent task',
      child_counts: { open: 1, total: 1 },
    })
    const child = task({
      uid: 'issue-child',
      short_id: 'child',
      qualified_id: 'example-project#child',
      title: 'Child task',
      child_counts: { open: 1, total: 1 },
      parent_short_id: parent.short_id,
    })
    const grandchild = task({
      uid: 'issue-grandchild',
      short_id: 'grandchild',
      qualified_id: 'example-project#grandchild',
      title: 'Grandchild task',
      parent_short_id: child.short_id,
    })
    render(IssueCollection, {
      props: {
        currentView: viewWithIssues([parent]),
        issueCatalog: [parent, child, grandchild],
        selectedIssueUID: null,
        loading: false,
        onSelect: () => {},
      },
    })

    const parentRow = screen.getByRole('button', { name: /Parent task/ })
    await fireEvent.keyDown(parentRow, { key: 'ArrowRight' })

    const childRow = await screen.findByRole('button', { name: /Child task/ })
    expect(childRow.getAttribute('aria-expanded')).toBe('false')

    await fireEvent.keyDown(childRow, { key: 'ArrowRight' })

    const grandchildRow = await screen.findByRole('button', { name: /Grandchild task/ })
    expect(grandchildRow.classList.contains('row--child')).toBe(true)
  })

  it('expands and collapses every visible task tree from the header controls', async () => {
    const parent = task({
      uid: 'issue-parent',
      short_id: 'parent',
      qualified_id: 'example-project#parent',
      title: 'Parent task',
      child_counts: { open: 1, total: 1 },
    })
    const child = task({
      uid: 'issue-child',
      short_id: 'child',
      qualified_id: 'example-project#child',
      title: 'Child task',
      child_counts: { open: 1, total: 1 },
      parent_short_id: parent.short_id,
    })
    const grandchild = task({
      uid: 'issue-grandchild',
      short_id: 'grandchild',
      qualified_id: 'example-project#grandchild',
      title: 'Grandchild task',
      parent_short_id: child.short_id,
    })
    render(IssueCollection, {
      props: {
        currentView: viewWithIssues([parent]),
        issueCatalog: [parent, child, grandchild],
        selectedIssueUID: null,
        loading: false,
        onSelect: () => {},
      },
    })

    const expandAll = screen.getByRole('button', { name: 'Expand all tasks' })
    const collapseAll = screen.getByRole('button', { name: 'Collapse all tasks' })
    expect(collapseAll.hasAttribute('disabled')).toBe(true)

    await fireEvent.click(expandAll)

    const parentRow = screen.getByRole('button', { name: /Parent task/ })
    const childRow = await screen.findByRole('button', { name: /Child task/ })
    const grandchildRow = await screen.findByRole('button', { name: /Grandchild task/ })
    expect(parentRow.getAttribute('aria-expanded')).toBe('true')
    expect(childRow.getAttribute('aria-expanded')).toBe('true')
    expect(grandchildRow.classList.contains('row--child')).toBe(true)
    expect(expandAll.hasAttribute('disabled')).toBe(true)
    expect(collapseAll.hasAttribute('disabled')).toBe(false)

    await fireEvent.click(collapseAll)

    await waitFor(() => {
      expect(parentRow.getAttribute('aria-expanded')).toBe('false')
    })
    expect(screen.queryByRole('button', { name: /Child task/ })).toBeNull()
    expect(screen.queryByRole('button', { name: /Grandchild task/ })).toBeNull()
    expect(collapseAll.hasAttribute('disabled')).toBe(true)
  })

  it('j and k move focus and selection through rows', async () => {
    const selected: string[] = []
    render(IssueCollection, {
      props: {
        currentView,
        selectedIssueUID: null,
        loading: false,
        onSelect: (issue: KataTaskSummary) => selected.push(issue.uid),
      },
    })

    const rows = visibleRows()
    rows[0]!.focus()
    await fireEvent.keyDown(rows[0]!, { key: 'j' })
    await fireEvent.keyUp(rows[1]!, { key: 'j' })
    expect(document.activeElement).toBe(rows[1])
    await waitFor(() => {
      expect(selected[selected.length - 1]).toBe(rows[1]!.dataset.uid)
    })

    await fireEvent.keyDown(rows[1]!, { key: 'k' })
    await fireEvent.keyUp(rows[0]!, { key: 'k' })
    expect(document.activeElement).toBe(rows[0])
    await waitFor(() => {
      expect(selected[selected.length - 1]).toBe(rows[0]!.dataset.uid)
    })
  })

  it('debounces keyboard navigation so only the final row is selected', async () => {
    const { selected, rows } = renderKeyboardList(viewWithIssues([...baseIssues, thirdIssue()]))
    await fireEvent.keyDown(rows[0]!, { key: 'j' })
    await fireEvent.keyDown(rows[1]!, { key: 'j', repeat: true })
    await fireEvent.keyUp(rows[2]!, { key: 'j' })
    expect(document.activeElement).toBe(rows[2])
    expect(selected).toEqual([])

    vi.advanceTimersByTime(50)
    expect(selected).toEqual([rows[2]!.dataset.uid])
  })

  it('holds selection while a navigation key repeats slower than the debounce', async () => {
    const { selected, rows } = renderKeyboardList(viewWithIssues([...baseIssues, thirdIssue()]))
    // OS key-repeat slower than the 50ms debounce: each repeat arrives
    // after the timer has already expired. The held key must keep the
    // selection pending so intermediate rows never commit.
    await fireEvent.keyDown(rows[0]!, { key: 'j' })
    vi.advanceTimersByTime(100)
    expect(selected).toEqual([])

    await fireEvent.keyDown(rows[1]!, { key: 'j', repeat: true })
    vi.advanceTimersByTime(100)
    expect(selected).toEqual([])

    await fireEvent.keyUp(rows[2]!, { key: 'j' })
    vi.advanceTimersByTime(50)
    expect(selected).toEqual([rows[2]!.dataset.uid])
  })

  it('commits the selection when Shift is released before the navigation key', async () => {
    const { selected, rows } = renderKeyboardList()
    // Shift+g jumps to the end; the keydown reports key "G" but releasing
    // Shift first makes the keyup report key "g". The physical code is
    // stable across both, so the held entry must still clear.
    await fireEvent.keyDown(rows[0]!, { key: 'G', code: 'KeyG', shiftKey: true })
    expect(document.activeElement).toBe(rows[rows.length - 1])
    vi.advanceTimersByTime(50)
    expect(selected).toEqual([])

    await fireEvent.keyUp(rows[rows.length - 1]!, { key: 'g', code: 'KeyG' })
    vi.advanceTimersByTime(50)
    expect(selected).toEqual([rows[rows.length - 1]!.dataset.uid])
  })

  it('drops a pending keyboard selection when workspace navigation begins', async () => {
    vi.useFakeTimers()
    const selected: string[] = []
    const props = {
      currentView,
      selectedIssueUID: null,
      loading: false,
      navigationGeneration: 0,
      onSelect: (issue: KataTaskSummary) => selected.push(issue.uid),
    }
    const { rerender } = render(IssueCollection, { props })

    const rows = visibleRows()
    rows[0]!.focus()
    await fireEvent.keyDown(rows[0]!, { key: 'j' })

    // Navigation starts while the key is still held: the view data has
    // not arrived yet (same currentView, no remount), so only the
    // generation bump can stop the release from committing stale.
    await rerender({ ...props, navigationGeneration: 1 })
    await fireEvent.keyUp(rows[1]!, { key: 'j' })
    vi.advanceTimersByTime(100)
    expect(selected).toEqual([])
  })

  it('clicking a row selects immediately and cancels a pending keyboard selection', async () => {
    const { selected, rows } = renderKeyboardList()
    await fireEvent.keyDown(rows[0]!, { key: 'j' })
    expect(selected).toEqual([])

    await fireEvent.click(rows[0]!)
    expect(selected).toEqual([rows[0]!.dataset.uid])

    await fireEvent.keyUp(rows[0]!, { key: 'j' })
    vi.advanceTimersByTime(100)
    expect(selected).toEqual([rows[0]!.dataset.uid])
  })

  it('Home and End jump to first and last rows', async () => {
    render(IssueCollection, {
      props: {
        currentView,
        selectedIssueUID: null,
        loading: false,
        onSelect: () => {},
      },
    })

    const rows = visibleRows()
    rows[0]!.focus()
    await fireEvent.keyDown(rows[0]!, { key: 'End' })
    expect(document.activeElement).toBe(rows[rows.length - 1])

    await fireEvent.keyDown(rows[rows.length - 1]!, { key: 'Home' })
    expect(document.activeElement).toBe(rows[0])
  })

  it('resets expanded child rows when resetGeneration changes', async () => {
    const parent = task({
      uid: 'issue-parent',
      short_id: 'parent',
      qualified_id: 'example-project#parent',
      title: 'Parent task',
      child_counts: { open: 1, total: 1 },
    })
    const child = task({
      uid: 'issue-child',
      short_id: 'child',
      qualified_id: 'example-project#child',
      title: 'Child task',
      parent_short_id: parent.short_id,
    })
    const { rerender } = render(IssueCollection, {
      props: {
        currentView: viewWithIssues([parent]),
        issueCatalog: [parent, child],
        selectedIssueUID: null,
        loading: false,
        resetGeneration: 0,
        onSelect: () => {},
      },
    })

    const parentRow = screen.getByRole('button', { name: /Parent task/ })
    await fireEvent.keyDown(parentRow, { key: 'ArrowRight' })
    expect(await screen.findByRole('button', { name: /Child task/ })).toBeTruthy()

    await rerender({
      currentView: viewWithIssues([parent]),
      issueCatalog: [parent, child],
      selectedIssueUID: null,
      loading: false,
      resetGeneration: 1,
      onSelect: () => {},
    })

    await waitFor(() => {
      expect(screen.queryByRole('button', { name: /Child task/ })).toBeNull()
    })
  })

  it('keeps expanded child rows across live refreshes until resetGeneration changes', async () => {
    const parent = task({
      uid: 'issue-parent',
      short_id: 'parent',
      qualified_id: 'example-project#parent',
      title: 'Parent task',
      child_counts: { open: 1, total: 1 },
    })
    const child = task({
      uid: 'issue-child',
      short_id: 'child',
      qualified_id: 'example-project#child',
      title: 'Child task',
      parent_short_id: parent.short_id,
    })
    const { rerender } = render(IssueCollection, {
      props: {
        currentView: viewWithIssues([parent]),
        issueCatalog: [parent, child],
        selectedIssueUID: null,
        loading: false,
        resetGeneration: 0,
        onSelect: () => {},
      },
    })

    const parentRow = screen.getByRole('button', { name: /Parent task/ })
    await fireEvent.keyDown(parentRow, { key: 'ArrowRight' })
    expect(await screen.findByRole('button', { name: /Child task/ })).toBeTruthy()

    await rerender({
      currentView: {
        ...viewWithIssues([{ ...parent, updated_at: '2026-05-17T08:00:00Z' }]),
        fetched_at: '2026-05-17T10:00:00Z',
      },
      issueCatalog: [{ ...parent, updated_at: '2026-05-17T08:00:00Z' }, child],
      selectedIssueUID: null,
      loading: false,
      resetGeneration: 0,
      onSelect: () => {},
    })

    expect(screen.getByRole('button', { name: /Child task/ })).toBeTruthy()
    expect(screen.getByRole('button', { name: /Parent task/ }).getAttribute('aria-expanded')).toBe(
      'true',
    )
  })

  it('reveals a hidden selected child when structural reset and reveal arrive together', async () => {
    const root = task({
      uid: 'issue-reset-reveal-root',
      short_id: 'reset-reveal-root',
      qualified_id: 'example-project#reset-reveal-root',
      title: 'Reset reveal root',
      child_counts: { open: 1, total: 1 },
    })
    const child = task({
      uid: 'issue-reset-reveal-child',
      short_id: 'reset-reveal-child',
      qualified_id: 'example-project#reset-reveal-child',
      title: 'Reset reveal child',
      parent_short_id: root.short_id,
    })
    const scrollIntoView = vi.fn()
    vi.spyOn(Element.prototype, 'scrollIntoView').mockImplementation(scrollIntoView)
    const { rerender } = render(IssueCollection, {
      props: {
        currentView: viewWithIssues([root]),
        issueCatalog: [root, child],
        selectedIssueUID: null,
        loading: false,
        resetGeneration: 0,
        onSelect: () => {},
      },
    })
    const refreshedRoot = { ...root, revision: root.revision + 1 }

    await rerender({
      currentView: viewWithIssues([refreshedRoot]),
      issueCatalog: [refreshedRoot, child],
      selectedIssueUID: child.uid,
      loading: false,
      resetGeneration: 1,
      revealRequest: { uid: child.uid, chain: [refreshedRoot, child], generation: 1 },
      onSelect: () => {},
    })

    expect(await screen.findByRole('button', { name: /Reset reveal child/ })).toBeTruthy()
    expect(
      screen.getByRole('button', { name: /Reset reveal root/ }).getAttribute('aria-expanded'),
    ).toBe('true')
    await waitFor(() => expect(scrollIntoView).toHaveBeenCalledWith({ block: 'nearest' }))
  })

  it('expands restored ancestors root-first and scrolls the selected row nearest', async () => {
    const root = task({
      uid: 'issue-reveal-root',
      short_id: 'reveal-root',
      qualified_id: 'example-project#reveal-root',
      title: 'Root task',
    })
    const parent = task({
      uid: 'issue-reveal-parent',
      short_id: 'reveal-parent',
      qualified_id: 'example-project#reveal-parent',
      title: 'Parent task',
      parent_short_id: root.short_id,
    })
    const child = task({
      uid: 'issue-reveal-child',
      short_id: 'reveal-child',
      qualified_id: 'example-project#reveal-child',
      title: 'Child task',
      parent_short_id: parent.short_id,
    })
    const scrollIntoView = vi.fn()
    vi.spyOn(Element.prototype, 'scrollIntoView').mockImplementation(scrollIntoView)
    const { rerender } = render(IssueCollection, {
      props: {
        currentView: viewWithIssues([child]),
        issueCatalog: [root, parent, child],
        selectedIssueUID: child.uid,
        loading: false,
        onSelect: () => {},
      },
    })

    await rerender({
      currentView: viewWithIssues([child]),
      issueCatalog: [root, parent, child],
      selectedIssueUID: child.uid,
      loading: false,
      revealRequest: { uid: child.uid, chain: [root, parent, child], generation: 1 },
      onSelect: () => {},
    })

    const rootRow = await screen.findByRole('button', { name: /Root task/ })
    const childRow = screen.getByRole('button', { name: /Child task/ })
    await waitFor(() => expect(scrollIntoView).toHaveBeenCalledWith({ block: 'nearest' }))
    expect(rootRow.getAttribute('aria-expanded')).toBe('true')
    expect(screen.getByRole('button', { name: /Parent task/ }).getAttribute('aria-expanded')).toBe(
      'true',
    )
    expect(screen.getAllByRole('button', { name: /Child task/ })).toHaveLength(1)
    expect(document.activeElement).not.toBe(childRow)
  })

  it('merges a restored reveal chain with authoritative siblings during expand all', async () => {
    const root = task({
      uid: 'issue-reveal-root',
      short_id: 'reveal-root',
      qualified_id: 'example-project#reveal-root',
      title: 'Root task',
      child_counts: { open: 2, total: 2 },
    })
    const restoredChild = task({
      uid: 'issue-reveal-child',
      short_id: 'reveal-child',
      qualified_id: 'example-project#reveal-child',
      title: 'Restored child',
      child_counts: { open: 1, total: 1 },
      parent_short_id: root.short_id,
    })
    const restoredGrandchild = task({
      uid: 'issue-reveal-restored-grandchild',
      short_id: 'reveal-restored-grandchild',
      qualified_id: 'example-project#reveal-restored-grandchild',
      title: 'Restored grandchild',
      parent_short_id: restoredChild.short_id,
    })
    const sibling = task({
      uid: 'issue-reveal-sibling',
      short_id: 'reveal-sibling',
      qualified_id: 'example-project#reveal-sibling',
      title: 'Sibling task',
      child_counts: { open: 1, total: 1 },
      parent_short_id: root.short_id,
    })
    const grandchild = task({
      uid: 'issue-reveal-grandchild',
      short_id: 'reveal-grandchild',
      qualified_id: 'example-project#reveal-grandchild',
      title: 'Sibling grandchild',
      parent_short_id: sibling.short_id,
    })
    const issueCatalog = [root, restoredChild, restoredGrandchild, sibling, grandchild]

    const { rerender } = render(IssueCollection, {
      props: {
        currentView: viewWithIssues([restoredChild]),
        issueCatalog,
        selectedIssueUID: restoredChild.uid,
        loading: false,
        onSelect: () => {},
      },
    })

    await rerender({
      currentView: viewWithIssues([restoredChild]),
      issueCatalog,
      selectedIssueUID: restoredChild.uid,
      loading: false,
      revealRequest: { uid: restoredChild.uid, chain: [root, restoredChild], generation: 1 },
      onSelect: () => {},
    })

    expect(await screen.findByRole('button', { name: /Sibling task/ })).toBeTruthy()
    expect(screen.getAllByRole('button', { name: /Restored child/ })).toHaveLength(1)

    await fireEvent.click(screen.getByRole('button', { name: 'Expand all tasks' }))

    expect(await screen.findByRole('button', { name: /Sibling grandchild/ })).toBeTruthy()
    expect(await screen.findByRole('button', { name: /Restored grandchild/ })).toBeTruthy()
    expect(screen.getAllByRole('button', { name: /Restored child/ })).toHaveLength(1)

    await rerender({
      currentView: viewWithIssues([root]),
      issueCatalog,
      selectedIssueUID: null,
      loading: false,
      revealRequest: null,
      onSelect: () => {},
    })
    expect(screen.getByRole('button', { name: /Restored child/ })).toBeTruthy()
    expect(screen.getByRole('button', { name: /Restored grandchild/ })).toBeTruthy()
  })

  it('keeps a contextual successor visible without admitting unrelated filtered siblings', async () => {
    const root = task({
      uid: 'issue-filtered-reveal-root',
      short_id: 'filtered-reveal-root',
      qualified_id: 'example-project#filtered-reveal-root',
      title: 'Filtered reveal root',
      child_counts: { open: 1, total: 3 },
    })
    const contextualChild = task({
      uid: 'issue-filtered-contextual-child',
      short_id: 'filtered-contextual-child',
      qualified_id: 'example-project#filtered-contextual-child',
      title: 'Closed contextual child',
      status: 'closed',
      parent_short_id: root.short_id,
    })
    const openSibling = task({
      uid: 'issue-filtered-open-sibling',
      short_id: 'filtered-open-sibling',
      qualified_id: 'example-project#filtered-open-sibling',
      title: 'Open sibling',
      parent_short_id: root.short_id,
    })
    const closedSibling = task({
      uid: 'issue-filtered-closed-sibling',
      short_id: 'filtered-closed-sibling',
      qualified_id: 'example-project#filtered-closed-sibling',
      title: 'Unrelated closed sibling',
      status: 'closed',
      parent_short_id: root.short_id,
    })
    const issueCatalog = [root, closedSibling, openSibling, contextualChild]

    const { rerender } = render(IssueCollection, {
      props: {
        currentView: viewWithIssues([root]),
        issueCatalog,
        selectedIssueUID: contextualChild.uid,
        loading: false,
        statusFilter: 'open',
        onSelect: () => {},
      },
    })

    await rerender({
      currentView: viewWithIssues([root]),
      issueCatalog,
      selectedIssueUID: contextualChild.uid,
      loading: false,
      statusFilter: 'open',
      revealRequest: {
        uid: contextualChild.uid,
        chain: [root, contextualChild],
        generation: 1,
      },
      onSelect: () => {},
    })

    expect(await screen.findByRole('button', { name: /Closed contextual child/ })).toBeTruthy()
    expect(screen.getByRole('button', { name: /Open sibling/ })).toBeTruthy()
    expect(screen.queryByRole('button', { name: /Unrelated closed sibling/ })).toBeNull()
  })

  it('drops a synthetic reveal successor and its owned expansion after reveal cleanup', async () => {
    const root = task({
      uid: 'issue-reveal-root-cleanup',
      short_id: 'reveal-root-cleanup',
      qualified_id: 'example-project#reveal-root-cleanup',
      title: 'Cleanup root',
      child_counts: { open: 1, total: 1 },
    })
    const restoredChild = task({
      uid: 'issue-reveal-child-cleanup',
      short_id: 'reveal-child-cleanup',
      qualified_id: 'example-project#reveal-child-cleanup',
      title: 'Temporary restored child',
      parent_short_id: root.short_id,
    })
    const { rerender } = render(IssueCollection, {
      props: {
        currentView: viewWithIssues([restoredChild]),
        issueCatalog: [root, restoredChild],
        selectedIssueUID: restoredChild.uid,
        loading: false,
        onSelect: () => {},
      },
    })

    await rerender({
      currentView: viewWithIssues([restoredChild]),
      issueCatalog: [root, restoredChild],
      selectedIssueUID: restoredChild.uid,
      loading: false,
      revealRequest: { uid: restoredChild.uid, chain: [root, restoredChild], generation: 1 },
      onSelect: () => {},
    })
    expect(await screen.findByRole('button', { name: /Temporary restored child/ })).toBeTruthy()

    await rerender({
      currentView: viewWithIssues([root]),
      issueCatalog: [root],
      selectedIssueUID: null,
      loading: false,
      revealRequest: null,
      onSelect: () => {},
    })

    await waitFor(() =>
      expect(screen.queryByRole('button', { name: /Temporary restored child/ })).toBeNull(),
    )
    expect(
      screen.getByRole('button', { name: /Cleanup root/ }).getAttribute('aria-expanded'),
    ).toBeNull()
  })

  it('releases the previous reveal expansion when a newer chain supersedes it', async () => {
    const oldRoot = task({
      uid: 'issue-old-reveal-root',
      short_id: 'old-reveal-root',
      qualified_id: 'example-project#old-reveal-root',
      title: 'Old reveal root',
      child_counts: { open: 1, total: 1 },
    })
    const oldChild = task({
      uid: 'issue-old-reveal-child',
      short_id: 'old-reveal-child',
      qualified_id: 'example-project#old-reveal-child',
      title: 'Old reveal child',
      parent_short_id: oldRoot.short_id,
    })
    const newRoot = task({
      uid: 'issue-new-reveal-root',
      short_id: 'new-reveal-root',
      qualified_id: 'example-project#new-reveal-root',
      title: 'New reveal root',
      child_counts: { open: 1, total: 1 },
    })
    const newChild = task({
      uid: 'issue-new-reveal-child',
      short_id: 'new-reveal-child',
      qualified_id: 'example-project#new-reveal-child',
      title: 'New reveal child',
      parent_short_id: newRoot.short_id,
    })
    const issueCatalog = [oldRoot, oldChild, newRoot, newChild]

    const { rerender } = render(IssueCollection, {
      props: {
        currentView: viewWithIssues([oldRoot, newRoot]),
        issueCatalog,
        selectedIssueUID: oldChild.uid,
        loading: false,
        onSelect: () => {},
      },
    })

    await rerender({
      currentView: viewWithIssues([oldRoot, newRoot]),
      issueCatalog,
      selectedIssueUID: oldChild.uid,
      loading: false,
      revealRequest: { uid: oldChild.uid, chain: [oldRoot, oldChild], generation: 1 },
      onSelect: () => {},
    })
    expect(await screen.findByRole('button', { name: /Old reveal child/ })).toBeTruthy()
    expect(
      screen.getByRole('button', { name: /Old reveal root/ }).getAttribute('aria-expanded'),
    ).toBe('true')

    await rerender({
      currentView: viewWithIssues([oldRoot, newRoot]),
      issueCatalog,
      selectedIssueUID: newChild.uid,
      loading: false,
      revealRequest: { uid: newChild.uid, chain: [newRoot, newChild], generation: 2 },
      onSelect: () => {},
    })

    expect(await screen.findByRole('button', { name: /New reveal child/ })).toBeTruthy()
    expect(
      screen.getByRole('button', { name: /Old reveal root/ }).getAttribute('aria-expanded'),
    ).toBe('false')
    expect(
      screen.getByRole('button', { name: /New reveal root/ }).getAttribute('aria-expanded'),
    ).toBe('true')
    expect(screen.queryByRole('button', { name: /Old reveal child/ })).toBeNull()
  })

  it('preserves a user-owned expansion when reveal cleanup crosses the same chain', async () => {
    const root = task({
      uid: 'issue-user-expanded-root',
      short_id: 'user-expanded-root',
      qualified_id: 'example-project#user-expanded-root',
      title: 'User expanded root',
      child_counts: { open: 1, total: 1 },
    })
    const child = task({
      uid: 'issue-user-expanded-child',
      short_id: 'user-expanded-child',
      qualified_id: 'example-project#user-expanded-child',
      title: 'User expanded child',
      parent_short_id: root.short_id,
    })
    const { rerender } = render(IssueCollection, {
      props: {
        currentView: viewWithIssues([root]),
        issueCatalog: [root, child],
        selectedIssueUID: null,
        loading: false,
        onSelect: () => {},
      },
    })

    const rootRow = screen.getByRole('button', { name: /User expanded root/ })
    await fireEvent.keyDown(rootRow, { key: 'ArrowRight' })
    expect(await screen.findByRole('button', { name: /User expanded child/ })).toBeTruthy()

    await rerender({
      currentView: viewWithIssues([root]),
      issueCatalog: [root, child],
      selectedIssueUID: child.uid,
      loading: false,
      revealRequest: { uid: child.uid, chain: [root, child], generation: 1 },
      onSelect: () => {},
    })
    await rerender({
      currentView: viewWithIssues([root]),
      issueCatalog: [root, child],
      selectedIssueUID: null,
      loading: false,
      revealRequest: null,
      onSelect: () => {},
    })

    expect(
      screen.getByRole('button', { name: /User expanded root/ }).getAttribute('aria-expanded'),
    ).toBe('true')
    expect(screen.getByRole('button', { name: /User expanded child/ })).toBeTruthy()
  })

  it('continues a seeded reveal chain from the accepted catalog', async () => {
    const root = task({
      uid: 'issue-reveal-root-failure',
      short_id: 'reveal-root-failure',
      qualified_id: 'example-project#reveal-root-failure',
      title: 'Fallback root',
    })
    const child = task({
      uid: 'issue-reveal-child-failure',
      short_id: 'reveal-child-failure',
      qualified_id: 'example-project#reveal-child-failure',
      title: 'Fallback child',
      parent_short_id: root.short_id,
    })
    const { rerender } = render(IssueCollection, {
      props: {
        currentView: viewWithIssues([child]),
        issueCatalog: [root, child],
        selectedIssueUID: child.uid,
        loading: false,
        onSelect: () => {},
      },
    })

    await rerender({
      currentView: viewWithIssues([child]),
      issueCatalog: [root, child],
      selectedIssueUID: child.uid,
      loading: false,
      revealRequest: { uid: child.uid, chain: [root, child], generation: 1 },
      onSelect: () => {},
    })

    const rootRow = await screen.findByRole('button', { name: /Fallback root/ })
    expect(await screen.findByRole('button', { name: /Fallback child/ })).toBeTruthy()
    expect(rootRow.getAttribute('aria-expanded')).toBe('true')
  })

  it('keeps group headings while scrolling to a restored top-level task', async () => {
    const scrollIntoView = vi.fn()
    vi.spyOn(Element.prototype, 'scrollIntoView').mockImplementation(scrollIntoView)

    const { rerender } = render(IssueCollection, {
      props: {
        currentView,
        selectedIssueUID: baseIssues[0]!.uid,
        loading: false,
        onSelect: () => {},
      },
    })
    await fireEvent.click(screen.getByRole('button', { name: /Sort by Priority/ }))

    await rerender({
      currentView,
      selectedIssueUID: baseIssues[0]!.uid,
      loading: false,
      revealRequest: { uid: baseIssues[0]!.uid, chain: [baseIssues[0]!], generation: 1 },
      onSelect: () => {},
    })

    await waitFor(() => expect(scrollIntoView).toHaveBeenCalledWith({ block: 'nearest' }))
    expect(screen.getByRole('heading', { level: 3, name: /^Overdue\s+1$/ })).toBeTruthy()
    expect(screen.getByRole('heading', { level: 3, name: /^Today\s+1$/ })).toBeTruthy()
  })

  it('does not reveal a non-ready ancestor for a ready task', async () => {
    const parent = task({
      uid: 'issue-blocked-parent',
      short_id: 'blocked-parent',
      qualified_id: 'example-project#blocked-parent',
      title: 'Blocked parent',
      child_counts: { open: 1, total: 1 },
    })
    const child = task({
      uid: 'issue-ready-child',
      short_id: 'ready-child',
      qualified_id: 'example-project#ready-child',
      title: 'Ready child',
      parent_short_id: parent.short_id,
    })
    render(IssueCollection, {
      props: {
        currentView: viewWithIssues([]),
        issueCatalog: [parent, child],
        selectedIssueUID: child.uid,
        loading: false,
        statusFilter: 'ready',
        readyIssueUIDs: new Set([child.uid]),
        revealRequest: { uid: child.uid, chain: [parent, child], generation: 1 },
        onSelect: () => {},
      },
    })

    expect(await screen.findByRole('button', { name: /Ready child/ })).toBeTruthy()
    expect(screen.queryByRole('button', { name: /Blocked parent/ })).toBeNull()
  })

  it('promotes a ready target instead of reconnecting it across a non-ready ancestor', async () => {
    const grandparent = task({
      uid: 'issue-ready-grandparent',
      short_id: 'ready-grandparent',
      qualified_id: 'example-project#ready-grandparent',
      title: 'Ready grandparent',
    })
    const parent = task({
      uid: 'issue-blocked-middle',
      short_id: 'blocked-middle',
      qualified_id: 'example-project#blocked-middle',
      title: 'Blocked middle',
      parent_short_id: grandparent.short_id,
    })
    const child = task({
      uid: 'issue-ready-target',
      short_id: 'ready-target',
      qualified_id: 'example-project#ready-target',
      title: 'Ready target',
      parent_short_id: parent.short_id,
    })

    render(IssueCollection, {
      props: {
        currentView: viewWithIssues([]),
        selectedIssueUID: child.uid,
        loading: false,
        statusFilter: 'ready',
        readyIssueUIDs: new Set([grandparent.uid, child.uid]),
        revealRequest: { uid: child.uid, chain: [grandparent, parent, child], generation: 1 },
        onSelect: () => {},
      },
    })

    expect(await screen.findByRole('button', { name: /Ready target/ })).toBeTruthy()
    expect(screen.queryByRole('button', { name: /Ready grandparent/ })).toBeNull()
    expect(screen.queryByRole('button', { name: /Blocked middle/ })).toBeNull()
  })

  it('clears reveal-owned expansion after ordinary row selection', async () => {
    const parent = task({
      uid: 'issue-reveal-parent',
      short_id: 'reveal-parent',
      qualified_id: 'example-project#reveal-parent',
      title: 'Parent task',
      child_counts: { open: 1, total: 1 },
    })
    const child = task({
      uid: 'issue-reveal-child',
      short_id: 'reveal-child',
      qualified_id: 'example-project#reveal-child',
      title: 'Child task',
      parent_short_id: parent.short_id,
    })
    const onSelect = vi.fn()
    const { rerender } = render(IssueCollection, {
      props: {
        currentView: viewWithIssues([parent, child]),
        issueCatalog: [parent, child],
        selectedIssueUID: child.uid,
        loading: false,
        onSelect,
      },
    })

    await rerender({
      currentView: viewWithIssues([parent, child]),
      issueCatalog: [parent, child],
      selectedIssueUID: child.uid,
      loading: false,
      revealRequest: { uid: child.uid, chain: [parent, child], generation: 1 },
      onSelect,
    })

    const childRow = await screen.findByRole('button', { name: /Child task/ })
    await fireEvent.click(childRow)

    await waitFor(() => expect(onSelect).toHaveBeenCalledWith(child))
    await waitFor(() => expect(screen.queryByRole('button', { name: /Child task/ })).toBeNull())
    expect(screen.getByRole('button', { name: /Parent task/ }).getAttribute('aria-expanded')).toBe(
      'false',
    )
  })

  it('uses the replacement snapshot catalog after the list resets', async () => {
    const parent = task({
      uid: 'issue-parent',
      short_id: 'parent',
      qualified_id: 'example-project#parent',
      title: 'Parent task',
      child_counts: { open: 1, total: 1 },
    })
    const staleChild = task({
      uid: 'issue-stale-child',
      short_id: 'stale-child',
      qualified_id: 'example-project#stale-child',
      title: 'Stale child',
      parent_short_id: parent.short_id,
    })
    const freshChild = task({
      uid: 'issue-fresh-child',
      short_id: 'fresh-child',
      qualified_id: 'example-project#fresh-child',
      title: 'Fresh child',
      parent_short_id: parent.short_id,
    })
    const { rerender } = render(IssueCollection, {
      props: {
        currentView: viewWithIssues([parent]),
        issueCatalog: [parent, staleChild],
        selectedIssueUID: null,
        loading: false,
        resetGeneration: 0,
        onSelect: () => {},
      },
    })

    await fireEvent.keyDown(screen.getByRole('button', { name: /Parent task/ }), {
      key: 'ArrowRight',
    })
    expect(await screen.findByRole('button', { name: /Stale child/ })).toBeTruthy()
    await rerender({
      currentView: viewWithIssues([{ ...parent, updated_at: '2026-05-17T08:00:00Z' }]),
      issueCatalog: [{ ...parent, updated_at: '2026-05-17T08:00:00Z' }, freshChild],
      selectedIssueUID: null,
      loading: false,
      resetGeneration: 1,
      onSelect: () => {},
    })

    expect(screen.queryByRole('button', { name: /Stale child/ })).toBeNull()

    await fireEvent.keyDown(screen.getByRole('button', { name: /Parent task/ }), {
      key: 'ArrowRight',
    })
    expect(await screen.findByRole('button', { name: /Fresh child/ })).toBeTruthy()
    expect(screen.queryByRole('button', { name: /Stale child/ })).toBeNull()
  })
})

// Shared setup for the fake-timer keyboard tests: renders the list,
// records selections, and focuses the first row ready for key events.
function renderKeyboardList(view: KataCurrentView = currentView) {
  vi.useFakeTimers()
  const selected: string[] = []
  render(IssueCollection, {
    props: {
      currentView: view,
      selectedIssueUID: null,
      loading: false,
      onSelect: (issue: KataTaskSummary) => selected.push(issue.uid),
    },
  })
  const rows = visibleRows()
  rows[0]!.focus()
  return { selected, rows }
}

function thirdIssue(): KataTaskSummary {
  return task({
    id: 103,
    uid: 'issue-water-plants',
    short_id: 'water-plants',
    qualified_id: 'Home#water-plants',
    title: 'Water plants',
    updated_at: '2026-05-13T08:00:00Z',
  })
}

function visibleRows(): HTMLElement[] {
  return screen
    .getAllByRole('button')
    .filter(
      (row): row is HTMLElement => row instanceof HTMLElement && row.classList.contains('row'),
    )
}

function visibleRowTitles(): string[] {
  return visibleRows()
    .filter((row) => !row.classList.contains('row--child'))
    .map((row) => row.querySelector('.title-text')?.textContent?.trim() ?? '')
}

function viewWithIssues(issues: KataTaskSummary[]): KataCurrentView {
  return {
    name: 'all',
    fetched_at: '2026-05-16T10:00:00Z',
    groups: [{ id: 'all', title: 'All Open', issues }],
  }
}

function task(overrides: Partial<KataTaskSummary>): KataTaskSummary {
  return {
    id: 1,
    uid: 'issue-uid',
    project_id: 2,
    project_uid: 'project-example',
    short_id: 'task',
    qualified_id: 'example-project#task',
    title: 'Task',
    status: 'open',
    project_name: 'example-project',
    metadata: {},
    revision: 1,
    author: 'user-a',
    owner: undefined,
    priority: undefined,
    labels: [],
    created_at: '2026-05-10T08:00:00Z',
    updated_at: '2026-05-15T08:00:00Z',
    ...overrides,
  }
}
