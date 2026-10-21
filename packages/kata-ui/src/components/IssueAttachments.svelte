<script lang="ts">
  import { Button, ImagePreview } from '@kenn-io/kit-ui'
  import { SvelteMap } from 'svelte/reactivity'
  import { onDestroy, untrack } from 'svelte'

  interface Props {
    attachments?: unknown
    baseURL: string
    fetchAttachment?: ((url: string) => Promise<Blob>) | undefined
    attachmentControlsDisabled?: boolean | undefined
    attachmentContextKey?: string | undefined
  }
  let {
    attachments,
    baseURL,
    fetchAttachment,
    attachmentControlsDisabled = false,
    attachmentContextKey = '',
  }: Props = $props()
  const previews = new SvelteMap<string, string>()
  const states = new SvelteMap<string, string>()
  const loading = new SvelteMap<string, boolean>()
  let generation = 0
  const uuid = /^[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}$/i
  function object(value: unknown): Record<string, unknown> {
    return typeof value === 'object' && value !== null && !Array.isArray(value)
      ? (value as Record<string, unknown>)
      : {}
  }
  const entries = $derived(
    Object.entries(object(attachments)).map(([id, raw]) => {
      const e = object(raw),
        d = object(e.docbank)
      const name = typeof e.name === 'string' ? e.name : id
      const type = typeof e.media_type === 'string' ? e.media_type : 'unknown'
      const size = typeof e.size === 'number' ? e.size : 0
      const valid =
        /^[0-9A-HJKMNP-TV-Z]{26}$/.test(id) &&
        typeof e.name === 'string' &&
        name.length > 0 &&
        new TextEncoder().encode(name).length <= 255 &&
        !/[/\\\p{Cc}]/u.test(name) &&
        name !== '.' &&
        name !== '..' &&
        /^[^\s/;]+\/[^\s;]+(?:;.*)?$/.test(type) &&
        Number.isSafeInteger(size) &&
        size >= 0 &&
        typeof d.node_id === 'number' &&
        Number.isSafeInteger(d.node_id) &&
        d.node_id > 0 &&
        typeof d.vault_uid === 'string' &&
        uuid.test(d.vault_uid) &&
        typeof d.content_version_id === 'string' &&
        uuid.test(d.content_version_id) &&
        typeof d.content_sha256 === 'string' &&
        /^[a-f0-9]{64}$/.test(d.content_sha256)
      return {
        id,
        name,
        type,
        size,
        valid,
        image:
          ['image/png', 'image/jpeg', 'image/gif', 'image/webp'].includes(type) && size <= 20 << 20,
      }
    }),
  )
  let previousAttachments: string | undefined
  let previousBaseURL: string | undefined
  let previousFetcher: Props['fetchAttachment']
  let previousContextKey: string | undefined
  function clearPreviews(): void {
    generation++
    for (const url of previews.values()) URL.revokeObjectURL(url)
    previews.clear()
    states.clear()
    loading.clear()
  }
  onDestroy(clearPreviews)
  $effect(() => {
    const identity = JSON.stringify(attachments)
    if (
      identity === previousAttachments &&
      baseURL === previousBaseURL &&
      fetchAttachment === previousFetcher &&
      attachmentContextKey === previousContextKey
    )
      return
    previousAttachments = identity
    previousBaseURL = baseURL
    previousFetcher = fetchAttachment
    previousContextKey = attachmentContextKey
    untrack(clearPreviews)
  })
  async function load(id: string, name: string, preview: boolean): Promise<void> {
    const fetcher = fetchAttachment
    if (!fetcher || loading.get(id)) return
    const accepted = generation
    loading.set(id, true)
    states.delete(id)
    try {
      const blob = await fetcher(`${baseURL}/${encodeURIComponent(id)}/content`)
      if (accepted !== generation) return
      if (blob.size > 100 << 20) throw new Error('attachment_too_large')
      const url = URL.createObjectURL(blob)
      if (preview) previews.set(id, url)
      else {
        const link = document.createElement('a')
        link.href = url
        link.download = name
        link.click()
        setTimeout(() => URL.revokeObjectURL(url), 0)
      }
    } catch (error) {
      if (accepted === generation)
        states.set(id, error instanceof Error ? error.message : 'docbank_unreachable')
    } finally {
      if (accepted === generation) loading.delete(id)
    }
  }
</script>

{#if entries.length > 0}
  <section aria-label="Attachments" class="attachments">
    <h3>Attachments</h3>
    <ul>
      {#each entries as entry (entry.id)}
        <li>
          <span>{entry.name}</span>
          <span class="description">{entry.type} · {entry.size} bytes</span>
          {#if !entry.valid}<span>invalid</span>
          {:else if entry.size > 100 << 20}<code
              >kata docbank attachments get &lt;issue&gt; {entry.id}</code
            >
          {:else}
            {#if entry.image && !previews.has(entry.id)}<Button
                size="sm"
                label={`Preview ${entry.name}`}
                disabled={
                  attachmentControlsDisabled ||
                  !fetchAttachment ||
                  Boolean(loading.get(entry.id))
                }
                onclick={() => load(entry.id, entry.name, true)}
              />{/if}
            <Button
              size="sm"
              label={`Download ${entry.name}`}
              disabled={
                attachmentControlsDisabled ||
                !fetchAttachment ||
                Boolean(loading.get(entry.id))
              }
              onclick={() => load(entry.id, entry.name, false)}
            />
          {/if}
          {#if states.has(entry.id)}<span role="status">{states.get(entry.id)}</span>{/if}
          {#if previews.has(entry.id)}<ImagePreview
              src={previews.get(entry.id)!}
              alt={entry.name}
              maxHeight="160px"
            />{/if}
        </li>
      {/each}
    </ul>
  </section>
{/if}

<style>
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
