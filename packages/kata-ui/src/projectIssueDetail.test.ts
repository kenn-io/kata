import { describe, expect, it } from 'vitest'

import type { ShowIssueResponseBody } from '../../../web/src/lib/api/generated/models/showIssueResponseBody.ts'
import { projectIssueDetail } from './projectIssueDetail.js'
import type { KataIssueDetailWire } from './types.js'

function acceptCanonicalWire(
  wire: ShowIssueResponseBody,
): KataIssueDetailWire {
  return wire
}

describe('projectIssueDetail', () => {
  it('accepts the generated canonical issue-detail response type', () => {
    expect(acceptCanonicalWire).toBeTypeOf('function')
  })

  it('accepts a canonical issue without optional project identity', () => {
    const wire: KataIssueDetailWire = {
      issue: {
        uid: '01TASK',
        title: 'Older daemon issue',
        status: 'open',
      },
    }

    expect(projectIssueDetail(wire).issue.projectUID).toBe('')
  })

  it('defaults optional fields and ignores additive response fields', () => {
    const wire = {
      issue: {
        uid: '01TASK',
        project_uid: '01PROJECT',
        project_name: 'Roadmap',
        short_id: 'abc4',
        qualified_id: 'roadmap#abc4',
        title: 'Ship shared detail',
        status: 'open',
      },
      future_field: { ignored: true },
    } as unknown as KataIssueDetailWire

    expect(projectIssueDetail(wire)).toMatchObject({
      issue: {
        uid: '01TASK',
        body: '',
        checklist: [],
        labels: [],
      },
      comments: [],
      links: [],
      children: [],
      pendingClaims: [],
    })
  })

  it('projects checklist, relationships, comments, and claim state', () => {
    const model = projectIssueDetail({
      issue: {
        uid: '01TASK',
        project_uid: '01PROJECT',
        project_name: 'Roadmap',
        short_id: 'abc4',
        qualified_id: 'roadmap#abc4',
        title: 'Ship shared detail',
        body: 'Shared **body**',
        status: 'open',
        owner: 'agent-a',
        priority: 1,
        metadata: {
          scheduled_on: '2026-08-09',
          deadline_on: '2026-08-10',
          checklist: [{ id: 'item-1', text: 'Publish package', done: true }],
        },
        labels: ['integration'],
        updated_at: '2026-08-08T20:00:00Z',
      },
      comments: [
        {
          id: 1,
          issue_id: 1,
          author: 'alice',
          body: 'Ready',
          created_at: '2026-08-08T20:01:00Z',
        },
      ],
      labels: [
        {
          issue_id: 1,
          label: 'integration',
          author: 'alice',
          created_at: '2026-08-08T20:00:00Z',
        },
      ],
      links: [
        {
          id: 1,
          project_id: 1,
          from: {
            uid: '01TASK',
            short_id: 'abc4',
            qualified_id: 'roadmap#abc4',
            status: 'open',
          },
          to: {
            uid: '01PEER',
            short_id: 'def5',
            qualified_id: 'roadmap#def5',
            status: 'closed',
          },
          type: 'blocks',
          author: 'alice',
          created_at: '2026-08-08T20:00:00Z',
        },
        {
          id: 2,
          from: {
            uid: '01TASK',
            short_id: 'abc4',
            qualified_id: 'roadmap#abc4',
            status: 'open',
          },
          to: {
            uid: '01PARENT',
            short_id: 'par1',
            qualified_id: 'roadmap#par1',
            status: 'open',
          },
          type: 'parent',
        },
        {
          id: 3,
          from: {
            uid: '01CHILD',
            short_id: 'chi1',
            qualified_id: 'roadmap#chi1',
            status: 'open',
          },
          to: {
            uid: '01TASK',
            short_id: 'abc4',
            qualified_id: 'roadmap#abc4',
            status: 'open',
          },
          type: 'parent',
        },
      ],
      parent: {
        uid: '01PARENT',
        short_id: 'par1',
        qualified_id: 'roadmap#par1',
        title: 'Parent',
        status: 'open',
      },
      children: [
        {
          uid: '01CHILD',
          project_uid: '01PROJECT',
          project_name: 'Roadmap',
          short_id: 'chi1',
          qualified_id: 'roadmap#chi1',
          title: 'Child',
          status: 'open',
        },
      ],
      claim: {
        claim_uid: '01CLAIM',
        holder: 'agent-a',
        claim_kind: 'work',
        purpose: 'implement',
        acquired_at: '2026-08-08T20:00:00Z',
        revision: 1,
        updated_at: '2026-08-08T20:00:00Z',
        project_id: 1,
        issue_uid: '01TASK',
        holder_instance_uid: '01INSTANCE',
        client_kind: 'cli',
      },
      pending_claims: [
        {
          request_uid: '01PENDING',
          holder: 'agent-b',
          holder_instance_uid: '01OTHER',
          client_kind: 'cli',
          claim_kind: 'work',
          requested_at: '2026-08-08T20:02:00Z',
        },
      ],
    } as unknown as KataIssueDetailWire)

    expect(model.issue.checklist).toEqual([{ id: 'item-1', text: 'Publish package', done: true }])
    expect(model.issue.labels).toEqual(['integration'])
    expect(model.links).toEqual([
      expect.objectContaining({ relation: 'blocks', peerUID: '01PEER' }),
    ])
    expect(model.parent?.title).toBe('Parent')
    expect(model.children[0]?.title).toBe('Child')
    expect(model.claim?.holder).toBe('agent-a')
    expect(model.pendingClaims[0]?.holder).toBe('agent-b')
  })
})
