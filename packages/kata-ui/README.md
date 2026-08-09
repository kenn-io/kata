# `@kenn-io/kata-ui`

Kata-owned Svelte presentation components for embedding read-only Kata issue details in trusted hosts.

The package performs no networking and owns no routing or persistence state. Hosts fetch a canonical Kata issue-detail response, verify its API schema with `supportsKataAPISchema`, project the response with `projectIssueDetail`, and render `IssueDetail`. Optional host actions remain neutral callbacks supplied by the embedding application.
