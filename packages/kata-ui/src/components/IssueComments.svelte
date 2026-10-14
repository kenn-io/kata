<script lang="ts">
  import { Markdown } from '@kenn-io/kit-ui'
  import {
    formatRelativeTime as timeAgo,
    formatTimestamp as localDateTimeLabel,
  } from '@kenn-io/kit-ui/utils/time'

  import type { KataIssueDetailModel } from '../types.js'
  import CreationAttribution from './CreationAttribution.svelte'

  interface Props {
    comments: KataIssueDetailModel['comments']
  }

  let { comments }: Props = $props()
</script>

<section class="detail-section" aria-labelledby="kata-comments-heading">
  <h3 id="kata-comments-heading">
    Comments{#if comments.length > 0}<span class="count">{comments.length}</span>{/if}
  </h3>
  {#if comments.length === 0}
    <p>No comments.</p>
  {:else}
    <ol>
      {#each comments as comment (comment.id)}
        <li>
          <header>
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
          <CreationAttribution creation={comment.creation} />
          <Markdown source={comment.body} class="comment-body" />
        </li>
      {/each}
    </ol>
  {/if}
</section>

<style>
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
