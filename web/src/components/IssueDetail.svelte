<script lang="ts">
  import type { ComponentProps } from 'svelte'

  import IssueEditor from './IssueEditor.svelte'

  let props: ComponentProps<typeof IssueEditor> = $props()
</script>

<IssueEditor {...props} />
