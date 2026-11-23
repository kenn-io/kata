<script lang="ts">
  import { Button } from '@kenn-io/kit-ui'

  import Modal from './Modal.svelte'

  interface Props {
    projectName: string
    inboxName?: string | undefined
    disabled?: boolean | undefined
    onConfirm: () => Promise<void>
  }

  let { projectName, inboxName = undefined, disabled = false, onConfirm }: Props = $props()

  let open = $state(false)
  let pending = $state(false)
  let error = $state<string | null>(null)

  function close(): void {
    if (pending) return
    open = false
    error = null
  }

  async function confirm(): Promise<void> {
    if (disabled || pending) return
    pending = true
    error = null
    try {
      await onConfirm()
      open = false
    } catch (caught) {
      error = caught instanceof Error ? caught.message : 'Could not change the Inbox.'
    } finally {
      pending = false
    }
  }
</script>

<Button size="sm" label="Make Inbox…" {disabled} onclick={() => (open = true)} />
<Modal {open} title="Make Inbox" onClose={close} width={420}>
  <p class="body">
    New tasks will start in <strong>{projectName}</strong>.
    {#if inboxName}<strong>{inboxName}</strong> stops being the Inbox; its tasks stay where they are.{/if}
  </p>
  {#if error}<p class="error" role="alert">{error}</p>{/if}
  {#snippet footer()}
    <Button size="sm" label="Cancel" disabled={pending} onclick={close} />
    <Button
      size="sm"
      tone="info"
      surface="solid"
      label="Make Inbox"
      disabled={disabled || pending}
      onclick={() => void confirm()}
    />
  {/snippet}
</Modal>

<style>
  .body {
    color: var(--text-primary);
    font-size: var(--font-size-md);
    line-height: 1.45;
  }
  .error {
    color: var(--accent-red);
    font-size: var(--font-size-sm);
  }
</style>
