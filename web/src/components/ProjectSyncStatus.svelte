<script lang="ts">
  import { untrack } from 'svelte'
  import { getProjectFederationStatus, type FederationProjectStatus } from '../lib/api/generated'

  interface Props {
    projectID: number
    projectUID: string
    refreshGeneration?: number
    visibilityGeneration?: number
  }
  let { projectID, projectUID, refreshGeneration = 0, visibilityGeneration = 0 }: Props = $props()
  let status = $state<FederationProjectStatus>()
  let loading = $state(true)
  let error = $state(false)
  let hasResult = $state(false)
  let refresh: ((manual: boolean, returning?: boolean) => void) | undefined
  let seenVisibility = untrack(() => visibilityGeneration)
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
    let stopped = false
    let disposed = false
    let pendingReturn = false
    status = undefined
    hasResult = false
    loading = true
    error = false

    async function read(manual: boolean, returning = false): Promise<void> {
      if (manual) {
        current?.abort()
        stopped = false
        pendingReturn = false
        loading = true
      } else {
        if (document.hidden || stopped) return
        if (current) {
          if (returning) pendingReturn = true
          return
        }
      }
      pendingReturn = false
      const abort = new AbortController()
      current = abort
      const finish = () => {
        clearTimeout(timeout)
        if (current !== abort) return
        current = undefined
        if (disposed) return
        loading = false
        if (pendingReturn && !document.hidden && !stopped) void read(false)
      }
      const timeout = setTimeout(() => {
        if (current !== abort || disposed) return
        error = true
        abort.abort()
        finish()
      }, 120_000)
      abort.signal.addEventListener('abort', () => clearTimeout(timeout), { once: true })
      try {
        const response = await getProjectFederationStatus(
          { projectId: id },
          { compact: true },
          { signal: abort.signal },
        )
        if (abort.signal.aborted || disposed) return
        if (response.status !== 200) {
          stopped =
            response.status >= 400 &&
            response.status < 500 &&
            response.status !== 408 &&
            response.status !== 429
          if ([401, 403, 404].includes(response.status)) {
            status = undefined
            hasResult = false
          }
          error = true
          return
        }
        const found = response.data.statuses.find(
          (entry) => entry.project_id === id && entry.project_uid === uid,
        )
        if (response.data.statuses.length && !found) {
          status = undefined
          hasResult = false
          stopped = true
          error = true
          return
        }
        status = found
        hasResult = true
        error = false
      } catch {
        if (!abort.signal.aborted && !disposed) error = true
      } finally {
        finish()
      }
    }
    refresh = (manual, returning) => {
      void read(manual, returning)
    }
    void read(false)
    return () => {
      disposed = true
      current?.abort()
      refresh = undefined
    }
  })

  $effect(() => {
    void refreshGeneration
    untrack(() => refresh?.(false))
  })

  $effect(() => {
    const generation = visibilityGeneration
    if (generation === seenVisibility) return
    seenVisibility = generation
    untrack(() => refresh?.(false, true))
  })
</script>

<section class="project-sync" aria-label="Project sync" aria-busy={loading}>
  <strong>Project sync</strong>
  {#if loading}
    <span role="status">Reading sync status…</span>
  {:else if error && !hasResult}
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
  {#if error && hasResult}<span class="stale" role="status">Sync status may be out of date.</span
    >{/if}
  <button type="button" onclick={() => refresh?.(true)}>Refresh sync</button>
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
  .stale {
    border-radius: var(--radius-sm);
    background: color-mix(in srgb, var(--accent-amber) 14%, transparent);
    color: var(--accent-amber);
    padding: var(--space-1) var(--space-2);
    font-weight: var(--font-weight-semibold);
  }
  button {
    color: var(--text-primary);
  }
</style>
