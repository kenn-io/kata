<script lang="ts">
  interface Props {
    onReload?: (() => void) | undefined
  }

  let { onReload = () => window.location.reload() }: Props = $props()
</script>

<section class="version-mismatch" role="alert" aria-labelledby="version-mismatch-heading">
  <h2 id="version-mismatch-heading">Kata was updated</h2>
  <p>This tab could not load the current application files. Reload it to use the latest version.</p>
  <button type="button" onclick={onReload}>Reload Kata</button>
</section>

<style>
  .version-mismatch {
    width: min(32rem, calc(100% - 2rem));
    margin: auto;
    padding: 1.5rem;
    border: 1px solid var(--border-default);
    border-radius: 0.75rem;
    background: var(--surface-raised);
    color: var(--text-primary);
  }

  h2,
  p {
    margin: 0 0 0.75rem;
  }

  button {
    border: 1px solid var(--border-default);
    border-radius: 0.375rem;
    padding: 0.5rem 0.75rem;
    background: var(--surface-interactive);
    color: inherit;
    font: inherit;
    cursor: pointer;
  }
</style>
