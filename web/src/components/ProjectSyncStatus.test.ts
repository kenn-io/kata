import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/svelte'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { setGeneratedFetch } from '../lib/api/client'
import ProjectSyncStatus from './ProjectSyncStatus.svelte'

const projectUID = '01J00000000000000000000002'
function response(projectID = 7, uid = projectUID, pending = 2, failed = true): Response {
  return Response.json({
    statuses: [
      {
        project_id: projectID,
        project_uid: uid,
        project_name: "spoke project’s'",
        role: 'replica',
        enabled: true,
        hub_url: 'https://daemon.example',
        pending_push_count: pending,
        last_successful_sync_at: '2026-10-01T12:00:00Z',
        credential_status: 'live',
        has_sync_error: failed,
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

async function openSync(name: RegExp = /Sync failed/) {
  await fireEvent.click(await screen.findByRole('button', { name }))
}

afterEach(() => {
  cleanup()
  vi.unstubAllGlobals()
  vi.useRealTimers()
  vi.restoreAllMocks()
})

describe('ProjectSyncStatus', () => {
  it('opens the failed sync chip to show waiting changes and a quoted recovery command', async () => {
    const writeText = vi.fn().mockResolvedValue(undefined)
    vi.stubGlobal('navigator', { ...navigator, clipboard: { writeText } })
    setGeneratedFetch(vi.fn().mockResolvedValueOnce(response()))
    render(ProjectSyncStatus, { props: { projectID: 7, projectUID } })
    await openSync()
    expect(screen.getByText('Sync failed · 2 changes waiting')).not.toBeNull()
    expect(screen.getByText('Syncs with daemon.example')).not.toBeNull()
    expect(
      screen.getByText('Connect the Kata CLI to this daemon with your own credential, then run:'),
    ).not.toBeNull()
    expect(
      screen.getByText('For a remote daemon, select it with its --daemon profile or KATA_SERVER.'),
    ).not.toBeNull()
    await fireEvent.click(screen.getByRole('button', { name: 'Copy PowerShell command' }))
    expect(writeText).toHaveBeenCalledWith("kata federation status --project 'spoke project’’s'''")
    expect(screen.getByText('1 generated · 1 reused · 1 without index')).not.toBeNull()
    expect(screen.getByText('3 of 32 embeddings, limited')).not.toBeNull()
  })

  it('says there are no changes to send when nothing is waiting', async () => {
    setGeneratedFetch(vi.fn().mockResolvedValueOnce(response(7, projectUID, 0, false)))
    render(ProjectSyncStatus, { props: { projectID: 7, projectUID } })
    await openSync(/^No changes to send$/)
    expect(screen.getByRole('dialog', { name: 'Project sync' }).textContent).toContain(
      'No changes to send',
    )
    expect(screen.queryByText(/Synced/)).toBeNull()
  })

  it('says how long ago the last sync finished', async () => {
    const result = await response().json()
    result.statuses[0].last_successful_sync_at = new Date(Date.now() - 12 * 60_000).toISOString()
    setGeneratedFetch(vi.fn().mockResolvedValueOnce(Response.json(result)))
    render(ProjectSyncStatus, { props: { projectID: 7, projectUID } })
    await openSync()
    expect(screen.getByText('12m ago')).not.toBeNull()
  })

  it('hides the chip for a local project', async () => {
    const fetcher = vi.fn().mockResolvedValue(Response.json({ statuses: [] }))
    setGeneratedFetch(fetcher)
    render(ProjectSyncStatus, { props: { projectID: 7, projectUID } })
    await waitFor(() => expect(fetcher).toHaveBeenCalledTimes(1))
    await waitFor(() => expect(screen.queryByRole('button')).toBeNull())
  })

  it('shows a disabled binding and its waiting changes', async () => {
    const result = await response(7, projectUID, 3, false).json()
    result.statuses[0].enabled = false
    setGeneratedFetch(vi.fn().mockResolvedValueOnce(Response.json(result)))
    render(ProjectSyncStatus, { props: { projectID: 7, projectUID } })
    await openSync(/^Sync disabled$/)
    expect(screen.getByText('Sync disabled · 3 changes waiting')).not.toBeNull()
  })

  it('keeps waiting changes visible when an open panel refreshes to disabled', async () => {
    const result = await response(7, projectUID, 3, false).json()
    result.statuses[0].enabled = false
    setGeneratedFetch(
      vi.fn().mockResolvedValueOnce(response()).mockResolvedValueOnce(Response.json(result)),
    )
    const view = render(ProjectSyncStatus, {
      props: { projectID: 7, projectUID, refreshGeneration: 0 },
    })
    await openSync()
    await view.rerender({ projectID: 7, projectUID, refreshGeneration: 1 })
    await screen.findByText('Sync disabled · 3 changes waiting')
    expect(screen.getByRole('button', { name: 'Sync disabled' })).not.toBeNull()
  })

  it('shows a neutral waiting count and a single command when both shells match', async () => {
    const result = await response().json()
    result.statuses[0].project_name = 'spoke-project'
    result.statuses[0].has_sync_error = false
    setGeneratedFetch(vi.fn().mockResolvedValueOnce(Response.json(result)))
    const view = render(ProjectSyncStatus, { props: { projectID: 7, projectUID } })
    const button = await screen.findByRole('button', { name: '2 to sync' })
    expect(button.className).toContain('kit-chip--tone-neutral')
    result.statuses[0].has_sync_error = true
    setGeneratedFetch(vi.fn().mockResolvedValueOnce(Response.json(result)))
    await view.rerender({ projectID: 7, projectUID, refreshGeneration: 1 })
    await openSync()
    expect(screen.getByRole('button', { name: 'Copy command' })).not.toBeNull()
    expect(screen.queryByText('PowerShell')).toBeNull()
  })

  it.each(['network', 'body', '503', 403, 'mismatch'])(
    'clears status after %s and retries on the next cycle',
    async (failure) => {
      const fetcher = vi.fn().mockResolvedValueOnce(response())
      if (failure === 'network') fetcher.mockRejectedValueOnce(new TypeError('network failure'))
      else if (failure === 'body')
        fetcher.mockResolvedValueOnce(
          new Response(
            new ReadableStream({
              start(controller) {
                controller.error(new TypeError('body read failed'))
              },
            }),
            { status: 200 },
          ),
        )
      else if (failure === 'mismatch') fetcher.mockResolvedValueOnce(response(9))
      else fetcher.mockResolvedValueOnce(new Response('', { status: Number(failure) }))
      setGeneratedFetch(fetcher.mockResolvedValueOnce(response()))
      const view = render(ProjectSyncStatus, {
        props: { projectID: 7, projectUID, refreshGeneration: 0 },
      })
      await openSync()
      await view.rerender({ projectID: 7, projectUID, refreshGeneration: 1 })
      await screen.findByText('Sync status unavailable. Refresh to try again.')
      expect(screen.queryByText('1 generated · 1 reused · 1 without index')).toBeNull()
      await view.rerender({ projectID: 7, projectUID, refreshGeneration: 2 })
      await screen.findByText('Sync failed · 2 changes waiting')
      expect(fetcher).toHaveBeenCalledTimes(3)
    },
  )

  it.each(['initial', 'background', 'manual'])(
    'coalesces cycles while a %s read is in flight',
    async (kind) => {
      let finish!: (result: Response) => void
      const fetcher = vi.fn()
      if (kind !== 'initial') fetcher.mockResolvedValueOnce(response())
      fetcher
        .mockImplementationOnce(
          () =>
            new Promise<Response>((resolve) => {
              finish = resolve
            }),
        )
        .mockResolvedValueOnce(response(7, projectUID, 0, false))
      setGeneratedFetch(fetcher)
      const view = render(ProjectSyncStatus, {
        props: { projectID: 7, projectUID, refreshGeneration: 0 },
      })
      if (kind !== 'initial') {
        await openSync()
        if (kind === 'manual')
          await fireEvent.click(screen.getByRole('button', { name: 'Refresh' }))
        else await view.rerender({ projectID: 7, projectUID, refreshGeneration: 1 })
      }
      await waitFor(() => expect(finish).toBeDefined())
      await view.rerender({ projectID: 7, projectUID, refreshGeneration: 3 })
      await view.rerender({ projectID: 7, projectUID, refreshGeneration: 4 })
      finish(response())
      await screen.findByRole('button', { name: 'No changes to send' })
      expect(fetcher).toHaveBeenCalledTimes(kind === 'initial' ? 2 : 3)
    },
  )

  it('keeps the unavailable chip during a background retry', async () => {
    const fetcher = vi
      .fn()
      .mockResolvedValueOnce(new Response('', { status: 503 }))
      .mockImplementationOnce(() => new Promise(() => {}))
    setGeneratedFetch(fetcher)
    const view = render(ProjectSyncStatus, {
      props: { projectID: 7, projectUID, refreshGeneration: 0 },
    })
    await openSync(/Sync status unavailable/)
    await view.rerender({ projectID: 7, projectUID, refreshGeneration: 1 })
    expect(screen.getByText('Sync status unavailable. Refresh to try again.')).not.toBeNull()
  })

  it('replaces a stalled read on manual refresh and ignores its late result', async () => {
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
      .mockResolvedValueOnce(response(7, projectUID, 0, false))
    setGeneratedFetch(fetcher)
    const view = render(ProjectSyncStatus, {
      props: { projectID: 7, projectUID, refreshGeneration: 0 },
    })
    await openSync()
    await view.rerender({ projectID: 7, projectUID, refreshGeneration: 1 })
    await fireEvent.click(screen.getByRole('button', { name: 'Refresh' }))
    await screen.findByRole('button', { name: 'No changes to send' })
    expect(oldSignal?.aborted).toBe(true)
    resolveOld(response())
    await new Promise((resolve) => setTimeout(resolve, 0))
    expect(screen.getByRole('button', { name: 'No changes to send' })).not.toBeNull()
  })

  it('clears sensitive details when a denied response body stalls, then retries', async () => {
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
    await openSync()
    await view.rerender({ projectID: 7, projectUID, refreshGeneration: 1 })
    await screen.findByText('Loading sync status…')
    expect(screen.queryByText('1 generated · 1 reused · 1 without index')).toBeNull()
    expect((screen.getByRole('button', { name: 'Refresh' }) as HTMLButtonElement).disabled).toBe(
      true,
    )
    closeBody()
    await screen.findByText('Sync status unavailable. Refresh to try again.')
    await fireEvent.click(screen.getByRole('button', { name: 'Refresh' }))
    await screen.findByText('Sync failed · 2 changes waiting')
  })

  it('waits until visible for its initial read', async () => {
    const hidden = vi.spyOn(document, 'hidden', 'get').mockReturnValue(true)
    const fetcher = vi.fn(async () => response())
    setGeneratedFetch(fetcher)
    const view = render(ProjectSyncStatus, {
      props: { projectID: 7, projectUID, refreshGeneration: 0 },
    })
    expect(fetcher).not.toHaveBeenCalled()
    expect(screen.queryByRole('button')).toBeNull()
    hidden.mockReturnValue(false)
    await view.rerender({ projectID: 7, projectUID, refreshGeneration: 1 })
    await screen.findByRole('button', { name: 'Sync failed' })
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
    expect(screen.getByRole('button', { name: 'Sync failed' })).not.toBeNull()
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
    await waitFor(() => expect(fetcher).toHaveBeenCalledTimes(1))
    await view.rerender({ projectID: 7, projectUID, refreshGeneration: 1 })
    await screen.findByRole('button', { name: 'Sync failed' })
  })

  it('lets a read finish while hidden and refreshes when visible again', async () => {
    let finish!: (value: Response) => void
    let signal: AbortSignal | undefined
    const hidden = vi.spyOn(document, 'hidden', 'get').mockReturnValue(false)
    const fetcher = vi
      .fn()
      .mockResolvedValueOnce(response())
      .mockImplementationOnce((_url, init) => {
        signal = init.signal
        return new Promise<Response>((resolve) => {
          finish = resolve
        })
      })
      .mockResolvedValueOnce(response())
    setGeneratedFetch(fetcher)
    const view = render(ProjectSyncStatus, {
      props: { projectID: 7, projectUID, refreshGeneration: 0 },
    })
    await screen.findByRole('button', { name: 'Sync failed' })
    await view.rerender({ projectID: 7, projectUID, refreshGeneration: 1 })
    await view.rerender({ projectID: 7, projectUID, refreshGeneration: 2 })
    hidden.mockReturnValue(true)
    expect(signal?.aborted).toBe(false)
    finish(response(7, projectUID, 0, false))
    await screen.findByRole('button', { name: 'No changes to send' })
    expect(fetcher).toHaveBeenCalledTimes(2)
    hidden.mockReturnValue(false)
    await view.rerender({ projectID: 7, projectUID, refreshGeneration: 3 })
    await waitFor(() => expect(fetcher).toHaveBeenCalledTimes(3))
  })

  it('reads only the selected project without private errors', async () => {
    const fetcher = vi.fn<typeof fetch>(async () => response())
    setGeneratedFetch(fetcher)
    render(ProjectSyncStatus, { props: { projectID: 7, projectUID } })
    await openSync()
    const url = new URL(String(fetcher.mock.calls[0]?.[0]), 'https://daemon.example')
    expect(url.pathname).toBe('/api/v1/projects/7/federation/status')
    expect(url.searchParams.get('compact')).toBe('true')
    expect(url.searchParams.get('omit_errors')).toBe('true')
    expect(screen.getByText('example-model, 2 dimensions')).not.toBeNull()
  })

  it('rejects stale project responses even when transport ignores cancellation', async () => {
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
    await openSync()
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

  it('closes on Escape and returns focus to the chip', async () => {
    setGeneratedFetch(vi.fn().mockResolvedValueOnce(response()))
    render(ProjectSyncStatus, { props: { projectID: 7, projectUID } })
    await openSync()
    expect(document.activeElement).toBe(screen.getByRole('dialog', { name: 'Project sync' }))
    await fireEvent.keyDown(document, { key: 'Escape' })
    expect(screen.queryByRole('dialog', { name: 'Project sync' })).toBeNull()
    expect(document.activeElement).toBe(screen.getByRole('button', { name: 'Sync failed' }))
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
