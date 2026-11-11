import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/svelte'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { setGeneratedFetch } from '../lib/api/client'
import ProjectSyncStatus from './ProjectSyncStatus.svelte'

const projectUID = '01J00000000000000000000002'
function response(projectID = 7, uid = projectUID, pending = 2): Response {
  return Response.json({
    statuses: [
      {
        project_id: projectID,
        project_uid: uid,
        role: 'replica',
        enabled: true,
        pending_push_count: pending,
        last_successful_sync_at: '2026-10-01T12:00:00Z',
        credential_status: 'live',
        last_error: 'private-token-never-display',
        embedding: {
          state: 'reused',
          artifact_limit: 32,
          limited: true,
          producer: {
            producer_instance_uid: '01J00000000000000000000003',
            recipe: { model: 'example-model', dimensions: 2 },
          },
          artifacts: [{ state: 'generated' }, { state: 'reused' }, { state: 'stored_unindexed' }],
        },
      },
    ],
  })
}
afterEach(() => {
  cleanup()
  vi.useRealTimers()
  vi.restoreAllMocks()
})
describe('ProjectSyncStatus', () => {
  it.each(['network', 'body', '503'])(
    'recovers on the next cycle after %s failure',
    async (failure) => {
      const fetcher = vi.fn().mockResolvedValueOnce(response())
      if (failure === 'network') fetcher.mockRejectedValueOnce(new TypeError('network failure'))
      else if (failure === 'body')
        fetcher.mockResolvedValueOnce(
          new Response(
            new ReadableStream({
              start(controller) {
                controller.error(new TypeError('connection dropped'))
              },
            }),
          ),
        )
      else fetcher.mockResolvedValueOnce(new Response('', { status: 503 }))
      setGeneratedFetch(fetcher.mockResolvedValueOnce(response()))
      const view = render(ProjectSyncStatus, {
        props: { projectID: 7, projectUID, refreshGeneration: 0 },
      })
      await screen.findByText('Pending: 2')
      await view.rerender({ projectID: 7, projectUID, refreshGeneration: 1 })
      await screen.findByText('Sync status unavailable. Refresh to retry.')
      await view.rerender({ projectID: 7, projectUID, refreshGeneration: 2 })
      await screen.findByText('Pending: 2')
    },
  )

  it('skips a cycle while a read is in progress', async () => {
    let finish!: (result: Response) => void
    const fetcher = vi.fn().mockImplementationOnce(
      () =>
        new Promise<Response>((resolve) => {
          finish = resolve
        }),
    )
    setGeneratedFetch(fetcher)
    const view = render(ProjectSyncStatus, {
      props: { projectID: 7, projectUID, refreshGeneration: 0 },
    })
    await waitFor(() => expect(finish).toBeDefined())
    await view.rerender({ projectID: 7, projectUID, refreshGeneration: 1 })
    finish(response())
    await screen.findByText('Pending: 2')
    expect(fetcher).toHaveBeenCalledTimes(1)
  })

  it('keeps the unavailable message while retrying without a previous result', async () => {
    const fetcher = vi
      .fn()
      .mockResolvedValueOnce(new Response('', { status: 503 }))
      .mockImplementationOnce(() => new Promise(() => {}))
    setGeneratedFetch(fetcher)
    const view = render(ProjectSyncStatus, {
      props: { projectID: 7, projectUID, refreshGeneration: 0 },
    })
    await screen.findByText('Sync status unavailable. Refresh to retry.')
    await view.rerender({ projectID: 7, projectUID, refreshGeneration: 1 })
    expect(screen.getByText('Sync status unavailable. Refresh to retry.')).not.toBeNull()
    expect(screen.queryByText('Reading sync status…')).toBeNull()
  })

  it.each(['background', 'manual'])('shows the right state during a %s refresh', async (kind) => {
    let resolveRead!: (value: Response) => void
    const fetcher = vi
      .fn()
      .mockResolvedValueOnce(response())
      .mockImplementationOnce(
        () =>
          new Promise<Response>((resolve) => {
            resolveRead = resolve
          }),
      )
    setGeneratedFetch(fetcher)
    const view = render(ProjectSyncStatus, {
      props: { projectID: 7, projectUID, refreshGeneration: 0 },
    })
    await screen.findByText('Pending: 2')
    if (kind === 'manual') {
      await fireEvent.click(screen.getByRole('button', { name: 'Refresh sync' }))
      expect(screen.getByText('Reading sync status…')).not.toBeNull()
      expect(screen.queryByText('Pending: 2')).toBeNull()
    } else {
      await view.rerender({ projectID: 7, projectUID, refreshGeneration: 1 })
      expect(screen.getByText('Pending: 2')).not.toBeNull()
      expect(screen.queryByText('Reading sync status…')).toBeNull()
    }
    resolveRead(response(7, projectUID, 0))
    await screen.findByText('Pending: 0')
  })

  it('replaces a stalled background read on manual refresh and ignores its late result', async () => {
    let resolveOld!: (value: Response) => void
    let oldSignal: AbortSignal | undefined
    const fetcher = vi
      .fn()
      .mockResolvedValueOnce(response())
      .mockImplementationOnce((_url, init) => {
        oldSignal = init.signal
        return new Promise<Response>((resolve) => {
          resolveOld = resolve
        })
      })
      .mockResolvedValueOnce(
        Response.json({
          statuses: [
            {
              project_id: 7,
              project_uid: projectUID,
              role: 'replica',
              enabled: true,
              pending_push_count: 0,
            },
          ],
        }),
      )
    setGeneratedFetch(fetcher)
    const view = render(ProjectSyncStatus, {
      props: { projectID: 7, projectUID, refreshGeneration: 0 },
    })
    await screen.findByText('Pending: 2')
    await view.rerender({ projectID: 7, projectUID, refreshGeneration: 1 })
    await view.rerender({ projectID: 7, projectUID, refreshGeneration: 2 })
    const button = screen.getByRole('button', { name: 'Refresh sync' }) as HTMLButtonElement
    expect(button.disabled).toBe(false)
    await fireEvent.click(button)
    await screen.findByText('Pending: 0')
    expect(oldSignal?.aborted).toBe(true)
    resolveOld(response())
    await new Promise((resolve) => setTimeout(resolve, 0))
    expect(screen.getByText('Pending: 0')).not.toBeNull()
    expect(fetcher).toHaveBeenCalledTimes(3)
  })

  it('disables refresh while a denied response body is stalled, then retries', async () => {
    let closeBody!: () => void
    const body = new ReadableStream({
      start(controller) {
        closeBody = () => controller.close()
      },
    })
    const fetcher = vi
      .fn()
      .mockResolvedValueOnce(response())
      .mockResolvedValueOnce(new Response(body, { status: 403 }))
      .mockResolvedValueOnce(response())
    setGeneratedFetch(fetcher)
    const view = render(ProjectSyncStatus, {
      props: { projectID: 7, projectUID, refreshGeneration: 0 },
    })
    await screen.findByText('Pending: 2')
    await view.rerender({ projectID: 7, projectUID, refreshGeneration: 1 })
    expect(screen.getByText('Reading sync status…')).not.toBeNull()
    expect(
      (screen.getByRole('button', { name: 'Refresh sync' }) as HTMLButtonElement).disabled,
    ).toBe(true)
    closeBody()
    await screen.findByText('Sync status unavailable. Refresh to retry.')
    await view.rerender({ projectID: 7, projectUID, refreshGeneration: 2 })
    await screen.findByText('Pending: 2')
  })

  it('waits until visible for its initial read', async () => {
    const hidden = vi.spyOn(document, 'hidden', 'get').mockReturnValue(true)
    const fetcher = vi.fn(async () => response())
    setGeneratedFetch(fetcher)
    const view = render(ProjectSyncStatus, {
      props: { projectID: 7, projectUID, refreshGeneration: 0 },
    })
    expect(fetcher).not.toHaveBeenCalled()
    expect(screen.getByText('Reading sync status…')).not.toBeNull()
    hidden.mockReturnValue(false)
    await view.rerender({ projectID: 7, projectUID, refreshGeneration: 1 })
    await screen.findByText('Pending: 2')
    hidden.mockRestore()
  })

  it('retries after a stalled read reaches its timeout', async () => {
    let signal: AbortSignal | undefined
    const fetcher = vi
      .fn()
      .mockImplementationOnce((_url, init) => {
        signal = init.signal
        return new Promise<Response>(() => {})
      })
      .mockResolvedValueOnce(response())
    setGeneratedFetch(fetcher)
    vi.useFakeTimers()
    const view = render(ProjectSyncStatus, {
      props: { projectID: 7, projectUID, refreshGeneration: 0 },
    })
    await vi.advanceTimersByTimeAsync(0)
    await view.rerender({ projectID: 7, projectUID, refreshGeneration: 1 })
    expect(signal?.aborted).toBe(false)
    await vi.advanceTimersByTimeAsync(120_000)
    expect(signal?.aborted).toBe(true)
    expect(screen.getByText('Sync status unavailable. Refresh to retry.')).not.toBeNull()
    await view.rerender({ projectID: 7, projectUID, refreshGeneration: 2 })
    await vi.advanceTimersByTimeAsync(0)
    expect(screen.getByText('Pending: 2')).not.toBeNull()
    vi.useRealTimers()
  })

  it('lets a slow refresh finish while hidden and waits until visible to refresh', async () => {
    let resolveSlow!: (value: Response) => void
    let signal: AbortSignal | undefined
    const hidden = vi.spyOn(document, 'hidden', 'get').mockReturnValue(false)
    const fetcher = vi
      .fn()
      .mockResolvedValueOnce(response())
      .mockImplementationOnce((_url, init) => {
        signal = init.signal
        return new Promise<Response>((resolve) => {
          resolveSlow = resolve
        })
      })
      .mockResolvedValueOnce(response())
    setGeneratedFetch(fetcher)
    const view = render(ProjectSyncStatus, {
      props: { projectID: 7, projectUID, refreshGeneration: 0 },
    })
    await screen.findByText('Pending: 2')
    await view.rerender({ projectID: 7, projectUID, refreshGeneration: 1 })
    expect(signal?.aborted).toBe(false)
    await view.rerender({ projectID: 7, projectUID, refreshGeneration: 2 })
    hidden.mockReturnValue(true)
    resolveSlow(response())
    await new Promise((resolve) => setTimeout(resolve, 0))
    expect(screen.getByText('Pending: 2')).not.toBeNull()
    expect(fetcher).toHaveBeenCalledTimes(2)
    hidden.mockReturnValue(false)
    await view.rerender({ projectID: 7, projectUID, refreshGeneration: 3 })
    await waitFor(() => expect(fetcher).toHaveBeenCalledTimes(3))
    hidden.mockRestore()
  })

  it.each([403, 404, 'mismatch', 'malformed 403', '403 body read error'])(
    'clears status after %s and retries on the next cycle',
    async (code) => {
      const fetcher = vi
        .fn()
        .mockResolvedValueOnce(response())
        .mockResolvedValueOnce(
          code === '403 body read error'
            ? new Response(
                new ReadableStream({
                  start(controller) {
                    controller.error(new TypeError('body read failed'))
                  },
                }),
                { status: 403 },
              )
            : code === 'mismatch'
              ? response(9)
              : code === 'malformed 403'
                ? new Response('{', {
                    status: 403,
                    headers: { 'Content-Type': 'application/json' },
                  })
                : new Response('', { status: Number(code) }),
        )
        .mockResolvedValueOnce(response())
      setGeneratedFetch(fetcher)
      const view = render(ProjectSyncStatus, {
        props: { projectID: 7, projectUID, refreshGeneration: 0 },
      })
      await screen.findByText('Pending: 2')
      await view.rerender({ projectID: 7, projectUID, refreshGeneration: 1 })
      await screen.findByText('Sync status unavailable. Refresh to retry.')
      expect(screen.queryByText('Pending: 2')).toBeNull()
      await view.rerender({ projectID: 7, projectUID, refreshGeneration: 2 })
      await screen.findByText('Pending: 2')
      expect(fetcher).toHaveBeenCalledTimes(3)
    },
  )

  it('updates an empty local project when federation appears', async () => {
    const fetcher = vi
      .fn()
      .mockResolvedValueOnce(Response.json({ statuses: [] }))
      .mockResolvedValueOnce(response())
    setGeneratedFetch(fetcher)
    const view = render(ProjectSyncStatus, {
      props: { projectID: 7, projectUID, refreshGeneration: 0 },
    })
    await screen.findByText('Local project')
    await view.rerender({ projectID: 7, projectUID, refreshGeneration: 1 })
    await screen.findByText('Pending: 2')
  })

  it('recovers on explicit refresh after access is revoked', async () => {
    const fetcher = vi
      .fn()
      .mockResolvedValueOnce(response())
      .mockResolvedValueOnce(new Response('', { status: 403 }))
      .mockResolvedValueOnce(response())
    setGeneratedFetch(fetcher)
    render(ProjectSyncStatus, { props: { projectID: 7, projectUID } })
    await screen.findByText('Pending: 2')
    await fireEvent.click(screen.getByRole('button', { name: 'Refresh sync' }))
    await screen.findByText('Sync status unavailable. Refresh to retry.')
    expect(screen.queryByText('Reused: 1')).toBeNull()
    await fireEvent.click(screen.getByRole('button', { name: 'Refresh sync' }))
    await screen.findByText('Pending: 2')
  })

  it('reads only the selected project and shows bounded retained counts without private errors', async () => {
    const fetcher = vi.fn<typeof fetch>(async () => response())
    setGeneratedFetch(fetcher)
    render(ProjectSyncStatus, { props: { projectID: 7, projectUID } })
    await screen.findByText('Reused: 1')
    const url = new URL(String(fetcher.mock.calls[0]?.[0]), 'https://daemon.example')
    expect(url.pathname).toBe('/api/v1/projects/7/federation/status')
    expect(url.searchParams.get('compact')).toBe('true')
    expect(screen.getByText('Retained window: 3/32 (limited)')).not.toBeNull()
    expect(screen.getByText('Retained without index: 1')).not.toBeNull()
    expect(screen.getByText('Recipe: example-model (2 dimensions)')).not.toBeNull()
    expect(screen.getByText('Sync needs attention')).not.toBeNull()
    expect(screen.queryByText('private-token-never-display')).toBeNull()
    expect(fetcher).toHaveBeenCalledTimes(1)
  })

  it('rejects stale project responses even when the transport ignores cancellation', async () => {
    let resolveOld!: (value: Response) => void
    setGeneratedFetch(
      vi.fn((url) =>
        String(url).includes('/projects/7/')
          ? new Promise<Response>((resolve) => {
              resolveOld = resolve
            })
          : Promise.resolve(response(8, '01J00000000000000000000004')),
      ),
    )
    const view = render(ProjectSyncStatus, { props: { projectID: 7, projectUID } })
    await waitFor(() => expect(resolveOld).toBeDefined())
    await view.rerender({ projectID: 8, projectUID: '01J00000000000000000000004' })
    await screen.findByText('Reused: 1')
    resolveOld(
      Response.json({
        statuses: [
          { project_id: 7, project_uid: projectUID, role: 'stale-private-project', enabled: true },
        ],
      }),
    )
    await new Promise((resolve) => setTimeout(resolve, 0))
    expect(screen.queryByText('stale-private-project')).toBeNull()
  })

  it('aborts an active read when authority removes the view', async () => {
    let signal: AbortSignal | undefined
    setGeneratedFetch(
      vi.fn((_url, init) => {
        signal = init?.signal ?? undefined
        return new Promise<Response>(() => {})
      }),
    )
    const view = render(ProjectSyncStatus, { props: { projectID: 7, projectUID } })
    await waitFor(() => expect(signal).toBeDefined())
    view.unmount()
    expect(signal?.aborted).toBe(true)
  })
})
