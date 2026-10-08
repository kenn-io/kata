<script lang="ts">
  import { Button, Markdown } from '@kenn-io/kit-ui'
  import { formatTimestamp } from '@kenn-io/kit-ui/utils/time'
  import type { KataCommentLink, KataReplyKind } from '../types.js'

  interface Props {
    links: KataCommentLink[]
    partial?: boolean
    onOpen?: ((link: KataCommentLink) => void) | undefined
  }
  let { links, partial = false, onOpen }: Props = $props()
  let selectedKind = $state<KataReplyKind | undefined>()
  const kinds = ['reply', 'confirm', 'refute', 'supersede'] as const
  const labels = {
    reply: 'Replies',
    confirm: 'Confirmations',
    refute: 'Refutations',
    supersede: 'Superseding replies',
  }
  const evidenceLabels = {
    reply: 'Replies',
    confirm: 'Confirmation replies',
    refute: 'Refutation replies',
    supersede: 'Superseding replies',
  }
  const singular = {
    reply: 'reply',
    confirm: 'confirmation reply',
    refute: 'refutation reply',
    supersede: 'superseding reply',
  }
  const tones = {
    reply: 'neutral',
    confirm: 'success',
    refute: 'danger',
    supersede: 'workflow',
  } as const
  const evidence = $derived(
    links
      .filter((link) => link.kind === selectedKind)
      .toSorted(
        (a, b) =>
          (a.created_at ?? '').localeCompare(b.created_at ?? '') ||
          (a.uid ?? '').localeCompare(b.uid ?? ''),
      ),
  )
  function count(kind: KataReplyKind): number {
    return links.filter((link) => link.kind === kind).length
  }
  function accessibleLabel(kind: KataReplyKind): string {
    const n = count(kind)
    const noun = n === 1 ? singular[kind] : singular[kind].replace(/reply$/, 'replies')
    return `Show ${partial ? 'at least ' : ''}${n} ${noun}`
  }
</script>

{#if links.length || partial}
  <div class="relations" aria-label="Incoming comment relations">
    {#each kinds as kind}
      {#if count(kind)}
        <Button
          size="sm"
          tone={tones[kind]}
          label={`${labels[kind]} ${count(kind)}${partial ? '+' : ''}`}
          ariaLabel={accessibleLabel(kind)}
          ariaExpanded={selectedKind === kind}
          onclick={() => (selectedKind = selectedKind === kind ? undefined : kind)}
        />
      {/if}
    {/each}
  </div>
  {#if partial}<p class="partial">More replies may be available</p>{/if}
  {#if selectedKind && evidence.length}
    <section class="evidence" aria-label={evidenceLabels[selectedKind]}>
      <ol>
        {#each evidence as link (link.uid)}
          <li>
            <header>
              <Button
                size="sm"
                label={link.handle ?? `(${link.status ?? 'unresolved'})`}
                ariaLabel={`Open ${link.handle ?? 'unresolved reply'}`}
                disabled={!onOpen || !link.uid || !link.issue_uid}
                onclick={() => onOpen?.(link)}
              />
              {#if link.author}<strong
                  >{link.author}{link.teammate ? ` / ${link.teammate}` : ''}</strong
                >{/if}
              {#if link.created_at}<time datetime={link.created_at}
                  >{formatTimestamp(link.created_at)}</time
                >{/if}
              {#if link.edited_at}<span>(edited)</span>{/if}
              {#if link.status}<span>({link.status})</span>{/if}
            </header>
            {#if link.target_edited}<p>Target edited after this reply</p>{/if}
            {#if link.body !== undefined}<Markdown source={link.body} />{:else}<p>
                Open reply to read its evidence
              </p>{/if}
          </li>
        {/each}
      </ol>
    </section>
  {/if}
{/if}

<style>
  .relations,
  header {
    display: flex;
    flex-wrap: wrap;
    gap: var(--space-3, 6px);
    align-items: center;
    margin-block: var(--space-3, 6px);
  }
  .evidence {
    margin-block: var(--space-4, 8px);
    border-left: 2px solid var(--border-muted);
    padding-left: var(--space-4, 8px);
    overflow-wrap: anywhere;
  }
  ol {
    list-style: none;
    padding: 0;
    margin: 0;
  }
  li + li {
    border-top: 1px solid var(--border-muted);
  }
  time,
  .partial {
    color: var(--text-muted);
    font-size: var(--font-size-sm);
  }
</style>
