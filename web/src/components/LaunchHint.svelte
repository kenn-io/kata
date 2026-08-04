<script lang="ts">
  interface Props {
    issueRef: string | undefined
  }

  let { issueRef }: Props = $props()
</script>

<section aria-labelledby="launch-heading">
  <h2 id="launch-heading">Launch Kata securely</h2>
  <p>This local tab is not authorized. Run <code>kata ui{issueRef ? ` ${issueRef}` : ''}</code>.</p>
</section>
