import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/svelte'
import { afterEach, describe, expect, it } from 'vitest'

import { setGeneratedFetch } from '../lib/api/client'
import type { UIProject } from '../lib/api/generated'
import type { MutationContext } from '../lib/mutations/controller'
import AccessManagement from './AccessManagement.svelte'

const projects = [
  { project: { id: 7, uid: 'project-uid', name: 'example-project' } },
] as unknown as UIProject[]

function serve(policy: { visibility: string; team_uids: string[] }) {
  const writes: unknown[] = []
  setGeneratedFetch(async (input, init) => {
    const path = new URL(String(input)).pathname
    if (init?.method === 'PUT' && path === '/api/v1/projects/7/access') {
      writes.push(JSON.parse(String(init.body)))
      return Response.json({ policy: { project_uid: 'project-uid', revision: 5, ...policy } })
    }
    if (path === '/api/v1/teams')
      return Response.json({ teams: [{ uid: 'team-one', name: 'example-team', revision: 1 }] })
    if (path === '/api/v1/projects/7/access')
      return Response.json({ policy: { project_uid: 'project-uid', revision: 4, ...policy } })
    throw new Error(`Unexpected request: ${init?.method ?? 'GET'} ${path}`)
  })
  return writes
}

function renderAccess() {
  return render(AccessManagement, {
    props: {
      projects,
      pending: false,
      onMutation: async (
        _draft: unknown,
        mutate: (context: MutationContext) => Promise<unknown>,
      ) => {
        await mutate({ headers: {} } as MutationContext)
        return true
      },
    },
  })
}

describe('AccessManagement', () => {
  afterEach(cleanup)

  it('shows Teams and Project visibility as two sections', async () => {
    serve({ visibility: 'all', team_uids: [] })
    renderAccess()

    expect(screen.getByRole('heading', { name: 'Teams' })).not.toBeNull()
    expect(screen.getByRole('heading', { name: 'Project visibility' })).not.toBeNull()
    expect(screen.getByText('Server owners see every project.')).not.toBeNull()
    expect(await screen.findByText('example-team')).not.toBeNull()
    expect(await screen.findByRole('combobox', { name: 'Project: example-project' })).not.toBeNull()
    expect(screen.queryByRole('button', { name: 'Save visibility' })).toBeNull()
  })

  it('saves visibility as it changes, waiting for a team pick under Selected teams', async () => {
    const writes = serve({ visibility: 'all', team_uids: [] })
    renderAccess()

    const visibility = await screen.findByRole('combobox', { name: 'Visibility: Everyone' })
    await waitFor(() => expect((visibility as HTMLButtonElement).disabled).toBe(false))
    await fireEvent.click(visibility)
    await fireEvent.click(screen.getByRole('option', { name: 'Selected teams' }))
    expect(screen.getByText('Pick a team to save.')).not.toBeNull()
    expect(writes).toHaveLength(0)

    await fireEvent.click(screen.getByRole('checkbox', { name: 'example-team' }))
    await waitFor(() => expect(writes).toHaveLength(1))
    expect(writes[0]).toEqual({ visibility: 'teams', team_uids: ['team-one'], revision: 4 })
  })
})
