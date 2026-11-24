<script lang="ts">
  // The completion form shared by the issue detail and the board. Closing
  // asserts the work is complete, so every close carries a reason, a note,
  // and the evidence that reason needs.
  import { Button, Card, RadioGroup } from '@kenn-io/kit-ui'
  import type { KataTaskCloseRequest } from '../lib/kata/types'
  import Modal from './Modal.svelte'

  interface Props {
    open: boolean
    title: string
    qualifiedId: string
    /** Open tasks this one blocks, which lose the blocker when it closes. */
    blockedPreview?: number | undefined
    onComplete: (request: KataTaskCloseRequest) => boolean | Promise<boolean>
    onCancel: () => void
  }

  let { open, title, qualifiedId, blockedPreview = 0, onComplete, onCancel }: Props = $props()

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
  const closeReasonOptions = closeReasons.map((reason) => ({
    value: reason.value,
    label: `${reason.label}. ${reason.description}`,
  }))

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

  let completeReason = $state<CloseReason>('done')
  let completeMessage = $state('')
  let evidenceType = $state<DoneEvidence>('test')
  let evidenceValue = $state('')
  let targetIssue = $state('')
  let pending = $state(false)
  let completeMessageInput: HTMLTextAreaElement | null = $state(null)

  function openTasks(count: number): string {
    return `${count} open ${count === 1 ? 'task' : 'tasks'}`
  }

  // Each opening starts a fresh form with the note focused.
  let wasOpen = false
  $effect(() => {
    if (open && !wasOpen) {
      completeReason = 'done'
      completeMessage = ''
      evidenceType = 'test'
      evidenceValue = ''
      targetIssue = ''
      pending = false
      queueMicrotask(() => completeMessageInput?.focus())
    }
    wasOpen = open
  })

  function closeCompleteDialog(): void {
    if (pending) return
    onCancel()
  }

  async function completeIssue(): Promise<void> {
    if (pending || !canComplete()) return
    pending = true
    try {
      await onComplete(closeRequest())
    } finally {
      pending = false
    }
  }

  function messageMinimum(): number {
    if (completeReason === 'wontfix') return 60
    if (completeReason === 'duplicate' || completeReason === 'superseded') return 20
    return 40
  }

  function messageHint(): string {
    const minimum = `at least ${messageMinimum()} characters`
    return completeReason === 'wontfix' ? `optional; ${minimum} if written` : minimum
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

  function handleCompleteKeydown(event: KeyboardEvent): void {
    if ((event.metaKey || event.ctrlKey) && event.key === 'Enter') {
      event.preventDefault()
      void completeIssue()
    }
  }
</script>

<Modal {open} title="Complete task" onClose={closeCompleteDialog} width={480}>
  <div class="complete-dialog" onkeydown={handleCompleteKeydown} role="presentation">
    <div class="complete-context">
      <p class="complete-task-title">{title}</p>
      <p class="complete-task-id">{qualifiedId}</p>
    </div>

    <RadioGroup
      legend="Reason"
      name="complete-reason"
      disabled={pending}
      value={completeReason}
      onchange={chooseCloseReason}
      options={closeReasonOptions}
    />

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
      <span>Completion note <small>({messageHint()}, markdown)</small></span>
      <textarea
        bind:this={completeMessageInput}
        rows="4"
        placeholder="What was done? Any follow-ups? Cmd/Ctrl+Enter to confirm."
        bind:value={completeMessage}
        disabled={pending}
      ></textarea>
    </label>
    <Card level="inset" padding="sm">
      <div>Comments and links stay.</div>
      {#if blockedPreview}<div>
          {openTasks(blockedPreview)}
          {blockedPreview === 1 ? 'loses' : 'lose'} this blocker.
        </div>{/if}
    </Card>
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
