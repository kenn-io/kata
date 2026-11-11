<script lang="ts">
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
  let hasResult = $state(false)
  let manualGeneration = $state(0)
  let lastIdentity = ''
  let counts = $derived.by(() => {
    const result: Record<string, number> = {}
    for (const artifact of status?.embedding?.artifacts ?? []) {
      if (['generated', 'reused', 'incompatible', 'stored_unindexed'].includes(artifact.state))
        result[artifact.state] = (result[artifact.state] ?? 0) + 1
    }
    return result
  })

  $effect(() => {
    const id = projectID
    const uid = projectUID
    void refreshGeneration
    void manualGeneration
    const identity = `${id}:${uid}`
    if (identity !== lastIdentity) {
      lastIdentity = identity
      status = undefined
      hasResult = false
      loading = true
      error = false
    }
    if (document.hidden) return
    const abort = new AbortController()
    void load(id, uid, abort.signal)
    return () => abort.abort()
  })

  async function load(id: number, uid: string, signal: AbortSignal): Promise<void> {
    try {
      const response = await getProjectFederationStatus(
        { projectId: id },
        { compact: true },
        { signal },
      )
      if (signal.aborted) return
      if (response.status !== 200) {
        if (
          response.status >= 400 &&
          response.status < 500 &&
          response.status !== 408 &&
          response.status !== 429
        ) {
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
        error = true
        return
      }
      status = found
      hasResult = true
      error = false
    } catch {
      if (!signal.aborted) error = true
    } finally {
      if (!signal.aborted) loading = false
    }
  }
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
  <button
    type="button"
    onclick={() => {
      loading = true
      manualGeneration++
    }}>Refresh sync</button
  >
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
