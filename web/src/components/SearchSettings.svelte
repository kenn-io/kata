<script lang="ts">
  import ArrowLeftIcon from '@lucide/svelte/icons/arrow-left'
  import { PageHeader, SearchByMeaningRow, type SearchByMeaningState } from '@kenn-io/kit-ui'
  import { untrack } from 'svelte'

  import { orvalFetch } from '../lib/api/client'
  import {
    getLocalEmbeddings,
    getRestartLocalEmbeddingsUrl,
    getTurnOnLocalEmbeddingsUrl,
    type LocalEmbeddingsStatus,
    type turnOnLocalEmbeddingsResponse,
  } from '../lib/api/generated'

  interface Props {
    canAdministerDaemon: boolean
    onback: () => void
    /**
     * Sends Turn on to the daemon serving this page, bypassing the daemon
     * switcher's proxy, which refuses it, with the same sign-in path as reads.
     */
    postFetch: typeof fetch
    /** Host the built-in server downloads to, when the daemon isn't on this computer. */
    remoteHost?: string | undefined
    originStable?: boolean
  }

  let {
    canAdministerDaemon,
    onback,
    postFetch,
    remoteHost = undefined,
    originStable = false,
  }: Props = $props()

  const pollMs = 2000
  const restartWaitMs = 120_000
  const reopenGuidance =
    'After restart, run kata ui to reopen. If this daemon uses a custom home, set KATA_HOME to it first.'
  const recoveryGuidance = 'Run kata daemon status, then reopen kata ui.'
  // Ollama shows its status only; switching to the built-in server comes later.
  const labels = { keepsIndex: '' }
  // Count-free indexing and a saved setting waiting on a restart use the
  // row's one state with no progress bar, so they say only that it's turning on.
  const turningOn: SearchByMeaningState = { kind: 'downloaded' }
  const turningOnLabels = { off: 'Turning on', downloaded: 'Keyword search works meanwhile' }
  const requestFailure = "Couldn't confirm Turn on with the daemon."

  let status = $state<LocalEmbeddingsStatus>()
  let loading = $state(true)
  let posting = $state(false)
  let restartRequestPending = false
  let restartDeadline = $state(0)
  let restartPending = $derived(restartDeadline > 0)
  // Set when a Turn on request fails; a read showing a job clears it.
  let requestError = $state<string>()
  // Aborted when the page goes away.
  let page: AbortController | undefined
  // Fences reads across setup requests and settled restart responses.
  let posts = 0
  let timer: ReturnType<typeof setTimeout> | undefined

  let job = $derived(status?.job)
  let restarting = $derived(job?.state === 'restart')
  let base = $derived(status ? rowState(status) : undefined)
  let countFree = $derived(base?.kind === 'indexing' && !base.total)
  let turning = $derived(restarting || countFree)
  let row = $derived(turning ? turningOn : base)
  let count = $derived(row?.kind === 'indexing' ? row.total : 0)
  let restartLabels = $derived({
    off: restartPending || status?.restart_in_progress ? 'Restarting...' : 'Turning on',
    turnOn: 'Restart daemon',
    downloaded: status?.restart_in_progress
      ? ''
      : remoteHost
        ? `Restart the daemon on ${remoteHost} to finish.`
        : (status?.can_restart && canAdministerDaemon) || restartPending
          ? originStable
            ? ''
            : reopenGuidance
          : 'Restart the daemon to finish: run kata daemon restart. If this daemon uses a custom home, set KATA_HOME to it first.',
  })
  let failure = $derived(
    requestError ??
      (!restartPending && (job?.state === 'failed' || job?.state === 'restart')
        ? job.error
        : undefined),
  )
  // Indexing keeps polling while failed, so the note clears once a pass succeeds.
  let indexingFailed = $derived(status?.indexing?.failed === true)

  // The borrowed state offers an action only when restart is available.
  let readonly = $derived(
    restarting
      ? !(status?.can_restart && canAdministerDaemon && !remoteHost && !restartPending)
      : turning || !(status?.can_change && canAdministerDaemon),
  )

  $effect(() => {
    const abort = new AbortController()
    page = abort
    // Reads state such as `posting`, which mustn't restart the page.
    untrack(() => void refresh())
    return () => {
      abort.abort()
      clearTimeout(timer)
    }
  })

  function rowState(current: LocalEmbeddingsStatus): SearchByMeaningState {
    const job = current.job
    if (job?.state === 'downloading')
      return { kind: 'downloading', doneBytes: job.downloaded, totalBytes: job.total }
    if (current.indexing)
      return {
        kind: 'indexing',
        done: current.indexing.done,
        total: current.indexing.total,
        ...(current.indexing.eta_seconds == null
          ? {}
          : { etaMs: current.indexing.eta_seconds * 1000 }),
      }
    switch (current.provider) {
      case 'none':
        return current.ollama_available || current.download_bytes === 0
          ? { kind: 'downloaded' }
          : {
              kind: 'off',
              downloadBytes: current.download_bytes,
              ...(remoteHost ? { host: remoteHost } : {}),
            }
      case 'ollama':
        return { kind: 'ollama' }
      default:
        return { kind: 'on' }
    }
  }

  // Polling runs only while something is under way.
  function active(current: LocalEmbeddingsStatus | undefined): boolean {
    const state = current?.job?.state
    return state === 'downloading' || state === 'restart' || !!current?.indexing
  }

  // Adopts every successful read and keeps the last one through failed reads.
  async function refresh(): Promise<void> {
    clearTimeout(timer)
    if (!page || page.signal.aborted || posting) return
    const signal = page.signal
    const readSignal = AbortSignal.any([signal, AbortSignal.timeout(pollMs * 5)])
    const post = posts
    let next: LocalEmbeddingsStatus | undefined
    try {
      const response = await getLocalEmbeddings({ signal: readSignal })
      if (!readSignal.aborted && response.status === 200) next = response.data
    } catch {
      // Keep polling while setup is under way.
    }
    if (signal.aborted || post !== posts) return
    if (next) {
      if (
        (next.job?.error ?? next.restart_in_progress) ||
        next.job?.state !== status?.job?.state ||
        next.provider !== status?.provider ||
        next.indexing
      )
        requestError = undefined
      if (next.restart_in_progress) restartDeadline = Date.now() + restartWaitMs
      if (
        restartPending &&
        (next.job?.state !== 'restart' ||
          ((next.job?.error || next.can_restart) &&
            (!restartRequestPending || status?.restart_in_progress)))
      ) {
        restartDeadline = 0
        posts++
        if (next.can_restart && !next.job?.error)
          requestError = "Couldn't confirm restart. Try again or run kata daemon restart."
      }
      status = next
    }
    if (restartPending && !next?.can_restart && Date.now() >= restartDeadline) {
      requestError = remoteHost
        ? `Couldn't reconnect. Run kata daemon status on ${remoteHost}.`
        : originStable
          ? `Couldn't reconnect. ${recoveryGuidance}`
          : 'Run kata daemon status.'
    }
    loading = false
    poll()
  }

  function poll(): void {
    clearTimeout(timer)
    if (page?.signal.aborted || posting || (!restartPending && !active(status))) return
    timer = setTimeout(() => void refresh(), pollMs)
  }

  async function restartDaemon(): Promise<void> {
    if (posting || restartPending || remoteHost || !page) return
    const reopen = originStable ? '' : `\n\n${reopenGuidance}`
    if (
      !confirm(
        `Restart this daemon? All projects and clients on it will disconnect briefly.\n\nIf a service manager runs this daemon, restart it there instead.${reopen}`,
      )
    )
      return
    const signal = page.signal
    restartDeadline = Date.now() + restartWaitMs
    requestError = undefined
    const post = ++posts
    restartRequestPending = true
    poll()
    try {
      const response = await postFetch(getRestartLocalEmbeddingsUrl(), {
        method: 'POST',
        signal: AbortSignal.any([signal, AbortSignal.timeout(pollMs * 5)]),
      })
      if (post === posts && !response.ok) {
        restartDeadline = 0
        requestError = "Couldn't restart the daemon. Try again or run kata daemon restart."
      }
    } catch {
      // A lost response can mean the daemon is restarting.
    } finally {
      if (post === posts) {
        restartRequestPending = false
        posts++
        if (!signal.aborted) poll()
      }
    }
  }

  async function turnOn(): Promise<void> {
    if (posting || !page) return
    const signal = page.signal
    posting = true
    posts++
    clearTimeout(timer)
    let accepted: LocalEmbeddingsStatus | undefined
    try {
      const response = await orvalFetch<turnOnLocalEmbeddingsResponse>(
        getTurnOnLocalEmbeddingsUrl(),
        { method: 'POST', signal },
        postFetch,
      )
      if (response.status === 202) accepted = response.data
    } catch {
      // Refused or lost; one read shows what happened.
    }
    posting = false
    if (signal.aborted) return
    if (accepted) {
      status = accepted
      requestError = undefined
      poll()
    } else {
      requestError = requestFailure
      void refresh()
    }
  }
</script>

<section class="search-settings" aria-label="Settings">
  <div class="settings-content">
    <PageHeader level={2} title="Settings" description="For this daemon and every project on it.">
      {#snippet before()}
        <button type="button" class="back-button" aria-label="Back to issues" onclick={onback}>
          <ArrowLeftIcon size={14} strokeWidth={1.8} aria-hidden="true" />
          <span>Issues</span>
        </button>
      {/snippet}
    </PageHeader>
    {#if row}
      <div class="search-row" class:turning-on={turning}>
        <!-- Disables Turn on while its request is in flight. -->
        <fieldset class="row-actions" disabled={posting}>
          <SearchByMeaningRow
            state={row}
            itemNoun={count === 1 ? 'issue' : 'issues'}
            labels={restarting ? restartLabels : countFree ? turningOnLabels : labels}
            {readonly}
            onturnon={restarting ? restartDaemon : turnOn}
            onusebuiltin={() => {}}
          />
        </fieldset>
        {#if failure}
          <p class="settings-note" role="alert">{failure}</p>
        {/if}
        {#if indexingFailed}
          <p class="settings-note" role="alert">
            Indexing hit an error and will retry. Run <code>kata health --json</code> for details.
          </p>
        {/if}
      </div>
    {:else if loading}
      <p class="settings-note" role="status">Loading…</p>
    {:else}
      <p class="settings-note" role="status">
        Search by meaning status is unavailable for this daemon.
      </p>
    {/if}
  </div>
</section>

<style>
  .search-settings {
    width: 100%;
    min-width: 0;
    min-height: 0;
    overflow: auto;
    padding: var(--space-5);
    background: var(--bg-primary);
  }

  .settings-content {
    display: grid;
    gap: var(--space-5);
    max-width: 880px;
  }

  /* Matches the row's own line gap, so a failure reads as part of the row. */
  .search-row {
    display: grid;
    gap: var(--space-4);
  }

  /* The title's line sets the first row's height in every state, so the
     title holds still and sits the same distance above what follows it. */
  .search-row :global(.kit-search-by-meaning) {
    min-height: 0;
    row-gap: var(--space-4);
  }

  /* Count-free indexing and a pending restart borrow the Off chip, so they
     take the info tone indexing's own Turning on chip wears. */
  .turning-on :global(.kit-chip--tone-muted) {
    background: color-mix(in srgb, var(--accent-blue) 15%, transparent);
    color: color-mix(in srgb, var(--accent-blue) 65%, var(--text-primary));
  }

  @media (min-width: 641px) {
    /* The button centers on the title's line without growing it. */
    .search-row :global(.kit-search-by-meaning__action) {
      display: flex;
      align-items: center;
      height: 0;
    }
  }

  .row-actions {
    min-width: 0;
    margin: 0;
    padding: 0;
    border: 0;
  }

  .back-button {
    display: inline-flex;
    align-items: center;
    gap: var(--space-2);
    padding: 0;
    border: 0;
    background: none;
    color: inherit;
    font: inherit;
    cursor: pointer;
  }

  .back-button:hover {
    color: var(--text-primary);
  }

  .settings-note {
    margin: 0;
    color: var(--text-muted);
    font-size: var(--font-size-xs);
    line-height: 1.45;
  }

  .settings-note[role='alert'] {
    color: var(--accent-red);
  }

  /* An error sits apart from the details above it, but no farther below a
     bare title than any other detail. */
  .row-actions:has(
      :global(.kit-search-by-meaning__warning),
      :global(.kit-search-by-meaning__progress),
      :global(.kit-search-by-meaning__line)
    )
    + [role='alert'] {
    margin-top: var(--space-2);
  }

  @media (max-width: 760px) {
    .search-settings {
      padding: var(--space-4);
    }
  }
</style>
