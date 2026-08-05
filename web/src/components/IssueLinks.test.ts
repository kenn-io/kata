import { cleanup, fireEvent, render, screen, within } from '@testing-library/svelte'
import { afterEach, describe, expect, it, vi } from 'vitest'

import { createKataLinkFilters } from '../lib/kata/linkFilters'
import type { KataTaskDetail, KataTaskLink, KataTaskSummary } from '../lib/kata/types'
import IssueLinks from './IssueLinks.svelte'

describe('IssueLinks', () => {
  afterEach(cleanup)

  it('renders parent, child, blocks, blocked-by, and related directions from accepted authority', () => {
    renderLinks()

    const links = screen.getByRole('region', { name: 'Links' })
    for (const relation of ['parent', 'child', 'blocks', 'blocked_by', 'related']) {
      expect(within(links).getByText(relation)).not.toBeNull()
    }
  })

  it('creates related links and preserves the draft until accepted replacement authority', async () => {
    const onEditIssue = vi.fn(async () => true)
    const view = renderLinks({ onEditIssue, draftResetGeneration: 0 })

    await fireEvent.input(screen.getByLabelText('Related issue'), {
      target: { value: 'example-workspace#shared-1' },
    })
    await fireEvent.click(screen.getByRole('button', { name: 'Link' }))

    expect(onEditIssue).toHaveBeenCalledWith('issue-selected', {
      links_delta: { add_related: ['example-workspace#shared-1'] },
    })
    expect((screen.getByLabelText('Related issue') as HTMLInputElement).value).toBe(
      'example-workspace#shared-1',
    )

    await view.rerender({ draftResetGeneration: 1 })
    expect((screen.getByLabelText('Related issue') as HTMLInputElement).value).toBe('')
  })

  it('navigates resolved peers by their stable identity', async () => {
    const onSelectIssue = vi.fn()
    renderLinks({ onSelectIssue })

    await fireEvent.click(screen.getByRole('button', { name: /parent parent-1 Parent issue/ }))
    expect(onSelectIssue).toHaveBeenCalledWith({ uid: 'issue-parent' })
  })

  it('navigates linked peers that are absent from the filtered collection', async () => {
    const onSelectIssue = vi.fn()
    renderLinks({ issueCatalog: [], onSelectIssue })

    const parentLink = screen.getByRole('button', {
      name: /parent parent-1 open/,
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

    expect(screen.getByRole('button', { name: /parent example-workspace#parent-1/ })).not.toBeNull()

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
