<script lang="ts">
  import { Checkbox } from '@kenn-io/kit-ui'
  import {
    addTeamMember,
    createTeam,
    deleteTeam,
    getProjectAccess,
    listTeams,
    removeTeamMember,
    setProjectAccess,
    showTeam,
    type ProjectAccessPolicy,
    type Team,
    type UIProject,
  } from '../lib/api/generated'
  import type { MutationContext, MutationResult } from '../lib/mutations/controller'

  interface Props {
    projects: readonly UIProject[]
    pending: boolean
    message?: string | undefined
    onMutation: (
      draft: unknown,
      mutate: (context: MutationContext) => Promise<MutationResult>,
    ) => Promise<boolean>
  }
  let { projects, pending, message, onMutation }: Props = $props()
  let teams = $state<Team[]>([])
  let name = $state('')
  let loading = $state(true)
  let error = $state<string>()
  let busy = $state(false)
  let teamUID = $state('')
  let members = $state<string[]>([])
  let member = $state('')
  let teamLoading = $state(false)
  let projectID = $state('')
  let policy = $state<ProjectAccessPolicy>()
  let visibility = $state<'all' | 'teams'>('all')
  let selectedTeams = $state<string[]>([])
  let policyLoading = $state(false)
  const controlID = $props.id()
  const lifetime = new AbortController()
  let teamAbort: AbortController | undefined
  let policyAbort: AbortController | undefined
  let listAbort: AbortController | undefined
  let disabled = $derived(pending || busy)

  $effect(() => {
    void refresh()
    return () => {
      lifetime.abort()
      listAbort?.abort()
      teamAbort?.abort()
      policyAbort?.abort()
    }
  })
  $effect(() => {
    const uid = teamUID
    member = ''
    void loadMembers(uid)
  })
  $effect(() => {
    const id = projectID
    void loadPolicy(id)
  })

  async function refresh(): Promise<void> {
    if (lifetime.signal.aborted) return
    listAbort?.abort()
    const abort = (listAbort = new AbortController())
    loading = true
    try {
      const response = await listTeams({ signal: abort.signal })
      if (abort.signal.aborted || lifetime.signal.aborted) return
      if (response.status !== 200) throw new Error()
      teams = response.data.teams
      if (!teams.some((team) => team.uid === teamUID)) teamUID = ''
      error = undefined
    } catch {
      if (!abort.signal.aborted && !lifetime.signal.aborted)
        error = 'Unable to read teams. Refresh to try again.'
    } finally {
      if (!abort.signal.aborted && !lifetime.signal.aborted) loading = false
    }
  }

  async function loadMembers(uid: string): Promise<void> {
    teamAbort?.abort()
    const abort = (teamAbort = new AbortController())
    members = []
    teamLoading = !!uid
    if (!uid || lifetime.signal.aborted) return
    try {
      const response = await showTeam({ teamUid: uid }, { signal: abort.signal })
      if (abort.signal.aborted || lifetime.signal.aborted) return
      if (response.status !== 200) throw new Error()
      members = response.data.members
      error = undefined
    } catch {
      if (!abort.signal.aborted && !lifetime.signal.aborted) error = 'Unable to read team members.'
    } finally {
      if (!abort.signal.aborted && !lifetime.signal.aborted) teamLoading = false
    }
  }

  async function loadPolicy(id: string): Promise<void> {
    policyAbort?.abort()
    const abort = (policyAbort = new AbortController())
    policy = undefined
    selectedTeams = []
    policyLoading = !!id
    if (!id || lifetime.signal.aborted) return
    try {
      const response = await getProjectAccess({ projectId: Number(id) }, { signal: abort.signal })
      if (abort.signal.aborted || lifetime.signal.aborted) return
      if (response.status !== 200) throw new Error()
      policy = response.data.policy
      visibility = policy.visibility === 'teams' ? 'teams' : 'all'
      selectedTeams = [...policy.team_uids]
      error = undefined
    } catch {
      if (!abort.signal.aborted && !lifetime.signal.aborted)
        error = 'Unable to read project visibility.'
    } finally {
      if (!abort.signal.aborted && !lifetime.signal.aborted) policyLoading = false
    }
  }

  async function mutate(
    draft: unknown,
    write: (context: MutationContext) => Promise<MutationResult>,
  ): Promise<boolean> {
    if (disabled || lifetime.signal.aborted) return false
    busy = true
    try {
      const changed = await onMutation(draft, (context) => {
        if (lifetime.signal.aborted) throw new Error('Authority changed.')
        return write(context)
      })
      return changed && !lifetime.signal.aborted
    } finally {
      busy = false
    }
  }

  async function create(): Promise<void> {
    const value = name.trim()
    if (!value || loading) return
    if (
      await mutate({ name: value }, (context) =>
        createTeam({ name: value }, { headers: context.headers, signal: lifetime.signal }),
      )
    ) {
      name = ''
      await refresh()
    }
  }

  async function changeMember(actor: string, add: boolean): Promise<void> {
    const uid = teamUID
    if (!uid || !actor || teamLoading) return
    if (
      await mutate({ teamUID: uid, actor, add }, (context) =>
        (add ? addTeamMember : removeTeamMember)(
          { teamUid: uid, actor },
          { headers: context.headers, signal: lifetime.signal },
        ),
      )
    ) {
      if (teamUID !== uid) return
      member = ''
      await loadMembers(uid)
    }
  }

  async function removeTeam(): Promise<void> {
    const uid = teamUID
    if (!uid || !confirm('Delete this team and revoke its project access?')) return
    if (
      await mutate({ teamUID: uid }, (context) =>
        deleteTeam({ teamUid: uid }, { headers: context.headers, signal: lifetime.signal }),
      )
    )
      await refresh()
  }

  async function savePolicy(): Promise<void> {
    if (!policy || policyLoading) return
    const id = projectID
    const draft = {
      visibility,
      team_uids: visibility === 'teams' ? [...selectedTeams] : [],
      revision: policy.revision,
    }
    if (
      await mutate(draft, (context) =>
        setProjectAccess({ projectId: Number(id) }, draft, {
          headers: context.headers,
          signal: lifetime.signal,
        }),
      )
    ) {
      if (projectID === id) await loadPolicy(id)
    }
  }
</script>

<div class="access-management">
  {#if message || error}<p role="alert">{message ?? error}</p>{/if}
  <button type="button" disabled={disabled || loading} onclick={() => void refresh()}
    >Refresh teams</button
  >
  <form
    onsubmit={(event) => {
      event.preventDefault()
      void create()
    }}
  >
    <label>New team name <input bind:value={name} required {disabled} /></label>
    <button type="submit" disabled={disabled || loading || !name.trim()}>Create team</button>
  </form>
  <label for={`${controlID}-team`}>Team</label>
  <select id={`${controlID}-team`} bind:value={teamUID} disabled={disabled || loading}>
    <option value="">Choose a team</option>
    {#each teams as team (team.uid)}<option value={team.uid}>{team.name}</option>{/each}
  </select>
  {#if teamUID}
    <ul aria-label="Team members">
      {#each members as account (account)}<li>
          {account}
          <button
            type="button"
            aria-label={`Remove ${account}`}
            disabled={disabled || teamLoading}
            onclick={() => void changeMember(account, false)}>Remove</button
          >
        </li>{/each}
    </ul>
    <form
      onsubmit={(event) => {
        event.preventDefault()
        void changeMember(member.trim(), true)
      }}
    >
      <label
        >Member account <input
          bind:value={member}
          required
          disabled={disabled || teamLoading}
        /></label
      >
      <button type="submit" disabled={disabled || teamLoading || !member.trim()}>Add member</button>
    </form>
    <button type="button" disabled={disabled || teamLoading} onclick={() => void removeTeam()}
      >Delete team</button
    >
  {/if}
  <label for={`${controlID}-project`}>Project visibility</label>
  <select id={`${controlID}-project`} bind:value={projectID} {disabled}>
    <option value="">Choose a project</option>
    {#each projects as entry (entry.project.uid)}<option value={String(entry.project.id)}
        >{entry.project.name}</option
      >{/each}
  </select>
  {#if projectID}
    <form
      onsubmit={(event) => {
        event.preventDefault()
        void savePolicy()
      }}
    >
      <label for={`${controlID}-visibility`}>Visibility</label>
      <select
        id={`${controlID}-visibility`}
        bind:value={visibility}
        disabled={disabled || policyLoading || !policy}
      >
        <option value="all">All enrolled users</option><option value="teams">Selected teams</option>
      </select>
      {#if visibility === 'teams'}
        <fieldset disabled={disabled || loading || policyLoading || !policy}>
          <legend>Teams with access</legend>
          {#each teams as team (team.uid)}
            <Checkbox
              label={team.name}
              checked={selectedTeams.includes(team.uid)}
              disabled={disabled || loading || policyLoading || !policy}
              onchange={(checked) => {
                selectedTeams = checked
                  ? [...selectedTeams, team.uid]
                  : selectedTeams.filter((uid) => uid !== team.uid)
              }}
            />
          {/each}
          <p>
            No selected teams means no team members have access. Daemon owners retain administration
            access.
          </p>
        </fieldset>
      {/if}
      <button type="submit" disabled={disabled || loading || policyLoading || !policy}
        >Save visibility</button
      >
      <button
        type="button"
        disabled={disabled || policyLoading}
        onclick={() => void loadPolicy(projectID)}>Reload visibility</button
      >
    </form>
  {/if}
</div>

<style>
  .access-management {
    display: grid;
    gap: var(--space-3);
    min-width: 0;
  }
  form {
    display: flex;
    flex-wrap: wrap;
    align-items: end;
    gap: var(--space-3);
  }
  label {
    display: grid;
    gap: 4px;
  }
  input,
  select {
    min-width: 0;
    max-width: 100%;
    padding: 6px 8px;
    border: 1px solid var(--border-default);
    border-radius: 4px;
    background: var(--bg-primary);
    color: var(--text-primary);
    font: inherit;
  }
  select {
    width: min(100%, 24rem);
  }
  button {
    width: fit-content;
    max-width: 100%;
    padding: 5px 10px;
    border: 1px solid var(--border-default);
    border-radius: 4px;
    background: var(--bg-primary);
    color: var(--text-primary);
    font: inherit;
  }
  button:disabled {
    opacity: var(--opacity-disabled);
    cursor: wait;
  }
  button:hover:not(:disabled) {
    background: var(--bg-surface-hover);
  }
  fieldset {
    min-width: 0;
    width: 100%;
    padding: var(--space-3);
    border: 1px solid var(--border-default);
  }
  li {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: var(--space-2);
    overflow-wrap: anywhere;
  }
  p {
    color: var(--text-muted);
    font-size: var(--font-size-sm);
  }
  ul {
    padding: 0;
    list-style: none;
    display: grid;
    gap: var(--space-2);
  }
</style>
