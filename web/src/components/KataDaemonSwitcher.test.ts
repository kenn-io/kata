import { cleanup, fireEvent, render, screen } from '@testing-library/svelte'
import { afterEach, describe, expect, test, vi } from 'vitest'

import KataDaemonSwitcher from './KataDaemonSwitcher.svelte'

describe('KataDaemonSwitcher', () => {
  afterEach(cleanup)

  test('ports configured daemon health and in-place selection', async () => {
    const onSelect = vi.fn()
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
          {
            id: 'example-remote',
            url: 'https://daemon.example',
            default: false,
            auth: 'token',
            health: 'auth_required',
          },
          {
            id: 'example-legacy',
            url: 'https://daemon.example',
            default: false,
            auth: 'none',
            health: 'upgrade_required',
          },
        ],
        activeId: 'example-local',
        onSelect,
      },
    })

    await fireEvent.click(screen.getByRole('button', { name: 'Switch Kata daemon: example-local' }))
    expect(screen.getByRole('menu', { name: 'Configured Kata daemons' })).not.toBeNull()
    expect(screen.getByText('needs auth')).not.toBeNull()
    expect(screen.getByText('update required')).not.toBeNull()
    await fireEvent.click(screen.getByRole('menuitemradio', { name: /example-remote/ }))
    expect(onSelect).toHaveBeenCalledWith('example-remote')
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
