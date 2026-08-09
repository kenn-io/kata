# `@kenn-io/kata-ui`

Kata-owned Svelte presentation components for embedding read-only Kata issue details in trusted hosts.

The package performs no networking and owns no routing or persistence state. Hosts fetch a canonical Kata issue-detail response, verify its API schema with `supportsKataAPISchema`, project the response with `projectIssueDetail`, and render `IssueDetail`. Optional host actions remain neutral callbacks supplied by the embedding application.

Forge linkage and other host metadata stay in the host. The package receives
only Kata wire data and host-supplied actions.

```svelte
<script lang="ts">
  import {
    IssueDetail,
    projectIssueDetail,
    supportsKataAPISchema,
    type KataIssueDetailModel,
    type KataIssueDetailWire,
  } from '@kenn-io/kata-ui'

  let { daemonURL, issueUID }: { daemonURL: string; issueUID: string } = $props()
  let detail: KataIssueDetailModel | undefined = $state()
  let unavailable = $state('')

  $effect(() => {
    void loadIssue(daemonURL, issueUID)
  })

  async function loadIssue(origin: string, uid: string): Promise<void> {
    detail = undefined
    unavailable = ''
    const health = await fetch(`${origin}/api/v1/health`).then((response) => response.json())
    if (!supportsKataAPISchema(health.api_schema_version ?? '')) {
      unavailable = 'This Kata daemon is not compatible with the embedded issue detail.'
      return
    }
    const wire: KataIssueDetailWire = await fetch(
      `${origin}/api/v1/issues/${encodeURIComponent(uid)}`,
    ).then((response) => response.json())
    detail = projectIssueDetail(wire)
  }
</script>

{#if detail}
  <IssueDetail {detail} />
{:else if unavailable}
  <p>{unavailable}</p>
{/if}
```

`supportsKataAPISchema` accepts Kata API schemas `>=0.9.0 <0.11.0`. A missing
or empty `api_schema_version` is incompatible for an embedding host. Read the
version from `GET /api/v1/health`; issue-detail responses do not carry it.
