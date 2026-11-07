<script lang="ts">
  import ArrowLeftIcon from '@lucide/svelte/icons/arrow-left'
  import {
    PageHeader,
    SearchByMeaningRow,
    SettingsSection,
    type SearchByMeaningState,
  } from '@kenn-io/kit-ui'
  import { untrack } from 'svelte'

  import { orvalFetch } from '../lib/api/client'
  import {
    getLocalEmbeddings,
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
  }

  let { canAdministerDaemon, onback, postFetch, remoteHost = undefined }: Props = $props()

  const pollMs = 2000
  // The design's estimate: 48,000 issues rebuild in about 20 minutes.
  const rebuildMsPerIssue = 25
  // kit-ui's rebuild line, plus the replica warning `kata daemon local-embeddings` prints.
  const labels = {
    rebuild:
      'Switching rebuilds the index, {time} for {count} {items}. Daemons sharing embeddings need the same model and settings before reusing them.',
  }
  // Non-owners get indexing without its counts, so the row's one state with
  // no progress bar says only that it's turning on.
  const turningOn: SearchByMeaningState = { kind: 'downloaded' }
  const turningOnLabels = { off: 'Turning on', downloaded: 'Keyword search works meanwhile' }
  // A failure replaces the row's own line, so it reads first and nothing
  // promises a switch the daemon refused.
  const failedLabels = { ...labels, download: '', downloadToHost: '', keepsIndex: '', rebuild: '' }
  const megabytes = new Intl.NumberFormat(undefined, {
    style: 'unit',
    unit: 'megabyte',
    unitDisplay: 'short',
    maximumFractionDigits: 0,
  })
  const requestFailure = "Couldn't confirm Turn on with the daemon. This page keeps checking."

  let status = $state<LocalEmbeddingsStatus>()
  let loading = $state(true)
  let posting = $state(false)
  // Set when a Turn on request fails or its response is lost; a read showing
  // a job or the built-in server settles what happened and clears it.
  let requestError = $state<string>()
  // Aborted when the page goes away.
  let page: AbortController | undefined
  // Aborted when Turn on starts, so a read answering before admission can't
  // land, and when the page goes away.
  let read: AbortController | undefined
  let timer: ReturnType<typeof setTimeout> | undefined

  let job = $derived(status?.job)
  let base = $derived(status ? rowState(status) : undefined)
  let countFree = $derived(base?.kind === 'indexing' && !base.total)
  let row = $derived(countFree ? turningOn : base)
  let count = $derived(
    row?.kind === 'indexing' ? row.total : row?.kind === 'ollama-other' ? row.itemCount : 0,
  )
  let failure = $derived(requestError ?? (job?.state === 'failed' ? job.error : undefined))
  let switchTarget = $derived(
    remoteHost ? `once to ${remoteHost}` : 'once, shared by all Kenn apps',
  )
  // kit-ui's Off line, for the files a switch from Ollama fetches when it's
  // approved, and below a failed setup in place of the row's own, unless the
  // failure is a setup the daemon can't change.
  let switchDownload = $derived(
    (failure
      ? status?.can_change && /^(off|ollama)/.test(row?.kind ?? '')
      : row?.kind.startsWith('ollama')) && status?.download_bytes
      ? `Downloads ${megabytes.format(status.download_bytes / 1_000_000)} ${switchTarget}`
      : undefined,
  )
  // Indexing keeps polling while failed, so the note clears once a pass succeeds.
  let indexingFailed = $derived(status?.indexing?.failed === true)

  // Count-free indexing borrows a state that offers Turn on, so it never shows a button.
  let readonly = $derived(countFree || !(status?.can_change && canAdministerDaemon))

  $effect(() => {
    const abort = new AbortController()
    page = abort
    // Reads state such as `posting`, which mustn't restart the page.
    untrack(() => void refresh())
    return () => {
      abort.abort()
      read?.abort()
      clearTimeout(timer)
    }
  })

  // A failed Ollama pass keeps the switch, since a broken Ollama setup is what fails it.
  function rowState(current: LocalEmbeddingsStatus): SearchByMeaningState {
    const job = current.job
    if (job?.state === 'downloading' || job?.state === 'restarting')
      return { kind: 'downloading', doneBytes: job.downloaded, totalBytes: job.total }
    if (current.indexing && !(current.indexing.failed && current.provider === 'ollama'))
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
        return current.rebuild
          ? {
              kind: 'ollama-other',
              itemCount: current.issues ?? 0,
              rebuildMs: (current.issues ?? 0) * rebuildMsPerIssue,
            }
          : { kind: 'ollama' }
      default:
        return { kind: 'on' }
    }
  }

  // Polling ends once the built-in server is in place with nothing left to do.
  function finished(current: LocalEmbeddingsStatus | undefined): boolean {
    return current?.provider === 'built_in' && !current.job && !current.indexing
  }

  // Adopts every successful read and keeps the last one through failed reads,
  // so the row holds its state while the daemon restarts.
  async function refresh(): Promise<void> {
    clearTimeout(timer)
    if (!page || page.signal.aborted || posting) return
    read?.abort()
    const abort = new AbortController()
    read = abort
    const signal = abort.signal
    let next: LocalEmbeddingsStatus | undefined
    try {
      const response = await getLocalEmbeddings({ signal })
      if (response.status === 200) next = response.data
    } catch {
      // A restarting daemon refuses connections; the next poll retries.
    }
    if (signal.aborted) return
    read = undefined
    if (next) {
      status = next
      if (next.job || next.provider === 'built_in') requestError = undefined
    }
    loading = false
    poll()
  }

  function poll(): void {
    clearTimeout(timer)
    if (page?.signal.aborted || posting || finished(status)) return
    timer = setTimeout(() => void refresh(), pollMs)
  }

  async function turnOn(): Promise<void> {
    if (posting || !page) return
    const signal = page.signal
    posting = true
    // The POST inspects status before admitting setup, so a read in flight
    // could answer Off after the daemon accepts; drop it and hold the timer.
    clearTimeout(timer)
    read?.abort()
    read = undefined
    let accepted: LocalEmbeddingsStatus | undefined
    try {
      const response = await orvalFetch<turnOnLocalEmbeddingsResponse>(
        getTurnOnLocalEmbeddingsUrl(),
        { method: 'POST', signal },
        postFetch,
      )
      if (response.status === 202) accepted = response.data
    } catch {
      // Refused or lost as the daemon restarts; later reads show what happened.
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
    <SettingsSection title="Search">
      {#if row}
        <div class="search-row">
          <!-- Disables Turn on while its request is in flight. -->
          <fieldset class="row-actions" disabled={posting}>
            <SearchByMeaningRow
              state={row}
              itemNoun={count === 1 ? 'issue' : 'issues'}
              labels={countFree ? turningOnLabels : failure ? failedLabels : labels}
              {readonly}
              onturnon={turnOn}
              onusebuiltin={turnOn}
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
          {#if switchDownload}
            <p class="settings-note">{switchDownload}</p>
          {/if}
        </div>
      {:else if loading}
        <p class="settings-note" role="status">Loading…</p>
      {:else}
        <p class="settings-note" role="status">
          Search by meaning status is unavailable for this daemon.
        </p>
      {/if}
    </SettingsSection>
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
    gap: var(--space-3);
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

  @media (max-width: 760px) {
    .search-settings {
      padding: var(--space-4);
    }
  }
</style>
