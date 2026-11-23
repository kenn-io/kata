<script lang="ts">
  import { Button, SegmentedControl } from '@kenn-io/kit-ui'
  import CheckIcon from '@lucide/svelte/icons/check'
  import RotateCcwIcon from '@lucide/svelte/icons/rotate-ccw'
  import type { KataTaskCloseRequest, KataTaskDetail } from '../lib/kata/types'
  import Modal from './Modal.svelte'
  import DialogPreview from './shared/DialogPreview.svelte'

  interface Props {
    issue: KataTaskDetail
    disabled?: boolean | undefined
    error?: string | undefined
    onCloseIssue: (request: KataTaskCloseRequest) => boolean | Promise<boolean>
    onReopenIssue: () => void | Promise<void>
  }

  let { issue, disabled = false, error = undefined, onCloseIssue, onReopenIssue }: Props = $props()

  type CloseReason = 'done' | 'wontfix' | 'duplicate' | 'superseded'
  type DoneEvidence = 'test' | 'commit' | 'pr' | 'reviewed-paths' | 'external'

  const closeReasonOptions = [
    { value: 'done', label: 'Done' },
    { value: 'wontfix', label: "Won't do" },
    { value: 'duplicate', label: 'Duplicate' },
    { value: 'superseded', label: 'Superseded' },
  ]

  const defaultWontDoMessage =
    'Removed from active work through the web issue detail because this task is no longer intended to be completed.'

  function chooseCloseReason(value: string | undefined): void {
    if (
      value === 'done' ||
      value === 'wontfix' ||
      value === 'duplicate' ||
      value === 'superseded'
    ) {
      completeReason = value
    }
  }

  let completeOpen = $state(false)
  let completeReason = $state<CloseReason>('done')
  let completeMessage = $state('')
  let evidenceType = $state<DoneEvidence>('test')
  let evidenceValue = $state('')
  let targetIssue = $state('')
  let pending = $state(false)
  let refused = $state(false)
  let completeMessageInput: HTMLTextAreaElement | null = $state(null)
  let trackedUID = $state<string | null>(null)
  const blockedCount = $derived(
    issue.links.filter(
      (link) =>
        link.type === 'blocks' && link.from.uid === issue.issue.uid && link.to.status === 'open',
    ).length,
  )

  $effect(() => {
    if (issue.issue.uid === trackedUID) return
    trackedUID = issue.issue.uid
    completeOpen = false
    completeReason = 'done'
    refused = false
    completeMessage = ''
    evidenceType = 'test'
    evidenceValue = ''
    targetIssue = ''
    pending = false
  })

  function openCompleteDialog(): void {
    if (disabled || pending) return
    completeReason = 'done'
    completeMessage = ''
    evidenceType = 'test'
    evidenceValue = ''
    targetIssue = ''
    completeOpen = true
    queueMicrotask(() => completeMessageInput?.focus())
  }

  function closeCompleteDialog(): void {
    if (pending) return
    completeOpen = false
  }

  async function completeIssue(): Promise<void> {
    if (pending || !canComplete()) return
    pending = true
    try {
      const ok = await onCloseIssue(closeRequest())
      refused = !ok
      if (ok) {
        completeOpen = false
        completeMessage = ''
        completeReason = 'done'
        evidenceType = 'test'
        evidenceValue = ''
        targetIssue = ''
      }
    } finally {
      pending = false
    }
  }

  function messageMinimum(): number {
    if (completeReason === 'wontfix') return 60
    if (completeReason === 'duplicate' || completeReason === 'superseded') return 20
    return 40
  }

  function canComplete(): boolean {
    const length = completeMessage.trim().length
    if (completeReason === 'wontfix') return length === 0 || length >= messageMinimum()
    if (length < messageMinimum()) return false
    if (completeReason === 'done') return evidenceValue.trim().length > 0
    if (completeReason === 'duplicate' || completeReason === 'superseded') {
      return targetIssue.trim().length > 0
    }
    return true
  }

  function closeRequest(): KataTaskCloseRequest {
    const message = completeMessage.trim()
    if (completeReason === 'duplicate' || completeReason === 'superseded') {
      return {
        reason: completeReason,
        message,
        evidence: [
          {
            type: completeReason === 'duplicate' ? 'duplicate-of' : 'superseded-by',
            issue_ref: targetIssue.trim(),
          },
        ],
      }
    }
    if (completeReason === 'done') {
      const value = evidenceValue.trim()
      const evidence =
        evidenceType === 'commit'
          ? { type: 'commit', sha: value }
          : evidenceType === 'pr'
            ? { type: 'pr', url: value }
            : evidenceType === 'external'
              ? { type: 'external', account: value }
              : evidenceType === 'reviewed-paths'
                ? {
                    type: 'reviewed-paths',
                    paths: value
                      .split(/[\n,]/)
                      .map((path) => path.trim())
                      .filter(Boolean),
                  }
                : { type: 'test', command: value }
      return { reason: completeReason, message, evidence: [evidence] }
    }
    return { reason: completeReason, message: message || defaultWontDoMessage, evidence: [] }
  }

  async function reopenIssue(): Promise<void> {
    if (pending) return
    pending = true
    try {
      await onReopenIssue()
    } finally {
      pending = false
    }
  }

  export function activate(): void {
    if (disabled) return
    if (issue.issue.status === 'closed') void reopenIssue()
    else openCompleteDialog()
  }

  function handleCompleteKeydown(event: KeyboardEvent): void {
    if ((event.metaKey || event.ctrlKey) && event.key === 'Enter') {
      event.preventDefault()
      void completeIssue()
    }
  }
</script>

{#if issue.issue.status === 'closed'}
  <Button
    class="detail-action"
    size="sm"
    label="Reopen"
    disabled={pending || disabled}
    onclick={() => void reopenIssue()}
  >
    <RotateCcwIcon size={13} strokeWidth={1.9} />
  </Button>
{:else}
  <Button
    class="detail-action"
    size="sm"
    tone="info"
    surface="solid"
    label="Complete…"
    disabled={pending || disabled}
    onclick={openCompleteDialog}
  >
    <CheckIcon size={13} strokeWidth={1.9} />
  </Button>
{/if}

<Modal open={completeOpen} title="Complete task" onClose={closeCompleteDialog} width={560}>
  <div class="complete-dialog" onkeydown={handleCompleteKeydown} role="presentation">
    <div class="complete-context">
      <p class="complete-task-title">{issue.issue.title}</p>
      <p class="complete-task-id">{issue.issue.qualified_id}</p>
    </div>

    <SegmentedControl
      ariaLabel="Reason"
      disabled={pending}
      value={completeReason}
      onchange={chooseCloseReason}
      options={closeReasonOptions}
    />

    {#if completeReason === 'done'}
      <span>Proof</span>
      <div class="complete-evidence">
        <label>
          <span>Evidence type</span>
          <select bind:value={evidenceType} disabled={pending}>
            <option value="test">Test command</option>
            <option value="commit">Commit SHA</option>
            <option value="pr">Pull request URL</option>
            <option value="reviewed-paths">Reviewed paths</option>
            <option value="external">External account</option>
          </select>
        </label>
        <label>
          <span>Evidence value</span>
          <input
            aria-label="Evidence value"
            bind:value={evidenceValue}
            placeholder={evidenceType === 'reviewed-paths'
              ? 'path/one, path/two'
              : evidenceType === 'external'
                ? 'where and how the work was completed'
                : ''}
            disabled={pending}
          />
        </label>
      </div>
    {:else if completeReason === 'duplicate' || completeReason === 'superseded'}
      <label class="complete-field">
        <span>Target issue</span>
        <input
          aria-label="Target issue"
          bind:value={targetIssue}
          placeholder="example-project#d4ex"
          disabled={pending}
        />
      </label>
    {/if}

    <label class="complete-message">
      <span
        >{completeReason === 'done'
          ? 'What was done'
          : completeReason === 'wontfix'
            ? 'Why not (optional)'
            : 'Note'}</span
      >
      <textarea
        bind:this={completeMessageInput}
        rows="4"
        bind:value={completeMessage}
        disabled={pending}
      ></textarea>
      {#if completeMessage.trim().length > 0 && completeMessage.trim().length < messageMinimum()}
        <small>{messageMinimum() - completeMessage.trim().length} more characters needed</small>
      {/if}
    </label>
    {#if refused && error}<p class="complete-error" role="alert">{error}</p>{/if}
    <DialogPreview>
      <div>Comments, links, and attachments stay.</div>
      {#if blockedCount}
        <div>
          {blockedCount} open {blockedCount === 1 ? 'task' : 'tasks'} will no longer be blocked by this
          task.
        </div>
      {/if}
      <div>Reopen from the Logbook.</div>
    </DialogPreview>
  </div>

  {#snippet footer()}
    <Button size="sm" label="Cancel" onclick={closeCompleteDialog} disabled={pending} />
    <Button
      size="sm"
      tone="info"
      surface="solid"
      label={pending ? 'Completing...' : 'Complete'}
      onclick={() => {
        void completeIssue()
      }}
      disabled={pending || !canComplete()}
    />
  {/snippet}
</Modal>

<style>
  .complete-error {
    color: var(--accent-red);
    margin: 0;
  }
  :global(.detail-action) {
    min-width: 98px;
  }

  :global(.detail-action svg) {
    flex: 0 0 auto;
  }

  .complete-dialog {
    display: grid;
    gap: var(--space-5);
  }

  .complete-context {
    display: grid;
    gap: 2px;
  }

  .complete-task-title {
    margin: 0;
    color: var(--text-primary);
    font-size: var(--font-size-lg);
    font-weight: 650;
    line-height: 1.3;
  }

  .complete-task-id {
    margin: 0;
    color: var(--text-muted);
    font-family: var(--font-mono);
    font-size: var(--font-size-xs);
  }

  .complete-message > span,
  .complete-field > span,
  .complete-evidence label > span {
    margin-bottom: 6px;
    color: var(--text-muted);
    font-size: var(--font-size-xs);
    font-weight: var(--font-weight-medium);
  }

  .complete-message small {
    color: var(--text-muted);
    font-size: var(--font-size-xs);
    font-weight: 500;
    text-transform: none;
  }

  .complete-message,
  .complete-field,
  .complete-evidence label {
    display: grid;
    gap: 2px;
  }

  .complete-message textarea {
    width: 100%;
    resize: vertical;
    border: 1px solid var(--border-default);
    border-radius: 6px;
    background: var(--bg-primary);
    color: var(--text-primary);
    font: inherit;
    font-size: var(--font-size-sm);
    line-height: 1.45;
    padding: 8px 10px;
  }

  .complete-evidence {
    display: grid;
    grid-template-columns: minmax(130px, 0.4fr) minmax(0, 1fr);
    gap: var(--space-5);
  }

  .complete-evidence select,
  .complete-evidence input,
  .complete-field input {
    min-width: 0;
    height: 34px;
    border: 1px solid var(--border-default);
    border-radius: 6px;
    background: var(--bg-primary);
    color: var(--text-primary);
    font: inherit;
    font-size: var(--font-size-sm);
    padding: 0 9px;
  }

  .complete-message textarea:focus {
    outline: 2px solid var(--focus-ring);
    outline-offset: 1px;
  }
</style>
