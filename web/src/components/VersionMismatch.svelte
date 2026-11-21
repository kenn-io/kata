<script lang="ts">
  import { Button } from '@kenn-io/kit-ui'
  import StatusPage from './shared/StatusPage.svelte'

  interface Props {
    onReload?: (() => void) | undefined
  }

  let { onReload = () => window.location.reload() }: Props = $props()
</script>

<StatusPage alert title="Kata was updated" description="Reload to use the new version.">
  <Button tone="info" surface="solid" label="Reload" onclick={onReload} />
</StatusPage>
