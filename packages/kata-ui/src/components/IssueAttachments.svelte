<script lang="ts">
  import { Button, ImagePreview } from '@kenn-io/kit-ui'
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
  let previousFetcher: Props['fetchAttachment']
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
    if (baseURL !== previousBaseURL) untrack(clearUploads)
    if (baseURL !== previousBaseURL || fetchAttachment !== previousFetcher) untrack(clearPreviews)
    else
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
    previousFetcher = fetchAttachment
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
    if (!fetcher || actionsDisabled || !attachmentsEnabled || loading.get(entry.id)) return
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
            <span>{entry.name}</span>
            <span class="description">{entry.media_type} · {entry.size} bytes</span>
            {#if attachmentsEnabled}
              {#if entry.size > 100 << 20}
                <code>kata docbank attachments get &lt;issue&gt; {entry.id}</code>
              {:else}
                {#if entry.image && !previews.has(entry.id)}
                  <Button
                    size="sm"
                    label={`Preview ${entry.name}`}
                    disabled={actionsDisabled || !fetchAttachment || Boolean(loading.get(entry.id))}
                    onclick={() => load(entry, true)}
                  />
                {/if}
                {#if entry.video && !previews.has(entry.id)}
                  <Button
                    size="sm"
                    label={`Play ${entry.name}`}
                    disabled={actionsDisabled || !fetchAttachment || Boolean(loading.get(entry.id))}
                    onclick={() => load(entry, true)}
                  />
                {/if}
                <Button
                  size="sm"
                  label={`Download ${entry.name}`}
                  disabled={actionsDisabled || !fetchAttachment || Boolean(loading.get(entry.id))}
                  onclick={() => load(entry, false)}
                />
              {/if}
            {/if}
            {#if states.has(entry.id)}<span role="status">{states.get(entry.id)}</span>{/if}
            {#if previews.has(entry.id)}
              <div class="preview">
                {#if entry.video}
                  <!-- svelte-ignore a11y_media_has_caption (User attachments may not include captions.) -->
                  <video
                    controls
                    preload="metadata"
                    src={previews.get(entry.id)}
                    aria-label={entry.name}
                  ></video>
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
  .preview {
    width: 100%;
  }
  video {
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
    gap: var(--space-4);
    padding: 0;
    list-style: none;
  }
  li {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: var(--space-4);
  }
  .description {
    color: var(--text-muted);
    font-size: var(--font-size-sm);
  }
</style>
