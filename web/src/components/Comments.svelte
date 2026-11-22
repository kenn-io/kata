<script lang="ts">
  import DetailSection from './DetailSection.svelte'
  import { tick } from 'svelte'
  /* eslint-disable svelte/prefer-svelte-reactivity -- reference counts are a transient search-result lookup. */
  import { commentEvidenceLength, trimCommentEvidence } from '../lib/kata/commentEvidence'
  import { CommentRelations } from '@kenn-io/kata-ui'
  import {
    Avatar,
    Button,
    Markdown,
    MentionTextarea,
    SegmentedControl,
    type MentionOption,
  } from '@kenn-io/kit-ui'
  import {
    formatRelativeTime as timeAgo,
    formatTimestamp as localDateTimeLabel,
  } from '@kenn-io/kit-ui/utils/time'

  import type { UIIssueReference } from '../lib/api/generated'
  import type { KataTaskDetail, KataCommentReplyIntent } from '../lib/kata/types'

  import type { KataCommentLink, KataReplyKind } from '@kenn-io/kata-ui'

  type Reference = UIIssueReference

  interface Props {
    selfActor?: string | undefined
    selectedCommentUID?: string | undefined
    // True while the task shows the list's copy and its comments load.
    pending?: boolean | undefined
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
    selfActor = undefined,
    selectedCommentUID,
    pending = false,
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

{#snippet composer()}
  <form
    class="comment-composer"
    class:comment-composer--active={Boolean(replyDraft) || commentDraft !== ''}
    onsubmit={(event) => {
      event.preventDefault()
      void submitComment()
    }}
  >
    {#if replyDraft}
      <div class="reply-target">
        <SegmentedControl
          ariaLabel="Reply kind"
          value={replyDraft.kind}
          disabled={actionsDisabled}
          options={[
            { value: 'reply', label: 'Reply' },
            { value: 'confirm', label: 'Confirm, with evidence' },
            { value: 'refute', label: 'Refute, with evidence' },
            { value: 'supersede', label: 'Supersede this comment' },
          ]}
          onchange={(kind) => updateReplyKind(kind as KataReplyKind)}
        />
        <Button size="sm" label="Cancel reply" disabled={actionsDisabled} onclick={cancelReply} />
      </div>
      {#if evidenceTooShort}<p>
          Confirm and refute replies need at least 40 characters of evidence. {commentEvidenceLength(
            commentDraft,
          )}/40
        </p>{/if}
    {/if}
    <MentionTextarea
      ariaLabel={replyDraft ? 'Reply evidence' : 'Comment'}
      rows={replyDraft || commentDraft ? 3 : 1}
      bind:value={() => commentDraft, updateCommentDraft}
      search={searchTaskReferences}
      emptyLabel="No matching tasks"
      placeholder={replyDraft ? 'Add your reply' : 'Add a comment'}
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
{/snippet}

<DetailSection
  title="Comments"
  count={!pending && issue.comments.length ? issue.comments.length : undefined}
>
  {#if showList}
    {#if pending}
      <p role="status" aria-label="Loading comments">Loading comments…</p>
    {:else if sortedComments.length > 0}
      <div class="comment-list" bind:this={listElement}>
        {#each sortedComments as comment (comment.id)}
          <article
            class="comment"
            class:selected={selectedCommentUID !== undefined && selectedCommentUID === comment.uid}
            data-comment-uid={comment.uid}
            tabindex="-1"
          >
            <div class="comment-meta">
              <Avatar name={comment.author} decorative size={20} />
              <span class="author"
                >{comment.author === selfActor ? 'You' : comment.author}{comment.teammate
                  ? ` / ${comment.teammate}`
                  : ''}</span
              >
              <time datetime={comment.created_at} title={localDateTimeLabel(comment.created_at)}>
                {timeAgo(comment.created_at)}
              </time>
              {#if comment.edited_at}<span title={localDateTimeLabel(comment.edited_at)}
                  >(edited)</span
                >{/if}
              {#if comment.uid && comment.handle}
                <button
                  type="button"
                  class="comment-action"
                  disabled={actionsDisabled || issue.issue.status !== 'open'}
                  onclick={() => beginReply(comment.uid!, 'reply')}>Reply</button
                >
              {/if}
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
            {#if comment.uid && comment.handle && replyDraft?.replyTo === comment.uid}
              <div class="reply-composer">{@render composer()}</div>
            {/if}
          </article>
        {/each}
      </div>
    {/if}
  {/if}
  {#if !replyDraft || !issue.comments.some((comment) => comment.uid === replyDraft?.replyTo)}
    {@render composer()}
  {/if}
</DetailSection>

<style>
  .comment-list {
    display: grid;
    gap: 4px;
    margin-bottom: 8px;
  }

  /* A record row: the avatar starts at the pane inset and a selected
     comment's highlight runs to the pane's edges. */
  .comment {
    margin-inline: -16px;
    padding: 6px 16px;
  }

  .comment.selected {
    background: var(--bg-inset);
  }

  .comment-meta {
    display: flex;
    align-items: center;
    gap: 8px;
    min-height: 24px;
    color: var(--text-secondary);
    font-size: var(--font-size-sm);
  }

  .author {
    color: var(--text-primary);
    font-weight: var(--font-weight-semibold);
  }

  .comment-meta time {
    white-space: nowrap;
  }

  /* Reply sits at the right inset and shows on hover and focus; a screen
     that can't hover shows it always. */
  .comment-action {
    height: 24px;
    margin-left: auto;
    border: 0;
    border-radius: var(--radius-md);
    background: transparent;
    color: var(--text-secondary);
    font: inherit;
    padding: 0 8px;
    cursor: pointer;
    opacity: 0;
  }

  .comment:hover .comment-action,
  .comment-action:focus-visible {
    opacity: 1;
  }

  @media (hover: none) {
    .comment-action {
      opacity: 1;
    }
  }

  .comment-action:hover:not(:disabled) {
    background: var(--bg-surface-hover);
    color: var(--text-primary);
  }

  .comment > :global(.comment-body),
  .comment-links,
  .reply-composer {
    padding-left: 28px;
  }

  .comment > :global(.comment-body) {
    margin-top: 2px;
  }

  .reply-target,
  .comment-links {
    display: flex;
    gap: var(--space-3);
    flex-wrap: wrap;
    align-items: center;
  }

  .reply-target {
    color: var(--text-secondary);
    font-size: var(--font-size-sm);
  }

  .reply-composer {
    margin-top: 8px;
  }

  .comment-composer {
    display: grid;
    gap: 8px;
  }

  .comment-composer p {
    margin: 0;
    color: var(--text-secondary);
    font-size: var(--font-size-sm);
  }

  .comment-composer :global(.comment-submit) {
    justify-self: end;
  }

  /* An empty composer is one line; its actions appear once it has focus
     or text. */
  .comment-composer:not(.comment-composer--active):not(:focus-within) :global(.comment-submit) {
    display: none;
  }
</style>
