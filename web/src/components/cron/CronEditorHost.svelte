<script module lang="ts">
  export interface CronEditorHandle {
    create: (kind: 'job' | 'workflow') => void
    openJob: (operation: 'edit' | 'toggle' | 'delete') => Promise<void>
    openWorkflow: () => Promise<void>
    isOpen: () => boolean
    isPending: () => boolean
    error: () => string | undefined
  }
</script>

<script lang="ts">
  import { DetailDrawer, Button } from '@kenn-io/kit-ui'
  import type { CronState } from '../../lib/cron/controller'
  import type { CronRouteState, CronJob } from '../../lib/cron/types'
  import { onDestroy } from 'svelte'
  import CronJobEditor from './CronJobEditor.svelte'
  import CronWorkflowEditor from './CronWorkflowEditor.svelte'
  import {
    buildWorkflowDraft,
    rebaseWorkflowDraft,
    serializeWorkflowDraft,
    type WorkflowDraft,
  } from '../../lib/cron/workflowDraft'
  import { CronClient } from '../../lib/cron/client'
  import { buildCronJobDraft, type CronDefinitionDraft } from '../../lib/cron/jobDraft'
  import { jobFields, setJobField } from '../../lib/cron/jobFields'
  import {
    CronMutations,
    freezeCronAttempt,
    type CronAttempt,
    type CronMutationOptions,
    type CronMutationOutcome,
  } from '../../lib/cron/mutations'
  import { isAuthenticationRequiredError } from '../../lib/auth/session'
  let {
    route,
    state: cronState,
    onNavigate,
    onRefresh,
    canMutate = false,
    mutationPending = false,
    mutationMessage = undefined,
    authorityScope = 0,
    onMutation = undefined,
    resolveIssue = undefined,
  }: {
    route: CronRouteState
    state: Readonly<CronState>
    onNavigate: (route: CronRouteState) => void
    onRefresh: () => void
    canMutate?: boolean
    mutationPending?: boolean
    mutationMessage?: string | undefined
    authorityScope?: string | number
    onMutation?: CronMutationOptions['onMutation'] | undefined
    resolveIssue?: ((projectID: number, reference: string) => Promise<string>) | undefined
  } = $props()
  const projects = $derived(
    cronState.projects.filter(
      (p) => !p.error && (!route.projectUIDs.length || route.projectUIDs.includes(p.project.uid)),
    ),
  )
  const selectionProject = $derived(
    cronState.projects.find((p) => p.project.uid === cronState.selected?.projectUID)?.project,
  )
  type Editor = { projectUID: string; scope: string | number } & (
    | { kind: 'job'; draft: CronDefinitionDraft }
    | { kind: 'workflow'; draft: WorkflowDraft }
  )
  let editor = $state.raw<Editor | undefined>(undefined)
  let intentOperation = $state<'save' | 'delete'>('save')
  let localPending = $state(false)
  let attempt = $state.raw<CronAttempt | undefined>(undefined)
  let outcome = $state.raw<CronMutationOutcome | undefined>(undefined)
  let localError = $state<string | undefined>(undefined)
  const client = new CronClient()
  let abort = new AbortController()
  let abortScope: string | number | undefined
  const mutations = new CronMutations({
    onMutation: (draft, send) => (onMutation ? onMutation(draft, send) : Promise.resolve(false)),
    scope: () => authorityScope,
  })
  const writable = $derived(canMutate && !!onMutation && !mutationPending && !localPending)
  const editorFenced = $derived(!!editor && editor.scope !== authorityScope)
  const uncertain = $derived(outcome?.kind === 'uncertain' || outcome?.kind === 'retryable')
  const workflows = $derived(
    cronState.projects
      .find((p) => p.project.uid === editor?.projectUID)
      ?.workflows.map((w) => ({ uid: w.uid, name: w.name, deleted: !!w.deleted_at })) ?? [],
  )
  $effect(() => {
    if (abortScope !== undefined && abortScope !== authorityScope) {
      abort.abort()
      abort = new AbortController()
      localPending = false
    }
    abortScope = authorityScope
  })
  onDestroy(() => abort.abort())
  // A definition read opens an editor only for the selection that asked for it.
  // Closing or changing the selection discards the read; open drafts stay.
  let selectionGeneration = 0
  const selectionKey = $derived(
    cronState.selected
      ? JSON.stringify([
          cronState.selected.kind,
          cronState.selected.projectUID,
          cronState.selected.uid,
        ])
      : '',
  )
  $effect(() => {
    void selectionKey
    selectionGeneration++
  })
  function isCurrent(controller: AbortController, scope: string | number) {
    return controller === abort && !controller.signal.aborted && scope === authorityScope
  }
  export function isOpen() {
    return !!editor
  }
  export function isPending() {
    return localPending
  }
  export function error(): string | undefined {
    return localError
  }
  function cancelEditor() {
    if (localPending || mutationPending || (uncertain && !editorFenced)) return
    editor = undefined
    attempt = undefined
    outcome = undefined
    localError = undefined
  }
  export function create(kind: 'job' | 'workflow' = 'job') {
    if (!writable) return
    const project = projects[0]?.project
    if (!project) return
    const context = { projectUID: project.uid, scope: authorityScope }
    editor =
      kind === 'workflow'
        ? { ...context, kind, draft: buildWorkflowDraft(undefined, project.id) }
        : { ...context, kind, draft: buildCronJobDraft(undefined, project.id) }
    intentOperation = 'save'
    attempt = undefined
    outcome = undefined
    localError = undefined
  }
  function changeProject(uid: string) {
    if (!editor || editor.draft.definitionEventUID || !writable || attempt) return
    const project = cronState.projects.find((p) => !p.error && p.project.uid === uid)?.project
    if (!project) return
    if (editor.kind === 'workflow') {
      editor = { ...editor, projectUID: uid, draft: { ...editor.draft, projectID: project.id } }
      return
    }
    const values = {
      ...editor.draft.values,
      workflow: '',
      issue: '',
      parent: '',
      trigger_issue: '',
    }
    editor = {
      ...editor,
      projectUID: uid,
      draft: { ...editor.draft, projectID: project.id, values },
    }
  }
  export async function openJob(operation: 'edit' | 'toggle' | 'delete') {
    if (!writable || !selectionProject || cronState.selected?.kind !== 'job') return
    const project = selectionProject,
      uid = cronState.selected.uid,
      scope = authorityScope,
      controller = abort,
      generation = selectionGeneration
    const observed = cronState.detail as CronJob
    const observedEvent = observed.definition_event_uid
    const desiredEnabled = !observed.definition.enabled
    localPending = true
    localError = undefined
    try {
      const record = await client.readDefinition('job', project.id, uid, controller.signal)
      if (!isCurrent(controller, scope) || !canMutate || generation !== selectionGeneration) return
      if (record.deletedAt) {
        localError = 'This job was deleted. Refresh the catalog.'
        return
      }
      if (operation !== 'edit' && record.definitionEventUID !== observedEvent) {
        localError = 'This job changed. Refresh the catalog and review it before trying again.'
        return
      }
      let draft = buildCronJobDraft(record, project.id)
      if (operation === 'toggle') draft = setJobField(draft, 'enabled', String(desiredEnabled))
      editor = { kind: 'job', draft, projectUID: project.uid, scope }
      attempt = undefined
      outcome = undefined
      intentOperation = operation === 'delete' ? 'delete' : 'save'
      if (operation !== 'edit') await execute(draft, operation === 'delete' ? 'delete' : 'save')
    } catch (error) {
      if (isCurrent(controller, scope) && generation === selectionGeneration)
        localError = isAuthenticationRequiredError(error)
          ? 'Authentication required.'
          : error instanceof Error
            ? error.message
            : 'Job could not be read.'
    } finally {
      if (isCurrent(controller, scope)) localPending = false
    }
  }
  export async function openWorkflow() {
    if (!writable || !selectionProject || cronState.selected?.kind !== 'workflow') return
    const project = selectionProject,
      uid = cronState.selected.uid,
      scope = authorityScope,
      controller = abort,
      generation = selectionGeneration
    localPending = true
    localError = undefined
    try {
      const record = await client.readDefinition('workflow', project.id, uid, controller.signal)
      if (!isCurrent(controller, scope) || !canMutate || generation !== selectionGeneration) return
      if (record.deletedAt) {
        localError = 'This workflow was deleted. Refresh the catalog.'
        return
      }
      editor = {
        kind: 'workflow',
        draft: buildWorkflowDraft(record, project.id),
        projectUID: project.uid,
        scope,
      }
      intentOperation = 'save'
      attempt = undefined
      outcome = undefined
    } catch (error) {
      if (isCurrent(controller, scope) && generation === selectionGeneration)
        localError = isAuthenticationRequiredError(error)
          ? 'Authentication required.'
          : error instanceof Error
            ? error.message
            : 'Workflow could not be read.'
    } finally {
      if (isCurrent(controller, scope)) localPending = false
    }
  }
  async function execute(
    draft: CronDefinitionDraft | WorkflowDraft,
    operation: 'save' | 'delete' = intentOperation,
  ) {
    if (!canMutate || mutationPending || editorFenced || abort.signal.aborted) return
    const scope = authorityScope,
      controller = abort
    localPending = true
    localError = undefined
    try {
      attempt ??= freezeCronAttempt(
        draft,
        operation,
        editor?.kind === 'workflow' ? serializeWorkflowDraft(editor.draft) : undefined,
      )
      const result = await mutations.execute(attempt, controller.signal)
      if (!isCurrent(controller, scope)) return
      outcome = result
      if (result.kind === 'saved' || result.kind === 'deleted') {
        editor = undefined
        attempt = undefined
        outcome = undefined
        if (result.kind === 'deleted') onNavigate({ ...route, selection: undefined })
        onRefresh()
      } else if (result.kind === 'blocked') {
        attempt = undefined
        localError = 'Save was rejected. Review the daemon error and correct the draft.'
      }
    } catch (error) {
      if (isCurrent(controller, scope))
        localError = error instanceof Error ? error.message : 'Job could not be saved.'
    } finally {
      if (isCurrent(controller, scope)) localPending = false
    }
  }
  async function reconcile() {
    if (!attempt || !writable || editorFenced) return
    localPending = true
    const scope = authorityScope,
      controller = abort
    try {
      const result = await mutations.reconcile(attempt, controller.signal)
      if (isCurrent(controller, scope)) {
        outcome = result
        if (result.kind === 'saved' || result.kind === 'deleted') {
          editor = undefined
          attempt = undefined
          outcome = undefined
          onRefresh()
        }
      }
    } finally {
      if (isCurrent(controller, scope)) localPending = false
    }
  }
  function rebase() {
    if (!editor || !outcome?.record || outcome.record.deletedAt || !writable || editorFenced) return
    try {
      if (editor.kind === 'workflow') {
        editor = { ...editor, draft: rebaseWorkflowDraft(editor.draft, outcome.record) }
        attempt = undefined
        outcome = undefined
        localError = undefined
        return
      }
      const intended = editor.draft
      let draft = buildCronJobDraft(outcome.record, intended.projectID)
      const priority = ['mode', 'trigger', 'issue_kind']
      const keys = [...intended.dirty].sort(
        (a, b) =>
          (priority.includes(a) ? priority.indexOf(a) : 3) -
          (priority.includes(b) ? priority.indexOf(b) : 3),
      )
      const fields = jobFields(intended, {
        persisted: !!intended.definitionEventUID,
        projectUID: editor.projectUID,
        workflows,
      })
      const lockedOwnership = fields.find((field) => field.key === 'mode')?.kind === 'readonly'
      const active = new Set(
        fields
          .filter(
            (field) =>
              !(
                field.key === 'overlap' &&
                intended.values.mode === 'recurring' &&
                !lockedOwnership
              ),
          )
          .map((field) => field.key),
      )
      // Replay editable selectors first, then final visible fields. Read-only
      // ownership fields still reach the setter and its immutable-binding guard.
      // Dormant text remains an editor buffer; the serializer already excludes
      // inactive fields and preserves the newer record's untouched raw values.
      for (const key of keys) {
        if (active.has(key)) draft = setJobField(draft, key, intended.values[key] ?? '')
        else {
          draft.values[key] = intended.values[key] ?? ''
          draft.dirty.add(key)
        }
      }
      for (const [key, value] of Object.entries(intended.values)) {
        if (
          key.startsWith('schedule:') ||
          (key === 'overlap:ordinary' && intended.dirty.has('overlap'))
        )
          draft.values[key] = value
      }
      editor = { ...editor, draft }
      attempt = undefined
      outcome = undefined
      localError = undefined
    } catch (error) {
      localError =
        error instanceof Error ? error.message : 'Edits cannot be applied to the newer definition.'
    }
  }
</script>

{#if editor}
  <DetailDrawer
    title={`${editor.draft.definitionEventUID ? 'Edit' : 'New'} ${editor.kind}`}
    onclose={cancelEditor}
  >
    {#if editorFenced}<p role="alert">
        Authority changed. Close this draft and reopen the definition before saving.
      </p>{/if}
    {#if !editor.draft.definitionEventUID}
      <label class="project-picker"
        >Project<select
          aria-label="Project"
          value={editor.projectUID}
          disabled={!writable || !!attempt || editorFenced}
          onchange={(event) => changeProject(event.currentTarget.value)}
          >{#each cronState.projects.filter((p) => !p.error) as p (p.project.uid)}<option
              value={p.project.uid}>{p.project.name}</option
            >{/each}</select
        ></label
      >
    {/if}
    {#if uncertain}<section aria-label="Uncertain write">
        <p role="status">
          The write outcome is not confirmed. Check the retained request before retrying or closing
          this draft.
        </p>
        <Button
          label="Check outcome"
          onclick={() => void reconcile()}
          disabled={!writable || editorFenced}
        />{#if outcome?.kind === 'retryable'}<Button
            label={attempt?.operation === 'delete' ? 'Retry delete' : 'Retry save'}
            onclick={() => editor && void execute(editor.draft)}
            disabled={!writable || editorFenced}
          />{/if}
      </section>{/if}
    {#if outcome?.kind === 'conflict' && !outcome.record}<p role="alert">
        This definition changed or was deleted. The draft is retained; close and reopen the
        definition to obtain a current version.
      </p>{/if}
    {#if editor.kind === 'workflow'}
      <CronWorkflowEditor
        draft={editor.draft}
        readonly={!canMutate ||
          editorFenced ||
          uncertain ||
          (outcome?.kind === 'conflict' && !outcome.record)}
        pending={mutationPending || localPending}
        conflict={outcome?.kind === 'conflict' ? outcome.record : undefined}
        error={[mutationMessage, localError].filter(Boolean).join(' ') || undefined}
        onchange={(draft) => {
          if (editor?.kind === 'workflow') editor = { ...editor, draft }
        }}
        onsubmit={(draft) => execute(draft)}
        oncancel={cancelEditor}
        onrebase={rebase}
      />
    {:else}<CronJobEditor
        operation={intentOperation}
        draft={editor.draft}
        {workflows}
        projectUID={editor.projectUID}
        readonly={!canMutate ||
          editorFenced ||
          uncertain ||
          (outcome?.kind === 'conflict' && !outcome.record)}
        pending={mutationPending || localPending}
        conflict={outcome?.kind === 'conflict' ? outcome.record : undefined}
        error={[mutationMessage, localError].filter(Boolean).join(' ') || undefined}
        onchange={(draft) => {
          if (editor?.kind === 'job') editor = { ...editor, draft }
        }}
        onsubmit={(draft) => execute(draft)}
        oncancel={cancelEditor}
        onrebase={rebase}
        resolveIssue={resolveIssue ??
          (async () => {
            throw new Error('Issue reference resolution is unavailable.')
          })}
      />{/if}
  </DetailDrawer>
{/if}

<style>
  .project-picker {
    display: flex;
    flex-direction: column;
    padding: var(--space-4);
    gap: var(--space-2);
  }
  p {
    font-size: var(--font-size-sm);
    color: var(--text-secondary);
  }
</style>
