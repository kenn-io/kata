<script lang="ts">
  import { onDestroy } from 'svelte'
  import { Button, TextInput, Checkbox } from '@kenn-io/kit-ui'
  import type { CronDefinitionDraft } from '../../lib/cron/jobDraft'
  import type { CronDefinitionRecord } from '../../lib/cron/client'
  import {
    jobFields,
    setJobField,
    validateJobDraft,
    type JobFieldContext,
  } from '../../lib/cron/jobFields'
  import { rawCronValue } from '../../lib/cron/document'
  let {
    draft,
    workflows,
    projectUID,
    readonly = false,
    pending = false,
    conflict = undefined,
    error = undefined,
    onchange,
    onsubmit,
    oncancel,
    onrebase = undefined,
    resolveIssue,
    operation = 'save',
  }: {
    operation?: 'save' | 'delete'
    draft: CronDefinitionDraft
    workflows: JobFieldContext['workflows']
    projectUID: string
    readonly?: boolean
    pending?: boolean
    conflict?: CronDefinitionRecord | undefined
    error?: string | undefined
    onchange: (draft: CronDefinitionDraft) => void
    onsubmit: (draft: CronDefinitionDraft) => void | Promise<void>
    oncancel: () => void
    onrebase?: (() => void) | undefined
    resolveIssue: (projectID: number, reference: string) => Promise<string>
  } = $props()
  let destroyed = false
  onDestroy(() => {
    destroyed = true
  })
  let current = $derived(draft)
  let errors = $state<Record<string, string>>({})
  // Text being typed stays as entered; the draft holds its canonical value, and
  // the field shows that value once focus leaves it.
  let buffers = $state<Record<string, string>>({})
  let emitted: CronDefinitionDraft | undefined
  $effect(() => {
    if (draft !== emitted) buffers = {}
  })
  let resolving = $state(false)
  const fields = $derived(
    jobFields(current, { persisted: !!current.definitionEventUID, projectUID, workflows }),
  )
  const disabled = $derived(readonly || pending || resolving || !!conflict)
  function change(key: string, value: string) {
    if (disabled || operation === 'delete') return
    try {
      current = setJobField(current, key, value)
      emitted = current
      onchange(current)
      if (Object.keys(errors).length) errors = validateJobDraft(current)
    } catch (e) {
      errors = { ...errors, [key]: e instanceof Error ? e.message : 'Invalid value' }
    }
  }
  function display(key: string) {
    if (key === 'other_options') return rawCronValue(current.original, ['options']) ?? '—'
    if (key === 'overlap' && current.values.mode === 'recurring') return 'allow'
    return current.values[key] ?? ''
  }
  async function submit() {
    if (disabled) return
    buffers = {}
    if (operation === 'delete') {
      await onsubmit(current)
      return
    }
    errors = validateJobDraft(current)
    if (
      current.values.mode === 'workflow' &&
      !workflows.some((w) => !w.deleted && w.uid === current.values.workflow)
    )
      errors = { ...errors, workflow: 'Select an available workflow in this project.' }
    if (Object.keys(errors).length) return
    resolving = true
    let captured = current
    let resolved = current
    try {
      for (const key of ['issue', 'parent', 'trigger_issue']) {
        if (!fields.some((f) => f.key === key && f.kind !== 'readonly')) continue
        const ref = resolved.values[key]
        if (!ref || /^[0-7][0-9A-HJKMNP-TV-Z]{25}$/.test(ref)) continue
        const uid = await resolveIssue(resolved.projectID, ref)
        // Recheck after each read: a daemon switch/authentication rejection must
        // never freeze a newly privileged attempt from an old draft.
        if (destroyed || readonly || pending || current !== captured) return
        resolved = setJobField(resolved, key, uid)
        current = resolved
        captured = resolved
        emitted = resolved
        onchange(resolved)
      }
      if (destroyed || readonly || pending || current !== captured) return
      current = resolved
      emitted = resolved
      onchange(resolved)
      await onsubmit(resolved)
    } catch (e) {
      errors = {
        ...errors,
        reference: e instanceof Error ? e.message : 'Issue reference could not be resolved.',
      }
    } finally {
      resolving = false
    }
  }
</script>

<form
  class="job-editor"
  aria-label="Job editor"
  onsubmit={(event) => {
    event.preventDefault()
    void submit()
  }}
>
  {#if error}<p role="alert">{error}</p>{/if}
  {#if conflict}
    <section aria-label="Definition conflict">
      <p role="alert">
        This job changed. Compare both versions, then explicitly rebase your edits before saving
        again.
      </p>
      <div class="comparison">
        <div>
          <h4>Your draft · {current.values.name}</h4>
          <pre>{Object.entries(current.values)
              .filter(([key]) => !key.includes(':'))
              .map(([key, value]) => `${key}: ${value}`)
              .join('\n')}</pre>
          <h4>Original definition</h4>
          <pre>{current.original.source}</pre>
        </div>
        <div>
          <h4>Newer version · {conflict.name}</h4>
          <pre>{conflict.document.source}</pre>
        </div>
      </div>
      <Button
        label="Rebase my edits on newer version"
        onclick={onrebase}
        disabled={readonly || pending || !onrebase || !!conflict.deletedAt}
      />
    </section>
  {/if}
  {#each fields as field (field.key)}
    <div class="field">
      {#if field.kind === 'readonly'}
        <span>{field.label}</span>
        <pre>{display(field.key) || '—'}</pre>
        <small>{field.readOnlyReason}</small>
      {:else if field.kind === 'choice'}
        <label for={`cron-job-${field.key}`}>{field.label}</label>
        <select
          id={`cron-job-${field.key}`}
          value={current.values[field.key] ?? ''}
          disabled={disabled || operation === 'delete'}
          onchange={(event) => change(field.key, event.currentTarget.value)}
        >
          {#if !field.choices?.some((c) => c.value === current.values[field.key])}<option
              value={current.values[field.key] ?? ''}
              >{current.values[field.key] || 'Select…'}</option
            >{/if}
          {#each field.choices ?? [] as choice (choice.value)}<option value={choice.value}
              >{choice.label}</option
            >{/each}
        </select>
      {:else if field.kind === 'bool'}
        <Checkbox
          label={field.label}
          checked={current.values[field.key] === 'true'}
          disabled={disabled || operation === 'delete'}
          onchange={(checked) => change(field.key, String(checked))}
        />
      {:else if field.kind === 'textarea'}
        <label for={`cron-job-${field.key}`}>{field.label}</label>
        <textarea
          id={`cron-job-${field.key}`}
          value={current.values[field.key] ?? ''}
          disabled={disabled || operation === 'delete'}
          aria-invalid={!!errors[field.key]}
          oninput={(event) => change(field.key, event.currentTarget.value)}
        ></textarea>
      {:else}
        <label for={`cron-job-${field.key}`}>{field.label}</label>
        <TextInput
          id={`cron-job-${field.key}`}
          value={buffers[field.key] ?? current.values[field.key] ?? ''}
          disabled={disabled || operation === 'delete'}
          invalid={!!errors[field.key]}
          oninput={(value) => {
            buffers = { ...buffers, [field.key]: value }
            change(field.key, value)
          }}
          onblur={() => {
            const rest = { ...buffers }
            delete rest[field.key]
            buffers = rest
          }}
          block
        />
      {/if}
      {#if errors[field.key]}<p role="alert">{errors[field.key]}</p>{/if}
    </div>
  {/each}
  {#if errors.reference}<p role="alert">{errors.reference}</p>{/if}
  <p>
    Durations accept whole seconds or units such as 2h30m. Run at accepts RFC3339 or local wall time
    in the selected timezone. The daemon validates the definition on save.
  </p>
  <footer>
    <Button
      label={resolving
        ? 'Resolving references…'
        : operation === 'delete'
          ? 'Confirm delete'
          : 'Save job'}
      type="submit"
      {disabled}
    /><Button label="Cancel" onclick={oncancel} disabled={pending || resolving} />
  </footer>
</form>

<style>
  .job-editor {
    display: flex;
    flex-direction: column;
    gap: var(--space-4);
    padding: var(--space-4);
  }
  .field {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
  }
  label,
  span {
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
    min-height: 6rem;
    resize: vertical;
  }
  pre {
    white-space: pre-wrap;
    overflow-wrap: anywhere;
    font-size: var(--font-size-xs);
    margin: 0;
  }
  p,
  small {
    font-size: var(--font-size-sm);
    color: var(--text-secondary);
  }
  [role='alert'] {
    color: var(--accent-red);
  }
  footer,
  .comparison {
    display: flex;
    gap: var(--space-3);
  }
  .comparison > div {
    flex: 1;
    min-width: 0;
  }
</style>
