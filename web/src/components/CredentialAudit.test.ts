import { cleanup, fireEvent, render, screen } from '@testing-library/svelte'
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

  it('shows redacted server-classified metadata without an observation line', () => {
    const { container } = render(CredentialAudit, {
      props: auditProps(),
    })

    expect(screen.getByRole('heading', { name: 'Credentials' })).not.toBeNull()
    expect(screen.queryByText('Observed by server')).toBeNull()
    expect(container.querySelector('select')).toBeNull()
    expect(screen.getByRole('columnheader', { name: 'Last observed use' })).not.toBeNull()
    expect(screen.getByText('Build worker')).not.toBeNull()
    expect(screen.getByText('issue subtree')).not.toBeNull()
    expect(screen.getByText('01J00000000000000000000001')).not.toBeNull()
    expect(screen.getAllByText('live').length).toBeGreaterThan(0)

    expect(screen.getByText('<script>alert(1)</script>')).not.toBeNull()
    expect(screen.queryByRole('button', { name: /revoke/i })).toBeNull()
    expect(screen.queryByRole('textbox', { name: /token/i })).toBeNull()
  })

  it('combines lifecycle, scope, and actor-or-name search filters', async () => {
    render(CredentialAudit, { props: auditProps() })

    expect(screen.getByRole('group', { name: 'Credential filters' })).not.toBeNull()

    await fireEvent.click(screen.getByRole('combobox', { name: 'Credential state: All' }))
    await fireEvent.click(screen.getByRole('option', { name: 'Live' }))
    expect(screen.getByText('Build worker')).not.toBeNull()
    expect(screen.queryByText('admin-user')).toBeNull()

    await fireEvent.click(screen.getByRole('combobox', { name: 'Credential scope: All' }))
    await fireEvent.click(screen.getByRole('option', { name: 'Unscoped' }))
    expect(screen.getByText('No credentials match these filters.')).not.toBeNull()

    await fireEvent.click(screen.getByRole('combobox', { name: 'Credential state: Live' }))
    await fireEvent.click(screen.getByRole('option', { name: 'Revoked' }))
    await fireEvent.input(screen.getByRole('searchbox', { name: 'Search credentials' }), {
      target: { value: 'SCRIPT>ALERT' },
    })
    expect(screen.getByText('<script>alert(1)</script>')).not.toBeNull()
    expect(screen.queryByText('Build worker')).toBeNull()
  })

  it('provides explicit refresh and return actions with loading and unavailable states', async () => {
    const onRefresh = vi.fn()
    const onBack = vi.fn()
    const view = render(CredentialAudit, { props: auditProps({ onRefresh, onBack }) })

    await fireEvent.click(screen.getByRole('button', { name: 'Refresh credentials' }))
    await fireEvent.click(screen.getByRole('button', { name: 'Back to issues' }))
    expect(onRefresh).toHaveBeenCalledOnce()
    expect(onBack).toHaveBeenCalledOnce()

    await view.rerender({ loading: true })
    expect(
      (screen.getByRole('button', { name: 'Refresh credentials' }) as HTMLButtonElement).disabled,
    ).toBe(true)
    expect(screen.getByRole('status').textContent).toContain('Refreshing credentials')

    await view.rerender({
      loading: false,
      error: 'Credential inventory is unavailable.',
      tokens: [],
    })
    expect(screen.getByRole('alert').textContent).toContain('Credential inventory is unavailable.')
  })
})

function auditProps(overrides: Record<string, unknown> = {}) {
  return {
    tokens,
    loading: false,
    onRefresh: vi.fn(),
    onBack: vi.fn(),
    ...overrides,
  }
}
