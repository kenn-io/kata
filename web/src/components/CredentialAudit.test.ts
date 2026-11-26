import { cleanup, fireEvent, render, screen, within } from '@testing-library/svelte'
import { afterEach, describe, expect, it, vi } from 'vitest'

import type { TokenOut } from '../lib/api/generated'
import CredentialAudit from './CredentialAudit.svelte'

const tokens: TokenOut[] = [
  {
    id: 7,
    name: 'Build worker',
    actor: 'agent-a',
    state: 'live',
    created_at: '2026-09-14T10:00:00Z',
    expires_at: '2026-09-16T10:00:00Z',
    last_used_at: '2026-09-15T09:30:00Z',
    revoked_at: null,
    scope: {
      kind: 'issue_subtree',
      project_uid: '01J00000000000000000000002',
      root_issue_uid: '01J00000000000000000000001',
    },
  },
  {
    id: 8,
    name: null,
    actor: 'admin-user',
    state: 'expired',
    created_at: '2026-09-01T10:00:00Z',
    expires_at: '2026-09-10T10:00:00Z',
    last_used_at: null,
    revoked_at: null,
  },
  {
    id: 9,
    name: '<img src=x onerror=alert(1)>',
    actor: '<script>alert(1)</script>',
    state: 'revoked',
    created_at: '2026-08-01T10:00:00Z',
    last_used_at: '2026-08-02T10:00:00Z',
    revoked_at: '2026-08-03T10:00:00Z',
  },
]

describe('CredentialAudit', () => {
  afterEach(cleanup)

  it('reads each credential as one compact row', () => {
    render(CredentialAudit, { props: auditProps() })

    expect(screen.getByRole('heading', { name: 'Credentials' })).not.toBeNull()
    expect(screen.queryByRole('table')).toBeNull()
    const rows = within(screen.getByRole('list', { name: 'Credentials' })).getAllByRole('listitem')
    expect(rows).toHaveLength(3)

    const scoped = rows[0]!.textContent ?? ''
    expect(scoped).toContain('Build worker')
    expect(scoped).toContain('agent-a')
    expect(scoped).toContain('Created')
    expect(scoped).toContain('Last used')
    expect(scoped).toContain('Expires')
    expect(scoped).toContain('Scoped to issue subtree')
    expect(scoped).toContain('01J00000000000000000000002')
    expect(scoped).toContain('01J00000000000000000000001')
    expect(scoped).not.toContain('Revoked')

    expect(rows[1]!.textContent).toContain('Expired')
    expect(rows[1]!.textContent).not.toContain('Last used')
    expect(rows[1]!.textContent).toContain('Never used')
    expect(rows[2]!.textContent).toContain('Revoked')
    expect(screen.getByText('<script>alert(1)</script>')).not.toBeNull()
    expect(scoped).toContain('ID 7')
    expect(rows[1]!.textContent).toContain('ID 8')
    expect(screen.queryByRole('button', { name: /revoke/i })).toBeNull()
  })

  it('shows both expiry and revocation dates for a revoked credential', () => {
    const revoked = { ...tokens[2]!, expires_at: '2026-09-10T10:00:00Z' }
    render(CredentialAudit, { props: auditProps({ tokens: [revoked] }) })

    const row = screen.getByRole('listitem')
    expect(row.querySelector('time[datetime="2026-09-10T10:00:00Z"]')).not.toBeNull()
    expect(row.querySelector('time[datetime="2026-08-03T10:00:00Z"]')).not.toBeNull()
  })

  it('shows the local expiry date matching the tooltip', () => {
    const originalTimezone = process.env.TZ
    process.env.TZ = 'America/New_York'
    try {
      const expires = '2026-12-01T00:30:00Z'
      render(CredentialAudit, {
        props: auditProps({ tokens: [{ ...tokens[0]!, expires_at: expires }] }),
      })
      const time = document.querySelector(`time[datetime="${expires}"]`)!
      expect(time.textContent).toMatch(/^Nov 30(?:, 2026)?$/)
      expect(time.getAttribute('title')).toContain('Nov 30, 2026')
    } finally {
      if (originalTimezone === undefined) delete process.env.TZ
      else process.env.TZ = originalTimezone
    }
  })

  it('filters a short credential list and says when none match', async () => {
    render(CredentialAudit, { props: auditProps() })

    expect(screen.getByRole('group', { name: 'Credential filters' })).not.toBeNull()
    await fireEvent.click(screen.getByRole('combobox', { name: 'Credential state: Any state' }))
    await fireEvent.click(screen.getByRole('option', { name: 'Live' }))
    expect(screen.getByText('Build worker')).not.toBeNull()
    expect(screen.queryByText('admin-user')).toBeNull()

    await fireEvent.click(screen.getByRole('combobox', { name: 'Credential scope: Any access' }))
    await fireEvent.click(screen.getByRole('option', { name: 'Full access' }))
    expect(screen.getByText('No matching credentials')).not.toBeNull()

    await fireEvent.click(screen.getByRole('combobox', { name: 'Credential state: Live' }))
    await fireEvent.click(screen.getByRole('option', { name: 'Revoked' }))
    await fireEvent.input(screen.getByRole('searchbox', { name: 'Search credentials' }), {
      target: { value: 'SCRIPT>ALERT' },
    })
    expect(screen.getByText('<script>alert(1)</script>')).not.toBeNull()
    expect(screen.queryByText('Build worker')).toBeNull()
  })

  it('refreshes, and shows loading, empty, and error states', async () => {
    const onRefresh = vi.fn()
    const view = render(CredentialAudit, { props: auditProps({ onRefresh }) })

    await fireEvent.click(screen.getByRole('button', { name: 'Refresh credentials' }))
    expect(onRefresh).toHaveBeenCalledOnce()

    await view.rerender({ loading: true })
    expect(
      (screen.getByRole('button', { name: 'Refresh credentials' }) as HTMLButtonElement).disabled,
    ).toBe(true)
    expect(screen.getByRole('status').textContent).toContain('Refreshing credentials')

    await view.rerender({ loading: true, tokens: [] })
    expect(screen.getByRole('status').textContent).toContain('Loading credentials')

    await view.rerender({ loading: false, tokens: [] })
    expect(screen.getByRole('status').textContent).toContain('This server has no credentials.')

    await view.rerender({ error: "Couldn't load credentials." })
    const alert = screen.getByRole('alert')
    expect(alert.dataset.tone).toBe('error')
    expect(within(alert).getByText("Couldn't load credentials.")).not.toBeNull()
  })
})

function auditProps(overrides: Record<string, unknown> = {}) {
  return {
    tokens,
    loading: false,
    onRefresh: vi.fn(),
    ...overrides,
  }
}
