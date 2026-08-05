<script lang="ts">
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

<section aria-labelledby="login-heading">
  <h2 id="login-heading">Log in to Kata</h2>
  <form aria-label="Log in to Kata" onsubmit={submit}>
    <label for="kata-token">Token</label>
    <input id="kata-token" type="password" autocomplete="current-password" bind:value={token} />
    <button type="submit" disabled={!token || submitting}>
      {submitting ? 'Logging in…' : 'Log in'}
    </button>
  </form>
  {#if error}<p role="alert">{error}</p>{/if}
</section>
