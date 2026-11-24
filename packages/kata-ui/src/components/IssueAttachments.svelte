<script lang="ts">
  import { Button, CopyButton, ImagePreview, VideoPlayer } from '@kenn-io/kit-ui'
  import { SvelteMap } from 'svelte/reactivity'
  import { onDestroy, untrack } from 'svelte'
  import type { KataAttachment } from '../types.js'

  interface Props {
    attachments?: Record<string, KataAttachment> | undefined
    baseURL: string
    issueRef?: string | undefined
    showEmpty?: boolean | undefined
    fetchAttachment?: ((url: string) => Promise<Blob>) | undefined
    uploadAttachment?: ((file: File) => Promise<void>) | undefined
    attachmentsEnabled?: boolean | undefined
    actionsDisabled?: boolean | undefined
  }
  let {
    attachments = {},
    baseURL,
    issueRef,
    showEmpty = false,
    fetchAttachment,
    uploadAttachment,
    attachmentsEnabled = false,
    actionsDisabled = false,
  }: Props = $props()
  const previews = new SvelteMap<string, string>()
  const states = new SvelteMap<string, string>()
  const loading = new SvelteMap<string, boolean>()
  const uploads = new SvelteMap<File, string>()
  let fileInput: HTMLInputElement | undefined = $state()
  let uploading = $state(false)
  let generation = 0
  let uploadGeneration = 0
  const canUpload = $derived(attachmentsEnabled && Boolean(uploadAttachment))
  const uploadDisabled = $derived(uploading || actionsDisabled)
  const entries = $derived(
    Object.entries(attachments).map(([id, e]) => {
      return {
        ...e,
        id,
        reference: JSON.stringify([
          attachmentsEnabled,
          e.name,
          e.media_type,
          e.size,
          e.docbank_version,
          e.origin ?? '',
        ]),
        image:
          ['image/png', 'image/jpeg', 'image/gif', 'image/webp'].includes(e.media_type) &&
          e.size <= 20 << 20,
        video:
          ['video/mp4', 'video/x-m4v', 'video/quicktime', 'video/webm', 'video/ogg'].includes(
            e.media_type,
          ) && e.size <= 100 << 20,
      }
    }),
  )
  const references = new Map<string, string>()
  let previousBaseURL: string | undefined
  // Quote only what the shell would split, so the usual command reads plainly.
  function shellQuote(value: string): string {
    return /^[\w.#:/@+-]+$/.test(value) ? value : `'${value.replaceAll("'", "'\\''")}'`
  }
  // "64 B · text": size and kind as a person would say them, after the name.
  function formatSize(bytes: number): string {
    if (bytes < 1024) return `${bytes} B`
    const units = ['KB', 'MB', 'GB']
    let value = bytes / 1024
    let unit = 0
    while (value >= 1024 && unit < units.length - 1) {
      value /= 1024
      unit++
    }
    return `${value < 10 ? value.toFixed(1) : Math.round(value)} ${units[unit]}`
  }
  function kindLabel(mediaType: string): string {
    const [type = '', subtype = ''] = (mediaType.toLowerCase().split(';')[0] ?? '')
      .trim()
      .split('/')
    const named: Record<string, string> = {
      'application/pdf': 'PDF',
      'application/json': 'JSON',
      'application/zip': 'ZIP archive',
      'text/markdown': 'Markdown',
      'text/csv': 'CSV',
      'text/html': 'HTML',
    }
    const known = named[`${type}/${subtype}`]
    if (known) return known
    if (type === 'text') return 'text'
    if (type === 'image') return `${subtype.replace(/^svg\+xml$/, 'SVG').toUpperCase()} image`
    if (type === 'video' || type === 'audio') return type
    return subtype ? subtype.replace(/^x-/, '') : 'file'
  }
  function clearPreviews(): void {
    generation++
    for (const url of previews.values()) URL.revokeObjectURL(url)
    previews.clear()
    states.clear()
    loading.clear()
  }
  function clearUploads(): void {
    uploadGeneration++
    uploads.clear()
    uploading = false
  }
  onDestroy(() => {
    clearPreviews()
    clearUploads()
  })
  $effect(() => {
    const next = new Map(entries.map((e) => [e.id, e.reference]))
    if (baseURL !== previousBaseURL) {
      untrack(clearUploads)
      untrack(clearPreviews)
    } else
      untrack(() => {
        for (const [id, reference] of references) {
          if (next.get(id) === reference) continue
          const url = previews.get(id)
          if (url) URL.revokeObjectURL(url)
          previews.delete(id)
          states.delete(id)
          loading.delete(id)
        }
      })
    references.clear()
    for (const [id, reference] of next) references.set(id, reference)
    previousBaseURL = baseURL
  })
  async function attachFiles(): Promise<void> {
    const uploader = uploadAttachment
    if (!canUpload || uploadDisabled || !fileInput || !uploader) return
    const files = Array.from(fileInput.files ?? [])
    fileInput.value = ''
    const accepted = uploadGeneration
    uploading = true
    for (const file of files) uploads.set(file, 'queued')
    for (const file of files) {
      if (accepted !== uploadGeneration) break
      uploads.set(file, 'uploading')
      try {
        await uploader(file)
        if (accepted === uploadGeneration) uploads.delete(file)
      } catch (error) {
        if (accepted === uploadGeneration)
          uploads.set(file, error instanceof Error ? error.message : 'docbank_unreachable')
      }
    }
    if (accepted === uploadGeneration) uploading = false
  }
  async function load(entry: (typeof entries)[number], preview: boolean): Promise<void> {
    const fetcher = fetchAttachment
    if (!fetcher || !attachmentsEnabled || loading.get(entry.id)) return
    const accepted = generation
    const current = () => accepted === generation && references.get(entry.id) === entry.reference
    loading.set(entry.id, true)
    states.delete(entry.id)
    try {
      const blob = await fetcher(`${baseURL}/${encodeURIComponent(entry.id)}/content`)
      if (!current()) return
      const url = URL.createObjectURL(preview ? new Blob([blob], { type: entry.media_type }) : blob)
      if (preview) previews.set(entry.id, url)
      else {
        const link = document.createElement('a')
        link.href = url
        link.download = entry.name
        link.click()
        setTimeout(() => URL.revokeObjectURL(url), 0)
      }
    } catch (error) {
      if (current())
        states.set(entry.id, error instanceof Error ? error.message : 'docbank_unreachable')
    } finally {
      if (current()) loading.delete(entry.id)
    }
  }
</script>

{#if entries.length > 0 || canUpload || showEmpty}
  <section aria-label="Attachments" class="attachments">
    <header>
      <h3>Attachments</h3>
      {#if entries.length > 0}<span class="note">{entries.length}</span>{:else if uploads.size === 0}<span
          class="note">No attachments</span
        >{/if}
      {#if canUpload}
        <Button
          size="sm"
          class="kata-attachments-action"
          label="Attach files"
          disabled={uploadDisabled}
          onclick={() => fileInput?.click()}
        />
        <input
          type="file"
          multiple
          hidden
          bind:this={fileInput}
          disabled={uploadDisabled}
          onchange={() => void attachFiles()}
        />
      {/if}
    </header>
    {#each uploads as [file, state] (file)}<span role="status">{file.name} {state}</span>{/each}
    {#if entries.length > 0}
      <ul>
        {#each entries as entry (entry.id)}
          <li id={`kata-attachment-${encodeURIComponent(entry.id)}`}>
            <div class="file-details">
              <span class="file-name" title={entry.name}>{entry.name}</span>
              <span class="description" title={`${entry.media_type} · ${entry.size} bytes`}
                >{formatSize(entry.size)} · {kindLabel(entry.media_type)}</span
              >
            </div>
            {#if attachmentsEnabled}
              <div class="file-actions">
                {#if entry.size > 100 << 20}
                  {#if issueRef}
                    {@const command = `kata docbank attachments get ${shellQuote(issueRef)} ${shellQuote(entry.id)}`}
                    <code class="get-command">{command}</code>
                    <CopyButton
                      text={command}
                      ariaLabel={`Copy download command for ${entry.name}`}
                    />
                  {:else}
                    <code>kata docbank attachments get &lt;issue&gt; {entry.id}</code>
                  {/if}
                {:else}
                  {#if (entry.image || entry.video) && !previews.has(entry.id)}
                    <Button
                      size="sm"
                      label={entry.video ? 'Play' : 'Preview'}
                      ariaLabel={`${entry.video ? 'Play' : 'Preview'} ${entry.name}`}
                      disabled={!fetchAttachment || Boolean(loading.get(entry.id))}
                      onclick={() => load(entry, true)}
                    />
                  {/if}
                  <Button
                    size="sm"
                    label="Download"
                    ariaLabel={`Download ${entry.name}`}
                    disabled={!fetchAttachment || Boolean(loading.get(entry.id))}
                    onclick={() => load(entry, false)}
                  />
                {/if}
              </div>
            {/if}
            {#if states.has(entry.id)}<span role="status">{states.get(entry.id)}</span>{/if}
            {#if previews.has(entry.id)}
              <div class="preview">
                {#if entry.video}
                  <VideoPlayer src={previews.get(entry.id)!} label={entry.name} />
                {:else}
                  <ImagePreview src={previews.get(entry.id)!} alt={entry.name} maxHeight="160px" />
                {/if}
              </div>
            {/if}
          </li>
        {/each}
      </ul>
    {/if}
  </section>
{/if}

<style>
  .attachments {
    display: grid;
    gap: var(--space-4);
    min-width: 0;
  }
  /* The section's heading line: count or empty text on its baseline, the
     upload action at the right inset. */
  header {
    display: flex;
    align-items: baseline;
    gap: var(--space-4);
    min-height: 28px;
    padding-top: 3px;
  }
  h3 {
    margin: 0;
    font-size: var(--font-size-lg);
    font-weight: var(--font-weight-semibold);
    line-height: 21px;
  }
  .note {
    color: var(--text-secondary);
    font-size: var(--font-size-sm);
    font-variant-numeric: tabular-nums;
  }
  header :global(.kata-attachments-action) {
    align-self: center;
    margin-left: auto;
  }
  .attachments :global(.kit-button) {
    border-color: transparent;
    background: transparent;
    color: var(--text-secondary);
  }
  .attachments :global(.kit-button:hover:not(:disabled)) {
    background: var(--bg-surface-hover);
    color: var(--text-primary);
  }
  ul {
    display: grid;
    gap: var(--space-2);
    margin: 0;
    padding: 0;
    list-style: none;
  }
  li {
    display: grid;
    grid-template-columns: minmax(0, 1fr) auto;
    align-items: center;
    column-gap: var(--space-4);
    row-gap: var(--space-2);
    min-height: 24px;
  }
  .file-details {
    display: flex;
    align-items: baseline;
    column-gap: var(--space-4);
    min-width: 0;
  }
  .file-name {
    min-width: 0;
    overflow: hidden;
    color: var(--text-primary);
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .description {
    flex: 0 0 auto;
    color: var(--text-secondary);
    font-size: var(--font-size-sm);
    white-space: nowrap;
  }
  .file-actions {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    justify-content: flex-end;
    gap: var(--space-2);
    max-width: 100%;
  }
  /* A file too large for the browser shows the command that fetches it on
     its own line; it scrolls sideways rather than crowding the name. */
  .file-actions:has(.get-command) {
    display: contents;
  }
  .get-command {
    grid-column: 1;
    grid-row: 2;
    overflow-x: auto;
    white-space: nowrap;
    padding: var(--space-2) var(--space-4);
    border-radius: var(--radius-md);
    background: var(--bg-inset);
    color: var(--text-secondary);
    font-size: var(--font-size-sm);
  }
  .file-actions:has(.get-command) :global(.kit-copy-btn) {
    grid-column: 2;
    grid-row: 1;
    justify-self: end;
  }
  .preview {
    grid-column: 1 / -1;
    min-width: 0;
  }
  .preview :global(video) {
    max-width: 100%;
    max-height: 320px;
  }
  @container kata-issue-detail (max-width: 440px) {
    li {
      grid-template-columns: minmax(0, 1fr);
    }
    .file-actions {
      justify-content: flex-start;
    }
  }
</style>
