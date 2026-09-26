// @vitest-environment jsdom

import { cleanup, fireEvent, render, screen, within } from '@testing-library/svelte'
import { afterEach, describe, expect, it, vi } from 'vitest'

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

describe('IssueDetail', () => {
  afterEach(cleanup)

  it('renders the complete read-only issue presentation', () => {
    render(IssueDetail, { props: { detail } })

    const region = screen.getByRole('region', { name: 'Kata issue detail' })
    expect(within(region).getByRole('heading', { name: 'Ship shared detail' })).toBeTruthy()
    expect(within(region).getByText('roadmap#abc4')).toBeTruthy()
    expect(within(region).getByRole('region', { name: 'Description' }).textContent).toContain(
      'Shared body',
    )
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
