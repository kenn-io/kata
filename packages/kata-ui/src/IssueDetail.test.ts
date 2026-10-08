// @vitest-environment jsdom

import { cleanup, fireEvent, render, screen, within } from '@testing-library/svelte'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { tick } from 'svelte'

import IssueDetail from './IssueDetail.svelte'
import type { KataIssueDetailModel } from './types.js'

const detail: KataIssueDetailModel = {
  issue: {
    uid: '01TASK',
    projectUID: '01PROJECT',
    projectName: 'Roadmap',
    reference: 'roadmap#abc4',
    title: 'Ship shared detail',
    body: 'Shared **body**',
    status: 'open',
    owner: 'agent-a',
    priority: 1,
    scheduledOn: '2026-08-09',
    deadlineOn: '2026-08-10',
    checklist: [{ id: 'item-1', text: 'Publish package', done: true }],
    labels: ['integration'],
    updatedAt: '2026-08-08T20:00:00Z',
  },
  comments: [
    {
      id: '1',
      author: 'alice',
      body: 'Ready to ship',
      createdAt: '2026-08-08T20:01:00Z',
    },
  ],
  links: [
    {
      id: '1',
      relation: 'blocks',
      peerUID: '01PEER',
      peerReference: 'roadmap#def5',
      peerStatus: 'closed',
    },
  ],
  parent: {
    uid: '01PARENT',
    reference: 'roadmap#par1',
    title: 'Parent',
    status: 'open',
  },
  children: [
    {
      uid: '01CHILD',
      reference: 'roadmap#chi1',
      title: 'Child',
      status: 'open',
    },
  ],
  claim: { holder: 'agent-a', kind: 'work', purpose: 'implement' },
  pendingClaims: [{ holder: 'agent-b', kind: 'work', purpose: '' }],
}

afterEach(cleanup)

describe('IssueDetail', () => {
  it('renders the complete read-only issue presentation', async () => {
    render(IssueDetail, { props: { detail } })

    const region = screen.getByRole('region', { name: 'Kata issue detail' })
    expect(within(region).getByRole('heading', { name: 'Ship shared detail' })).toBeTruthy()
    expect(within(region).getByText('roadmap#abc4')).toBeTruthy()
    const description = within(region).getByRole('region', {
      name: 'Description',
    })
    await vi.waitFor(() => expect(description.textContent).toContain('Shared body'))
    expect(within(region).getByText('P1')).toBeTruthy()
    expect(within(region).getByText('Publish package')).toBeTruthy()
    expect(within(region).getByText('integration')).toBeTruthy()
    expect(within(region).getByText('roadmap#def5')).toBeTruthy()
    expect(within(region).getByText('Ready to ship')).toBeTruthy()
    expect(within(region).getByText('Claimed by agent-a')).toBeTruthy()
    expect(within(region).getByText('Pending: agent-b')).toBeTruthy()
    expect(within(region).queryByRole('button', { name: /edit/i })).toBeNull()
    expect(within(region).queryByText(/recurrence/i)).toBeNull()
    expect(within(region).queryByText(/history/i)).toBeNull()
  })

  it('shows the teammate beside the accountable author', () => {
    render(IssueDetail, {
      props: {
        detail: {
          ...detail,
          comments: [
            {
              id: '7',
              author: 'coordinator',
              teammate: 'reviewer-7',
              body: 'Check retries',
              createdAt: '2026-09-13T12:00:00Z',
            },
          ],
        },
      },
    })

    expect(screen.getByText('coordinator / reviewer-7')).toBeTruthy()
  })

  it('invokes only host-supplied actions', async () => {
    const invoke = vi.fn()
    render(IssueDetail, {
      props: {
        detail,
        actions: [{ id: 'open', label: 'Open in Kata', invoke }],
      },
    })

    const action = screen.getByRole('button', { name: 'Open in Kata' })
    expect(action.classList.contains('kit-button')).toBe(true)

    await fireEvent.click(action)
    expect(invoke).toHaveBeenCalledOnce()
  })

  it('aligns status and label chips in the header before the host actions', () => {
    const { container } = render(IssueDetail, {
      props: {
        detail,
        actions: [{ id: 'edit', label: 'Edit issue', invoke: vi.fn() }],
      },
    })

    const header = container.querySelector('header')!
    const status = within(header).getByText('open')
    const label = within(header).getByText('integration')
    const edit = within(header).getByRole('button', { name: 'Edit issue' })
    expect(status.compareDocumentPosition(label)).toBe(Node.DOCUMENT_POSITION_FOLLOWING)
    expect(label.compareDocumentPosition(edit)).toBe(Node.DOCUMENT_POSITION_FOLLOWING)
  })

  it('opens parent, child, and linked issues through the host callback', async () => {
    const onOpenIssue = vi.fn()
    render(IssueDetail, { props: { detail, onOpenIssue } })

    const links = screen.getByRole('region', { name: 'Links' })
    await fireEvent.click(within(links).getByRole('button', { name: 'Open parent roadmap#par1' }))
    await fireEvent.click(within(links).getByRole('button', { name: 'Open child roadmap#chi1' }))
    await fireEvent.click(within(links).getByRole('button', { name: 'Open blocks roadmap#def5' }))

    expect(onOpenIssue.mock.calls).toEqual([['01PARENT'], ['01CHILD'], ['01PEER']])
  })

  it('keeps link references as text when the host has no navigation', () => {
    render(IssueDetail, { props: { detail } })

    const links = screen.getByRole('region', { name: 'Links' })
    expect(within(links).getByText('roadmap#par1')).toBeTruthy()
    expect(within(links).queryByRole('button')).toBeNull()
  })
})

it('opens a comment endpoint and starts a typed reply from its canonical UID', async () => {
  const onOpenComment = vi.fn()
  const onReplyComment = vi.fn()
  const linked = {
    ...detail,
    comments: [
      {
        ...detail.comments[0]!,
        uid: 'reply-comment',
        handle: 'c:def456',
        editedAt: '2030-01-02T00:00:00Z',
        reply: {
          uid: 'target-comment',
          issue_uid: 'other-issue',
          handle: 'abcd:abc123',
          kind: 'reply',
          author: 'finder',
          target_edited: true,
        },
        backlinks: [
          {
            uid: 'backlink-comment',
            issue_uid: 'third-issue',
            handle: 'efgh:ghi789',
            kind: 'confirm',
          },
        ],
      },
    ],
  }
  render(IssueDetail, {
    props: { detail: linked, onOpenComment, onReplyComment },
  })
  expect(screen.getByText('c:def456')).toBeDefined()
  expect(screen.getByText('(edited)')).toBeDefined()
  expect(screen.getByText('Target edited after this reply')).toBeDefined()
  await fireEvent.click(screen.getByRole('button', { name: /Replies to abcd:abc123/ }))
  expect(onOpenComment).toHaveBeenCalledWith('other-issue', 'target-comment', {
    issueUID: '01TASK',
    commentUID: 'reply-comment',
  })
  await fireEvent.click(screen.getByRole('button', { name: 'Show 1 confirmation reply' }))
  await fireEvent.click(screen.getByRole('button', { name: 'Open efgh:ghi789' }))
  expect(onOpenComment).toHaveBeenCalledWith('third-issue', 'backlink-comment', {
    issueUID: '01TASK',
    commentUID: 'reply-comment',
  })
  await fireEvent.click(screen.getByRole('button', { name: 'Refute' }))
  expect(onReplyComment).toHaveBeenCalledWith('reply-comment', 'refute')
})

it('focuses a linked comment when its destination snapshot arrives', async () => {
  const initial = {
    ...detail,
    comments: [{ ...detail.comments[0]!, uid: 'source-comment' }],
  }
  const { container, rerender } = render(IssueDetail, {
    props: { detail: initial, selectedCommentUID: 'destination-comment' },
  })
  await tick()
  await tick()
  await rerender({
    detail: {
      ...initial,
      comments: [{ ...initial.comments[0]!, uid: 'destination-comment' }],
    },
    selectedCommentUID: 'destination-comment',
  })
  await vi.waitFor(() =>
    expect(document.activeElement).toBe(container.querySelector('#comment-destination-comment')),
  )
})

it('keeps user focus during a refresh after linked-comment navigation', async () => {
  const linked = {
    ...detail,
    comments: [{ ...detail.comments[0]!, uid: 'destination-comment' }],
  }
  const actions = [{ id: 'edit', label: 'Edit issue', invoke: vi.fn() }]
  const { container, rerender } = render(IssueDetail, {
    props: {
      detail: linked,
      selectedCommentUID: 'destination-comment',
      actions,
    },
  })
  await vi.waitFor(() =>
    expect(document.activeElement).toBe(container.querySelector('#comment-destination-comment')),
  )
  const edit = screen.getByRole('button', { name: 'Edit issue' })
  edit.focus()
  await rerender({
    detail: {
      ...linked,
      comments: [{ ...linked.comments[0]!, body: 'Updated evidence' }],
    },
    selectedCommentUID: 'destination-comment',
    actions,
  })
  await tick()
  expect(document.activeElement).toBe(edit)
})

it('shows conflicting direct relation counts and chronological evidence without hiding originals', async () => {
  const links = [
    {
      uid: 'confirm-late',
      issue_uid: 'other',
      kind: 'confirm',
      handle: 'bbbb:222222',
      author: 'worker-b',
      teammate: 'reviewer',
      body: 'Second reproduction',
      created_at: '2026-08-08T20:03:00Z',
      target_edited: true,
    },
    {
      uid: 'confirm-early',
      issue_uid: 'other',
      kind: 'confirm',
      handle: 'bbbb:111111',
      author: 'worker-a',
      body: 'First reproduction',
      created_at: '2026-08-08T20:02:00Z',
    },
    {
      uid: 'refute',
      issue_uid: 'other',
      kind: 'refute',
      handle: 'bbbb:333333',
      author: 'worker-c',
      body: 'Counterexample',
      created_at: '2026-08-08T20:04:00Z',
    },
    {
      uid: 'supersede-a',
      issue_uid: 'other',
      kind: 'supersede',
      handle: 'bbbb:444444',
      author: 'worker-d',
      body: 'Replacement one',
      created_at: '2026-08-08T20:05:00Z',
    },
    {
      uid: 'supersede-b',
      issue_uid: 'other',
      kind: 'supersede',
      handle: 'bbbb:555555',
      author: 'worker-e',
      body: 'Replacement two',
      created_at: '2026-08-08T20:06:00Z',
    },
  ]
  const open = vi.fn()
  const model = {
    ...detail,
    comments: [{ ...detail.comments[0]!, uid: 'original', handle: 'c:aaaaaa', backlinks: links }],
  }
  render(IssueDetail, { props: { detail: model, onOpenComment: open } })
  await vi.waitFor(() => expect(screen.getByText('Ready to ship')).toBeTruthy(), { timeout: 10000 })
  expect(screen.getByRole('button', { name: 'Show 1 refutation reply' }).textContent).toContain(
    'Refutations 1',
  )
  expect(screen.getByRole('button', { name: 'Show 2 superseding replies' }).textContent).toContain(
    'Superseding replies 2',
  )
  const confirmations = screen.getByRole('button', { name: 'Show 2 confirmation replies' })
  confirmations.focus()
  expect(document.activeElement).toBe(confirmations)
  await fireEvent.click(confirmations)
  const evidence = screen.getByRole('region', { name: 'Confirmation replies' })
  await vi.waitFor(() => expect(within(evidence).getByText('Second reproduction')).toBeTruthy(), {
    timeout: 10000,
  })
  expect(
    within(evidence)
      .getAllByRole('listitem')
      .map((el) => el.textContent),
  ).toEqual([
    expect.stringContaining('First reproduction'),
    expect.stringContaining('Second reproduction'),
  ])
  expect(within(evidence).getByText('worker-b / reviewer')).toBeTruthy()
  expect(within(evidence).getByText('Target edited after this reply')).toBeTruthy()
  await fireEvent.click(within(evidence).getByRole('button', { name: /Open bbbb:111111/ }))
  expect(open).toHaveBeenCalledWith('other', 'confirm-early', {
    issueUID: '01TASK',
    commentUID: 'original',
  })
})

it('refreshes incoming counts from accepted data and labels partial evidence honestly', async () => {
  const model = {
    ...detail,
    comments: [
      {
        ...detail.comments[0]!,
        uid: 'original',
        backlinksPartial: true,
        backlinks: [
          {
            uid: 'reply',
            kind: 'reply',
            author: 'worker',
            body: 'Response',
            created_at: '2026-08-08T20:02:00Z',
          },
        ],
      },
    ],
  }
  const { rerender } = render(IssueDetail, { props: { detail: model } })
  expect(screen.getByRole('button', { name: 'Show at least 1 reply' }).textContent).toContain(
    'Replies 1+',
  )
  expect(screen.getByText('More replies may be available')).toBeTruthy()
  await rerender({
    detail: { ...detail, comments: [{ ...detail.comments[0]!, uid: 'original', backlinks: [] }] },
  })
  expect(screen.queryByRole('button', { name: /Show.*repl/ })).toBeNull()
  await vi.waitFor(() => expect(screen.getByText('Ready to ship')).toBeTruthy(), { timeout: 10000 })
})

it('keeps unresolved outgoing states and exposes an explicit return route', async () => {
  const onOpenComment = vi.fn()
  const { rerender } = render(IssueDetail, {
    props: {
      detail: {
        ...detail,
        comments: [
          {
            ...detail.comments[0]!,
            uid: 'response',
            reply: { kind: 'confirm', status: 'pending' },
          },
        ],
      },
      onOpenComment,
      returnComment: { issueUID: 'source-issue', commentUID: 'source-comment' },
    },
  })
  expect(
    screen.getByRole('button', { name: /Confirms \(pending\)/ }).hasAttribute('disabled'),
  ).toBe(true)
  await fireEvent.click(screen.getByRole('button', { name: 'Return to original comment' }))
  expect(onOpenComment).toHaveBeenCalledWith('source-issue', 'source-comment')
  for (const status of ['removed', 'moved']) {
    await rerender({
      detail: {
        ...detail,
        comments: [
          { ...detail.comments[0]!, uid: 'response', reply: { kind: 'supersede', status } },
        ],
      },
      onOpenComment,
    })
    expect(
      screen
        .getByRole('button', { name: new RegExp(`Supersedes \\(${status}\\)`) })
        .hasAttribute('disabled'),
    ).toBe(true)
  }
})
