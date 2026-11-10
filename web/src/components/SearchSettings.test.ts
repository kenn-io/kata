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

function renderSettings(props: { canAdministerDaemon?: boolean; postFetch?: typeof fetch } = {}) {
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
    vi.useRealTimers()
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
      expect(screen.queryByRole('button', { name: 'Turn on' })).toBeNull()
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
