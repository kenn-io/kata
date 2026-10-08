<script lang="ts">
  import { tick } from 'svelte'
  import CommentRelations from './CommentRelations.svelte'
  import { Button, Markdown } from '@kenn-io/kit-ui'
  import {
    formatRelativeTime as timeAgo,
    formatTimestamp as localDateTimeLabel,
  } from '@kenn-io/kit-ui/utils/time'

  import type {
    KataIssueDetailModel,
    KataIssueDetailProps,
    KataReplyKind,
    KataCommentLink,
  } from '../types.js'

  interface Props {
    issueUID: string
    comments: KataIssueDetailModel['comments']
    onOpenComment?: KataIssueDetailProps['onOpenComment']
    onReplyComment?: KataIssueDetailProps['onReplyComment']
    actionsDisabled?: boolean
    selectedCommentUID?: string | undefined
  }

  let {
    comments,
    issueUID,
    onOpenComment,
    onReplyComment,
    actionsDisabled = false,
    selectedCommentUID,
  }: Props = $props()
  let listElement = $state<HTMLOListElement>()
  let requestedCommentUID: string | undefined
  let focusedCommentUID: string | undefined
  $effect(() => {
    const uid = selectedCommentUID
    const list = listElement
    const available = comments.some((comment) => comment.uid === uid)
    if (requestedCommentUID !== uid) {
      requestedCommentUID = uid
      focusedCommentUID = undefined
    }
    if (!uid || !list || !available || focusedCommentUID === uid) return
    void tick().then(() => {
      if (selectedCommentUID !== uid || listElement !== list || focusedCommentUID === uid) return
      const target = [...list.querySelectorAll<HTMLElement>('li')].find(
        (item) => item.id === `comment-${uid}`,
      )
      if (!target) return
      target.scrollIntoView?.({ block: 'nearest' })
      target.focus({ preventScroll: true })
      focusedCommentUID = uid
    })
  })
  const kinds: KataReplyKind[] = ['reply', 'confirm', 'refute', 'supersede']
  function linkLabel(link: KataCommentLink): string {
    return `${({ reply: 'Replies to', confirm: 'Confirms', refute: 'Refutes', supersede: 'Supersedes' } as Record<string, string>)[link.kind] ?? link.kind} ${link.handle ?? `(${link.status ?? 'unresolved'})`}${link.author ? ` (${link.author}${link.teammate ? ` / ${link.teammate}` : ''})` : ''}${link.status && link.handle ? ` (${link.status})` : ''}`
  }
  function open(link: KataCommentLink, sourceUID?: string): void {
    if (link.issue_uid && link.uid)
      onOpenComment?.(
        link.issue_uid,
        link.uid,
        sourceUID ? { issueUID, commentUID: sourceUID } : undefined,
      )
  }
</script>

<section class="detail-section" aria-labelledby="kata-comments-heading">
  <h3 id="kata-comments-heading">
    Comments{#if comments.length > 0}<span class="count">{comments.length}</span>{/if}
  </h3>
  {#if comments.length === 0}
    <p>No comments.</p>
  {:else}
    <ol bind:this={listElement}>
      {#each comments as comment (comment.id)}
        <li
          id={comment.uid ? `comment-${comment.uid}` : undefined}
          class:selected={selectedCommentUID !== undefined && comment.uid === selectedCommentUID}
          tabindex="-1"
        >
          <header>
            {#if comment.handle}<span class="handle">{comment.handle}</span>{/if}
            {#if comment.editedAt}<span title={localDateTimeLabel(comment.editedAt)}>(edited)</span
              >{/if}
            <span class="avatar" aria-hidden="true">{comment.author.slice(0, 1)}</span>
            <strong
              >{comment.teammate
                ? `${comment.author} / ${comment.teammate}`
                : comment.author}</strong
            >
            <time datetime={comment.createdAt} title={localDateTimeLabel(comment.createdAt)}
              >{timeAgo(comment.createdAt)}</time
            >
          </header>
          {#if comment.reply}
            <div class="comment-link">
              ↳ <Button
                size="sm"
                label={linkLabel(comment.reply)}
                disabled={!onOpenComment || !comment.reply.issue_uid || !comment.reply.uid}
                onclick={() => open(comment.reply!, comment.uid)}
              />
              {#if comment.reply.target_edited}<span>Target edited after this reply</span>{/if}
            </div>
          {/if}
          <Markdown source={comment.body} class="comment-body" />
          <CommentRelations
            links={comment.backlinks ?? []}
            partial={comment.backlinksPartial ?? false}
            onOpen={onOpenComment ? (link) => open(link, comment.uid) : undefined}
          />
          {#if onReplyComment && comment.uid && comment.handle}
            <div class="reply-actions">
              {#each kinds as kind}<Button
                  size="sm"
                  label={kind[0]!.toUpperCase() + kind.slice(1)}
                  disabled={actionsDisabled}
                  onclick={() => onReplyComment?.(comment.uid!, kind)}
                />{/each}
            </div>
          {/if}
        </li>
      {/each}
    </ol>
  {/if}
</section>

<style>
  li.selected {
    background: var(--bg-inset, #ecedf2);
    outline: 1px solid var(--border-muted, #e2e4e8);
  }
  .comment-link,
  .reply-actions {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: var(--space-3, 6px);
    margin-block: var(--space-3, 6px);
  }
  .handle {
    font-family: var(--font-mono, monospace);
  }
  .detail-section {
    min-width: 0;
  }

  h3 {
    margin: 0 0 var(--space-2, 4px);
    color: var(--text-primary, #202124);
    font-size: var(--font-size-md, 0.8125rem);
    font-weight: var(--font-weight-semibold, 600);
  }

  .count {
    margin-left: var(--space-3, 6px);
    color: var(--text-muted, #656a73);
    font-weight: var(--font-weight-medium, 500);
  }

  ol {
    margin: 0;
    padding: 0;
    list-style: none;
  }

  li {
    padding: var(--space-5, 12px) 0;
  }

  li + li {
    border-top: 1px solid var(--border-muted, #e2e4e8);
  }

  header {
    display: flex;
    align-items: center;
    gap: var(--space-4, 8px);
    margin-bottom: var(--space-3, 6px);
    font-size: var(--font-size-sm, 0.75rem);
  }

  .avatar {
    display: inline-grid;
    flex: 0 0 auto;
    place-items: center;
    width: 20px;
    height: 20px;
    border-radius: 50%;
    background: var(--bg-inset, #ecedf2);
    color: var(--text-secondary, #555b6e);
    font-size: var(--font-size-2xs, 0.625rem);
    font-weight: var(--font-weight-semibold, 600);
    text-transform: uppercase;
  }

  strong {
    font-weight: var(--font-weight-semibold, 600);
  }

  time,
  p {
    color: var(--text-muted, #656a73);
  }

  p {
    margin: 0;
  }

  /* Indent the body under the author so the avatar column stays clear. */
  li > :global(.comment-body) {
    padding-left: 28px;
  }
</style>
