import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/svelte'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { setGeneratedFetch } from '../lib/api/client'
import ProjectSyncStatus from './ProjectSyncStatus.svelte'

const projectUID = '01J00000000000000000000002'
function response(projectID = 7, uid = projectUID): Response {
  return Response.json({
    statuses: [
      {
        project_id: projectID,
        project_uid: uid,
        role: 'replica',
        enabled: true,
        pending_push_count: 2,
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
afterEach(cleanup)
describe('ProjectSyncStatus', () => {
  it('refreshes after returning while an older read is in flight', async () => {
    let resolveSlow!: (value: Response) => void
    const fetcher = vi
      .fn()
      .mockResolvedValueOnce(response())
      .mockImplementationOnce(
        () =>
          new Promise<Response>((resolve) => {
            resolveSlow = resolve
          }),
      )
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
    resolveSlow(response())
    await screen.findByText('Pending: 0')
    expect(fetcher).toHaveBeenCalledTimes(3)
  })

  it('keeps the last summary after a temporary server failure', async () => {
    const fetcher = vi
      .fn()
      .mockResolvedValueOnce(response())
      .mockResolvedValueOnce(new Response('', { status: 503 }))
      .mockResolvedValueOnce(new Response('', { status: 403 }))
    setGeneratedFetch(fetcher)
    const view = render(ProjectSyncStatus, {
      props: { projectID: 7, projectUID, refreshGeneration: 0 },
    })
    await screen.findByText('Pending: 2')
    await view.rerender({ projectID: 7, projectUID, refreshGeneration: 1 })
    await screen.findByText('Sync status may be out of date.')
    expect(screen.getByText('Pending: 2')).not.toBeNull()
    await view.rerender({ projectID: 7, projectUID, refreshGeneration: 2 })
    await screen.findByText('Sync status unavailable. Refresh to retry.')
    expect(screen.queryByText('Pending: 2')).toBeNull()
  })

  it.each(['network', 408, 429])(
    'keeps the last summary after a transient %s failure',
    async (failure) => {
      const fetcher = vi.fn().mockResolvedValueOnce(response())
      if (failure === 'network') fetcher.mockRejectedValueOnce(new TypeError('network failure'))
      else fetcher.mockResolvedValueOnce(new Response('', { status: Number(failure) }))
      setGeneratedFetch(fetcher)
      const view = render(ProjectSyncStatus, {
        props: { projectID: 7, projectUID, refreshGeneration: 0 },
      })
      await screen.findByText('Pending: 2')
      await view.rerender({ projectID: 7, projectUID, refreshGeneration: 1 })
      await screen.findByText('Sync status may be out of date.')
      expect(screen.getByText('Pending: 2')).not.toBeNull()
    },
  )

  it('clears the prior summary while a denied response body is stalled, then retries', async () => {
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
    expect(screen.queryByText('Pending: 2')).toBeNull()
    expect(screen.getByText('Reading sync status…')).not.toBeNull()
    expect(
      (screen.getByRole('button', { name: 'Refresh sync' }) as HTMLButtonElement).disabled,
    ).toBe(true)
    closeBody()
    await screen.findByText('Sync status unavailable. Refresh to retry.')
    await view.rerender({ projectID: 7, projectUID, refreshGeneration: 2 })
    await screen.findByText('Pending: 2')
  })

  it('clears the prior summary when a denied response body fails to read', async () => {
    const body = new ReadableStream({
      start(controller) {
        controller.error(new TypeError('body read failed'))
      },
    })
    const fetcher = vi
      .fn()
      .mockResolvedValueOnce(response())
      .mockResolvedValueOnce(new Response(body, { status: 403 }))
    setGeneratedFetch(fetcher)
    const view = render(ProjectSyncStatus, {
      props: { projectID: 7, projectUID, refreshGeneration: 0 },
    })
    await screen.findByText('Pending: 2')
    await view.rerender({ projectID: 7, projectUID, refreshGeneration: 1 })
    await screen.findByText('Sync status unavailable. Refresh to retry.')
    expect(screen.queryByText('Pending: 2')).toBeNull()
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

  it('finishes a slow initial read across refresh cycles and refreshes once', async () => {
    let resolveSlow!: (value: Response) => void
    let slowSignal: AbortSignal | undefined
    const fetcher = vi
      .fn()
      .mockImplementationOnce((_url, init) => {
        slowSignal = init.signal
        return new Promise<Response>((resolve) => {
          resolveSlow = resolve
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
    await waitFor(() => expect(slowSignal).toBeDefined())
    await view.rerender({ projectID: 7, projectUID, refreshGeneration: 1 })
    await view.rerender({ projectID: 7, projectUID, refreshGeneration: 2 })
    expect(slowSignal?.aborted).toBe(false)
    expect(fetcher).toHaveBeenCalledTimes(1)
    resolveSlow(response())
    await screen.findByText('Pending: 0')
    expect(fetcher).toHaveBeenCalledTimes(2)
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
    expect(
      (screen.getByRole('button', { name: 'Refresh sync' }) as HTMLButtonElement).disabled,
    ).toBe(true)
    expect(signal?.aborted).toBe(false)
    hidden.mockReturnValue(true)
    resolveSlow(response())
    await new Promise((resolve) => setTimeout(resolve, 0))
    expect(screen.getByText('Pending: 2')).not.toBeNull()
    expect(fetcher).toHaveBeenCalledTimes(2)
    hidden.mockReturnValue(false)
    await view.rerender({ projectID: 7, projectUID, refreshGeneration: 2 })
    await waitFor(() => expect(fetcher).toHaveBeenCalledTimes(3))
    hidden.mockRestore()
  })

  it.each([403, 404, 'mismatch', 'malformed 403'])(
    'clears status after %s and retries on the next cycle',
    async (code) => {
      const fetcher = vi
        .fn()
        .mockResolvedValueOnce(response())
        .mockResolvedValueOnce(
          code === 'mismatch'
            ? response(9)
            : code === 'malformed 403'
              ? new Response('{', { status: 403, headers: { 'Content-Type': 'application/json' } })
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
