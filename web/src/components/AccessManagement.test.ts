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
        const result = (await mutate({ headers: {} } as MutationContext)) as { status: number }
        return result.status < 300
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

  it.each(['retry', 'empty draft', 'reload failure'])(
    'handles a visibility conflict: %s',
    async (scenario) => {
      let reads = 0
      const writes: unknown[] = []
      let finishReload!: (response: Response) => void
      setGeneratedFetch(async (input, init) => {
        const path = new URL(String(input)).pathname
        if (path === '/api/v1/teams')
          return Response.json({
            teams: [
              { uid: 'team-one', name: 'example-team', revision: 1 },
              { uid: 'team-two', name: 'second-team', revision: 1 },
            ],
          })
        if (init?.method === 'PUT') {
          writes.push(JSON.parse(String(init.body)))
          return writes.length === 1
            ? Response.json({ code: 'revision_conflict' }, { status: 409 })
            : Response.json({
                policy: { revision: 6, visibility: 'teams', team_uids: ['team-one'] },
              })
        }
        if (path === '/api/v1/projects/7/access') {
          reads++
          if (reads === 2 && scenario === 'reload failure')
            return new Promise<Response>((resolve) => (finishReload = resolve))
          return Response.json({
            policy: { revision: reads === 1 ? 4 : 5, visibility: 'all', team_uids: [] },
          })
        }
        throw new Error(`Unexpected request: ${path}`)
      })
      render(AccessManagement, {
        props: {
          projects: [
            ...projects,
            { project: { id: 8, uid: 'project-two', name: 'second-project' } } as UIProject,
          ],
          pending: false,
          onMutation: async (
            _draft: unknown,
            mutate: (context: MutationContext) => Promise<unknown>,
          ) => {
            const result = (await mutate({ headers: {} } as MutationContext)) as { status: number }
            return result.status < 300
          },
        },
      })
      const visibility = await screen.findByRole('combobox', { name: 'Visibility: Everyone' })
      await waitFor(() => expect((visibility as HTMLButtonElement).disabled).toBe(false))
      await fireEvent.click(visibility)
      await fireEvent.click(screen.getByRole('option', { name: 'Selected teams' }))
      expect(screen.getByText('Pick a team to save.')).not.toBeNull()
      expect(writes).toHaveLength(0)
      const team = screen.getByRole('checkbox', { name: 'example-team' }) as HTMLInputElement
      await fireEvent.click(team)
      await waitFor(() => expect(reads).toBe(2))
      expect(writes[0]).toEqual({ visibility: 'teams', team_uids: ['team-one'], revision: 4 })
      if (scenario === 'reload failure') {
        expect(
          (
            screen.getByRole('combobox', {
              name: 'Visibility: Selected teams',
            }) as HTMLButtonElement
          ).disabled,
        ).toBe(true)
        finishReload(new Response(null, { status: 500 }))
        await screen.findByText("Couldn't load project visibility.")
        expect(
          (
            screen.getByRole('combobox', {
              name: 'Visibility: Selected teams',
            }) as HTMLButtonElement
          ).disabled,
        ).toBe(true)
        await fireEvent.click(screen.getByRole('button', { name: 'Refresh teams' }))
        const retry = await screen.findByRole('button', { name: 'Retry saving visibility' })
        await waitFor(() => expect((retry as HTMLButtonElement).disabled).toBe(false))
        expect(
          (screen.getByRole('checkbox', { name: 'example-team' }) as HTMLInputElement).checked,
        ).toBe(true)
        await fireEvent.click(retry)
        await waitFor(() => expect(writes).toHaveLength(2))
        expect(writes[1]).toEqual({ visibility: 'teams', team_uids: ['team-one'], revision: 5 })
        return
      }
      const retry = (await screen.findByRole('button', {
        name: 'Retry saving visibility',
      })) as HTMLButtonElement
      await waitFor(() => expect(retry.disabled).toBe(false))
      expect(team.checked).toBe(true)
      if (scenario === 'empty draft') {
        await fireEvent.click(team)
        expect(retry.disabled).toBe(true)
        await fireEvent.click(retry)
        expect(writes).toHaveLength(1)
      } else {
        await fireEvent.click(retry)
        await waitFor(() => expect(writes).toHaveLength(2))
        expect(writes[1]).toEqual({ visibility: 'teams', team_uids: ['team-one'], revision: 5 })
      }
    },
  )

  it('retries an owner-only draft after a visibility conflict', async () => {
    let reads = 0
    const writes: unknown[] = []
    setGeneratedFetch(async (input, init) => {
      const path = new URL(String(input)).pathname
      if (path === '/api/v1/teams')
        return Response.json({ teams: [{ uid: 'team-one', name: 'example-team', revision: 1 }] })
      if (init?.method === 'PUT') {
        writes.push(JSON.parse(String(init.body)))
        return writes.length === 1
          ? Response.json({ code: 'revision_conflict' }, { status: 409 })
          : Response.json({
              policy: {
                project_uid: 'project-uid',
                revision: 6,
                visibility: 'teams',
                team_uids: [],
              },
            })
      }
      if (path === '/api/v1/projects/7/access') {
        reads++
        return Response.json({
          policy: {
            project_uid: 'project-uid',
            revision: reads === 1 ? 4 : 5,
            visibility: reads === 1 ? 'teams' : 'all',
            team_uids: reads === 1 ? ['team-one'] : [],
          },
        })
      }
      throw new Error(`Unexpected request: ${init?.method ?? 'GET'} ${path}`)
    })
    renderAccess()

    const team = (await screen.findByRole('checkbox', { name: 'example-team' })) as HTMLInputElement
    await fireEvent.click(team)
    await waitFor(() => expect(writes).toHaveLength(1))
    expect(writes[0]).toEqual({ visibility: 'teams', team_uids: [], revision: 4 })

    const retry = (await screen.findByRole('button', {
      name: 'Retry saving visibility',
    })) as HTMLButtonElement
    await waitFor(() => expect(retry.disabled).toBe(false))
    await fireEvent.click(retry)
    await waitFor(() => expect(writes).toHaveLength(2))
    expect(writes[1]).toEqual({ visibility: 'teams', team_uids: [], revision: 5 })
  })

  it('drops a deleted team from the retained visibility draft on refresh', async () => {
    let teamReads = 0
    let policyReads = 0
    const writes: unknown[] = []
    setGeneratedFetch(async (input, init) => {
      const path = new URL(String(input)).pathname
      if (path === '/api/v1/teams') {
        teamReads++
        return Response.json({
          teams:
            teamReads === 1
              ? [
                  { uid: 'team-one', name: 'example-team', revision: 1 },
                  { uid: 'team-two', name: 'second-team', revision: 1 },
                ]
              : [{ uid: 'team-two', name: 'second-team', revision: 2 }],
        })
      }
      if (init?.method === 'PUT') {
        writes.push(JSON.parse(String(init.body)))
        return writes.length === 1
          ? Response.json({ code: 'revision_conflict' }, { status: 409 })
          : Response.json({
              policy: {
                project_uid: 'project-uid',
                revision: 7,
                visibility: 'teams',
                team_uids: ['team-two'],
              },
            })
      }
      if (path === '/api/v1/projects/7/access') {
        policyReads++
        return Response.json({
          policy: {
            project_uid: 'project-uid',
            revision: policyReads,
            visibility: 'teams',
            team_uids: policyReads === 1 ? ['team-one'] : ['team-two'],
          },
        })
      }
      throw new Error(`Unexpected request: ${init?.method ?? 'GET'} ${path}`)
    })
    renderAccess()

    const team = (await screen.findByRole('checkbox', { name: 'second-team' })) as HTMLInputElement
    await fireEvent.click(team)
    await waitFor(() => expect(writes).toHaveLength(1))
    expect(writes[0]).toEqual({
      visibility: 'teams',
      team_uids: ['team-one', 'team-two'],
      revision: 1,
    })

    await screen.findByRole('button', { name: 'Retry saving visibility' })
    await fireEvent.click(screen.getByRole('button', { name: 'Refresh teams' }))
    await waitFor(() => expect(screen.queryByRole('checkbox', { name: 'example-team' })).toBeNull())
    expect(
      (screen.getByRole('checkbox', { name: 'second-team' }) as HTMLInputElement).checked,
    ).toBe(true)

    await fireEvent.click(screen.getByRole('button', { name: 'Retry saving visibility' }))
    await waitFor(() => expect(writes).toHaveLength(2))
    expect(writes[1]).toEqual({ visibility: 'teams', team_uids: ['team-two'], revision: 3 })
  })

  it('creates teams and adds members using visible submit buttons', async () => {
    const requests: string[] = []
    setGeneratedFetch(async (input, init) => {
      const path = new URL(String(input)).pathname
      if (init?.method === 'POST' || init?.method === 'PUT') requests.push(path)
      if (path === '/api/v1/teams/team-one') return Response.json({ members: [] })
      if (path === '/api/v1/teams')
        return Response.json({ teams: [{ uid: 'team-one', name: 'example-team', revision: 1 }] })
      return Response.json({ policy: { revision: 4, visibility: 'all', team_uids: [] } })
    })
    renderAccess()
    await fireEvent.click(await screen.findByText('example-team'))
    await fireEvent.click(await screen.findByRole('button', { name: 'Add member' }))
    await fireEvent.input(screen.getByLabelText('Member account'), { target: { value: 'user-a' } })
    await fireEvent.click(screen.getByRole('button', { name: 'Add member' }))
    await waitFor(() => expect(requests).toContain('/api/v1/teams/team-one/members/user-a'))
    await fireEvent.click(screen.getByRole('button', { name: 'New team' }))
    await fireEvent.input(screen.getByLabelText('New team name'), { target: { value: 'new-team' } })
    await fireEvent.click(screen.getByRole('button', { name: 'Create team' }))
    await waitFor(() => expect(requests).toContain('/api/v1/teams'))
  })
})
