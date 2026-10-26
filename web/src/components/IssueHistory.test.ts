import { cleanup, render, screen } from '@testing-library/svelte'
import { afterEach, describe, expect, it } from 'vitest'

import type { KataTaskEvent } from '../lib/kata/types'
import IssueHistory from './IssueHistory.svelte'

function event(uid: string, created_at: string): KataTaskEvent {
  return {
    event_id: 1,
    event_uid: uid,
    origin_instance_uid: 'instance-a',
    type: 'issue.commented',
    project_id: 1,
    project_uid: 'project-a',
    project_name: 'example-project',
    actor: 'user-a',
    created_at,
  }
}

afterEach(cleanup)

describe('IssueHistory', () => {
  it('shows the closing session and its optional AgentsView link', () => {
    const closed = {
      ...event('close-a', '2026-08-01T12:00:00Z'),
      type: 'issue.closed',
      payload: {
        transcript: {
          agent: 'codex',
          session_id: '00000000-0000-4000-8000-000000000001',
          url: 'https://agentsview.example/sessions/codex/00000000-0000-4000-8000-000000000001',
        },
      },
    }
    render(IssueHistory, { props: { events: [closed] } })
    const link = screen.getByRole('link', { name: /codex.*00000000-0000-4000-8000-000000000001/ })
    expect(link.getAttribute('href')).toBe(closed.payload.transcript.url)
    expect(link.getAttribute('rel')).toContain('noreferrer')
  })

  it.each([
    undefined,
    'javascript:alert(1)',
    'https://user:secret@agentsview.example',
    'https://agentsview.example?token=secret',
    'https://agentsview.example/#secret',
  ])('keeps provenance readable without an unsafe link: %s', (url) => {
    render(IssueHistory, {
      props: {
        events: [
          {
            ...event('close-b', '2026-08-01T12:00:00Z'),
            type: 'issue.closed',
            payload: {
              transcript: {
                agent: 'claude',
                session_id: '00000000-0000-4000-8000-000000000002',
                url,
              },
            },
          },
        ],
      },
    })
    expect(screen.getByText(/claude.*00000000-0000-4000-8000-000000000002/)).toBeTruthy()
    expect(screen.queryByRole('link')).toBeNull()
  })

  it.each([
    null,
    [],
    { agent: 'unknown', session_id: '00000000-0000-4000-8000-000000000001' },
    { agent: 'codex', session_id: '/private/chat.jsonl' },
  ])('omits malformed session coordinates: %s', (transcript) => {
    render(IssueHistory, {
      props: {
        events: [
          {
            ...event('close-c', '2026-08-01T12:00:00Z'),
            type: 'issue.closed',
            payload: { transcript },
          },
        ],
      },
    })
    expect(screen.queryByText(/Session:/)).toBeNull()
    expect(screen.queryByRole('link')).toBeNull()
  })

  it('shows when each event happened beside its description', () => {
    const { container } = render(IssueHistory, {
      props: { events: [event('event-a', '2026-08-01T12:00:00Z')] },
    })

    const row = screen.getByRole('listitem')
    const time = row.querySelector('time[datetime="2026-08-01T12:00:00Z"]')
    expect(time).not.toBeNull()
    expect(time?.textContent?.trim()).not.toBe('')
    expect(time?.getAttribute('title')).toMatch(/2026/)
    expect(container.querySelectorAll('time')).toHaveLength(1)
  })

  it('omits the time for an event without a readable timestamp', () => {
    const { container } = render(IssueHistory, {
      props: { events: [event('event-b', 'not-a-date')] },
    })

    expect(screen.getByRole('listitem')).not.toBeNull()
    expect(container.querySelector('time')).toBeNull()
  })
})

const attachmentID = '01K7E222222222222222222222'
const frozenAttachment = {
  name: 'report.pdf',
  media_type: 'application/pdf',
  size: 12,
  docbank_version: '6f1c2e8a-2222-4222-8222-222222222222',
}
function attachmentEvent(snapshot: unknown = frozenAttachment): KataTaskEvent {
  return {
    ...event('close-document', '2026-08-01T12:00:00Z'),
    type: 'issue.closed',
    payload: {
      evidence: [
        {
          type: 'attachment',
          attachment_id: attachmentID,
          ...(snapshot === undefined ? {} : { attachment: snapshot }),
        },
      ],
    },
  }
}
it('links frozen attachment evidence to the matching panel entry', () => {
  render(IssueHistory, {
    props: {
      events: [attachmentEvent()],
      metadata: { [`attachment.${attachmentID}`]: frozenAttachment },
    },
  })
  expect(screen.getByRole('link', { name: /report.pdf/ }).getAttribute('href')).toBe(
    `#kata-attachment-${attachmentID}`,
  )
  expect(screen.getByText(/application\/pdf.*12 bytes/)).toBeTruthy()
})
it.each(['removed', 'changed'])(
  'retains the frozen descriptor when the reference is %s',
  (state) => {
    render(IssueHistory, {
      props: {
        events: [attachmentEvent()],
        metadata:
          state === 'removed'
            ? {}
            : {
                [`attachment.${attachmentID}`]: {
                  ...frozenAttachment,
                  name: 'new.pdf',
                  docbank_version: '6f1c2e8a-3333-4333-8333-333333333333',
                },
              },
      },
    })
    expect(screen.getByText(new RegExp(`report.pdf.*${state}`))).toBeTruthy()
    expect(screen.queryByRole('link')).toBeNull()
  },
)
it('renders legacy id-only evidence from current metadata', () => {
  const closed = attachmentEvent()
  closed.payload = { evidence: [{ type: 'attachment', attachment_id: attachmentID }] }
  render(IssueHistory, {
    props: { events: [closed], metadata: { [`attachment.${attachmentID}`]: frozenAttachment } },
  })
  expect(screen.getByRole('link', { name: /report.pdf/ })).toBeTruthy()
})
it('does not retarget malformed historical descriptors', () => {
  render(IssueHistory, {
    props: {
      events: [attachmentEvent({ name: 'bad', docbank_version: frozenAttachment.docbank_version })],
      metadata: { [`attachment.${attachmentID}`]: frozenAttachment },
    },
  })
  expect(screen.queryByRole('link')).toBeNull()
  expect(screen.getByText(/invalid snapshot/)).toBeTruthy()
})
