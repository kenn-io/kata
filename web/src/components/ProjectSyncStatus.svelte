<script lang="ts">
  import { untrack } from 'svelte'
  import { CopyButton } from '@kenn-io/kit-ui'
  import type { FederationProjectStatus } from '../lib/api/generated'
  import { readProjectSync } from '../lib/projectSyncRead'

  interface Props {
    projectID: number
    projectUID: string
    refreshGeneration?: number
  }
  let { projectID, projectUID, refreshGeneration = 0 }: Props = $props()
  let status = $state<FederationProjectStatus>()
  let loading = $state(true)
  let reading = $state(false)
  let error = $state(false)
  let stale = $state(false)
  let hasResult = false
  let queued = false
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

  function resetView(): void {
    status = undefined
    loading = true
    error = false
    stale = false
    reading = false
    hasResult = false
    queued = false
  }

  function runQueued(): void {
    const next = queued
    queued = false
    if (next) refresh?.()
  }

  $effect(() => {
    void identity
    const [id, uid] = untrack(() => [projectID, projectUID] as const)
    let current: AbortController | undefined
    let disposed = false
    resetView()

    async function read(manual = false): Promise<void> {
      if (document.hidden) return
      if (current) {
        queued = true
        return
      }
      error = false
      loading = manual || !hasResult
      reading = true
      const abort = new AbortController()
      current = abort
      const finish = () => {
        clearTimeout(timeout)
        if (current !== abort) return
        current = undefined
        if (disposed) return
        loading = false
        reading = false
        runQueued()
      }
      const timeout = setTimeout(() => {
        status = undefined
        hasResult = false
        error = true
        abort.abort()
        finish()
      }, 120_000)
      abort.signal.addEventListener('abort', () => clearTimeout(timeout), { once: true })
      try {
        const result = await readProjectSync(id, uid, abort.signal, status, () => {
          if (!abort.signal.aborted && !disposed) {
            status = undefined
            hasResult = false
            loading = true
          }
        })
        if (abort.signal.aborted || disposed) return
        ;({ status, stale } = result)
        hasResult = true
      } catch {
        if (!abort.signal.aborted && !disposed) {
          status = undefined
          hasResult = false
          error = true
        }
      } finally {
        finish()
      }
    }
    refresh = (manual) => void read(manual)
    return () => {
      disposed = true
      current?.abort()
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

<section class="project-sync" aria-label="Project sync" aria-busy={reading}>
  <strong>Project sync</strong>
  {#if loading}
    <span role="status">Reading sync status…</span>
  {:else if error}
    <span role="status">Sync status unavailable. Refresh to retry.</span>
  {:else if !status}
    <span>Local project</span>
  {:else}
    <span>{status.enabled ? status.role : 'Sync disabled'}</span>
    {#if stale}<span class="stale" role="status">Sync status may be out of date.</span>{/if}
    <span>Pending: {status.pending_push_count}</span>
    {#if status.last_successful_sync_at}
      <span
        >Last sync: <time datetime={status.last_successful_sync_at}
          >{status.last_successful_sync_at}</time
        ></span
      >
    {/if}
    {#if status.last_error}
      {@const bashCommand = `kata federation status --project '${status.project_name.replaceAll("'", "'\\''")}'`}
      {@const powershellCommand = `kata federation status --project '${status.project_name.replaceAll("'", "''")}'`}
      <div class="sync-attention">
        <span class="attention">Sync needs attention</span>
        <span>Run in a CLI connected to this daemon.</span>
        <span>Browser: {window.location.origin}</span>
        <div class="command">
          <span class="shell">Bash/Zsh</span>
          <code>{bashCommand}</code>
          <CopyButton
            text={bashCommand}
            ariaLabel="Copy Bash/Zsh sync status command"
            copiedAriaLabel="Copied Bash/Zsh sync status command"
          />
        </div>
        <div class="command">
          <span class="shell">PowerShell</span>
          <code>{powershellCommand}</code>
          <CopyButton
            text={powershellCommand}
            ariaLabel="Copy PowerShell sync status command"
            copiedAriaLabel="Copied PowerShell sync status command"
          />
        </div>
      </div>
    {/if}
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
  <button type="button" disabled={loading || reading} onclick={() => refresh?.(true)}
    >Refresh sync</button
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
  .sync-attention {
    display: flex;
    flex-wrap: wrap;
    flex-basis: 100%;
    align-items: center;
    gap: var(--space-2) var(--space-3);
    min-width: 0;
  }
  .command {
    display: flex;
    flex-basis: 100%;
    align-items: center;
    gap: var(--space-2);
    min-width: 0;
  }
  .shell {
    flex: 0 0 5.5rem;
    color: var(--text-primary);
  }
  code {
    font-size: var(--font-size-sm);
    overflow-wrap: anywhere;
    min-width: 0;
  }
  .attention,
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
