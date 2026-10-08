<script lang="ts">
  import { tick } from 'svelte'
  /* eslint-disable svelte/prefer-svelte-reactivity -- reference counts are a transient search-result lookup. */
  import { commentEvidenceLength, trimCommentEvidence } from '../lib/kata/commentEvidence'
  import { CommentRelations } from '@kenn-io/kata-ui'
  import { Button, Markdown, MentionTextarea, type MentionOption } from '@kenn-io/kit-ui'
  import {
    formatRelativeTime as timeAgo,
    formatTimestamp as localDateTimeLabel,
  } from '@kenn-io/kit-ui/utils/time'

  import type { UIIssueReference } from '../lib/api/generated'
  import type { KataTaskDetail, KataCommentReplyIntent } from '../lib/kata/types'

  import type { KataCommentLink, KataReplyKind } from '@kenn-io/kata-ui'

  type Reference = UIIssueReference

  interface Props {
    selectedCommentUID?: string | undefined
    navigationActive?: boolean | undefined
    initialReply?: KataCommentReplyIntent | undefined
    showList?: boolean | undefined
    commentError?: string | undefined
    onCancelReply?: (() => void) | undefined
    onOpenComment?:
      | ((
          issueUID: string,
          commentUID: string,
          source?: { issueUID: string; commentUID: string },
        ) => void)
      | undefined
    issue: KataTaskDetail
    searchReferences: (query: string) => Promise<Reference[]>
    actionsDisabled?: boolean | undefined
    draftResetGeneration?: number | undefined
    draftFenceGeneration?: number | undefined
    onAddComment: (
      uid: string,
      body: string,
      reply?: KataCommentReplyIntent,
    ) => boolean | Promise<boolean>
  }

  interface PendingDraftReset {
    uid: string
    generation: number
    value: string
    revision: number
  }

  let {
    issue,
    selectedCommentUID,
    navigationActive = true,
    initialReply = undefined,
    showList = true,
    commentError = undefined,
    onCancelReply = undefined,
    onOpenComment = undefined,
    searchReferences,
    actionsDisabled = false,
    draftResetGeneration = 0,
    draftFenceGeneration = 0,
    onAddComment,
  }: Props = $props()

  let listElement = $state<HTMLDivElement>()
  let requestedCommentUID: string | undefined
  let focusedCommentUID: string | undefined
  $effect(() => {
    const uid = selectedCommentUID
    const list = listElement
    const available = issue.comments.some((comment) => comment.uid === uid)
    if (requestedCommentUID !== uid) {
      requestedCommentUID = uid
      focusedCommentUID = undefined
    }
    if (!uid || !list || !available || !navigationActive || focusedCommentUID === uid) return
    void tick().then(() => {
      if (
        selectedCommentUID !== uid ||
        listElement !== list ||
        !navigationActive ||
        focusedCommentUID === uid
      )
        return
      const target = [...list.querySelectorAll<HTMLElement>('[data-comment-uid]')].find(
        (item) => item.dataset.commentUid === uid,
      )
      if (!target) return
      target.scrollIntoView?.({ block: 'nearest' })
      target.focus({ preventScroll: true })
      focusedCommentUID = uid
    })
  })

  let commentDraft = $state('')
  const replyKindID = $props.id()
  const replyKinds: KataReplyKind[] = ['reply', 'confirm', 'refute', 'supersede']
  let replyDraft = $state<KataCommentReplyIntent | undefined>()
  let lastInitialReply: KataCommentReplyIntent | undefined
  $effect(() => {
    if (initialReply !== lastInitialReply) {
      lastInitialReply = initialReply
      replyDraft = initialReply ? { ...initialReply } : undefined
    }
  })
  const evidenceTooShort = $derived(
    (replyDraft?.kind === 'confirm' || replyDraft?.kind === 'refute') &&
      commentEvidenceLength(commentDraft) < 40,
  )
  let failedReplyKey = $state<string | undefined>()
  const currentReplyKey = $derived(
    replyDraft
      ? JSON.stringify([
          issue.issue.uid,
          trimCommentEvidence(commentDraft),
          replyDraft.replyTo,
          replyDraft.kind,
          replyDraft.force,
          draftFenceGeneration,
        ])
      : undefined,
  )
  const duplicateReply = $derived(
    Boolean(
      replyDraft &&
      !replyDraft.force &&
      failedReplyKey === currentReplyKey &&
      (commentError?.includes('duplicate_reply') ||
        commentError?.includes('a reply of this kind already exists')),
    ),
  )
  function beginReply(uid: string, kind: KataReplyKind): void {
    if (actionsDisabled || issue.issue.status !== 'open') return
    replyDraft = { replyTo: uid, kind, force: false }
  }
  function cancelReply(): void {
    replyDraft = undefined
    onCancelReply?.()
  }
  function updateReplyKind(kind: KataReplyKind): void {
    if (replyDraft) replyDraft = { ...replyDraft, kind, force: false }
  }
  function openComment(link: KataCommentLink, sourceUID?: string): void {
    if (link.issue_uid && link.uid)
      onOpenComment?.(
        link.issue_uid,
        link.uid,
        sourceUID ? { issueUID: issue.issue.uid, commentUID: sourceUID } : undefined,
      )
  }
  function linkLabel(link: KataCommentLink): string {
    return `${({ reply: 'Replies to', confirm: 'Confirms', refute: 'Refutes', supersede: 'Supersedes' } as Record<string, string>)[link.kind] ?? link.kind} ${link.handle ?? `(${link.status ?? 'unresolved'})`}${link.status && link.handle ? ` (${link.status})` : ''}`
  }
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
    if (value !== commentDraft && replyDraft?.force) replyDraft = { ...replyDraft, force: false }
    commentDraft = value
    if (!actionsDisabled) commentDraftGeneration = draftFenceGeneration
    commentDraftRevision += 1
  }

  async function submitComment(): Promise<void> {
    if (
      actionsDisabled ||
      commentDraftFenced ||
      evidenceTooShort ||
      (replyDraft && issue.issue.status !== 'open')
    )
      return
    const draft = commentDraft
    const body = replyDraft ? trimCommentEvidence(draft) : draft.trim()
    if (!body) return
    const mutationUID = issue.issue.uid
    const resetGeneration = draftResetGeneration
    const draftRevision = commentDraftRevision
    const intent = replyDraft ? { ...replyDraft } : undefined
    const submittedReplyKey = currentReplyKey
    const ok = intent
      ? await onAddComment(mutationUID, body, intent)
      : await onAddComment(mutationUID, body)
    if (!ok) {
      failedReplyKey = submittedReplyKey
      return
    }
    if (issue.issue.uid !== mutationUID) return
    failedReplyKey = undefined
    if (intent && replyDraft?.replyTo === intent.replyTo && replyDraft?.kind === intent.kind) {
      replyDraft = undefined
      onCancelReply?.()
    }
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
    {#if replyDraft}
      <div class="reply-target">
        Reply to {issue.comments.find((c) => c.uid === replyDraft?.replyTo)?.handle ??
          replyDraft.replyTo}
        <label for={replyKindID}>Reply kind</label>
        <select
          id={replyKindID}
          aria-label="Reply kind"
          disabled={actionsDisabled}
          bind:value={() => replyDraft?.kind ?? 'reply', updateReplyKind}
          >{#each replyKinds as kind (kind)}<option value={kind}>{kind}</option>{/each}</select
        >
        <Button size="sm" label="Cancel reply" disabled={actionsDisabled} onclick={cancelReply} />
      </div>
      {#if evidenceTooShort}<p>Confirm and refute need at least 40 characters of evidence.</p>{/if}
    {/if}
    <MentionTextarea
      ariaLabel={replyDraft ? 'Reply evidence' : 'Comment'}
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
      disabled={actionsDisabled ||
        commentDraftFenced ||
        evidenceTooShort ||
        (replyDraft ? trimCommentEvidence(commentDraft) : commentDraft.trim()) === '' ||
        Boolean(replyDraft && issue.issue.status !== 'open')}
    />
    {#if duplicateReply}<Button
        size="sm"
        label="Send another reply"
        disabled={actionsDisabled || commentDraftFenced || evidenceTooShort}
        onclick={() => {
          if (replyDraft) replyDraft = { ...replyDraft, force: true }
          void submitComment()
        }}
      />{/if}
  </form>
  {#if showList}
    {#if sortedComments.length === 0}
      <p>No comments</p>
    {:else}
      <div class="comment-list" bind:this={listElement}>
        {#each sortedComments as comment (comment.id)}
          <article
            class="comment"
            class:selected={selectedCommentUID !== undefined && selectedCommentUID === comment.uid}
            data-comment-uid={comment.uid}
            tabindex="-1"
          >
            <div class="comment-meta">
              {#if comment.handle}<span>{comment.handle}</span>{/if}
              {#if comment.edited_at}<span title={localDateTimeLabel(comment.edited_at)}
                  >(edited)</span
                >{/if}
              <span class="avatar" aria-hidden="true">{comment.author.slice(0, 1)}</span>
              <span class="author"
                >{comment.teammate
                  ? `${comment.author} / ${comment.teammate}`
                  : comment.author}</span
              >
              <time datetime={comment.created_at} title={localDateTimeLabel(comment.created_at)}>
                {timeAgo(comment.created_at)}
              </time>
            </div>
            {#if comment.reply}<div class="comment-links">
                ↳ <Button
                  size="sm"
                  label={linkLabel(comment.reply)}
                  disabled={!onOpenComment || !comment.reply.uid || !comment.reply.issue_uid}
                  onclick={() => openComment(comment.reply!, comment.uid)}
                />{#if comment.reply.target_edited}<span>Target edited after this reply</span>{/if}
              </div>{/if}
            <Markdown source={comment.body} class="comment-body" />
            <CommentRelations
              links={comment.backlinks ?? []}
              partial={comment.backlinks_truncated ?? false}
              onOpen={onOpenComment ? (link) => openComment(link, comment.uid) : undefined}
            />
            {#if comment.uid && comment.handle}<div class="comment-links">
                {#each replyKinds as kind (kind)}<Button
                    size="sm"
                    label={kind[0]!.toUpperCase() + kind.slice(1)}
                    disabled={actionsDisabled || issue.issue.status !== 'open'}
                    onclick={() => beginReply(comment.uid!, kind)}
                  />{/each}
              </div>{/if}
          </article>
        {/each}
      </div>
    {/if}
  {/if}
</section>

<style>
  .comment.selected {
    background: var(--bg-inset);
    outline: 1px solid var(--border-muted);
  }
  .reply-target,
  .comment-links {
    display: flex;
    gap: var(--space-3);
    flex-wrap: wrap;
    align-items: center;
  }
  .comments h3 {
    margin: 0 0 var(--space-4);
    color: var(--text-primary);
    font-size: var(--font-size-md);
    font-weight: var(--font-weight-semibold);
  }

  .comment-composer {
    display: grid;
    gap: var(--space-4);
    margin-bottom: var(--space-4);
  }

  .comment-composer :global(.comment-submit) {
    justify-self: end;
  }

  .comment {
    padding: var(--space-5) 0;
  }

  .comment + .comment {
    border-top: 1px solid var(--border-muted);
  }

  .comment-meta {
    display: flex;
    align-items: center;
    gap: var(--space-4);
    margin-bottom: var(--space-3);
    color: var(--text-muted);
    font-size: var(--font-size-sm);
  }

  .avatar {
    display: inline-grid;
    flex: 0 0 auto;
    place-items: center;
    width: 20px;
    height: 20px;
    border-radius: 50%;
    background: var(--bg-inset);
    color: var(--text-secondary);
    font-size: var(--font-size-2xs);
    font-weight: var(--font-weight-semibold);
    text-transform: uppercase;
  }

  .author {
    color: var(--text-primary);
    font-weight: var(--font-weight-semibold);
  }

  .comment-meta time {
    white-space: nowrap;
  }

  .comment > :global(.comment-body) {
    padding-left: 28px;
  }
</style>
