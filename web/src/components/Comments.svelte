<script lang="ts">
  /* eslint-disable svelte/no-at-html-tags -- kit-ui sanitizes rendered markdown. */
  /* eslint-disable svelte/prefer-svelte-reactivity -- reference counts are a transient search-result lookup. */
  import { Button, MentionTextarea, type MentionOption } from '@kenn-io/kit-ui'
  import { renderMarkdown, renderMarkdownSync } from '@kenn-io/kit-ui/utils/markdown'
  import {
    formatRelativeTime as timeAgo,
    formatTimestamp as localDateTimeLabel,
  } from '@kenn-io/kit-ui/utils/time'

  import type { components } from '../lib/api/schema'
  import type { KataTaskDetail } from '../lib/kata/types'

  type Reference = components['schemas']['UIIssueReference']

  interface Props {
    issue: KataTaskDetail
    searchReferences: (query: string) => Promise<Reference[]>
    actionsDisabled?: boolean | undefined
    draftResetGeneration?: number | undefined
    draftFenceGeneration?: number | undefined
    onAddComment: (uid: string, body: string) => boolean | Promise<boolean>
  }

  interface PendingDraftReset {
    uid: string
    generation: number
    value: string
    revision: number
  }

  let {
    issue,
    searchReferences,
    actionsDisabled = false,
    draftResetGeneration = 0,
    draftFenceGeneration = 0,
    onAddComment,
  }: Props = $props()

  let commentDraft = $state('')
  let commentDraftGeneration = $state(0)
  let commentDraftRevision = 0
  let lastDraftResetGeneration = $state<number | null>(null)
  let pendingCommentReset = $state<PendingDraftReset | null>(null)

  const sortedComments = $derived.by(() => {
    const comments = issue.comments ?? []
    return [...comments].sort((a, b) => {
      const ta = Date.parse(a.created_at)
      const tb = Date.parse(b.created_at)
      if (Number.isNaN(ta) || Number.isNaN(tb)) return 0
      return tb - ta
    })
  })
  const commentDraftFenced = $derived(
    commentDraft.trim() !== '' && commentDraftGeneration !== draftFenceGeneration,
  )

  $effect(() => {
    const nextGeneration = draftResetGeneration
    if (lastDraftResetGeneration === null) {
      lastDraftResetGeneration = nextGeneration
      return
    }
    if (nextGeneration === lastDraftResetGeneration) return
    lastDraftResetGeneration = nextGeneration
    const uid = issue.issue.uid
    if (pendingCommentReset?.uid === uid && pendingCommentReset.generation !== nextGeneration) {
      if (
        commentDraftRevision === pendingCommentReset.revision &&
        commentDraft === pendingCommentReset.value
      ) {
        commentDraft = ''
      }
    }
    pendingCommentReset = null
  })

  function updateCommentDraft(value: string): void {
    commentDraft = value
    if (!actionsDisabled) commentDraftGeneration = draftFenceGeneration
    commentDraftRevision += 1
  }

  async function submitComment(): Promise<void> {
    if (actionsDisabled || commentDraftFenced) return
    const draft = commentDraft
    const body = draft.trim()
    if (!body) return
    const mutationUID = issue.issue.uid
    const resetGeneration = draftResetGeneration
    const draftRevision = commentDraftRevision
    const ok = await onAddComment(mutationUID, body)
    if (!ok || issue.issue.uid !== mutationUID) return
    if (draftResetGeneration !== resetGeneration) {
      if (commentDraftRevision === draftRevision && commentDraft === draft) commentDraft = ''
    } else {
      pendingCommentReset = {
        uid: mutationUID,
        generation: resetGeneration,
        value: draft,
        revision: draftRevision,
      }
    }
  }

  async function searchTaskReferences(query: string): Promise<MentionOption[]> {
    const references = await searchReferences(query)
    const counts = new Map<string, number>()
    for (const task of references) counts.set(task.short_id, (counts.get(task.short_id) ?? 0) + 1)
    return references.map((task) => ({
      id: task.uid,
      insert: counts.get(task.short_id)! > 1 ? task.qualified_id : task.short_id,
      label: task.title,
      meta: task.project_name,
    }))
  }
</script>

<section class="comments" aria-labelledby="kata-comments-title">
  <h3 id="kata-comments-title">Comments</h3>
  <form
    class="comment-composer"
    onsubmit={(event) => {
      event.preventDefault()
      void submitComment()
    }}
  >
    <MentionTextarea
      ariaLabel="Comment"
      rows={3}
      bind:value={() => commentDraft, updateCommentDraft}
      search={searchTaskReferences}
      emptyLabel="No matching tasks"
      placeholder="Add a comment..."
      disabled={actionsDisabled}
    />
    <Button
      type="submit"
      tone="info"
      surface="solid"
      size="sm"
      class="comment-submit"
      label="Add comment"
      disabled={actionsDisabled || commentDraftFenced || commentDraft.trim() === ''}
    />
  </form>
  {#if sortedComments.length === 0}
    <p>No comments</p>
  {:else}
    <div class="comment-list">
      {#each sortedComments as comment (comment.id)}
        <article class="comment">
          <div class="comment-meta">
            <span>{comment.author}</span>
            <time datetime={comment.created_at} title={localDateTimeLabel(comment.created_at)}>
              {timeAgo(comment.created_at)}
            </time>
          </div>
          <div class="comment-body markdown-body">
            {#await renderMarkdown(comment.body)}
              {@html renderMarkdownSync(comment.body)}
            {:then html}
              {@html html}
            {/await}
          </div>
        </article>
      {/each}
    </div>
  {/if}
</section>

<style>
  .comments {
    margin: 0 0 22px;
  }

  .comments h3 {
    margin: 0 0 8px;
    color: var(--text-muted);
    font-size: var(--font-size-xs);
    font-weight: 650;
    text-transform: uppercase;
  }

  .comment-composer {
    display: grid;
    gap: 8px;
    margin-bottom: 12px;
  }

  .comment-composer :global(.comment-submit) {
    justify-self: end;
  }

  .comment-list {
    display: grid;
    gap: 8px;
  }

  .comment {
    border: 1px solid var(--border-default);
    border-radius: 6px;
    background: var(--bg-secondary);
    padding: 8px 10px;
  }

  .comment-meta {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 8px;
    color: var(--text-muted);
    font-size: var(--font-size-xs);
    margin-bottom: 4px;
  }

  .comment-meta time {
    flex: 0 0 auto;
    white-space: nowrap;
  }

  .comment-body :global(p) {
    margin: 0;
    color: var(--text-secondary);
    font-size: var(--font-size-sm);
    line-height: 1.45;
    white-space: pre-wrap;
  }
</style>
