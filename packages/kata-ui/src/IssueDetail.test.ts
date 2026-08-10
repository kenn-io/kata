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

  it('invokes only host-supplied actions', async () => {
    const invoke = vi.fn()
    render(IssueDetail, {
      props: {
        detail,
        actions: [{ id: 'open', label: 'Open in Kata', invoke }],
      },
    })

    await fireEvent.click(screen.getByRole('button', { name: 'Open in Kata' }))
    expect(invoke).toHaveBeenCalledOnce()
  })
})
