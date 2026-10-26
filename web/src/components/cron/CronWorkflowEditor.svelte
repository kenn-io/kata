<script lang="ts">
  import { Button, TextInput } from '@kenn-io/kit-ui'
  import type { CronDefinitionRecord } from '../../lib/cron/client'
  import {
    addWorkflowStep,
    deleteWorkflowStep,
    serializeWorkflowDraft,
    setWorkflowField,
    setWorkflowStepField,
    validateWorkflowDraft,
    workflowDependents,
    type WorkflowDraft,
  } from '../../lib/cron/workflowDraft'
  import CronWorkflowStep from './CronWorkflowStep.svelte'
  import CronWorkflowGraph from './CronWorkflowGraph.svelte'
  let {
    draft,
    readonly = false,
    pending = false,
    conflict = undefined,
    error = undefined,
    onchange,
    onsubmit,
    oncancel,
    onrebase = undefined,
  }: {
    draft: WorkflowDraft
    readonly?: boolean
    pending?: boolean
    conflict?: CronDefinitionRecord | undefined
    error?: string | undefined
    onchange: (draft: WorkflowDraft) => void
    onsubmit: (draft: WorkflowDraft) => void | Promise<void>
    oncancel: () => void
    onrebase?: (() => void) | undefined
  } = $props()
  let current = $derived(draft)
  const problems = $derived(validateWorkflowDraft(current))
  const disabled = $derived(readonly || pending || !!conflict)
  let keyErrors = $state<Record<string, string | undefined>>({})
  let deleting = $state<string | undefined>(undefined)
  const dependents = $derived(deleting ? workflowDependents(current, deleting) : [])
  let submitError = $state<string | undefined>(undefined)
  function update(next: WorkflowDraft) {
    if (disabled) return
    current = next
    submitError = undefined
    onchange(next)
  }
  function remove(id: string, confirmed = false) {
    if (disabled) return
    if (!confirmed && workflowDependents(current, id).length) {
      deleting = id
      return
    }
    update(deleteWorkflowStep(current, id, confirmed))
    keyErrors = { ...keyErrors, [id]: undefined }
    deleting = undefined
  }
  async function submit() {
    if (disabled || problems.length || Object.values(keyErrors).some(Boolean)) return
    try {
      serializeWorkflowDraft(current)
      await onsubmit(current)
    } catch (error) {
      submitError = error instanceof Error ? error.message : 'Workflow could not be saved.'
    }
  }
</script>

<form
  class="workflow-editor"
  aria-label="Workflow editor"
  onsubmit={(event) => {
    event.preventDefault()
    void submit()
  }}
>
  {#if error}<p role="alert">{error}</p>{/if}
  {#if submitError}<p role="alert">{submitError}</p>{/if}
  {#if conflict}
    <section aria-label="Definition conflict">
      <p role="alert">
        This workflow changed. Compare both versions, then explicitly rebase your edits before
        saving again.
      </p>
      <h4>Your draft · {current.values.name}</h4>
      <pre>{JSON.stringify(
          {
            name: current.values.name,
            about: current.about,
            input: current.input,
            options: current.options.source,
            steps: current.steps.map((s) => ({
              key: s.key,
              kind: s.kind,
              command: s.command,
              prompt: s.prompt,
              after: s.after,
              retries: s.retries,
              options: s.options.source,
            })),
          },
          null,
          2,
        )}</pre>
      <h4>Original definition</h4>
      <pre>{current.original.source}</pre>
      <h4>Newer version · {conflict.name}</h4>
      <pre>{conflict.document.source}</pre>
      <Button
        label="Rebase my edits on newer version"
        onclick={onrebase}
        disabled={readonly || pending || !onrebase || !!conflict.deletedAt}
      />
    </section>
  {/if}
  <label for="workflow-name">Name</label>
  <TextInput
    id="workflow-name"
    value={current.values.name ?? ''}
    invalid={problems.some((p) => p.field === 'name')}
    {disabled}
    oninput={(value) => update(setWorkflowField(current, 'name', value))}
    block
  />
  {#each [{ key: 'about', label: 'About', value: current.about }, { key: 'input', label: 'Input', value: current.input }, { key: 'options', label: 'Workflow options', value: current.options.source }] as field (field.key)}
    <label for={`workflow-${field.key}`}>{field.label}</label>
    <textarea
      id={`workflow-${field.key}`}
      value={field.value}
      {disabled}
      aria-invalid={problems.some((p) => !p.editorID && p.field === field.key)}
      oninput={(event) =>
        update(
          setWorkflowField(
            current,
            field.key as 'about' | 'input' | 'options',
            event.currentTarget.value,
          ),
        )}
    ></textarea>
  {/each}
  {#each problems.filter((p) => !p.editorID) as problem, i (`${problem.field}:${i}`)}<p
      role="alert"
    >
      {problem.field}: {problem.message}
    </p>{/each}
  <p>
    Dependencies name earlier steps, separated by commas. Options accept strict JSON objects;
    numeric values remain exact.
  </p>
  {#each current.steps as step (step.editorID)}
    <CronWorkflowStep
      {step}
      problems={problems.filter((p) => p.editorID === step.editorID)}
      {disabled}
      onchange={(field, value) =>
        update(setWorkflowStepField(current, step.editorID, field, value))}
      onkeyerror={(message) => {
        keyErrors = { ...keyErrors, [step.editorID]: message }
      }}
      ondelete={() => remove(step.editorID)}
    />
  {/each}
  {#if deleting}
    <section aria-label="Confirm step deletion">
      <p>
        Direct dependents: {dependents.join(', ')}. Delete this step and remove their references?
      </p>
      <Button
        label="Delete step and remove references"
        {disabled}
        onclick={() => deleting && remove(deleting, true)}
      />
      <Button
        label="Keep step"
        {disabled}
        onclick={() => {
          deleting = undefined
        }}
      />
    </section>
  {/if}
  <div class="actions">
    <Button
      label="Add command step"
      {disabled}
      onclick={() => update(addWorkflowStep(current, 'command'))}
    /><Button
      label="Add prompt step"
      {disabled}
      onclick={() => update(addWorkflowStep(current, 'prompt'))}
    />
  </div>
  <CronWorkflowGraph draft={current} />
  <footer>
    <Button label="Save workflow" type="submit" {disabled} /><Button
      label="Cancel"
      onclick={oncancel}
      disabled={pending}
    />
  </footer>
</form>

<style>
  .workflow-editor {
    display: flex;
    flex-direction: column;
    gap: var(--space-3);
    padding: var(--space-4);
  }
  label,
  p {
    font-size: var(--font-size-sm);
    color: var(--text-secondary);
  }
  textarea {
    font: inherit;
    color: var(--text-primary);
    background: var(--bg-surface);
    border: 1px solid var(--border-default);
    border-radius: var(--radius-sm);
    padding: var(--space-2);
    min-height: 5rem;
    resize: vertical;
  }
  pre {
    white-space: pre-wrap;
    overflow-wrap: anywhere;
    font-size: var(--font-size-xs);
  }
  [role='alert'] {
    color: var(--accent-red);
  }
  footer,
  .actions {
    display: flex;
    gap: var(--space-2);
    flex-wrap: wrap;
  }
</style>
