<script lang="ts">
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
      error = 'Login failed. Check the token and try again.'
    } finally {
      submitting = false
    }
  }
</script>

<section class="login-view" aria-labelledby="login-heading">
  <h2 id="login-heading">Log in to Kata</h2>
  <form aria-label="Log in to Kata" onsubmit={submit}>
    <label for="kata-token">Token</label>
    <input id="kata-token" type="password" autocomplete="current-password" bind:value={token} />
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
</section>

<style>
  .login-view {
    display: grid;
    width: min(calc(100vw - var(--space-8)), 360px);
    gap: var(--space-6);
    text-align: left;
  }

  h2 {
    font-size: var(--font-size-xl);
    line-height: 1.25;
    text-align: center;
  }

  form {
    display: grid;
    gap: var(--space-4);
  }

  label {
    color: var(--text-secondary);
    font-size: var(--font-size-sm);
    font-weight: var(--font-weight-semibold);
  }

  input {
    width: 100%;
    min-height: 36px;
  }

  form :global(.login-submit) {
    width: 100%;
    margin-top: var(--space-2);
  }

  [role='alert'] {
    color: var(--accent-red);
    font-size: var(--font-size-sm);
  }
</style>
