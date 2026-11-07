import { cleanup, fireEvent, render, screen } from '@testing-library/svelte'
import { afterEach, describe, expect, test, vi } from 'vitest'

import { createCredentialedFetch, setGeneratedFetch } from '../lib/api/client'
import type { LocalEmbeddingsStatus } from '../lib/api/generated'
import SearchSettings from './SearchSettings.svelte'

function status(patch: Partial<LocalEmbeddingsStatus> = {}): LocalEmbeddingsStatus {
  return {
    provider: 'none',
    download_bytes: 290_000_000,
    ollama_available: false,
    rebuild: false,
    can_change: true,
    ...patch,
  }
}

function renderSettings(
  props: { canAdministerDaemon?: boolean; remoteHost?: string; postFetch?: typeof fetch } = {},
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
    expect(screen.getByText('Downloads 290 MB once, shared by all Kenn apps')).not.toBeNull()
    expect(screen.queryByRole('button', { name: 'Turn on' })).toBeNull()
  })

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

  test('shows a failed job under the row and keeps its button for a retry', async () => {
    vi.useFakeTimers()
    setGeneratedFetch(
      vi.fn(async () =>
        Response.json(
          status({ job: { state: 'failed', downloaded: 0, total: 0, error: 'Disk is full.' } }),
        ),
      ),
    )
    renderSettings()
    await settle()
    expect(screen.getByRole('alert').textContent).toBe('Disk is full.')
    expect(screen.getByRole('button', { name: 'Turn on' })).not.toBeNull()
  })

  test('follows a lost POST through Off, failed reads, sign-in, indexing, and On', async () => {
    vi.useFakeTimers()
    const requireSignIn = vi.fn(() => true)
    const read = vi
      .fn<typeof fetch>()
      .mockResolvedValueOnce(Response.json(status()))
      .mockResolvedValueOnce(Response.json(status()))
      .mockRejectedValueOnce(new TypeError('Failed to fetch'))
      .mockResolvedValueOnce(new Response(null, { status: 401 }))
      .mockResolvedValueOnce(
        Response.json(
          status({ provider: 'built_in', indexing: { done: 0, total: 1, failed: false } }),
        ),
      )
      .mockResolvedValueOnce(Response.json(status({ provider: 'built_in' })))
    setGeneratedFetch(createCredentialedFetch(() => undefined, read, requireSignIn))
    vi.stubGlobal(
      'fetch',
      vi.fn(async () => {
        throw new TypeError('Failed to fetch')
      }),
    )
    renderSettings()
    await settle()
    await fireEvent.click(screen.getByRole('button', { name: 'Turn on' }))
    await settle()
    expect(screen.getByRole('alert').textContent).toContain("Couldn't confirm Turn on")
    await vi.advanceTimersByTimeAsync(6000)
    expect(requireSignIn).toHaveBeenCalledOnce()
    expect(
      screen.getByText('Indexing 0 of 1 issue · Keyword search works meanwhile'),
    ).not.toBeNull()
    expect(screen.queryByRole('alert')).toBeNull()
    await vi.advanceTimersByTimeAsync(10000)
    expect(screen.getByText('On', { exact: true })).not.toBeNull()
    expect(read).toHaveBeenCalledTimes(6)
  })

  test('aborts a read at Turn on and ignores its late Off response', async () => {
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
    setGeneratedFetch(read)
    vi.stubGlobal(
      'fetch',
      vi.fn(async () =>
        Response.json(
          status({ job: { state: 'downloading', downloaded: 0, total: 290_000_000 } }),
          { status: 202 },
        ),
      ),
    )
    renderSettings()
    await settle()
    await vi.advanceTimersByTimeAsync(2000)
    await fireEvent.click(screen.getByRole('button', { name: 'Turn on' }))
    await settle()
    expect(read.mock.calls[1]?.[1]?.signal?.aborted).toBe(true)
    late(Response.json(status()))
    await settle()
    expect(
      screen.getByText('Downloading 0 MB of 290 MB · Keyword search works meanwhile'),
    ).not.toBeNull()
  })

  test('sends a rejected POST through sign-in recovery', async () => {
    vi.useFakeTimers()
    setGeneratedFetch(vi.fn(async () => Response.json(status())))
    const requireSignIn = vi.fn(() => true)
    const postFetch = createCredentialedFetch(
      () => undefined,
      vi.fn(async () => new Response(null, { status: 401 })),
      requireSignIn,
    )
    renderSettings({ postFetch })
    await settle()
    await fireEvent.click(screen.getByRole('button', { name: 'Turn on' }))
    await settle()
    expect(requireSignIn).toHaveBeenCalledOnce()
  })

  test('aborts reads and stops polling when navigation removes Settings', async () => {
    vi.useFakeTimers()
    const read = vi
      .fn<typeof fetch>()
      .mockResolvedValueOnce(Response.json(status()))
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

  test('keeps polling until the first indexing pass finishes', async () => {
    vi.useFakeTimers()
    const read = vi
      .fn<typeof fetch>()
      .mockResolvedValueOnce(
        Response.json(
          status({ job: { state: 'restarting', downloaded: 290_000_000, total: 290_000_000 } }),
        ),
      )
      .mockResolvedValueOnce(
        Response.json(
          status({ provider: 'built_in', indexing: { failed: false, done: 0, total: 0 } }),
        ),
      )
      .mockResolvedValueOnce(
        Response.json(
          status({ provider: 'built_in', indexing: { failed: false, done: 40, total: 212 } }),
        ),
      )
      .mockResolvedValueOnce(Response.json(status({ provider: 'built_in' })))
    setGeneratedFetch(read)
    renderSettings()
    await settle()
    await vi.advanceTimersByTimeAsync(2000)
    expect(
      screen.getByText('Indexing 0 of 0 issues · Keyword search works meanwhile'),
    ).not.toBeNull()
    await vi.advanceTimersByTimeAsync(2000)
    expect(
      screen.getByText('Indexing 40 of 212 issues · Keyword search works meanwhile'),
    ).not.toBeNull()
    await vi.advanceTimersByTimeAsync(2000)
    expect(screen.getByText('On', { exact: true })).not.toBeNull()
    await vi.advanceTimersByTimeAsync(4000)
    expect(read).toHaveBeenCalledTimes(4)
  })

  test('says when the daemon has no status to report', async () => {
    vi.useFakeTimers()
    setGeneratedFetch(vi.fn(async () => new Response(null, { status: 404 })))
    renderSettings()
    expect(screen.getByText('Loading…')).not.toBeNull()
    await settle()
    expect(
      screen.getByText('Search by meaning status is unavailable for this daemon.'),
    ).not.toBeNull()
  })

  test('shows an indexing failure while retrying and clears it after recovery', async () => {
    vi.useFakeTimers()
    const read = vi
      .fn<typeof fetch>()
      .mockResolvedValueOnce(
        Response.json(
          status({ provider: 'built_in', indexing: { done: 0, total: 1, failed: true } }),
        ),
      )
      .mockResolvedValueOnce(
        Response.json(
          status({ provider: 'built_in', indexing: { done: 1, total: 2, failed: false } }),
        ),
      )
      .mockResolvedValueOnce(Response.json(status({ provider: 'built_in' })))
    setGeneratedFetch(read)
    renderSettings()
    await settle()
    expect(screen.getByRole('alert').textContent).toBe(
      'Indexing hit an error and will retry. Run kata health --json for details.',
    )
    expect(screen.getByText('Turning on')).not.toBeNull()
    await vi.advanceTimersByTimeAsync(2000)
    expect(screen.queryByRole('alert')).toBeNull()
    expect(
      screen.getByText('Indexing 1 of 2 issues · Keyword search works meanwhile'),
    ).not.toBeNull()
    await vi.advanceTimersByTimeAsync(2000)
    expect(screen.getByText('On', { exact: true })).not.toBeNull()
  })
})
