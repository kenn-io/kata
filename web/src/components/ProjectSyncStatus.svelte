<script lang="ts">
  import { tick, untrack } from 'svelte'
  import ChevronDownIcon from '@lucide/svelte/icons/chevron-down'
  import CircleAlertIcon from '@lucide/svelte/icons/circle-alert'
  import TriangleAlertIcon from '@lucide/svelte/icons/triangle-alert'
  import {
    Chip,
    CopyButton,
    autoReposition,
    dismissable,
    floatingPopoverStyle,
  } from '@kenn-io/kit-ui'
  import { getProjectFederationStatus, type FederationProjectStatus } from '../lib/api/generated'
  import { relativeTime } from '../lib/kata/dates'
  import { markColor } from '../lib/marks'

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

  let open = $state(false)
  // The chip's host: placement measures it, and Escape focuses its button.
  let trigger = $state<HTMLSpanElement>()
  let panel = $state<HTMLDivElement>()
  let panelStyle = $state('')
  let tone = $derived.by<'danger' | 'warning' | 'neutral'>(() => {
    if (error) return 'warning'
    if (status?.has_sync_error) return 'danger'
    return 'neutral'
  })
  let visible = $derived(open || error || !!(status && (status.enabled || status.has_sync_error)))
  let chipLabel = $derived.by(() => {
    if (error) return 'Sync status unavailable'
    if (!status) return 'Sync status unavailable'
    if (status.has_sync_error) return 'Sync failed'
    if (!status.enabled) return 'Sync disabled'
    return status.pending_push_count > 0 ? `${status.pending_push_count} to sync` : 'Synced'
  })

  // The panel opens on the state the chip names: a failure, with any changes
  // still waiting, comes first; what to run about it comes under it.
  let stateLine = $derived.by(() => {
    if (!status) return ''
    const pending = status.pending_push_count
    if (status.has_sync_error)
      return pending > 0
        ? `Sync failed · ${pending} ${pending === 1 ? 'change' : 'changes'} waiting`
        : 'Sync failed'
    if (!status.enabled) return 'Sync disabled'
    if (pending > 0) return `${pending} ${pending === 1 ? 'change' : 'changes'} waiting to sync`
    return 'Synced'
  })

  // Who this project syncs with, by the hub's host rather than a role.
  let hubLine = $derived.by(() => {
    if (!status) return ''
    if (status.role === 'hub') return 'Other servers sync with this one'
    if (!status.hub_url) return 'Syncs with a hub'
    try {
      return `Syncs with ${new URL(status.hub_url).host}`
    } catch {
      return 'Syncs with a hub'
    }
  })

  // "12m" alone reads as a duration; say it is in the past.
  function ago(iso: string): string {
    const text = relativeTime(iso, new Date())
    if (text === 'now') return 'Just now'
    return /^\d+[mhd]$/.test(text) ? `${text} ago` : text
  }

  // Stored values ("stored_unindexed", "active") read as words.
  function words(value: string): string {
    const text = value.replaceAll('_', ' ')
    return text.charAt(0).toUpperCase() + text.slice(1)
  }

  // Only the counts that aren't zero, in one line.
  let artifactSummary = $derived(
    [
      ['generated', 'generated'],
      ['reused', 'reused'],
      ['incompatible', 'incompatible'],
      ['stored_unindexed', 'without index'],
    ]
      .filter(([state]) => (counts[state!] ?? 0) > 0)
      .map(([state, label]) => `${counts[state!]} ${label}`)
      .join(' · '),
  )

  $effect(() => {
    if (!open) return
    const cleanups = [
      dismissable({
        owners: () => [trigger, panel],
        dismiss: () => {
          open = false
        },
        escapeFocus: () => trigger?.querySelector('button'),
      }),
      autoReposition(() => [panel], position),
    ]
    return () => cleanups.forEach((cleanup) => cleanup())
  })

  function position(): void {
    if (!trigger || !panel) return
    panelStyle = floatingPopoverStyle({
      trigger: trigger.getBoundingClientRect(),
      viewportWidth: window.innerWidth,
      viewportHeight: window.innerHeight,
      popoverWidth: panel.offsetWidth,
      popoverHeight: panel.offsetHeight,
      align: 'start',
    })
  }

  async function toggle(): Promise<void> {
    open = !open
    if (!open) return
    await tick()
    position()
  }

  function resetView(): void {
    open = false
    status = undefined
    loading = true
    error = false
    reading = false
  }

  $effect(() => {
    void identity
    const [id, uid] = untrack(() => [projectID, projectUID] as const)
    let current: AbortController | undefined
    let disposed = false
    // Coalesce refreshes during a read into one follow-up.
    let pending = false
    resetView()

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
      loading = manual || (loading && !error)
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
      abort.signal.addEventListener('abort', () => clearTimeout(timeout), { once: true })
      try {
        const result = await getProjectFederationStatus(
          { projectId: id },
          { compact: true, omit_errors: true },
          {
            signal: abort.signal,
            onResponse: (raw) => {
              if (raw.status >= 400 && raw.status < 500 && !abort.signal.aborted && !disposed) {
                status = undefined
                loading = true
              }
            },
          },
        )
        if (abort.signal.aborted || disposed) return
        if (result.status !== 200) throw new Error()
        const found = result.data.statuses.find(
          (entry) => entry.project_id === id && entry.project_uid === uid,
        )
        if (result.data.statuses.length && !found) throw new Error()
        status = found
        error = false
      } catch {
        if (!abort.signal.aborted && !disposed) {
          status = undefined
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

{#if visible}
  <div class="project-sync" aria-busy={reading}>
    <span
      class="chip-host"
      bind:this={trigger}
      {@attach (host) => host.querySelector('button')?.setAttribute('aria-haspopup', 'dialog')}
    >
      <Chip interactive uppercase={false} {tone} expanded={open} onclick={() => void toggle()}>
        {#if tone === 'danger'}
          <span class="chip-icon danger"><CircleAlertIcon size={14} aria-hidden="true" /></span>
        {:else if tone === 'warning'}
          <span class="chip-icon warning"><TriangleAlertIcon size={14} aria-hidden="true" /></span>
        {/if}{chipLabel}
        {#snippet trailing()}<ChevronDownIcon size={14} aria-hidden="true" />{/snippet}
      </Chip>
    </span>
    {#if open}
      <div
        bind:this={panel}
        class="sync-panel kit-popover-card"
        style={panelStyle}
        role="dialog"
        aria-label="Project sync"
      >
        <header class="sync-panel-header">
          <strong class="sync-panel-title"
            >{#if status?.project_name}<span
                class="project-mark"
                style:background={markColor(status.project_name)}
              ></span>{/if}Project sync</strong
          >
          <button
            type="button"
            class="sync-refresh"
            disabled={loading}
            onclick={() => refresh?.(true)}>Refresh</button
          >
        </header>
        {#if loading}
          <span role="status">Loading sync status…</span>
        {:else if error}
          <span role="status">Sync status unavailable. Refresh to try again.</span>
        {:else if !status}
          <span>Not synced with a hub</span>
        {:else}
          <div class="sync-state">
            <strong role="status">{stateLine}</strong>
            {#if status.enabled}
              <span>{hubLine}</span>
            {/if}
          </div>
          {#if status.has_sync_error}
            {@const bashCommand = `kata federation status --project '${status.project_name.replaceAll("'", "'\\''")}'`}
            {@const powershellName = status.project_name.replace(/['‘’‚‛]/g, '$&$&')}
            {@const legacyName = powershellName.replace(/(\\*)"/g, '$1$1\\"')}
            {@const powershellCommand =
              legacyName === powershellName
                ? `kata federation status --project '${powershellName}'`
                : `kata federation status --project $(if ($PSVersionTable.PSVersion -lt [version]'7.3' -or $PSNativeCommandArgumentPassing -eq 'Legacy') { '${legacyName}' } else { '${powershellName}' })`}
            <div class="sync-attention">
              <span>To see the error, run this on the server's machine:</span>
              {#if bashCommand === powershellCommand}
                <div class="command">
                  <code>{bashCommand}</code>
                  <CopyButton
                    text={bashCommand}
                    ariaLabel="Copy command"
                    copiedAriaLabel="Copied command"
                  />
                </div>
              {:else}
                <span class="shell">Bash/Zsh</span>
                <div class="command">
                  <code>{bashCommand}</code>
                  <CopyButton
                    text={bashCommand}
                    ariaLabel="Copy Bash/Zsh command"
                    copiedAriaLabel="Copied Bash/Zsh command"
                  />
                </div>
                <span class="shell">PowerShell</span>
                <div class="command">
                  <code>{powershellCommand}</code>
                  <CopyButton
                    text={powershellCommand}
                    ariaLabel="Copy PowerShell command"
                    copiedAriaLabel="Copied PowerShell command"
                  />
                </div>
              {/if}
              <span class="sync-note"
                >Needs a Kata CLI newer than 0.23.0 unless you own this server.</span
              >
            </div>
          {/if}
          {#if status.last_successful_sync_at || status.credential_status || status.embedding}
            <dl class="sync-facts">
              {#if status.last_successful_sync_at}
                <dt>Last sync</dt>
                <dd>
                  <time
                    datetime={status.last_successful_sync_at}
                    title={new Date(status.last_successful_sync_at).toLocaleString()}
                    >{ago(status.last_successful_sync_at)}</time
                  >
                </dd>
              {/if}
              {#if status.credential_status}
                <dt>Credential</dt>
                <dd>{words(status.credential_status)}</dd>
              {/if}
              {#if status.embedding}
                <dt>Embeddings</dt>
                <dd>{words(status.embedding.state)}</dd>
                <dt>Producer</dt>
                <dd>
                  {#if status.embedding.producer}
                    <code>{status.embedding.producer.producer_instance_uid}</code>
                  {:else}
                    None selected
                  {/if}
                </dd>
                {#if status.embedding.producer}
                  <dt>Model</dt>
                  <dd>
                    {status.embedding.producer.recipe.model}, {status.embedding.producer.recipe
                      .dimensions} dimensions
                  </dd>
                {/if}
                {#if artifactSummary}
                  <dt>Stored</dt>
                  <dd>{artifactSummary}</dd>
                {/if}
                <dt>Retained</dt>
                <dd>
                  {status.embedding.artifacts.length} of {status.embedding.artifact_limit} embeddings{status
                    .embedding.limited
                    ? ', limited'
                    : ''}
                </dd>
              {/if}
            </dl>
          {/if}
        {/if}
      </div>
    {/if}
  </div>
{/if}

<style>
  .project-sync {
    display: inline-flex;
    min-width: 0;
  }
  .chip-host {
    display: inline-flex;
    min-width: 0;
  }
  /* The label reads as a sentence; on a failure only the icon carries the tone. */
  .chip-host :global(.kit-chip) {
    font-weight: var(--font-weight-medium);
  }
  .chip-host :global(.kit-chip--tone-danger),
  .chip-host :global(.kit-chip--tone-warning) {
    color: var(--text-primary);
  }
  .chip-icon {
    margin-inline-end: var(--space-2);
  }
  .chip-icon.danger {
    color: var(--accent-red);
  }
  .chip-icon.warning {
    color: var(--accent-amber);
  }
  .sync-panel {
    position: fixed;
    z-index: var(--z-popover);
    display: flex;
    flex-direction: column;
    gap: var(--space-5);
    /* As wide as its longest line, from 360px up to 560px; a command longer
       than that scrolls sideways inside its own box. */
    width: max-content;
    min-width: min(360px, calc(100vw - var(--space-6)));
    max-width: min(560px, calc(100vw - var(--space-6)));
    max-height: calc(100vh - var(--space-6));
    overflow-y: auto;
    padding: var(--space-6);
    color: var(--text-secondary);
    font-size: var(--font-size-sm);
    overflow-wrap: anywhere;
  }
  .sync-panel-header {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: var(--space-4);
    color: var(--text-primary);
  }
  .sync-panel-title {
    display: inline-flex;
    align-items: center;
    gap: var(--space-3);
    font-weight: var(--font-weight-semibold);
  }
  /* The same square mark the sidebar shows beside the project. */
  .project-mark {
    flex: 0 0 10px;
    width: 10px;
    height: 10px;
    border-radius: 3px;
  }
  /* A quiet text button whose label lines up with the panel's right edge. */
  .sync-refresh {
    margin: calc(-1 * var(--space-1)) calc(-1 * var(--space-3));
    padding: var(--space-1) var(--space-3);
    border-radius: var(--radius-sm);
    color: var(--text-secondary);
  }
  .sync-refresh:hover:not(:disabled) {
    background: var(--bg-surface-hover);
    color: var(--text-primary);
  }
  .sync-refresh:disabled {
    cursor: default;
    color: var(--text-muted);
  }
  .sync-panel span {
    min-width: 0;
  }
  .sync-state {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
    color: var(--text-primary);
  }
  .sync-state strong {
    font-weight: var(--font-weight-medium);
  }
  .sync-attention {
    display: flex;
    flex-direction: column;
    gap: var(--space-3);
    min-width: 0;
    color: var(--text-primary);
  }
  .sync-note {
    color: var(--text-secondary);
    font-size: var(--font-size-xs);
  }
  .sync-facts {
    display: grid;
    grid-template-columns: max-content minmax(0, 1fr);
    gap: var(--space-2) var(--space-6);
    margin: 0;
    padding-top: var(--space-5);
    border-top: 1px solid var(--border-default);
  }
  .sync-facts dt {
    color: var(--text-secondary);
  }
  .sync-facts dd {
    margin: 0;
    min-width: 0;
    color: var(--text-primary);
  }
  .sync-facts code {
    font-size: var(--font-size-xs);
  }
  /* The command sits in an inset box and scrolls sideways inside it. */
  .command {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    min-width: 0;
    padding: var(--space-2) var(--space-2) var(--space-2) var(--space-4);
    border-radius: var(--radius-md);
    background: var(--bg-inset);
  }
  .shell {
    margin-bottom: calc(-1 * var(--space-1));
    color: var(--text-secondary);
    font-size: var(--font-size-xs);
  }
  .shell ~ .shell {
    margin-top: var(--space-2);
  }
  code {
    flex: 1;
    font-size: var(--font-size-xs);
    overflow-wrap: anywhere;
    min-width: 0;
  }
  .command code {
    overflow-x: auto;
    overflow-wrap: normal;
    white-space: pre;
  }
</style>
