<script lang="ts">
  import { untrack } from 'svelte'
  import { getProjectFederationStatus, type FederationProjectStatus } from '../lib/api/generated'

  interface Props {
    projectID: number
    projectUID: string
    refreshGeneration?: number
  }
  let { projectID, projectUID, refreshGeneration = 0 }: Props = $props()
  let status = $state<FederationProjectStatus>()
  let loading = $state(true)
  let error = $state(false)
  let refresh: ((manual?: boolean) => void) | undefined
  let identity = $derived(`${projectID}:${projectUID}`)
  let counts = $derived.by(() => {
    const result: Record<string, number> = {}
    for (const artifact of status?.embedding?.artifacts ?? []) {
      if (['generated', 'reused', 'incompatible', 'stored_unindexed'].includes(artifact.state))
        result[artifact.state] = (result[artifact.state] ?? 0) + 1
    }
    return result
  })

  $effect(() => {
    void identity
    const [id, uid] = untrack(() => [projectID, projectUID] as const)
    let current: AbortController | undefined
    // Coalesce refreshes during a read into one follow-up.
    let pending = false
    status = undefined
    loading = true
    error = false

    async function read(manual = false): Promise<void> {
      if (document.hidden) return
      if (current) {
        if (!manual) {
          pending = true
          return
        }
        current.abort()
        current = undefined
      }
      pending = false
      if (manual) loading = true
      const abort = new AbortController()
      current = abort
      const finish = () => {
        clearTimeout(timeout)
        if (current !== abort) return
        current = undefined
        loading = false
        if (pending) {
          pending = false
          void read()
        }
      }
      const timeout = setTimeout(() => {
        status = undefined
        error = true
        abort.abort()
        finish()
      }, 120_000)
      try {
        const result = await getProjectFederationStatus(
          { projectId: id },
          {
            signal: abort.signal,
            onResponse: (raw) => {
              if (raw.status >= 400 && raw.status < 500 && !abort.signal.aborted) {
                status = undefined
                loading = true
              }
            },
          },
        )
        if (abort.signal.aborted) return
        if (result.status !== 200) throw new Error()
        const found = result.data.statuses.find(
          (entry) => entry.project_id === id && entry.project_uid === uid,
        )
        if (result.data.statuses.length && !found) throw new Error()
        status = found
        error = false
      } catch {
        if (!abort.signal.aborted) {
          status = undefined
          error = true
        }
      } finally {
        finish()
      }
    }
    refresh = (manual) => void read(manual)
    return () => {
      current?.abort()
      current = undefined
      refresh = undefined
    }
  })

  $effect(() => {
    void projectID
    void projectUID
    void refreshGeneration
    untrack(() => refresh?.())
  })
</script>

<section class="project-sync" aria-label="Project sync" aria-busy={loading}>
  <strong>Project sync</strong>
  {#if loading}
    <span role="status">Reading sync status…</span>
  {:else if error}
    <span role="status">Sync status unavailable. Refresh to retry.</span>
  {:else if !status}
    <span>Local project</span>
  {:else}
    <span>{status.enabled ? status.role : 'Sync disabled'}</span>
    <span>Pending: {status.pending_push_count}</span>
    {#if status.last_successful_sync_at}
      <span
        >Last sync: <time datetime={status.last_successful_sync_at}
          >{status.last_successful_sync_at}</time
        ></span
      >
    {/if}
    {#if status.last_error}<span>Sync needs attention</span>{/if}
    {#if status.credential_status}<span>Credential: {status.credential_status}</span>{/if}
    {#if status.embedding}
      <span>Embeddings: {status.embedding.state}</span>
      <span>Producer: {status.embedding.producer?.producer_instance_uid ?? 'None selected'}</span>
      {#if status.embedding.producer}
        <span
          >Recipe: {status.embedding.producer.recipe.model} ({status.embedding.producer.recipe
            .dimensions} dimensions)</span
        >
      {/if}
      <span>Generated: {counts.generated ?? 0}</span>
      <span>Reused: {counts.reused ?? 0}</span>
      <span>Incompatible: {counts.incompatible ?? 0}</span>
      <span>Retained without index: {counts.stored_unindexed ?? 0}</span>
      <span
        >Retained window: {status.embedding.artifacts.length}/{status.embedding
          .artifact_limit}{status.embedding.limited ? ' (limited)' : ''}</span
      >
    {/if}
  {/if}
  <button type="button" disabled={loading} onclick={() => refresh?.(true)}>Refresh sync</button>
</section>

<style>
  .project-sync {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: var(--space-2) var(--space-4);
    padding: var(--space-3) var(--space-5);
    border-bottom: 1px solid var(--border-primary);
    color: var(--text-secondary);
    font-size: var(--font-size-sm);
    min-width: 0;
    overflow-wrap: anywhere;
  }
  .project-sync span {
    min-width: 0;
  }
  button {
    color: var(--text-primary);
  }
</style>
