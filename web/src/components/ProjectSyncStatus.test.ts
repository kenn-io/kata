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
    await fireEvent.click(screen.getByRole('button', { name: 'Copy Bash/Zsh sync status command' }))
    expect(writeText).toHaveBeenCalledWith(command)
    await fireEvent.click(
      screen.getByRole('button', { name: 'Copy PowerShell sync status command' }),
    )
    expect(writeText).toHaveBeenCalledWith("kata federation status --project 'spoke project''s'")
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

  it.each(['stalled', '503'])(
    'keeps stale status after %s and retries on a later cycle',
    async (failure) => {
      let signal: AbortSignal | undefined
      const fetcher = vi
        .fn()
        .mockResolvedValueOnce(response())
        .mockImplementationOnce((_url, init) => {
          signal = init.signal
          return failure === 'stalled'
            ? new Promise<Response>(() => {})
            : Promise.resolve(new Response('', { status: 503 }))
        })
        .mockResolvedValueOnce(response())
      setGeneratedFetch(fetcher)
      const view = render(ProjectSyncStatus, {
        props: { projectID: 7, projectUID, refreshGeneration: 0 },
      })
      await screen.findByText('Pending: 2')
      vi.useFakeTimers()
      await view.rerender({ projectID: 7, projectUID, refreshGeneration: 1 })
      await vi.advanceTimersByTimeAsync(failure === 'stalled' ? 120_000 : 0)
      if (failure === 'stalled') expect(signal?.aborted).toBe(true)
      expect(screen.getByText('Sync status may be out of date.')).not.toBeNull()
      expect(screen.getByText('Pending: 2')).not.toBeNull()
      await view.rerender({ projectID: 7, projectUID, refreshGeneration: 2 })
      await vi.advanceTimersByTimeAsync(0)
      expect(fetcher).toHaveBeenCalledTimes(3)
      expect(screen.queryByText('Sync status may be out of date.')).toBeNull()
      vi.useRealTimers()
    },
  )

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

  it('keeps slow reads across refresh cycles and lets manual refresh replace one', async () => {
    let resolveSlow!: (value: Response) => void
    let resolveAborted!: (value: Response) => void
    let slowSignal: AbortSignal | undefined
    const fetcher = vi
      .fn()
      .mockResolvedValueOnce(response())
      .mockImplementationOnce(
        () =>
          new Promise<Response>((resolve) => {
            resolveSlow = resolve
          }),
      )
      .mockImplementationOnce((_url, init) => {
        slowSignal = init.signal
        return new Promise<Response>((resolve) => {
          resolveAborted = resolve
        })
      })
      .mockResolvedValueOnce(response())
    setGeneratedFetch(fetcher)
    const view = render(ProjectSyncStatus, {
      props: { projectID: 7, projectUID, refreshGeneration: 0 },
    })
    await screen.findByText('Pending: 2')
    await view.rerender({ projectID: 7, projectUID, refreshGeneration: 1 })
    await view.rerender({ projectID: 7, projectUID, refreshGeneration: 2 })
    await view.rerender({ projectID: 7, projectUID, refreshGeneration: 3 })
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
    await screen.findByText('Pending: 0')
    expect(screen.queryByRole('button', { name: /Copy .* sync status command/ })).toBeNull()
    await view.rerender({ projectID: 7, projectUID, refreshGeneration: 4 })
    expect(fetcher).toHaveBeenCalledTimes(3)
    await fireEvent.click(screen.getByRole('button', { name: 'Refresh sync' }))
    await screen.findByText('Pending: 2')
    expect(slowSignal?.aborted).toBe(true)
    resolveAborted(response(9))
    await new Promise((resolve) => setTimeout(resolve, 0))
    expect(screen.getByText('Pending: 2')).not.toBeNull()
  })

  it.each([403, 404, 'mismatch'])(
    'clears status after %s and waits for manual refresh',
    async (code) => {
      const fetcher = vi
        .fn()
        .mockResolvedValueOnce(response())
        .mockResolvedValueOnce(
          code === 'mismatch' ? response(9) : new Response('', { status: Number(code) }),
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
      expect(fetcher).toHaveBeenCalledTimes(2)
      await fireEvent.click(screen.getByRole('button', { name: 'Refresh sync' }))
      await screen.findByText('Pending: 2')
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
    expect(screen.queryByRole('button', { name: /Copy .* sync status command/ })).toBeNull()
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
