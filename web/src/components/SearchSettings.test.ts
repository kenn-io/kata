import { cleanup, fireEvent, render, screen } from '@testing-library/svelte'
import { afterEach, describe, expect, test, vi } from 'vitest'

import { setGeneratedFetch } from '../lib/api/client'
import type { LocalEmbeddingsStatus } from '../lib/api/generated'
import SearchSettings from './SearchSettings.svelte'

function status(patch: Partial<LocalEmbeddingsStatus> = {}): LocalEmbeddingsStatus {
  return {
    provider: 'none',
    download_bytes: 290_000_000,
    ollama_available: false,
    can_change: true,
    can_restart: false,
    ...patch,
  }
}

function renderSettings(
  props: {
    canAdministerDaemon?: boolean
    postFetch?: typeof fetch
    originStable?: boolean
    remoteHost?: string
  } = {},
) {
  return render(SearchSettings, {
    props: { canAdministerDaemon: true, onback: vi.fn(), postFetch: fetch, ...props },
  })
}

async function settle(): Promise<void> {
  await vi.advanceTimersByTimeAsync(0)
}

describe('SearchSettings', () => {
  afterEach(() => {
    cleanup()
    vi.restoreAllMocks()
    vi.unstubAllGlobals()
    vi.useRealTimers()
  })

  test.each([
    [true, undefined],
    [false, undefined],
    [true, 'daemon.example'],
    [false, 'daemon.example'],
  ])(
    'shows an ongoing restart for admin=%s and remote=%s',
    async (canAdministerDaemon, remoteHost) => {
      vi.useFakeTimers()
      setGeneratedFetch(
        vi.fn(async () =>
          Response.json(status({ job: { state: 'restarting', downloaded: 1, total: 1 } })),
        ),
      )
      const { container } = renderSettings({ canAdministerDaemon, remoteHost })
      await settle()
      expect(screen.getByText('Restarting...')).not.toBeNull()
      expect(container.textContent).not.toContain('Restart the daemon')
    },
  )

  test('asks before sending a single restart request', async () => {
    vi.useFakeTimers()
    setGeneratedFetch(
      vi.fn(async () =>
        Response.json(
          status({ can_restart: true, job: { state: 'restart', downloaded: 1, total: 1 } }),
        ),
      ),
    )
    const postFetch = vi.fn<typeof fetch>(async () => new Response(null, { status: 202 }))
    const confirmRestart = vi.fn().mockReturnValueOnce(false).mockReturnValue(true)
    vi.stubGlobal('confirm', confirmRestart)
    renderSettings({ postFetch })
    await settle()
    const button = screen.getByRole('button', { name: 'Restart daemon' })
    await fireEvent.click(button)
    expect(postFetch).not.toHaveBeenCalled()
    expect(confirm).toHaveBeenCalledWith(
      expect.stringContaining('If a service manager runs this daemon, restart it there instead.'),
    )
    await fireEvent.click(button)
    await fireEvent.click(button)
    expect(postFetch).toHaveBeenCalledOnce()
    expect(postFetch.mock.calls[0]?.[0]).toBe('/api/v1/embeddings/local/restart')
    expect(postFetch.mock.calls[0]?.[1]?.method).toBe('POST')
    expect(confirm).toHaveBeenCalledWith(expect.stringContaining('run kata ui'))
  })

  test('allows retry when a status read reports failure while the restart response hangs', async () => {
    vi.useFakeTimers()
    const pending = status({
      can_restart: true,
      job: { state: 'restart', downloaded: 1, total: 1 },
    })
    const failed = status({
      can_restart: true,
      job: { state: 'restart', downloaded: 1, total: 1, error: 'Restart failed' },
    })
    setGeneratedFetch(
      vi
        .fn<typeof fetch>()
        .mockResolvedValueOnce(Response.json(pending))
        .mockResolvedValueOnce(Response.json(failed))
        .mockRejectedValue(new TypeError('disconnected')),
    )
    vi.stubGlobal(
      'confirm',
      vi.fn(() => true),
    )
    const postFetch = vi
      .fn<typeof fetch>()
      .mockImplementationOnce(
        (_input, init) =>
          new Promise((_resolve, reject) => {
            init?.signal?.addEventListener('abort', () => reject(new TypeError('disconnected')))
          }),
      )
      .mockResolvedValue(new Response(null, { status: 202 }))
    renderSettings({ postFetch, originStable: true })
    await settle()
    await fireEvent.click(screen.getByRole('button', { name: 'Restart daemon' }))
    await vi.advanceTimersByTimeAsync(2000)
    expect(screen.getByRole('alert').textContent).toContain('Restart failed')
    await fireEvent.click(screen.getByRole('button', { name: 'Restart daemon' }))
    expect(postFetch).toHaveBeenCalledTimes(2)
    await vi.advanceTimersByTimeAsync(2000)
    expect(screen.queryByRole('alert')).toBeNull()
  })

  test('keeps a retry pending when a poll returns the previous failure', async () => {
    vi.useFakeTimers()
    const failed = status({
      can_restart: true,
      job: { state: 'restart', downloaded: 1, total: 1, error: 'Restart failed' },
    })
    setGeneratedFetch(vi.fn(async () => Response.json(failed)))
    vi.stubGlobal(
      'confirm',
      vi.fn(() => true),
    )
    let rejectPost!: (response: Response) => void
    const postFetch = vi.fn<typeof fetch>(
      () =>
        new Promise((resolve) => {
          rejectPost = resolve
        }),
    )
    renderSettings({ postFetch, originStable: true })
    await settle()
    expect(screen.getByRole('alert').textContent).toContain('Restart failed')
    await fireEvent.click(screen.getByRole('button', { name: 'Restart daemon' }))
    await vi.advanceTimersByTimeAsync(2000)
    expect(screen.queryByRole('alert')).toBeNull()
    expect(screen.getByText('Restarting...')).not.toBeNull()
    expect(screen.queryByRole('button', { name: 'Restart daemon' })).toBeNull()
    rejectPost(new Response(null, { status: 500 }))
    await settle()
    expect(screen.getByRole('alert').textContent).toContain("Couldn't restart the daemon")
  })

  test('ignores a late restart response after status confirms success', async () => {
    vi.useFakeTimers()
    const pending = status({
      can_restart: true,
      job: { state: 'restart', downloaded: 1, total: 1 },
    })
    setGeneratedFetch(
      vi
        .fn<typeof fetch>()
        .mockResolvedValueOnce(Response.json(pending))
        .mockImplementation(async () =>
          Response.json(status({ provider: 'built_in', can_change: false })),
        ),
    )
    vi.stubGlobal(
      'confirm',
      vi.fn(() => true),
    )
    let resolvePost!: (response: Response) => void
    const postFetch = vi.fn<typeof fetch>(
      () =>
        new Promise((resolve) => {
          resolvePost = resolve
        }),
    )
    renderSettings({ postFetch, originStable: true })
    await settle()
    await fireEvent.click(screen.getByRole('button', { name: 'Restart daemon' }))
    await vi.advanceTimersByTimeAsync(2000)
    expect(screen.getByText('On', { exact: true })).not.toBeNull()
    resolvePost(new Response(null, { status: 500 }))
    await settle()
    expect(screen.queryByRole('alert')).toBeNull()
  })

  test('allows retry after a restart request is lost while the daemon stays up', async () => {
    vi.useFakeTimers()
    const pending = status({
      can_restart: true,
      job: { state: 'restart', downloaded: 1, total: 1 },
    })
    setGeneratedFetch(vi.fn(async () => Response.json(pending)))
    vi.stubGlobal(
      'confirm',
      vi.fn(() => true),
    )
    const postFetch = vi.fn<typeof fetch>().mockRejectedValue(new TypeError('disconnected'))
    renderSettings({ postFetch, originStable: true })
    await settle()
    await fireEvent.click(screen.getByRole('button', { name: 'Restart daemon' }))
    await vi.advanceTimersByTimeAsync(122_000)
    expect(screen.getByRole('alert').textContent).toContain("Couldn't confirm restart")
    await fireEvent.click(screen.getByRole('button', { name: 'Restart daemon' }))
    expect(postFetch).toHaveBeenCalledTimes(2)
  })

  test.each([
    [true, "Couldn't reconnect. Run kata daemon status, then reopen kata ui."],
    [false, 'Run kata daemon status, then reopen kata ui.'],
  ])('offers recovery when originStable=%s does not return', async (originStable, message) => {
    vi.useFakeTimers()
    vi.spyOn(AbortSignal, 'timeout').mockImplementation((ms) => {
      const controller = new AbortController()
      setTimeout(() => controller.abort(), ms)
      return controller.signal
    })
    let available = false
    const read = vi
      .fn<typeof fetch>()
      .mockResolvedValueOnce(
        Response.json(
          status({ can_restart: true, job: { state: 'restart', downloaded: 1, total: 1 } }),
        ),
      )
      .mockImplementation(async (_input, init) => {
        if (!available) throw new TypeError('disconnected')
        return new Promise((resolve, reject) => {
          const timer = setTimeout(
            () => resolve(Response.json(status({ provider: 'built_in', can_change: false }))),
            50,
          )
          init?.signal?.addEventListener('abort', () => {
            clearTimeout(timer)
            reject(new TypeError('disconnected'))
          })
        })
      })
    setGeneratedFetch(read)
    vi.stubGlobal(
      'confirm',
      vi.fn(() => true),
    )
    renderSettings({
      postFetch: vi.fn(async () => new Response(null, { status: 202 })),
      originStable,
    })
    await settle()
    await fireEvent.click(screen.getByRole('button', { name: 'Restart daemon' }))
    await vi.advanceTimersByTimeAsync(120_000)
    expect(screen.getByRole('alert').textContent).toBe(message)
    expect(screen.getByText('Reconnecting...', { exact: true })).not.toBeNull()
    if (originStable) {
      available = true
      await vi.advanceTimersByTimeAsync(4000)
      expect(screen.getByText('On', { exact: true })).not.toBeNull()
      expect(screen.queryByRole('alert')).toBeNull()
    }
  })

  test('keeps remote restart status during another client restart and outage', async () => {
    vi.useFakeTimers()
    setGeneratedFetch(
      vi
        .fn<typeof fetch>()
        .mockResolvedValueOnce(
          Response.json(status({ job: { state: 'restarting', downloaded: 1, total: 1 } })),
        )
        .mockRejectedValue(new TypeError('disconnected')),
    )
    renderSettings({ remoteHost: 'daemon.example', originStable: true })
    await settle()
    await vi.advanceTimersByTimeAsync(122_000)
    expect(screen.getByText('Restarting...')).not.toBeNull()
    expect(screen.queryByRole('button', { name: 'Restart daemon' })).toBeNull()
    expect(screen.queryByRole('alert')).toBeNull()
  })

  test('offers recovery when another tab restarts and a read hangs', async () => {
    vi.useFakeTimers()
    vi.spyOn(AbortSignal, 'timeout').mockImplementation((ms) => {
      const controller = new AbortController()
      setTimeout(() => controller.abort(), ms)
      return controller.signal
    })
    const read = vi
      .fn<typeof fetch>()
      .mockResolvedValueOnce(
        Response.json(status({ job: { state: 'restarting', downloaded: 1, total: 1 } })),
      )
      .mockImplementation(
        (_input, init) =>
          new Promise((_resolve, reject) => {
            init?.signal?.addEventListener('abort', () => reject(new TypeError('disconnected')))
          }),
      )
    setGeneratedFetch(read)
    renderSettings({ originStable: true })
    await settle()
    await vi.advanceTimersByTimeAsync(120_000)
    expect(screen.getByRole('alert').textContent).toContain('kata daemon status')
    expect(screen.getByRole('alert').textContent).toContain('reopen kata ui')
  })

  test.each([true, false])(
    'shows a helper failure after an interrupted read, local request=%s',
    async (localRequest) => {
      vi.useFakeTimers()
      vi.spyOn(AbortSignal, 'timeout').mockImplementation((ms) => {
        const controller = new AbortController()
        setTimeout(() => controller.abort(), ms)
        return controller.signal
      })
      const pending = status({
        can_restart: localRequest,
        job: { state: localRequest ? 'restart' : 'restarting', downloaded: 1, total: 1 },
      })
      const failed = status({
        can_restart: true,
        job: { state: 'restart', downloaded: 1, total: 1, error: 'Restart failed' },
      })
      setGeneratedFetch(
        vi
          .fn<typeof fetch>()
          .mockResolvedValueOnce(Response.json(pending))
          .mockImplementationOnce(
            (_input, init) =>
              new Promise((_resolve, reject) => {
                init?.signal?.addEventListener('abort', () => reject(new TypeError('disconnected')))
              }),
          )
          .mockResolvedValue(Response.json(failed)),
      )
      vi.stubGlobal(
        'confirm',
        vi.fn(() => true),
      )
      renderSettings({ postFetch: vi.fn(async () => new Response(null, { status: 202 })) })
      await settle()
      if (localRequest)
        await fireEvent.click(screen.getByRole('button', { name: 'Restart daemon' }))
      expect(screen.getByText('Restarting...', { exact: true })).not.toBeNull()
      expect(screen.queryByText(/run kata daemon restart/)).toBeNull()
      await vi.advanceTimersByTimeAsync(16_000)
      expect(screen.getByRole('alert').textContent).toContain('Restart failed')
      expect(screen.getByRole('button', { name: 'Restart daemon' })).not.toBeNull()
    },
  )

  test('observes another client restarting after a rejected request', async () => {
    vi.useFakeTimers()
    const pending = status({
      can_restart: true,
      job: { state: 'restart', downloaded: 1, total: 1 },
    })
    setGeneratedFetch(
      vi
        .fn<typeof fetch>()
        .mockResolvedValueOnce(Response.json(pending))
        .mockResolvedValueOnce(Response.json(pending))
        .mockResolvedValue(Response.json(status({ provider: 'built_in', can_change: false }))),
    )
    vi.stubGlobal(
      'confirm',
      vi.fn(() => true),
    )
    renderSettings({
      postFetch: vi.fn(async () => new Response(null, { status: 409 })),
      originStable: true,
    })
    await settle()
    await fireEvent.click(screen.getByRole('button', { name: 'Restart daemon' }))
    expect(screen.getByRole('alert').textContent).toContain("Couldn't restart")
    expect(screen.getByRole('button', { name: 'Restart daemon' })).not.toBeNull()
    await vi.advanceTimersByTimeAsync(2000)
    expect(screen.getByRole('alert').textContent).toContain("Couldn't restart")
    await vi.advanceTimersByTimeAsync(2000)
    expect(screen.getByText('On', { exact: true })).not.toBeNull()
    expect(screen.queryByRole('alert')).toBeNull()
  })

  test.each([
    ['a viewer who cannot administer the daemon', false, true],
    ['a daemon that cannot change its setup', true, false],
  ])('shows status with no button for %s', async (_name, canAdministerDaemon, canChange) => {
    vi.useFakeTimers()
    setGeneratedFetch(vi.fn(async () => Response.json(status({ can_change: canChange }))))
    renderSettings({ canAdministerDaemon })
    await settle()
    expect(screen.getByText('Off', { exact: true })).not.toBeNull()
    expect(screen.queryByRole('button', { name: 'Turn on' })).toBeNull()
  })

  test('renders count-free indexing without progress counts', async () => {
    vi.useFakeTimers()
    setGeneratedFetch(
      vi.fn(async () =>
        Response.json(
          status({
            can_change: false,
            provider: 'built_in',
            download_bytes: 0,
            indexing: { done: 0, total: 0, failed: false },
          }),
        ),
      ),
    )
    renderSettings({ canAdministerDaemon: false })
    await settle()
    expect(screen.getByText('Turning on', { exact: true })).not.toBeNull()
    expect(screen.queryByText(/Indexing 0 of 0/)).toBeNull()
    expect(screen.queryByRole('progressbar')).toBeNull()
  })

  test.each([undefined, 'daemon.example'])(
    'keeps shared download wording and retry after failure for host=%s',
    async (remoteHost) => {
      vi.useFakeTimers()
      setGeneratedFetch(
        vi.fn(async () =>
          Response.json(
            status({
              job: {
                state: 'failed',
                downloaded: 0,
                total: 0,
                error: "Install pgvector in this daemon's database, then try again.",
              },
            }),
          ),
        ),
      )
      render(SearchSettings, {
        props: { canAdministerDaemon: true, onback: vi.fn(), postFetch: fetch, remoteHost },
      })
      await settle()
      expect(
        screen.getByText(
          remoteHost
            ? `Downloads 290 MB once to ${remoteHost}`
            : 'Downloads 290 MB once, shared by all Kenn apps',
        ),
      ).not.toBeNull()
      expect(screen.getByRole('alert').textContent).toContain('Install pgvector')
      expect(screen.getByRole('button', { name: 'Turn on' }).hasAttribute('disabled')).toBe(false)
    },
  )

  test('shows Ollama as on without a switch or download action', async () => {
    vi.useFakeTimers()
    setGeneratedFetch(
      vi.fn(async () =>
        Response.json(status({ provider: 'ollama', can_change: false, download_bytes: 0 })),
      ),
    )
    renderSettings()
    await settle()
    expect(screen.getByText('On · Ollama', { exact: true })).not.toBeNull()
    expect(screen.queryByText('Switching keeps your index')).toBeNull()
    expect(screen.queryByText(/Downloads 290 MB/)).toBeNull()
  })

  test.each([undefined, 'daemon.example'])(
    'shows the manual restart step for host=%s after download',
    async (remoteHost) => {
      vi.useFakeTimers()
      const read = vi
        .fn<typeof fetch>()
        .mockResolvedValueOnce(Response.json(status()))
        .mockResolvedValue(
          Response.json(
            status({
              can_change: false,
              job: { state: 'restart', downloaded: 290_000_000, total: 290_000_000 },
            }),
          ),
        )
      setGeneratedFetch(read)
      const postFetch = vi.fn<typeof fetch>(async () =>
        Response.json(
          status({
            can_change: false,
            job: { state: 'downloading', downloaded: 0, total: 290_000_000 },
          }),
          { status: 202 },
        ),
      )
      render(SearchSettings, {
        props: { canAdministerDaemon: true, onback: vi.fn(), postFetch, remoteHost },
      })
      await settle()
      await fireEvent.click(screen.getByRole('button', { name: 'Turn on' }))
      await settle()
      await vi.advanceTimersByTimeAsync(2000)
      expect(
        screen.getByText(
          remoteHost
            ? `Restart the daemon on ${remoteHost} to finish.`
            : 'Restart the daemon to finish: run kata daemon restart. If this daemon uses a custom home, set KATA_HOME to it first.',
        ),
      ).not.toBeNull()
      expect(screen.queryByRole('button', { name: 'Restart daemon' })).toBeNull()
    },
  )

  test('sends one unproxied request per click and shows the download it starts', async () => {
    vi.useFakeTimers()
    const read = vi.fn<typeof fetch>(async () => Response.json(status()))
    setGeneratedFetch(read)
    let accept!: () => void
    const turnOn = vi.fn<typeof fetch>(
      () =>
        new Promise<Response>((resolve) => {
          accept = () =>
            resolve(
              Response.json(
                status({ job: { state: 'downloading', downloaded: 0, total: 290_000_000 } }),
                { status: 202 },
              ),
            )
        }),
    )
    vi.stubGlobal('fetch', turnOn)
    renderSettings()
    await settle()
    const button = screen.getByRole('button', { name: 'Turn on' })
    await fireEvent.click(button)
    await fireEvent.click(button)
    accept()
    await settle()
    expect(turnOn).toHaveBeenCalledTimes(1)
    const request = turnOn.mock.calls[0]!
    expect(new URL(String(request[0])).pathname).toBe('/api/v1/embeddings/local')
    expect(request[1]?.method).toBe('POST')
    expect(read.mock.calls.every(([, init]) => (init?.method ?? 'GET') === 'GET')).toBe(true)
    expect(
      screen.getByText('Downloading 0 MB of 290 MB · Keyword search works meanwhile'),
    ).not.toBeNull()
  })

  test('ignores the failed-request refresh if a later Turn on starts first', async () => {
    vi.useFakeTimers()
    let late!: (response: Response) => void
    const read = vi
      .fn<typeof fetch>()
      .mockResolvedValueOnce(Response.json(status()))
      .mockImplementationOnce(
        () =>
          new Promise<Response>((resolve) => {
            late = resolve
          }),
      )
      .mockResolvedValue(
        Response.json(
          status({
            can_change: false,
            job: { state: 'restart', downloaded: 290_000_000, total: 290_000_000 },
          }),
        ),
      )
    setGeneratedFetch(read)
    const postFetch = vi
      .fn<typeof fetch>()
      .mockRejectedValueOnce(new TypeError('Failed to fetch'))
      .mockResolvedValueOnce(
        Response.json(
          status({
            can_change: false,
            job: { state: 'downloading', downloaded: 0, total: 290_000_000 },
          }),
          { status: 202 },
        ),
      )
    renderSettings({ postFetch })
    await settle()
    await fireEvent.click(screen.getByRole('button', { name: 'Turn on' }))
    await settle()
    expect(read).toHaveBeenCalledTimes(2)
    await fireEvent.click(screen.getByRole('button', { name: 'Turn on' }))
    await settle()
    late(Response.json(status()))
    await settle()
    expect(screen.getByText(/^Downloading /)).not.toBeNull()
  })

  test('stops polling while Off', async () => {
    vi.useFakeTimers()
    const read = vi.fn<typeof fetch>(async () => Response.json(status()))
    setGeneratedFetch(read)
    renderSettings()
    await settle()
    await vi.advanceTimersByTimeAsync(10000)
    expect(read).toHaveBeenCalledOnce()
  })

  test('aborts reads and stops polling when navigation removes Settings', async () => {
    vi.useFakeTimers()
    const read = vi
      .fn<typeof fetch>()
      .mockResolvedValueOnce(
        Response.json(
          status({
            can_change: false,
            job: { state: 'downloading', downloaded: 0, total: 290_000_000 },
          }),
        ),
      )
      .mockImplementation(() => new Promise<Response>(() => {}))
    setGeneratedFetch(read)
    const view = renderSettings()
    await settle()
    await vi.advanceTimersByTimeAsync(2000)
    view.unmount()
    expect(read.mock.calls[1]?.[1]?.signal?.aborted).toBe(true)
    await vi.advanceTimersByTimeAsync(10000)
    expect(read).toHaveBeenCalledTimes(2)
  })
})
