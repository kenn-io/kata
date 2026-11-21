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
    expect(screen.queryByText('Account')).toBeNull()
    expect(screen.getByText('needs auth')).not.toBeNull()
    expect(screen.getByText('update required')).not.toBeNull()
    await fireEvent.click(screen.getByRole('menuitemradio', { name: /example-remote/ }))
    expect(onSelect).toHaveBeenCalledWith('example-remote')

    onSelect.mockClear()
    await fireEvent.click(screen.getByRole('button', { name: 'Switch Kata daemon: example-local' }))
    await fireEvent.click(screen.getByRole('menuitemradio', { name: /example-legacy/ }))
    expect(onSelect).not.toHaveBeenCalled()
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

  test('opens with the keyboard and returns focus without selecting on Escape', async () => {
    const onSelect = vi.fn()
    const onOpen = vi.fn()
    render(KataDaemonSwitcher, {
      props: {
        daemons: [
          { id: 'local', url: '', default: true, auth: 'none', health: 'connected' },
          { id: 'remote', url: '', default: false, auth: 'none', health: 'connected' },
        ],
        activeId: 'local',
        account: 'Example account',
        credential: 'Read-only',
        onSelect,
        onOpen,
      },
    })
    const trigger = screen.getByRole('button', { name: 'Switch Kata daemon: local' })
    trigger.focus()
    await fireEvent.keyDown(trigger, { key: 'ArrowDown' })
    const first = await screen.findByRole('menuitemradio', { name: /local/ })
    expect(screen.getByText('Example account')).not.toBeNull()
    expect(screen.getByText('Read-only')).not.toBeNull()
    await fireEvent.keyDown(first, { key: 'ArrowDown' })
    expect(document.activeElement).toBe(screen.getByRole('menuitemradio', { name: /remote/ }))
    await fireEvent.keyDown(document.activeElement!, { key: 'Escape' })
    expect(document.activeElement).toBe(trigger)
    expect(screen.queryByRole('menuitemradio')).toBeNull()
    expect(onOpen).toHaveBeenCalledOnce()
    expect(onSelect).not.toHaveBeenCalled()
  })
})
