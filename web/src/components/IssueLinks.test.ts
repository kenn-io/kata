import { cleanup, fireEvent, render, screen, within, waitFor } from '@testing-library/svelte'
import { afterEach, describe, expect, it, vi } from 'vitest'

import { createKataLinkFilters } from '../lib/kata/linkFilters'
import type { KataTaskDetail, KataTaskLink, KataTaskSummary } from '../lib/kata/types'
import IssueLinks from './IssueLinks.svelte'

describe('IssueLinks', () => {
  afterEach(cleanup)

  it('groups links by direction', () => {
    renderLinks()
    const links = screen.getByRole('region', { name: 'Links' })
    for (const group of ['Parent', 'Child', 'Blocks', 'Blocked by', 'Related']) {
      expect(within(within(links).getByRole('group', { name: group })).getByText('1')).toBeTruthy()
    }
  })

  it.each([
    { query: 'Shared', option: /Shared task/, ref: 'example-workspace#shared-1' },
    { query: 'other#abcd', option: 'Use "other#abcd"', ref: 'other#abcd' },
  ])('adds the selected reference for $query', async ({ query, option, ref }) => {
    const onEditIssue = vi.fn(async () => true)
    const result = {
      uid: 'peer',
      qualified_id: 'example-workspace#shared-1',
      short_id: 'shared-1',
      project_uid: 'project-b',
      project_name: 'example-workspace',
      title: 'Shared task',
      status: 'open',
    }
    renderLinks({
      onEditIssue,
      searchReferences: vi.fn(async () => (query === 'Shared' ? [result] : [])),
    })
    await fireEvent.click(screen.getByRole('button', { name: 'Add' }))
    await fireEvent.click(screen.getByRole('button', { name: 'Search tasks: Find task' }))
    await fireEvent.input(screen.getByPlaceholderText('Search tasks'), { target: { value: query } })
    await fireEvent.mouseDown(await screen.findByRole('option', { name: option }))
    await waitFor(() =>
      expect(onEditIssue).toHaveBeenCalledWith('issue-selected', {
        links_delta: { add_related: [ref] },
      }),
    )
  })

  it('navigates resolved peers by their stable identity', async () => {
    const onSelectIssue = vi.fn()
    renderLinks({ onSelectIssue })

    await fireEvent.click(screen.getByRole('button', { name: /Parent parent-1 Parent issue/ }))
    expect(onSelectIssue).toHaveBeenCalledWith({ uid: 'issue-parent' })
  })

  it('navigates linked peers that are absent from the filtered collection', async () => {
    const onSelectIssue = vi.fn()
    renderLinks({ issueCatalog: [], onSelectIssue })

    const parentLink = screen.getByRole('button', {
      name: /Parent parent-1/,
    }) as HTMLButtonElement
    expect(parentLink.disabled).toBe(false)
    await fireEvent.click(parentLink)
    expect(onSelectIssue).toHaveBeenCalledWith({ uid: 'issue-parent' })
  })

  it('filters and qualifies linked peers from endpoint authority when the catalog omits them', async () => {
    const selected = issue()
    selected.links = [
      {
        ...selected.links[0]!,
        to: {
          ...selected.links[0]!.to,
          qualified_id: 'example-workspace#parent-1',
          status: 'closed',
        },
      },
    ]
    const view = renderLinks({
      issue: selected,
      issueCatalog: [],
      linkFilters: createKataLinkFilters('closed'),
    })

    const closed = screen
      .getByRole('button', { name: /Parent example-workspace#parent-1/ })
      .closest('details')!
    expect(closed.open).toBe(false)
    await fireEvent.click(within(closed).getByText('1 closed'))
    expect(closed.open).toBe(true)

    await view.rerender({ linkFilters: createKataLinkFilters('open') })
    expect(screen.queryByRole('button', { name: /example-workspace#parent-1/ })).toBeNull()
  })

  it('filters five links and restores a carried filter on a short list', async () => {
    const selected = issue()
    const allLinks = [...selected.links]
    selected.links = [link(1, 'related', 'issue-selected', 'issue-related')]
    const onLinkFiltersChange = vi.fn()
    const view = renderLinks({ issue: selected, onLinkFiltersChange })
    await fireEvent.click(screen.getByRole('button', { name: 'Filter links' }))
    await fireEvent.click(screen.getByRole('checkbox', { name: 'Related' }))
    await view.rerender({ linkFilters: onLinkFiltersChange.mock.calls[0]![0]! })
    expect(screen.getByText('No matching links')).toBeTruthy()
    await fireEvent.click(screen.getByRole('checkbox', { name: 'Related' }))
    await view.rerender({ linkFilters: onLinkFiltersChange.mock.calls[1]![0]! })

    await view.rerender({ issue: { ...selected, links: allLinks } })
    await fireEvent.click(screen.getByRole('checkbox', { name: 'Parent' }))
    await view.rerender({ linkFilters: onLinkFiltersChange.mock.calls[2]![0]! })
    expect(screen.queryByRole('button', { name: /Parent parent-1/ })).toBeNull()
    expect(screen.getByRole('button', { name: /Child child-1/ })).toBeTruthy()
    await fireEvent.click(screen.getByRole('checkbox', { name: 'Open' }))
    await view.rerender({ issue: selected, linkFilters: onLinkFiltersChange.mock.calls[3]![0]! })
    expect(screen.getByText('No matching links')).toBeTruthy()
    await fireEvent.click(screen.getByRole('checkbox', { name: 'Open' }))
    await view.rerender({ linkFilters: onLinkFiltersChange.mock.calls[4]![0]! })
    expect(screen.getByRole('button', { name: /Related related-1/ })).toBeTruthy()
  })
})

function renderLinks(overrides: Record<string, unknown> = {}) {
  const selected = issue()
  return render(IssueLinks, {
    props: {
      issue: selected,
      issueCatalog: peers(),
      linkFilters: createKataLinkFilters('all'),
      onLinkFiltersChange: vi.fn(),
      actionsDisabled: false,
      onEditIssue: vi.fn(async () => true),
      onSelectIssue: vi.fn(),
      ...overrides,
    },
  })
}

function issue(): KataTaskDetail {
  const selected = task('issue-selected', 'selected-1', 'Selected issue')
  return {
    issue: { ...selected, body: 'Body' },
    comments: [],
    labels: [],
    links: [
      link(1, 'parent', 'issue-selected', 'issue-parent'),
      link(2, 'parent', 'issue-child', 'issue-selected'),
      link(3, 'blocks', 'issue-selected', 'issue-blocked'),
      link(4, 'blocks', 'issue-blocker', 'issue-selected'),
      link(5, 'related', 'issue-selected', 'issue-related'),
    ],
  }
}

function peers(): KataTaskSummary[] {
  return [
    task('issue-parent', 'parent-1', 'Parent issue'),
    task('issue-child', 'child-1', 'Child issue'),
    task('issue-blocked', 'blocked-1', 'Blocked issue'),
    task('issue-blocker', 'blocker-1', 'Blocking issue'),
    task('issue-related', 'related-1', 'Related issue'),
  ]
}

function task(uid: string, shortID: string, title: string): KataTaskSummary {
  return {
    id: peersIDs[uid] ?? 1,
    uid,
    project_id: 1,
    project_uid: 'project-a',
    project_name: 'example-project',
    short_id: shortID,
    qualified_id: `example-project#${shortID}`,
    title,
    status: 'open',
    metadata: {},
    revision: 1,
    author: 'user-a',
    created_at: '2026-08-01T12:00:00Z',
    updated_at: '2026-08-01T12:00:00Z',
  }
}

function link(
  id: number,
  type: KataTaskLink['type'],
  fromUID: string,
  toUID: string,
): KataTaskLink {
  const records = new Map(
    peers()
      .concat(task('issue-selected', 'selected-1', 'Selected issue'))
      .map((item) => [item.uid, item]),
  )
  return {
    id,
    project_id: 1,
    from: {
      uid: fromUID,
      short_id: records.get(fromUID)!.short_id,
      qualified_id: records.get(fromUID)!.qualified_id,
      status: records.get(fromUID)!.status,
    },
    to: {
      uid: toUID,
      short_id: records.get(toUID)!.short_id,
      qualified_id: records.get(toUID)!.qualified_id,
      status: records.get(toUID)!.status,
    },
    type,
    author: 'user-a',
    created_at: '2026-08-01T12:00:00Z',
  }
}

const peersIDs: Record<string, number> = {
  'issue-selected': 1,
  'issue-parent': 2,
  'issue-child': 3,
  'issue-blocked': 4,
  'issue-blocker': 5,
  'issue-related': 6,
}
