import { cleanup, render, screen, within } from '@testing-library/svelte'
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

  it.each(['issue.created', 'issue.commented', 'issue.updated', 'issue.closed', 'issue.future'])(
    'shows the canonical actor and teammate for %s',
    (type) => {
      render(IssueHistory, {
        props: {
          events: [
            {
              ...event('tagged', '2026-08-01T12:00:00Z'),
              type,
              actor: 'alice',
              teammate: 'claude.opus-5-5',
              payload: {
                teammate: 'payload-worker',
                author: 'payload-author',
                metadata: { teammate: 'creation-worker' },
              },
            },
          ],
        },
      })
      const row = within(screen.getByRole('listitem'))
      expect(row.getByText('alice / claude.opus-5-5').textContent).toBe('alice / claude.opus-5-5')
      expect(row.queryByText(/payload-worker|payload-author|creation-worker/)).toBeNull()
    },
  )

  it.each([undefined, ''])('shows the actor alone for an untagged event: %s', (teammate) => {
    render(IssueHistory, {
      props: {
        events: [
          {
            ...event('legacy', '2026-08-01T12:00:00Z'),
            actor: 'alice',
            teammate,
            payload: { teammate: 'payload-worker' },
          },
        ],
      },
    })
    expect(within(screen.getByRole('listitem')).getByText('alice').textContent).toBe('alice')
    expect(screen.queryByText(/alice \/|payload-worker/)).toBeNull()
  })

  it('keeps maximum-length attribution readable as text', () => {
    const teammate = 'A'.repeat(64)
    render(IssueHistory, {
      props: { events: [{ ...event('long', ''), actor: 'alice', teammate }] },
    })
    expect(screen.getByText(`alice / ${teammate}`).textContent).toBe(`alice / ${teammate}`)
  })

  it.each([
    ['<img src=x onerror="alert(1)">', 'codex', '<img src=x onerror="alert(1)"> / codex'],
    ['alice', '<script>alert(1)</script>', 'alice / <script>alert(1)</script>'],
    ['<b>alice & bob</b>', '', '<b>alice & bob</b>'],
  ])('renders attribution as escaped text: %s / %s', (actor, teammate, label) => {
    render(IssueHistory, {
      props: { events: [{ ...event('escaped', ''), actor, teammate }] },
    })
    const row = screen.getByRole('listitem')
    expect(within(row).getByText(label).textContent).toBe(label)
    expect(row.querySelector('img, script, b')).toBeNull()
  })

  it('updates attribution on the same event row and preserves other rows', async () => {
    const first = { ...event('first', '2026-08-01T12:00:00Z'), actor: 'alice' }
    const second = {
      ...event('second', '2026-08-01T13:00:00Z'),
      actor: 'bob',
      teammate: 'codex',
    }
    const view = render(IssueHistory, { props: { events: [first, second] } })
    const [firstRow, secondRow] = screen.getAllByRole('listitem')
    expect(within(firstRow!).getByText('alice')).toBeTruthy()
    expect(within(secondRow!).getByText('bob / codex')).toBeTruthy()

    await view.rerender({ events: [{ ...first, teammate: 'claude.opus-5-5' }, second] })
    expect(within(firstRow!).getByText('alice / claude.opus-5-5')).toBeTruthy()
    expect(within(secondRow!).getByText('bob / codex')).toBeTruthy()
    expect(within(firstRow!).queryByText('alice')).toBeNull()

    await view.rerender({ events: [{ ...first, actor: 'carol', teammate: '' }, second] })
    expect(within(firstRow!).getByText('carol')).toBeTruthy()
    expect(within(firstRow!).queryByText('alice / claude.opus-5-5')).toBeNull()
    expect(within(secondRow!).getByText('bob / codex')).toBeTruthy()
  })

  it('shows the closing session and its optional AgentsView link', () => {
    const closed = {
      ...event('close-a', '2026-08-01T12:00:00Z'),
      type: 'issue.closed',
      actor: 'alice',
      teammate: 'codex.gpt-5.1',
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
    const row = link.closest('li')!
    expect(within(row).getByText('alice / codex.gpt-5.1')).toBeTruthy()
    expect(within(row).getByText('closed (done)')).toBeTruthy()
    expect(row.querySelector('time')?.getAttribute('datetime')).toBe(closed.created_at)
  })

  it.each(['pi', 'omp'])('shows the closing %s session', (agent) => {
    render(IssueHistory, {
      props: {
        events: [
          {
            ...event('close-native', '2026-08-01T12:00:00Z'),
            type: 'issue.closed',
            payload: { transcript: { agent, session_id: '00000000-0000-4000-8000-000000000001' } },
          },
        ],
      },
    })
    expect(screen.getByText(`Session: ${agent} 00000000-0000-4000-8000-000000000001`)).toBeTruthy()
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
