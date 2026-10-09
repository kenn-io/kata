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

describe('IssueHistory', () => {
  afterEach(cleanup)

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
