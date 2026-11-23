<script lang="ts">
  import PlusIcon from '@lucide/svelte/icons/plus'
  import RefreshCwIcon from '@lucide/svelte/icons/refresh-cw'
  import {
    Avatar,
    Button,
    Checkbox,
    Disclosure,
    SelectDropdown,
    SettingsSection,
  } from '@kenn-io/kit-ui'

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
  import SettingsAlert from './SettingsAlert.svelte'

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
  let naming = $state(false)
  let loading = $state(true)
  let teamsError = $state<string>()
  let membersError = $state<string>()
  let policyError = $state<string>()
  let busy = $state(false)
  let teamUID = $state('')
  let members = $state<string[]>([])
  let member = $state('')
  let addingMember = $state(false)
  let teamLoading = $state(false)
  let projectID = $state('')
  let policy = $state<ProjectAccessPolicy>()
  let visibility = $state<'all' | 'teams'>('all')
  let selectedTeams = $state<string[]>([])
  let policyLoading = $state(false)
  const lifetime = new AbortController()
  let teamAbort: AbortController | undefined
  let policyAbort: AbortController | undefined
  let listAbort: AbortController | undefined
  let disabled = $derived(pending || busy)
  let policyDisabled = $derived(disabled || loading || policyLoading || !policy)
  let projectOptions = $derived(
    projects.map((entry) => ({ value: String(entry.project.id), label: entry.project.name })),
  )

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
    addingMember = false
    void loadMembers(uid)
  })
  // Visibility always shows a project: the first, until another is picked.
  $effect(() => {
    if (!projectOptions.some((option) => option.value === projectID))
      projectID = projectOptions[0]?.value ?? ''
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
      const response = await listTeams({ signal: abort.signal, retry: true })
      if (abort.signal.aborted || lifetime.signal.aborted) return
      if (response.status !== 200) throw new Error()
      teams = response.data.teams
      if (!teams.some((team) => team.uid === teamUID)) teamUID = ''
      teamsError = undefined
    } catch {
      if (!abort.signal.aborted && !lifetime.signal.aborted)
        teamsError = "Couldn't load teams. Refresh to try again."
    } finally {
      if (!abort.signal.aborted && !lifetime.signal.aborted) loading = false
    }
  }

  // Refresh reloads everything the section shows: teams, the open team's
  // members, and the chosen project's visibility.
  async function refreshAll(): Promise<void> {
    const uid = teamUID
    const id = projectID
    await Promise.all([
      refresh(),
      uid ? loadMembers(uid) : undefined,
      id ? loadPolicy(id) : undefined,
    ])
  }

  async function loadMembers(uid: string): Promise<void> {
    teamAbort?.abort()
    const abort = (teamAbort = new AbortController())
    members = []
    membersError = undefined
    teamLoading = !!uid
    if (!uid || lifetime.signal.aborted) return
    try {
      const response = await showTeam({ teamUid: uid }, { signal: abort.signal })
      if (abort.signal.aborted || lifetime.signal.aborted) return
      if (response.status !== 200) throw new Error()
      members = response.data.members
      membersError = undefined
    } catch {
      if (!abort.signal.aborted && !lifetime.signal.aborted)
        membersError = "Couldn't load team members."
    } finally {
      if (!abort.signal.aborted && !lifetime.signal.aborted) teamLoading = false
    }
  }

  async function loadPolicy(id: string): Promise<void> {
    policyAbort?.abort()
    const abort = (policyAbort = new AbortController())
    policy = undefined
    policyError = undefined
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
      policyError = undefined
    } catch {
      if (!abort.signal.aborted && !lifetime.signal.aborted)
        policyError = "Couldn't load project visibility."
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
      naming = false
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
      addingMember = false
      await loadMembers(uid)
    }
  }

  async function removeTeam(uid: string): Promise<void> {
    const selectedProjectID = projectID
    if (!uid || !confirm('Delete this team and revoke its project access?')) return
    if (
      await mutate({ teamUID: uid }, (context) =>
        deleteTeam({ teamUid: uid }, { headers: context.headers, signal: lifetime.signal }),
      )
    ) {
      teams = teams.filter((team) => team.uid !== uid)
      if (teamUID === uid) teamUID = ''
      await refresh()
      if (selectedProjectID && projectID === selectedProjectID) await loadPolicy(selectedProjectID)
    }
  }

  // Visibility saves as it changes, against the revision it was loaded at.
  // Selected teams with none picked waits for a pick instead of saving.
  async function savePolicy(
    nextVisibility: 'all' | 'teams',
    nextTeams: readonly string[],
  ): Promise<void> {
    if (!policy || policyLoading) return
    visibility = nextVisibility
    selectedTeams = [...nextTeams]
    if (nextVisibility === 'teams' && nextTeams.length === 0) return
    const id = projectID
    const draft = {
      visibility: nextVisibility,
      team_uids: nextVisibility === 'teams' ? [...nextTeams] : [],
      revision: policy.revision,
    }
    await mutate(draft, (context) =>
      setProjectAccess({ projectId: Number(id) }, draft, {
        headers: context.headers,
        signal: lifetime.signal,
      }),
    )
    if (projectID === id && !lifetime.signal.aborted) await loadPolicy(id)
  }

  function memberCount(count: number): string {
    return `${count} ${count === 1 ? 'member' : 'members'}`
  }
</script>

<div class="access-management">
  {#if message}<SettingsAlert>{message}</SettingsAlert>{/if}

  <SettingsSection title="Teams">
    <div class="title-action">
      <Button
        size="sm"
        ariaLabel="Refresh teams"
        disabled={disabled || loading}
        onclick={() => void refreshAll()}
      >
        <RefreshCwIcon size={14} aria-hidden="true" />
        <span>Refresh</span>
      </Button>
    </div>

    {#if teamsError}<SettingsAlert>{teamsError}</SettingsAlert>{/if}
    {#if loading && teams.length === 0}
      <p class="note" role="status">Loading teams…</p>
    {:else if teams.length === 0}
      <p class="note">No teams yet.</p>
    {:else}
      <ul class="teams" aria-label="Teams">
        {#each teams as team (team.uid)}
          {@const open = teamUID === team.uid}
          <li class="team">
            <Disclosure
              variant="plain"
              {open}
              ontoggle={(next) => {
                if (next) teamUID = team.uid
                else if (teamUID === team.uid) teamUID = ''
              }}
            >
              {#snippet summary()}
                <span class="team-name">{team.name}</span>
                {#if open && !teamLoading && !membersError}
                  <span class="team-count">{memberCount(members.length)}</span>
                {/if}
              {/snippet}
              {#if !open}
                <!-- Only the open team's members are loaded. -->
              {:else if teamLoading}
                <p class="note" role="status">Loading members…</p>
              {:else if membersError}
                <SettingsAlert>{membersError}</SettingsAlert>
              {:else}
                <ul class="members" aria-label="Team members">
                  {#each members as account (account)}
                    <li class="member">
                      <Avatar name={account} decorative size={18} />
                      <span class="member-name">{account}</span>
                      <Button
                        size="sm"
                        surface="soft"
                        class="row-action"
                        ariaLabel={`Remove ${account}`}
                        disabled={disabled || loading}
                        onclick={() => void changeMember(account, false)}>Remove</Button
                      >
                    </li>
                  {/each}
                </ul>
                {#if addingMember}
                  <form
                    class="inline-field"
                    onsubmit={(event) => {
                      event.preventDefault()
                      void changeMember(member.trim(), true)
                    }}
                  >
                    <!-- svelte-ignore a11y_autofocus -->
                    <input
                      bind:value={member}
                      aria-label="Member account"
                      placeholder="Account"
                      autofocus
                      required
                      pattern=".*\S.*"
                      disabled={disabled || loading}
                      onkeydown={(event) => {
                        if (event.key === 'Escape') {
                          member = ''
                          addingMember = false
                        }
                      }}
                    />
                  </form>
                {:else}
                  <button
                    type="button"
                    class="add-row"
                    disabled={disabled || loading}
                    onclick={() => (addingMember = true)}
                  >
                    <PlusIcon size={14} aria-hidden="true" />
                    <span>Add member</span>
                  </button>
                {/if}
              {/if}
            </Disclosure>
            {#if open}
              <div class="team-action">
                <button
                  type="button"
                  class="text-action"
                  aria-label={`Delete ${team.name}`}
                  disabled={disabled || teamLoading || loading}
                  onclick={() => void removeTeam(team.uid)}>Delete…</button
                >
              </div>
            {/if}
          </li>
        {/each}
      </ul>
    {/if}

    {#if naming}
      <form
        class="inline-field"
        onsubmit={(event) => {
          event.preventDefault()
          void create()
        }}
      >
        <!-- svelte-ignore a11y_autofocus -->
        <input
          bind:value={name}
          aria-label="New team name"
          placeholder="Team name"
          autofocus
          required
          pattern=".*\S.*"
          {disabled}
          onkeydown={(event) => {
            if (event.key === 'Escape') {
              name = ''
              naming = false
            }
          }}
        />
      </form>
    {:else}
      <button
        type="button"
        class="add-row"
        disabled={disabled || loading}
        onclick={() => (naming = true)}
      >
        <PlusIcon size={14} aria-hidden="true" />
        <span>New team</span>
      </button>
    {/if}
  </SettingsSection>

  <SettingsSection title="Project visibility" description="Server owners see every project.">
    {#if policyError}<SettingsAlert>{policyError}</SettingsAlert>{/if}
    {#if projectOptions.length === 0}
      <p class="note">No projects yet.</p>
    {:else}
      <div class="settings-row">
        <span class="row-label">Project</span>
        <SelectDropdown
          title="Project"
          value={projectID}
          options={projectOptions}
          {disabled}
          onchange={(value) => {
            projectID = value
          }}
        />
      </div>
      <div class="settings-row">
        <span class="row-label">Visibility</span>
        <SelectDropdown
          title="Visibility"
          value={visibility}
          options={[
            { value: 'all', label: 'Everyone' },
            { value: 'teams', label: 'Selected teams' },
          ]}
          disabled={policyDisabled}
          onchange={(value) =>
            void savePolicy(
              value === 'teams' ? 'teams' : 'all',
              value === 'teams' ? selectedTeams : [],
            )}
        />
      </div>
      {#if visibility === 'teams'}
        <fieldset class="settings-row team-picks" disabled={policyDisabled}>
          <legend class="row-label">Teams with access</legend>
          <div class="picks">
            {#each teams as team (team.uid)}
              <Checkbox
                label={team.name}
                checked={selectedTeams.includes(team.uid)}
                disabled={policyDisabled}
                onchange={(checked) =>
                  void savePolicy(
                    'teams',
                    checked
                      ? [...selectedTeams, team.uid]
                      : selectedTeams.filter((uid) => uid !== team.uid),
                  )}
              />
            {:else}
              <p class="note">Create a team above to pick it here.</p>
            {/each}
            {#if teams.length > 0 && selectedTeams.length === 0}
              <p class="note">Pick a team to save.</p>
            {/if}
          </div>
        </fieldset>
      {/if}
    {/if}
  </SettingsSection>
</div>

<style>
  .access-management {
    display: grid;
    gap: var(--space-7);
    min-width: 0;
  }

  /* Refresh sits at the right end of the section's title row. */
  .title-action {
    position: absolute;
    top: 0;
    right: 0;
    display: flex;
    align-items: center;
    height: var(--layout-header-h);
  }

  .title-action :global(.kit-button) {
    gap: var(--space-2);
  }

  .note {
    margin: 0;
    color: var(--text-secondary);
    font-size: var(--font-size-sm);
  }

  .teams,
  .members {
    display: grid;
    margin: 0;
    padding: 0;
    list-style: none;
  }

  .team {
    position: relative;
    min-width: 0;
  }

  .team :global(.kit-disclosure__summary) {
    padding-right: 96px;
    font-size: var(--font-size-md);
  }

  .team-name {
    font-weight: var(--font-weight-semibold);
  }

  .team-count {
    margin-left: var(--space-2);
    color: var(--text-secondary);
    font-size: var(--font-size-sm);
    font-weight: 400;
  }

  /* Delete… sits on the open team's own row, at the right. */
  .team-action {
    position: absolute;
    top: 0;
    right: 0;
    display: flex;
    align-items: center;
    height: calc(1lh + 2 * var(--space-2));
  }

  .members {
    gap: var(--space-1);
  }

  .member {
    display: flex;
    align-items: center;
    gap: var(--space-3);
    min-height: 32px;
    color: var(--text-primary);
  }

  .member-name {
    flex: 1;
    min-width: 0;
    overflow-wrap: anywhere;
  }

  /* Remove shows on the row being pointed at or focused. */
  .member :global(.row-action) {
    opacity: 0;
  }

  .member:hover :global(.row-action),
  .member:focus-within :global(.row-action) {
    opacity: 1;
  }

  .text-action {
    padding: 2px var(--space-3);
    border: 0;
    border-radius: var(--radius-sm);
    background: none;
    color: var(--text-secondary);
    font: inherit;
    font-size: var(--font-size-md);
    cursor: pointer;
  }

  .text-action:hover:not(:disabled) {
    background: var(--bg-surface-hover);
    color: var(--text-primary);
  }

  .text-action:disabled {
    cursor: wait;
    opacity: var(--opacity-disabled);
  }

  .add-row {
    display: inline-flex;
    align-items: center;
    gap: var(--space-2);
    width: fit-content;
    min-height: 32px;
    padding: 0;
    border: 0;
    background: none;
    color: var(--text-secondary);
    font: inherit;
    font-size: var(--font-size-md);
    cursor: pointer;
  }

  .add-row:hover:not(:disabled) {
    color: var(--text-primary);
  }

  .add-row:disabled {
    cursor: wait;
    opacity: var(--opacity-disabled);
  }

  .inline-field input {
    box-sizing: border-box;
    width: var(--layout-settings-select-w);
    max-width: 100%;
    padding: 6px 10px;
    border: 1px solid var(--border-default);
    border-radius: var(--radius-md);
    background: var(--bg-primary);
    color: var(--text-primary);
    font: inherit;
  }

  /* A settings row: a 160px label column with its control right after it. */
  .settings-row {
    display: grid;
    grid-template-columns: var(--layout-settings-label-w) minmax(0, var(--layout-settings-select-w));
    align-items: center;
    min-height: 40px;
    margin: 0;
    padding: 0;
    border: 0;
    min-width: 0;
  }

  .settings-row :global(.kit-select-dropdown) {
    width: 100%;
  }

  .row-label {
    padding: 0;
    color: var(--text-primary);
  }

  /* A fieldset's legend can't sit in a grid cell, so the label floats into
     the first column and the picks fill the second. */
  .team-picks {
    display: block;
  }

  .team-picks .row-label {
    float: left;
    width: var(--layout-settings-label-w);
    line-height: 28px;
  }

  .picks {
    display: grid;
    gap: var(--space-2);
    margin-left: var(--layout-settings-label-w);
    padding-top: 4px;
  }

  @media (max-width: 760px) {
    .settings-row {
      grid-template-columns: 1fr;
      gap: var(--space-2);
    }

    .team-picks .row-label {
      float: none;
    }

    .picks {
      margin-left: 0;
    }
  }
</style>
