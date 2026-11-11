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
        project_name: "spoke project's",
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
afterEach(() => {
  cleanup()
  vi.unstubAllGlobals()
})
describe('ProjectSyncStatus', () => {
  it('copies a quoted status command without showing the private error', async () => {
    const writeText = vi.fn().mockResolvedValue(undefined)
    vi.stubGlobal('navigator', { ...navigator, clipboard: { writeText } })
    setGeneratedFetch(vi.fn(async () => response()))
    render(ProjectSyncStatus, { props: { projectID: 7, projectUID } })
    const command = "kata federation status --project 'spoke project'\\''s'"
    await screen.findByText(command)
    await fireEvent.click(screen.getByRole('button', { name: 'Copy sync status command' }))
    expect(writeText).toHaveBeenCalledWith(command)
    expect(screen.queryByText('private-token-never-display')).toBeNull()
  })

  it('refreshes after return when an earlier read is still pending', async () => {
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
      .mockResolvedValueOnce(response())
    setGeneratedFetch(fetcher)
    const view = render(ProjectSyncStatus, {
      props: { projectID: 7, projectUID, refreshGeneration: 0, visibilityGeneration: 0 },
    })
    await screen.findByText('Pending: 2')
    await view.rerender({ projectID: 7, projectUID, refreshGeneration: 1, visibilityGeneration: 0 })
    expect(fetcher).toHaveBeenCalledTimes(2)
    await view.rerender({ projectID: 7, projectUID, refreshGeneration: 1, visibilityGeneration: 1 })
    expect(fetcher).toHaveBeenCalledTimes(2)
    resolveSlow(response())
    await waitFor(() => expect(fetcher).toHaveBeenCalledTimes(3))
  })

  it('marks a stalled read stale and retries on a later cycle', async () => {
    let signal: AbortSignal | undefined
    const fetcher = vi
      .fn()
      .mockResolvedValueOnce(response())
      .mockImplementationOnce((_url, init) => {
        signal = init.signal
        return new Promise<Response>(() => {})
      })
      .mockResolvedValueOnce(response())
    setGeneratedFetch(fetcher)
    const view = render(ProjectSyncStatus, {
      props: { projectID: 7, projectUID, refreshGeneration: 0 },
    })
    await screen.findByText('Pending: 2')
    vi.useFakeTimers()
    await view.rerender({ projectID: 7, projectUID, refreshGeneration: 1 })
    await vi.advanceTimersByTimeAsync(120_000)
    expect(signal?.aborted).toBe(true)
    expect(screen.getByText('Sync status may be out of date.')).not.toBeNull()
    await view.rerender({ projectID: 7, projectUID, refreshGeneration: 2 })
    await vi.advanceTimersByTimeAsync(0)
    expect(fetcher).toHaveBeenCalledTimes(3)
    expect(screen.queryByText('Sync status may be out of date.')).toBeNull()
    vi.useRealTimers()
  })

  it('accepts a slow read after multiple refresh cycles', async () => {
    let resolveSlow!: (value: Response) => void
    let signal: AbortSignal | undefined
    const fetcher = vi
      .fn()
      .mockResolvedValueOnce(response())
      .mockImplementationOnce((_url, init) => {
        signal = init.signal
        return new Promise<Response>((resolve) => {
          resolveSlow = resolve
        })
      })
    setGeneratedFetch(fetcher)
    const view = render(ProjectSyncStatus, {
      props: { projectID: 7, projectUID, refreshGeneration: 0 },
    })
    await screen.findByText('Pending: 2')
    await view.rerender({ projectID: 7, projectUID, refreshGeneration: 1 })
    await waitFor(() => expect(signal).toBeDefined())
    vi.useFakeTimers()
    await vi.advanceTimersByTimeAsync(30_000)
    await view.rerender({ projectID: 7, projectUID, refreshGeneration: 2 })
    await vi.advanceTimersByTimeAsync(35_000)
    await view.rerender({ projectID: 7, projectUID, refreshGeneration: 3 })
    expect(signal?.aborted).toBe(false)
    expect(fetcher).toHaveBeenCalledTimes(2)
    resolveSlow(
      Response.json({
        statuses: [
          {
            project_id: 7,
            project_uid: projectUID,
            enabled: true,
            role: 'replica',
            pending_push_count: 0,
          },
        ],
      }),
    )
    await vi.advanceTimersByTimeAsync(0)
    expect(screen.getByText('Pending: 0')).not.toBeNull()
    vi.useRealTimers()
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

  it('keeps a slow read across refresh cycles and lets manual refresh replace it', async () => {
    let resolveSlow!: (value: Response) => void
    let slowSignal: AbortSignal | undefined
    const fetcher = vi
      .fn()
      .mockResolvedValueOnce(response())
      .mockImplementationOnce((_url, init) => {
        slowSignal = init.signal
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
    await waitFor(() => expect(slowSignal).toBeDefined())
    vi.useFakeTimers()
    await vi.advanceTimersByTimeAsync(30_000)
    await view.rerender({ projectID: 7, projectUID, refreshGeneration: 2 })
    await vi.advanceTimersByTimeAsync(30_000)
    await view.rerender({ projectID: 7, projectUID, refreshGeneration: 3 })
    await vi.advanceTimersByTimeAsync(5_000)
    vi.useRealTimers()
    expect(fetcher).toHaveBeenCalledTimes(2)
    expect(slowSignal?.aborted).toBe(false)
    await fireEvent.click(screen.getByRole('button', { name: 'Refresh sync' }))
    await waitFor(() => expect(fetcher).toHaveBeenCalledTimes(3))
    expect(slowSignal?.aborted).toBe(true)
    resolveSlow(response(9))
    await screen.findByText('Pending: 2')
  })

  it('preserves a stale summary after a temporary failure and recovers', async () => {
    const fetcher = vi
      .fn()
      .mockResolvedValueOnce(response())
      .mockResolvedValueOnce(new Response('', { status: 503 }))
      .mockResolvedValueOnce(response())
    setGeneratedFetch(fetcher)
    const view = render(ProjectSyncStatus, {
      props: { projectID: 7, projectUID, refreshGeneration: 0 },
    })
    await screen.findByText('Pending: 2')
    await view.rerender({ projectID: 7, projectUID, refreshGeneration: 1 })
    await screen.findByText('Sync status may be out of date.')
    expect(screen.getByText('Pending: 2')).not.toBeNull()
    await view.rerender({ projectID: 7, projectUID, refreshGeneration: 2 })
    await waitFor(() => expect(screen.queryByText('Sync status may be out of date.')).toBeNull())
  })

  it.each([403, 404])('clears status after %i and waits for manual refresh', async (code) => {
    const fetcher = vi
      .fn()
      .mockResolvedValueOnce(response())
      .mockResolvedValueOnce(new Response('', { status: code }))
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
    expect(fetcher).toHaveBeenCalledTimes(2)
    await fireEvent.click(screen.getByRole('button', { name: 'Refresh sync' }))
    await screen.findByText('Pending: 2')
  })

  it('clears obsolete status after a mismatched identity', async () => {
    const fetcher = vi.fn().mockResolvedValueOnce(response()).mockResolvedValueOnce(response(9))
    setGeneratedFetch(fetcher)
    const view = render(ProjectSyncStatus, {
      props: { projectID: 7, projectUID, refreshGeneration: 0 },
    })
    await screen.findByText('Pending: 2')
    await view.rerender({ projectID: 7, projectUID, refreshGeneration: 1 })
    await screen.findByText('Sync status unavailable. Refresh to retry.')
    expect(screen.queryByText('Pending: 2')).toBeNull()
  })

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

  it('reads only the selected project and shows bounded retained counts without private errors', async () => {
    const fetcher = vi.fn<typeof fetch>(async () => response())
    setGeneratedFetch(fetcher)
    render(ProjectSyncStatus, { props: { projectID: 7, projectUID } })
    await screen.findByText('Reused: 1')
    expect(String(fetcher.mock.calls[0]?.[0])).toContain('/projects/7/federation/status')
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

  it('clears a prior status when access is revoked and recovers on explicit refresh', async () => {
    const fetcher = vi
      .fn()
      .mockResolvedValueOnce(response())
      .mockResolvedValueOnce(new Response('', { status: 403 }))
      .mockResolvedValueOnce(response())
    setGeneratedFetch(fetcher)
    render(ProjectSyncStatus, { props: { projectID: 7, projectUID } })
    await screen.findByText('Reused: 1')
    await fireEvent.click(screen.getByRole('button', { name: 'Refresh sync' }))
    await screen.findByText('Sync status unavailable. Refresh to retry.')
    expect(screen.queryByText('Reused: 1')).toBeNull()
    await fireEvent.click(screen.getByRole('button', { name: 'Refresh sync' }))
    await screen.findByText('Reused: 1')
  })

  it('rejects mismatched identities and aborts when authority removes the view', async () => {
    let signal: AbortSignal | undefined
    setGeneratedFetch(
      vi.fn(async (_url, init) => {
        signal = init?.signal ?? undefined
        return response(9)
      }),
    )
    const view = render(ProjectSyncStatus, { props: { projectID: 7, projectUID } })
    await screen.findByText('Sync status unavailable. Refresh to retry.')
    view.unmount()
    expect(signal?.aborted).toBe(true)
  })
})
