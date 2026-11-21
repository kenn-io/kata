import { cleanup, fireEvent, render, screen } from '@testing-library/svelte'
import { afterEach, describe, expect, test, vi } from 'vitest'

import KataDaemonSwitcher from './KataDaemonSwitcher.svelte'

describe('KataDaemonSwitcher', () => {
  afterEach(cleanup)

  test('routes daemon health, selection, and account information', async () => {
    const onSelect = vi.fn()
    const onOpen = vi.fn()
    const props = {
      daemons: [
        {
          id: 'example-local',
          url: '',
          default: true,
          auth: 'none' as const,
          health: 'connected' as const,
        },
        {
          id: 'example-remote',
          url: 'https://daemon.example',
          default: false,
          auth: 'token' as const,
          health: 'auth_required' as const,
        },
        {
          id: 'example-legacy',
          url: 'https://daemon.example',
          default: false,
          auth: 'none' as const,
          health: 'upgrade_required' as const,
        },
      ],
      activeId: 'example-local',
      onSelect,
      onOpen,
    }
    const { rerender } = render(KataDaemonSwitcher, { props })

    await fireEvent.click(screen.getByRole('button', { name: 'Switch Kata daemon: example-local' }))
    expect(screen.getByRole('menu', { name: 'Configured Kata daemons' })).not.toBeNull()
    expect(onOpen).toHaveBeenCalledOnce()
    expect(onSelect).not.toHaveBeenCalled()
    expect(screen.queryByText('Account')).toBeNull()
    expect(screen.getByText('needs auth')).not.toBeNull()
    expect(screen.getByText('update required')).not.toBeNull()
    expect(
      screen.getByRole('menuitemradio', { name: /example-legacy/ }).getAttribute('aria-disabled'),
    ).toBe('true')
    await fireEvent.click(screen.getByRole('menuitemradio', { name: /example-remote/ }))
    expect(onSelect).toHaveBeenCalledWith('example-remote')

    await rerender({ ...props, account: 'Example account', credential: 'Read-only' })
    await fireEvent.click(screen.getByRole('button', { name: 'Switch Kata daemon: example-local' }))
    expect(screen.getByText('Example account')).not.toBeNull()
    expect(screen.getByText('Read-only')).not.toBeNull()
  })

  test('shows reconnecting status without replacing the active daemon label', () => {
    render(KataDaemonSwitcher, {
      props: {
        daemons: [
          {
            id: 'example-local',
            url: '',
            default: true,
            auth: 'none',
            health: 'connected',
          },
        ],
        activeId: 'example-local',
        activeStatusLabel: 'Reconnecting…',
        onSelect: vi.fn(),
      },
    })

    expect(screen.getByRole('status', { name: 'Kata daemon status' }).textContent).toContain(
      'Reconnecting…',
    )
    expect(
      screen.getByRole('status', { name: 'Kata daemon status' }).closest('button'),
    ).not.toBeNull()
    expect(screen.getByText('example-local')).not.toBeNull()
  })
})
