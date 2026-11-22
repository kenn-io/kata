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
    for (const group of ['Parent 1', 'Child 1', 'Blocks 1', 'Blocked by 1', 'Related 1']) {
      expect(within(links).getByText(group)).not.toBeNull()
    }
  })

  it('searches and adds a related task', async () => {
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
    renderLinks({ onEditIssue, searchReferences: vi.fn(async () => [result]) })
    await fireEvent.click(screen.getByRole('button', { name: 'Add' }))
    await fireEvent.click(screen.getByRole('button', { name: 'Search tasks: Find task' }))
    await fireEvent.input(screen.getByPlaceholderText('Search tasks'), {
      target: { value: 'Shared' },
    })
    await waitFor(() => expect(screen.getByRole('option', { name: /Shared task/ })).toBeTruthy())
    await fireEvent.mouseDown(screen.getByRole('option', { name: /Shared task/ }))
    await waitFor(() =>
      expect(onEditIssue).toHaveBeenCalledWith('issue-selected', {
        links_delta: { add_related: ['example-workspace#shared-1'] },
      }),
    )
  })

  it('accepts a manually entered task reference', async () => {
    const onEditIssue = vi.fn(async () => true)
    renderLinks({ onEditIssue })
    await fireEvent.click(screen.getByRole('button', { name: 'Add' }))
    await fireEvent.click(screen.getByRole('button', { name: 'Search tasks: Find task' }))
    await fireEvent.input(screen.getByPlaceholderText('Search tasks'), {
      target: { value: 'other#abcd' },
    })
    await fireEvent.mouseDown(await screen.findByRole('option', { name: 'Use "other#abcd"' }))
    await waitFor(() =>
      expect(onEditIssue).toHaveBeenCalledWith('issue-selected', {
        links_delta: { add_related: ['other#abcd'] },
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
      draftResetGeneration: 0,
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
