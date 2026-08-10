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

  let {
    apiSchemaVersion,
    wire,
  }: { apiSchemaVersion?: string; wire: KataIssueDetailWire } = $props()

  const compatible = $derived(supportsKataAPISchema(apiSchemaVersion ?? ''))
  const detail: KataIssueDetailModel | undefined = $derived(
    compatible ? projectIssueDetail(wire) : undefined,
  )
</script>

{#if detail}
  <IssueDetail {detail} />
{:else}
  <p>This Kata daemon is not compatible with the embedded issue detail.</p>
{/if}
```

`supportsKataAPISchema` accepts Kata API schemas `>=0.9.0 <0.11.0`. A missing
or empty `api_schema_version` is incompatible for an embedding host. Read the
version from `GET /api/v1/health`; issue-detail responses do not carry it.
The embedding host must fetch health and issue data through its authenticated
backend or credential broker, then pass the resulting version and wire data to
the component. Do not expose daemon bearer tokens or browser-session headers to
the package.
