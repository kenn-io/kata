# Native federation request signing

Kata adds optional native request signatures to its existing enrollment bearer
transport. The restricted ingress always requires signatures; the hub decides
whether they are mandatory on ordinary listeners. Native project,
actor, capability and transaction checks remain authoritative. This design
adds no database migration and no gateway service.

## Protocol choice

[RFC 9421](https://www.rfc-editor.org/rfc/rfc9421.html) defines the signature
base and structured fields; [RFC 9530](https://www.rfc-editor.org/rfc/rfc9530.html) defines
Content-Digest. HMAC-SHA256 uses a separately provisioned secret with a public
key ID. Kata uses httpsign v0.6.2 and httpsfv for standard canonicalization,
then restricts their flexible APIs to a fixed profile. An independent stdlib
HMAC reference test checks emitted bytes, including the empty-body digest.

The profile covers the method, exact target URI, body digest, content type and
native Authorization bearer. Creation, expiry, random nonce, key ID and
algorithm are mandatory signature parameters. Signing wraps the existing
HTTP transports so metadata, replication and lease operations share source
selection and origin pinning. Rebind metadata uses the same signer, with an
explicitly authorized new target. Local lifecycle and enrollment administration
remain on the ordinary daemon listener.

A fixed hub external HTTPS base reconstructs the proxy-visible target URI.
The proxy strips only its configured prefix and preserves raw query and body.
Forwarded headers cannot select signature authority. Canonical path restrictions
remove disagreements among URL decoding, ServeMux and reverse proxies.

## Admission and restart safety

Native enrollment auth precedes MAC verification and body decoding. MAC
verification precedes bounded body hashing. Freshness, key validity and
cancellation are checked again before atomic replay admission and after durable
publication. Separate admission pools span handler execution: two ingests and
sixteen metadata, poll or lease requests. Control requests have a 64 KiB body
limit, while the native 64 MiB adoption limit remains available for ingests.
Bounded memory buffering avoids a second temporary-file lifecycle for incoming
event bodies. Saturated pools return `429` with `Retry-After`.

A durable maximum-admitted-expiry watermark complements the bounded nonce
cache. Atomic publication fsyncs the file and directory before dispatch. One
stable lock inode gives one process ownership. A 90-second signature lifetime
covers the 60-second request budget plus signing and transit time. On restart,
a 96-second monotonic quarantine and wall time past the persisted expiry
prevent forgotten nonces from remaining valid. Runtime wallclock comparisons
use Unix wall values so Go's monotonic timestamps cannot conceal a wallclock
rollback. Failed state
publication permanently fences that verifier. Missing state requires explicit
initialization, which never overwrites existing state.

This trades restart availability for a small fixed durable state. It avoids
production schema changes and an independent replay service. Active replicas
must accept disjoint secrets; sharing keys across independently owned replay
files is unsupported. Loss recovery requires rotating all accepted secrets
before initializing fresh state.

Unavailable current key sources reject only requests using those keys, and
expired keys are skipped at startup. Malformed policy and duplicate available
secrets remain configuration errors. Explicit source selection validates the
secret on the selected daemon through an owner-authorized API operation;
the CLI stores no local replacement credential.

## Restricted native listener

An opt-in private listener reuses native route handlers through an explicit
operation allowlist. It requires signing configuration, valid signatures and
project-scoped enrollments, and cannot expose UI, administrator, enrollment
creation, force-release or arbitrary API routes.
The same verifier protects all daemon listeners. Native connection, header,
body, duration and concurrent admission limits bound work before execution.

The [operator guide](../operations/federation-signing.md) owns the exact wire
profile, limits, configuration, rotation and recovery procedures. Future hub
relay endpoints must be explicitly reviewed before entering this allowlist;
using the common native client transport will sign their outbound requests.
