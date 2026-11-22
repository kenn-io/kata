<script lang="ts">
  import { Button } from '@kenn-io/kit-ui'
  import type { ComponentProps } from 'svelte'
  import IssueEditor from './IssueEditor.svelte'

  let props: ComponentProps<typeof IssueEditor> = $props()
</script>

{#if props.returnComment && props.onSelectIssue}
  <Button
    size="sm"
    label="Return to original comment"
    onclick={() =>
      props.onSelectIssue?.({
        uid: props.returnComment!.issueUID,
        commentUID: props.returnComment!.commentUID,
      })}
  />
{/if}
<IssueEditor {...props} />
