<script lang="ts">
  import StatusPage from './shared/StatusPage.svelte'
  import { Button } from '@kenn-io/kit-ui'

  interface Props {
    returnPath: string
    onLogin: (token: string, returnPath: string) => Promise<void>
  }

  let { returnPath, onLogin }: Props = $props()
  let token = $state('')
  let submitting = $state(false)
  let error = $state('')

  async function submit(event: SubmitEvent): Promise<void> {
    event.preventDefault()
    if (!token || submitting) return
    submitting = true
    error = ''
    try {
      await onLogin(token, returnPath)
      token = ''
    } catch {
      error = "That token didn't work. Check it and try again."
    } finally {
      submitting = false
    }
  }
</script>

<StatusPage title="Log in">
  <form aria-label="Log in" onsubmit={submit}>
    <label for="kata-token">Token</label>
    <input
      id="kata-token"
      type="password"
      autocomplete="current-password"
      aria-invalid={error ? true : undefined}
      bind:value={token}
    />
    <Button
      type="submit"
      tone="info"
      surface="solid"
      class="login-submit"
      label={submitting ? 'Logging in…' : 'Log in'}
      disabled={!token || submitting}
    />
  </form>
  {#if error}<p role="alert">{error}</p>{/if}
</StatusPage>

<style>
  form {
    display: grid;
    width: 100%;
    text-align: left;
    gap: var(--space-4);
  }

  label {
    color: var(--text-secondary);
    font-size: var(--font-size-sm);
    font-weight: var(--font-weight-semibold);
  }

  /* The field takes kit-ui's control height, the same as the Log in button. */
  input {
    box-sizing: border-box;
    width: 100%;
    height: var(--kit-control-height, 28px);
    padding-block: 0;
  }

  input[aria-invalid='true'] {
    border-color: var(--accent-red);
  }

  form :global(.login-submit) {
    width: 100%;
    margin-top: var(--space-2);
  }

  /* Until a token is typed, Log in stays the page's blue action, dimmed. */
  form :global(.kit-button.kit-button--solid.login-submit:disabled) {
    background: var(--accent-blue);
    border-color: var(--accent-blue);
    color: var(--bg-surface);
    opacity: var(--opacity-disabled);
  }

  [role='alert'] {
    color: var(--accent-red);
    font-size: var(--font-size-sm);
  }
</style>
