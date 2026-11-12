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
    ...patch,
  }
}

function renderSettings(
  props: {
    canAdministerDaemon?: boolean
    postFetch?: typeof fetch
    originStable?: boolean
    remoteHost?: string | undefined
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
    vi.unstubAllGlobals()
    vi.restoreAllMocks()
    vi.useRealTimers()
  })

  test.each([
    [true, undefined],
    [false, undefined],
    [true, 'daemon.example'],
    [false, 'daemon.example'],
  ])(
    'shows another tab restarting for admin=%s and host=%s',
    async (canAdministerDaemon, remoteHost) => {
      vi.useFakeTimers()
      setGeneratedFetch(
        vi.fn(async () =>
          Response.json({
            ...status({
              can_change: false,
              can_restart: false,
              job: { state: 'restart', downloaded: 1, total: 1 },
            }),
            restart_in_progress: true,
          }),
        ),
      )
      const { container } = renderSettings({ canAdministerDaemon, remoteHost })
      await settle()
      expect(screen.getByText('Restarting...')).not.toBeNull()
      expect(container.textContent).not.toContain('Restart the daemon')
    },
  )

  test('confirms a restart and shows On when the daemon returns', async () => {
    vi.useFakeTimers()
    const waiting = status({
      can_change: false,
      can_restart: true,
      job: { state: 'restart', downloaded: 1, total: 1 },
    })
    setGeneratedFetch(
      vi
        .fn<typeof fetch>()
        .mockResolvedValueOnce(Response.json(waiting))
        .mockResolvedValueOnce(
          Response.json({ ...waiting, can_restart: false, restart_in_progress: true }),
        )
        .mockResolvedValue(Response.json(status({ provider: 'built_in', can_change: false }))),
    )
    const postFetch = vi.fn<typeof fetch>(async () => new Response(null, { status: 202 }))
    const confirm = vi.fn().mockReturnValueOnce(false).mockReturnValueOnce(true)
    vi.stubGlobal('confirm', confirm)
    renderSettings({ postFetch })
    await settle()
    await fireEvent.click(screen.getByRole('button', { name: 'Restart daemon' }))
    expect(postFetch).not.toHaveBeenCalled()
    await fireEvent.click(screen.getByRole('button', { name: 'Restart daemon' }))
    expect(postFetch).toHaveBeenCalledOnce()
    expect(confirm).toHaveBeenCalledWith(expect.stringContaining('service manager'))
    expect(screen.getByText('Restarting...')).not.toBeNull()
    await vi.advanceTimersByTimeAsync(4000)
    expect(screen.getByText('On', { exact: true })).not.toBeNull()
  })

  test.each([undefined, 'Earlier failure'])(
    'keeps waiting with previous error %s while restart is in transit',
    async (error) => {
      vi.useFakeTimers()
      const waiting = status({
        can_change: false,
        can_restart: true,
        job: { state: 'restart', downloaded: 1, total: 1, ...(error ? { error } : {}) },
      })
      let current = waiting
      setGeneratedFetch(vi.fn(async () => Response.json(current)))
      let resolvePost!: (response: Response) => void
      const postFetch = vi.fn<typeof fetch>(
        () =>
          new Promise((resolve) => {
            resolvePost = resolve
          }),
      )
      vi.stubGlobal(
        'confirm',
        vi.fn(() => true),
      )
      renderSettings({ postFetch })
      await settle()
      await fireEvent.click(screen.getByRole('button', { name: 'Restart daemon' }))
      await vi.advanceTimersByTimeAsync(4000)
      expect(screen.queryByRole('alert')).toBeNull()
      expect(screen.queryByRole('button', { name: 'Restart daemon' })).toBeNull()
      expect(postFetch).toHaveBeenCalledOnce()
      current = status({ provider: 'built_in', can_change: false })
      await vi.advanceTimersByTimeAsync(2000)
      expect(screen.getByText('On', { exact: true })).not.toBeNull()
      resolvePost(new Response(null, { status: 500 }))
      await settle()
      expect(screen.queryByRole('alert')).toBeNull()
    },
  )

  test.each(['disconnected', 'timeout', 'in progress'])(
    'handles a stalled restart request: %s',
    async (mode) => {
      vi.useFakeTimers()
      vi.spyOn(AbortSignal, 'timeout').mockImplementation((ms) => {
        const controller = new AbortController()
        setTimeout(() => controller.abort(), ms)
        return controller.signal
      })
      const waiting = status({
        can_change: false,
        can_restart: true,
        job: { state: 'restart', downloaded: 1, total: 1 },
      })
      let current = waiting
      const read = vi.fn<typeof fetch>(async () => Response.json(current))
      setGeneratedFetch(read)
      const postFetch =
        mode === 'disconnected'
          ? vi.fn<typeof fetch>().mockRejectedValue(new TypeError('disconnected'))
          : vi.fn<typeof fetch>(
              (_url, init) =>
                new Promise((_resolve, reject) => {
                  init?.signal?.addEventListener('abort', () =>
                    reject(new DOMException('Aborted', 'AbortError')),
                  )
                }),
            )
      vi.stubGlobal(
        'confirm',
        vi.fn(() => true),
      )
      renderSettings({ postFetch })
      await settle()
      await fireEvent.click(screen.getByRole('button', { name: 'Restart daemon' }))
      if (mode === 'in progress')
        current = { ...waiting, can_restart: false, restart_in_progress: true }
      if (mode === 'disconnected') {
        await vi.advanceTimersByTimeAsync(2000)
      } else {
        await vi.advanceTimersByTimeAsync(9999)
        expect(screen.queryByRole('button', { name: 'Restart daemon' })).toBeNull()
        await vi.advanceTimersByTimeAsync(2001)
      }
      if (mode === 'in progress') {
        await vi.advanceTimersByTimeAsync(120_000)
        expect(screen.queryByRole('alert')).toBeNull()
        expect(screen.queryByRole('button', { name: 'Restart daemon' })).toBeNull()
        const count = read.mock.calls.length
        current = status({ provider: 'built_in', can_change: false })
        await vi.advanceTimersByTimeAsync(2000)
        expect(read.mock.calls.length).toBeGreaterThan(count)
        expect(screen.getByText('On', { exact: true })).not.toBeNull()
      } else {
        expect(screen.getByRole('button', { name: 'Restart daemon' })).not.toBeNull()
        expect(screen.getByRole('alert').textContent).toContain("Couldn't confirm restart")
        if (mode === 'disconnected') {
          await fireEvent.click(screen.getByRole('button', { name: 'Restart daemon' }))
          expect(postFetch).toHaveBeenCalledTimes(2)
        }
      }
    },
  )

  test('ignores an earlier status read after the restart request is accepted', async () => {
    vi.useFakeTimers()
    const waiting = status({
      can_change: false,
      can_restart: true,
      job: { state: 'restart', downloaded: 1, total: 1 },
    })
    let resolveRead!: (response: Response) => void
    setGeneratedFetch(
      vi
        .fn<typeof fetch>()
        .mockResolvedValueOnce(Response.json(waiting))
        .mockImplementationOnce(
          () =>
            new Promise((resolve) => {
              resolveRead = resolve
            }),
        ),
    )
    let resolvePost!: (response: Response) => void
    const postFetch = vi.fn<typeof fetch>(
      () =>
        new Promise((resolve) => {
          resolvePost = resolve
        }),
    )
    vi.stubGlobal(
      'confirm',
      vi.fn(() => true),
    )
    renderSettings({ postFetch })
    await settle()
    await fireEvent.click(screen.getByRole('button', { name: 'Restart daemon' }))
    await vi.advanceTimersByTimeAsync(2000)
    resolvePost(new Response(null, { status: 202 }))
    await settle()
    resolveRead(Response.json(waiting))
    await settle()
    expect(screen.queryByRole('alert')).toBeNull()
    expect(screen.queryByRole('button', { name: 'Restart daemon' })).toBeNull()
  })

  test.each([
    [true, undefined, true, "Couldn't reconnect. Run kata daemon status, then reopen kata ui."],
    [false, undefined, false, 'Run kata daemon status.'],
    [
      false,
      'daemon.example',
      true,
      "Couldn't reconnect. Run kata daemon status on daemon.example.",
    ],
  ])(
    'gives observing admin=%s host=%s recovery guidance',
    async (canAdministerDaemon, remoteHost, originStable, message) => {
      vi.useFakeTimers()
      let recovery: 'offline' | 'progress' | 'on' = 'offline'
      const read = vi
        .fn<typeof fetch>()
        .mockResolvedValueOnce(
          Response.json(
            status({
              can_change: false,
              job: { state: 'restart', downloaded: 1, total: 1 },
              restart_in_progress: true,
            }),
          ),
        )
        .mockImplementation(async () =>
          recovery === 'on'
            ? Response.json(status({ provider: 'built_in', can_change: false }))
            : recovery === 'progress'
              ? Response.json(
                  status({
                    can_change: false,
                    job: { state: 'restart', downloaded: 1, total: 1 },
                    restart_in_progress: true,
                  }),
                )
              : new Response(null, { status: 503 }),
        )
      setGeneratedFetch(read)
      renderSettings({ canAdministerDaemon, remoteHost, originStable })
      await settle()
      await vi.advanceTimersByTimeAsync(120_000)
      expect(screen.getByRole('alert').textContent).toBe(message)
      const count = read.mock.calls.length
      recovery = 'progress'
      await vi.advanceTimersByTimeAsync(2000)
      expect(screen.getByText('Restarting...')).not.toBeNull()
      expect(screen.queryByRole('alert')).toBeNull()
      recovery = 'on'
      await vi.advanceTimersByTimeAsync(2000)
      expect(read.mock.calls.length).toBeGreaterThan(count)
      expect(screen.getByText('On', { exact: true })).not.toBeNull()
      expect(screen.queryByRole('alert')).toBeNull()
    },
  )

  test('continues polling after a stalled status read times out', async () => {
    vi.useFakeTimers()
    vi.spyOn(AbortSignal, 'timeout').mockImplementation((ms) => {
      const controller = new AbortController()
      setTimeout(() => controller.abort(), ms)
      return controller.signal
    })
    const waiting = status({
      can_change: false,
      job: { state: 'restart', downloaded: 1, total: 1 },
      restart_in_progress: true,
    })
    const read = vi
      .fn<typeof fetch>()
      .mockResolvedValueOnce(Response.json(waiting))
      .mockImplementationOnce(
        (_url, init) =>
          new Promise((_resolve, reject) => {
            init?.signal?.addEventListener('abort', () =>
              reject(new DOMException('Aborted', 'AbortError')),
            )
          }),
      )
      .mockResolvedValue(Response.json(status({ provider: 'built_in', can_change: false })))
    setGeneratedFetch(read)
    renderSettings()
    await settle()
    await vi.advanceTimersByTimeAsync(14_000)
    expect(screen.getByText('On', { exact: true })).not.toBeNull()
  })

  test('reports a refused restart', async () => {
    vi.useFakeTimers()
    setGeneratedFetch(
      vi.fn(async () =>
        Response.json(
          status({
            can_change: false,
            can_restart: true,
            job: { state: 'restart', downloaded: 1, total: 1 },
          }),
        ),
      ),
    )
    vi.stubGlobal(
      'confirm',
      vi.fn(() => true),
    )
    renderSettings({ postFetch: vi.fn(async () => new Response(null, { status: 403 })) })
    await settle()
    await fireEvent.click(screen.getByRole('button', { name: 'Restart daemon' }))
    expect(screen.getByRole('alert').textContent).toContain("Couldn't restart")
    expect(screen.getByRole('button', { name: 'Restart daemon' })).not.toBeNull()
  })

  test('permits retry after helper failure', async () => {
    vi.useFakeTimers()
    const waiting = status({
      can_change: false,
      can_restart: true,
      job: { state: 'restart', downloaded: 1, total: 1 },
    })
    setGeneratedFetch(
      vi
        .fn<typeof fetch>()
        .mockResolvedValueOnce(Response.json(waiting))
        .mockResolvedValue(
          Response.json({ ...waiting, job: { ...waiting.job, error: 'Restart failed' } }),
        ),
    )
    vi.stubGlobal(
      'confirm',
      vi.fn(() => true),
    )
    const postFetch = vi.fn<typeof fetch>(async () => new Response(null, { status: 202 }))
    renderSettings({ postFetch, originStable: true })
    await settle()
    await fireEvent.click(screen.getByRole('button', { name: 'Restart daemon' }))
    await vi.advanceTimersByTimeAsync(2000)
    expect(screen.getByRole('alert').textContent).toContain('Restart failed')
    await fireEvent.click(screen.getByRole('button', { name: 'Restart daemon' }))
    expect(postFetch).toHaveBeenCalledTimes(2)
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
