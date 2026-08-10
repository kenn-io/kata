<script lang="ts">
  /* eslint-disable svelte/no-at-html-tags -- kit-ui sanitizes rendered markdown. */
  import { renderMarkdownSync } from '@kenn-io/kit-ui/utils/markdown'
  import { formatTimestamp as localDateTimeLabel } from '@kenn-io/kit-ui/utils/time'

  import type { KataIssueDetailModel } from '../types.js'

  interface Props {
    comments: KataIssueDetailModel['comments']
  }

  let { comments }: Props = $props()
</script>

<section class="detail-section" aria-labelledby="kata-comments-heading">
  <h3 id="kata-comments-heading">Comments</h3>
  {#if comments.length === 0}
    <p>No comments.</p>
  {:else}
    <ol>
      {#each comments as comment (comment.id)}
        <li>
          <header>
            <strong>{comment.author}</strong>
            <time datetime={comment.createdAt} title={comment.createdAt}
              >{localDateTimeLabel(comment.createdAt)}</time
            >
          </header>
          <div class="markdown-body">
            {@html renderMarkdownSync(comment.body)}
          </div>
        </li>
      {/each}
    </ol>
  {/if}
</section>

<style>
  .detail-section {
    min-width: 0;
    border-top: 1px solid var(--border-muted, #e2e4e8);
    padding-top: 16px;
  }

  h3 {
    margin: 0 0 9px;
    color: var(--text-muted, #656a73);
    font-size: var(--font-size-xs, 0.75rem);
    font-weight: 650;
    letter-spacing: 0.04em;
    text-transform: uppercase;
  }

  ol {
    display: grid;
    gap: 8px;
    margin: 0;
    padding: 0;
    list-style: none;
  }

  li {
    border: 1px solid var(--border-muted, #e2e4e8);
    border-radius: 8px;
    padding: 12px;
  }

  header {
    display: flex;
    justify-content: space-between;
    gap: 12px;
    margin-bottom: 8px;
    font-size: var(--font-size-xs, 0.75rem);
  }

  time,
  p {
    color: var(--text-muted, #656a73);
  }

  p,
  .markdown-body :global(:first-child) {
    margin-top: 0;
  }

  p,
  .markdown-body :global(:last-child) {
    margin-bottom: 0;
  }
</style>
