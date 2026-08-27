<script lang="ts">
  import { Button } from '@kenn-io/kit-ui'
  import CheckIcon from '@lucide/svelte/icons/check'
  import RotateCcwIcon from '@lucide/svelte/icons/rotate-ccw'
  import type { KataTaskCloseRequest, KataTaskDetail } from '../lib/kata/types'
  import Modal from './Modal.svelte'

  interface Props {
    issue: KataTaskDetail
    onCloseIssue: (request: KataTaskCloseRequest) => boolean | Promise<boolean>
    onReopenIssue: () => void | Promise<void>
  }

  let { issue, onCloseIssue, onReopenIssue }: Props = $props()

  type CloseReason = 'done' | 'wontfix' | 'duplicate' | 'superseded'
  type DoneEvidence = 'test' | 'commit' | 'pr' | 'reviewed-paths' | 'external'

  const closeReasons: ReadonlyArray<{
    value: CloseReason
    label: string
    description: string
  }> = [
    { value: 'done', label: 'Done', description: 'Completed as intended.' },
    { value: 'wontfix', label: "Won't do", description: 'Decided not to pursue.' },
    { value: 'duplicate', label: 'Duplicate', description: 'Tracked elsewhere.' },
    { value: 'superseded', label: 'Superseded', description: 'Replaced by another task.' },
  ]

  let completeOpen = $state(false)
  let completeReason = $state<CloseReason>('done')
  let completeMessage = $state('')
  let evidenceType = $state<DoneEvidence>('test')
  let evidenceValue = $state('')
  let targetIssue = $state('')
  let pending = $state(false)
  let completeMessageInput: HTMLTextAreaElement | null = $state(null)
  let trackedUID = $state<string | null>(null)

  $effect(() => {
    if (issue.issue.uid === trackedUID) return
    trackedUID = issue.issue.uid
    completeOpen = false
    completeReason = 'done'
    completeMessage = ''
    evidenceType = 'test'
    evidenceValue = ''
    targetIssue = ''
    pending = false
  })

  function openCompleteDialog(): void {
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
    if (completeMessage.trim().length < messageMinimum()) return false
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
    return { reason: completeReason, message, evidence: [] }
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
    disabled={pending}
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
    label="Complete"
    disabled={pending}
    onclick={openCompleteDialog}
  >
    <CheckIcon size={13} strokeWidth={1.9} />
  </Button>
{/if}

<Modal open={completeOpen} title="Complete task" onClose={closeCompleteDialog} width={480}>
  <div class="complete-dialog" onkeydown={handleCompleteKeydown} role="presentation">
    <div class="complete-context">
      <p class="complete-task-title">{issue.issue.title}</p>
      <p class="complete-task-id">{issue.issue.qualified_id}</p>
    </div>

    <fieldset class="complete-reasons" disabled={pending}>
      <legend>Reason</legend>
      {#each closeReasons as reason (reason.value)}
        <label class="complete-reason">
          <input
            type="radio"
            name="complete-reason"
            value={reason.value}
            bind:group={completeReason}
          />
          <span>
            <strong>{reason.label}</strong>
            <small>{reason.description}</small>
          </span>
        </label>
      {/each}
    </fieldset>

    {#if completeReason === 'done'}
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
      <span>Completion note <small>(at least {messageMinimum()} characters, markdown)</small></span>
      <textarea
        bind:this={completeMessageInput}
        rows="4"
        placeholder="What was done? Any follow-ups? Cmd/Ctrl+Enter to confirm."
        bind:value={completeMessage}
        disabled={pending}
      ></textarea>
    </label>
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

  .complete-reasons {
    display: grid;
    gap: 8px;
    margin: 0;
    padding: 0;
    border: 0;
  }

  .complete-reasons legend,
  .complete-message > span,
  .complete-field > span,
  .complete-evidence label > span {
    margin-bottom: 6px;
    color: var(--text-muted);
    font-size: var(--font-size-xs);
    font-weight: 650;
    text-transform: uppercase;
  }

  .complete-message small {
    color: var(--text-muted);
    font-size: var(--font-size-xs);
    font-weight: 500;
    text-transform: none;
  }

  .complete-reason {
    display: flex;
    align-items: flex-start;
    gap: 8px;
    min-height: 38px;
    border: 1px solid var(--border-default);
    border-radius: 6px;
    background: var(--bg-primary);
    padding: 8px 10px;
    color: var(--text-primary);
    cursor: pointer;
  }

  .complete-reason:has(input:checked) {
    border-color: var(--accent-blue);
    background: color-mix(in srgb, var(--accent-blue) 9%, var(--bg-primary));
  }

  .complete-reason input {
    margin-top: 2px;
  }

  .complete-reason span,
  .complete-message,
  .complete-field,
  .complete-evidence label {
    display: grid;
    gap: 2px;
  }

  .complete-reason strong {
    font-size: var(--font-size-sm);
    font-weight: 650;
  }

  .complete-reason small {
    color: var(--text-muted);
    font-size: var(--font-size-xs);
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
