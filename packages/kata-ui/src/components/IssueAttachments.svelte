<script lang="ts">
  import { Button, ImagePreview, VideoPlayer } from '@kenn-io/kit-ui'
  import { SvelteMap } from 'svelte/reactivity'
  import { onDestroy, untrack } from 'svelte'
  import type { KataAttachment } from '../types.js'

  interface Props {
    attachments?: Record<string, KataAttachment> | undefined
    baseURL: string
    fetchAttachment?: ((url: string) => Promise<Blob>) | undefined
    uploadAttachment?: ((file: File) => Promise<void>) | undefined
    attachmentsEnabled?: boolean | undefined
    actionsDisabled?: boolean | undefined
  }
  let {
    attachments = {},
    baseURL,
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
    Object.entries(attachments).map(([id, e]) => ({
      ...e,
      id,
      reference: JSON.stringify([e.name, e.media_type, e.size, e.docbank_version]),
      image:
        ['image/png', 'image/jpeg', 'image/gif', 'image/webp'].includes(e.media_type) &&
        e.size <= 20 << 20,
      video:
        ['video/mp4', 'video/x-m4v', 'video/quicktime', 'video/webm', 'video/ogg'].includes(
          e.media_type,
        ) && e.size <= 100 << 20,
    })),
  )
  const references = new Map<string, string>()
  let previousBaseURL: string | undefined
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

{#if entries.length > 0 || canUpload}
  <section aria-label="Attachments" class="attachments">
    <header>
      <h3>Attachments</h3>
      {#if canUpload}
        <Button
          size="sm"
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
          <li>
            <div class="file-details">
              <span class="file-name">{entry.name}</span>
              <span class="description">{entry.media_type} · {entry.size} bytes</span>
            </div>
            {#if attachmentsEnabled}
              <div class="file-actions">
                {#if entry.size > 100 << 20}
                  <code>kata docbank attachments get &lt;issue&gt; {entry.id}</code>
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
  header {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: var(--space-4);
  }
  .attachments {
    min-width: 0;
  }
  .preview {
    grid-column: 1 / -1;
    min-width: 0;
  }
  .preview :global(video) {
    max-width: 100%;
    max-height: 320px;
  }
  h3 {
    margin: 0;
    font-size: var(--font-size-md);
    font-weight: var(--font-weight-semibold);
  }
  ul {
    display: grid;
    gap: var(--space-6);
    margin: var(--space-4) 0 0;
    padding: 0;
    list-style: none;
  }
  li {
    display: grid;
    grid-template-columns: minmax(0, 1fr) auto;
    align-items: center;
    gap: var(--space-4);
  }
  .file-details {
    display: grid;
    gap: var(--space-1);
    min-width: 0;
    overflow-wrap: anywhere;
  }
  .file-name {
    font-weight: var(--font-weight-medium);
  }
  .file-actions {
    display: flex;
    flex-wrap: wrap;
    justify-content: flex-end;
    gap: var(--space-3);
    max-width: 100%;
  }
  code {
    overflow-wrap: anywhere;
  }
  @container kata-issue-detail (max-width: 440px) {
    li {
      grid-template-columns: minmax(0, 1fr);
    }
    .file-actions {
      justify-content: flex-start;
    }
  }
  .description {
    color: var(--text-muted);
    font-size: var(--font-size-sm);
  }
</style>
