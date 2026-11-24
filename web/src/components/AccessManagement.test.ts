import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/svelte'
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
  let revision = 4
  setGeneratedFetch(async (input, init) => {
    const path = new URL(String(input)).pathname
    if (init?.method === 'PUT' && path === '/api/v1/projects/7/access') {
      const draft = JSON.parse(String(init.body))
      writes.push(draft)
      policy = { visibility: draft.visibility, team_uids: draft.team_uids }
      return Response.json({
        policy: { project_uid: 'project-uid', revision: ++revision, ...policy },
      })
    }
    if (path === '/api/v1/teams')
      return Response.json({ teams: [{ uid: 'team-one', name: 'example-team', revision: 1 }] })
    if (path === '/api/v1/projects/7/access')
      return Response.json({ policy: { project_uid: 'project-uid', revision, ...policy } })
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

  it('saves removal of the last selected team', async () => {
    const writes = serve({ visibility: 'teams', team_uids: ['team-one'] })
    renderAccess()

    const team = await screen.findByRole('checkbox', { name: 'example-team' })
    await waitFor(() => expect((team as HTMLInputElement).disabled).toBe(false))
    await fireEvent.click(team)
    await waitFor(() =>
      expect(writes).toEqual([{ visibility: 'teams', team_uids: [], revision: 4 }]),
    )
    expect(await screen.findByText('No teams have access. Server owners still do.')).not.toBeNull()
  })

  it('keeps read errors independent when other reads succeed', async () => {
    setGeneratedFetch(async (input) => {
      const path = new URL(String(input)).pathname
      if (path === '/api/v1/teams')
        return Response.json({ teams: [{ uid: 'team-one', name: 'example-team', revision: 1 }] })
      if (path === '/api/v1/teams/team-one') return new Response(null, { status: 500 })
      if (path === '/api/v1/projects/7/access') return new Response(null, { status: 500 })
      throw new Error(`Unexpected request: ${path}`)
    })
    renderAccess()
    await screen.findByText("Couldn't load project visibility.")
    await fireEvent.click(await screen.findByText('example-team'))
    await screen.findByText("Couldn't load team members.")
    expect(
      screen
        .getAllByRole('alert')
        .map((alert) => within(alert).getByText(/^Couldn't load/).textContent),
    ).toEqual(["Couldn't load team members.", "Couldn't load project visibility."])
    await fireEvent.click(screen.getByRole('button', { name: 'Refresh teams' }))
    await waitFor(() => expect(screen.getByText("Couldn't load team members.")).not.toBeNull())
    expect(screen.getByText("Couldn't load project visibility.")).not.toBeNull()
  })

  it('rejects whitespace-only team and member names with browser validation', async () => {
    setGeneratedFetch(async (input) => {
      const path = new URL(String(input)).pathname
      if (path === '/api/v1/teams/team-one') return Response.json({ members: [] })
      if (path === '/api/v1/teams')
        return Response.json({ teams: [{ uid: 'team-one', name: 'example-team', revision: 1 }] })
      return Response.json({ policy: { revision: 4, visibility: 'all', team_uids: [] } })
    })
    renderAccess()
    await fireEvent.click(await screen.findByText('example-team'))
    await fireEvent.click(await screen.findByRole('button', { name: 'Add member' }))
    const member = screen.getByLabelText('Member account') as HTMLInputElement
    await fireEvent.input(member, { target: { value: '   ' } })
    expect(member.checkValidity()).toBe(false)
    await fireEvent.click(screen.getByRole('button', { name: 'New team' }))
    const team = screen.getByLabelText('New team name') as HTMLInputElement
    await fireEvent.input(team, { target: { value: '   ' } })
    expect(team.checkValidity()).toBe(false)
  })

  it('keeps visibility disabled when the policy reload after a failed write fails', async () => {
    let reads = 0
    let finishReload!: (response: Response) => void
    setGeneratedFetch(async (input, init) => {
      const path = new URL(String(input)).pathname
      if (path === '/api/v1/teams')
        return Response.json({ teams: [{ uid: 'team-one', name: 'example-team', revision: 1 }] })
      if (init?.method === 'PUT') return new Response(null, { status: 409 })
      if (path === '/api/v1/projects/7/access') {
        if (++reads === 1)
          return Response.json({ policy: { revision: 4, visibility: 'all', team_uids: [] } })
        return new Promise<Response>((resolve) => (finishReload = resolve))
      }
      throw new Error(`Unexpected request: ${path}`)
    })
    render(AccessManagement, {
      props: {
        projects,
        pending: false,
        onMutation: async (
          _draft: unknown,
          mutate: (context: MutationContext) => Promise<unknown>,
        ) => {
          await mutate({ headers: {} } as MutationContext)
          return false
        },
      },
    })
    const visibility = await screen.findByRole('combobox', { name: 'Visibility: Everyone' })
    await waitFor(() => expect((visibility as HTMLButtonElement).disabled).toBe(false))
    await fireEvent.click(visibility)
    await fireEvent.click(screen.getByRole('option', { name: 'Selected teams' }))
    await fireEvent.click(screen.getByRole('checkbox', { name: 'example-team' }))
    await waitFor(() => expect(reads).toBe(2))
    expect(
      (screen.getByRole('combobox', { name: 'Visibility: Selected teams' }) as HTMLButtonElement)
        .disabled,
    ).toBe(true)
    finishReload(new Response(null, { status: 500 }))
    await screen.findByText("Couldn't load project visibility.")
    expect(
      (screen.getByRole('combobox', { name: 'Visibility: Selected teams' }) as HTMLButtonElement)
        .disabled,
    ).toBe(true)
  })
})
