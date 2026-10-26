<script lang="ts">
  import { Button, TextInput } from '@kenn-io/kit-ui'
  import type { WorkflowStepDraft, WorkflowProblem } from '../../lib/cron/workflowDraft'
  let {
    step,
    problems,
    disabled = false,
    onchange,
    ondelete,
    onkeyerror,
  }: {
    step: WorkflowStepDraft
    problems: WorkflowProblem[]
    disabled?: boolean
    onchange: (
      field: 'key' | 'kind' | 'command' | 'prompt' | 'after' | 'retries' | 'options',
      value: string,
    ) => void
    ondelete: () => void
    onkeyerror: (message: string | undefined) => void
  } = $props()
  let keyBuffer = $state.raw<{ canonical: string; text: string } | undefined>(undefined)
  const keyText = $derived(keyBuffer?.canonical === step.key ? keyBuffer.text : step.key)
  let keyError = $state<string | undefined>(undefined)
  const fields = $derived([
    {
      key: step.kind === 'prompt' ? 'prompt' : 'command',
      label: step.kind === 'prompt' ? 'Prompt' : 'Command',
      value: step.kind === 'prompt' ? step.prompt : step.command,
    },
    { key: 'after', label: 'Runs after', value: step.after.join(', ') },
    { key: 'retries', label: 'Retries', value: step.retries },
    { key: 'options', label: 'Step options', value: step.options.source },
  ] as const)
  function rename(value: string) {
    if (disabled) return
    const previous = step.key
    try {
      onchange('key', value)
      keyBuffer = { canonical: value, text: value }
      keyError = undefined
      onkeyerror(undefined)
    } catch (error) {
      keyBuffer = { canonical: previous, text: value }
      keyError = error instanceof Error ? error.message : 'Invalid key'
      onkeyerror(keyError)
    }
  }
</script>

<fieldset
  class={{ invalid: !!problems.length || !!keyError }}
  aria-label={`Step ${step.key}`}
  {disabled}
>
  <legend>Step {step.key}</legend>
  <label for={`workflow-key-${step.editorID}`}>Key</label>
  <TextInput
    id={`workflow-key-${step.editorID}`}
    value={keyText}
    invalid={!!keyError || problems.some((p) => p.field === 'key')}
    oninput={rename}
    {disabled}
    block
  />
  {#if keyError}<p role="alert">{keyError}</p>{/if}
  <label for={`workflow-kind-${step.editorID}`}>Kind</label>
  <select
    id={`workflow-kind-${step.editorID}`}
    value={step.kind}
    {disabled}
    onchange={(event) => onchange('kind', event.currentTarget.value)}
  >
    <option value="command">Command</option><option value="prompt">Prompt</option>
  </select>
  {#each fields as field (field.key)}
    <label for={`workflow-${field.key}-${step.editorID}`}>{field.label}</label>
    {#if field.key === 'after' || field.key === 'retries'}
      <TextInput
        id={`workflow-${field.key}-${step.editorID}`}
        value={field.value}
        invalid={problems.some((p) => p.field === field.key)}
        {disabled}
        oninput={(value) => onchange(field.key, value)}
        block
      />
    {:else}
      <textarea
        id={`workflow-${field.key}-${step.editorID}`}
        value={field.value}
        {disabled}
        aria-invalid={problems.some((p) => p.field === field.key)}
        oninput={(event) => onchange(field.key, event.currentTarget.value)}
      ></textarea>
    {/if}
  {/each}
  {#each problems as problem, i (`${problem.field}:${problem.code}:${i}`)}<p role="alert">
      {problem.field}: {problem.message}
    </p>{/each}
  <Button label="Delete step" onclick={ondelete} {disabled} />
</fieldset>

<style>
  fieldset {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
    margin: 0;
    border: 1px solid var(--border-default);
    border-radius: var(--radius-sm);
    padding: var(--space-3);
    min-width: 0;
  }
  fieldset.invalid {
    border-color: var(--accent-red);
  }
  label,
  legend {
    font-size: var(--font-size-sm);
    color: var(--text-secondary);
  }
  select,
  textarea {
    font: inherit;
    color: var(--text-primary);
    background: var(--bg-surface);
    border: 1px solid var(--border-default);
    border-radius: var(--radius-sm);
    padding: var(--space-2);
  }
  textarea {
    min-height: 5rem;
    resize: vertical;
  }
  p {
    margin: 0;
    font-size: var(--font-size-sm);
    color: var(--accent-red);
  }
</style>
