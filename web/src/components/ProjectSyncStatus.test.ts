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
