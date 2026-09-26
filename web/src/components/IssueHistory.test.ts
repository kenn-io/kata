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
