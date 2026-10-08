---
title: Federation request signing
description: Configure native signed federation and a restricted HTTPS ingress.
last_edited: 2026-10-08
---

# Federation request signing

Native signing adds an independent HMAC secret to each scoped federation
enrollment. The spoke signs its requests; the hub verifies signatures before
decoding federation payloads. The enrollment bearer, project scope, actor and
capabilities still control access. Native signing is unreleased.
Operators need a release containing it before deploying it.

Existing private deployments stay unsigned unless the hub enables required
signing. Optional mode rejects invalid supplied signatures. Required mode
rejects unsigned enrollment requests on every daemon listener; a spoke cannot
negotiate it away. Owner-local administration keeps its existing authority.
The restricted ingress listener always requires signatures, even when signing
is optional on the ordinary daemon listeners.

## Prepare the hub and spoke

1. Enable hub federation and create a project-scoped enrollment using
   [the native enrollment workflow](federation.md). Use CLI enrollment and join
   for required-signing hubs. The TUI wizard does not accept signing references
   and its metadata step cannot complete against a required-signing hub.
   Record the enrollment ID.
   Ingress rejects global enrollments, even when the signature is valid.
2. Provision a separate secret with at least 64 bytes, independently of the
   enrollment bearer. For example, `openssl rand -hex 32` produces 64 ASCII
   bytes. Store it in an owner-only file or a named environment variable on
   each daemon. Never put the literal secret in a command or committed config.
3. On the hub host, initialize its replay state once:

   ```sh
   kata federation signing init-replay
   ```

4. Configure the hub's private config. The proxy must strip `/federation`
   before forwarding to the restricted listener. Replace `enrollment_id` with
   the native enrollment's numeric ID.

   ```toml
   [federation.signing]
   required = true
   external_url = "https://daemon.example/federation"
   # Defaults to <KATA_HOME>/federation-signing-replay.state.
   # replay_state_file = "/absolute/private/path/replay.state"

   [[federation.signing.key]]
   key_id = "spoke-key-1"
   enrollment_id = 1
   key_env = "KATA_SPOKE_SIGNING_KEY"
   # Use key_file instead of key_env for an owner-only file.

   [federation.ingress]
   enabled = true
   listen = "127.0.0.1:7780"
   ```

5. Start the hub normally. Signing initially returns `503` for at least
   96 seconds, and until wall time has passed the persisted maximum admitted
   signature expiry. This gate also applies after a restart.
6. Run the native enrollment join command on the spoke with
   `--hub-url https://daemon.example/federation`, adding
   `--signing-key-id spoke-key-1 --signing-key-env KATA_SPOKE_SIGNING_KEY`.
   The same source must exist in the spoke CLI and daemon environments because
   the CLI fetches metadata before saving the enrollment.

For an existing spoke, select the key without replacing its bearer. This
command uses the selected daemon, including `KATA_SERVER` or a daemon catalog
selection, and requires daemon-owner authority:

```sh
kata federation signing configure --project-uid <local-project-uid> \
  --key-id spoke-key-1 --key-env KATA_SPOKE_SIGNING_KEY
```

The selected daemon reads and validates the source in its own environment.
For `signing configure`, `--key-file` must be an absolute path on that daemon's
host; the CLI does not need access to the source. The join command still needs
the source in both environments for its initial metadata request.

Configuration stores source references, never the signing secret. Selecting a
missing, unreadable or short source fails explicitly. File sources must be
regular and owner-only on Unix. The hub rereads sources for each request;
removing a source revokes that key without waiting for a reload. Unavailable
keys reject their requests without preventing daemon startup or other keys
from working. Restoring a file source takes effect on the next request;
environment sources must be available in the running daemon's environment.
Expired keys do not require a source at startup. Key IDs are public selectors,
not credentials.

## Route only federation

Use an existing HTTPS reverse proxy with a fixed prefix-stripping route to the
restricted listener. It exposes only project metadata, event pull and ingest,
issue assignment claim, and issue lease acquire, renew, release and read.
Valid signed requests cannot use this listener for the UI, arbitrary main API,
enrollment creation, administrator actions, force release, or local lifecycle
commands. Join, leave, rebind and credential administration run against the
spoke's ordinary owner-authorized daemon API. Join metadata, rebind and normal
federation traffic use native signed clients. Leave also lists and revokes hub
enrollments with hub-admin authority, which this restricted listener does not
expose. When the spoke's hub URL routes only to this listener, run
`kata federation leave <project> --local-only` on the spoke, then revoke the
recorded enrollment with `kata federation revoke <id>` on the hub. The local
leave does not revoke the hub token; it remains valid until the hub-side
command succeeds.

The listener is disabled by default and requires signing configuration when
enabled. Its default bind is `127.0.0.1:0`, so choose a fixed private port when
configuring a proxy. Non-loopback literal private binds require
`[auth].trust_private_network = true`. Hostnames,
wildcard binds and public IPs are rejected. Starting the listener does not
change the main listener, firewall, network ACLs or routing services. Protect
the proxy-to-listener hop with loopback or an explicitly trusted private
network. TLS terminates at the proxy; spokes validate the HTTPS certificate.

`external_url` is the exact public HTTPS base, including any prefix. The hub
reconstructs the target from this fixed base and the received path and raw
query. Forwarded host, scheme and prefix headers have no authority. Encoded
path aliases, dot segments, double slashes and an empty trailing `?` are
rejected. Preserve the raw query and body through the proxy. This federation
prefix support does not add path-prefix support to the browser UI.

Spokes pin both bearer and signing source to the configured base. They reject
redirects and requests to other origins or prefixes. Signed requests disable
transparent HTTP transport retries; native retries create fresh signatures.
For an intentional
rebind, explicitly select the new target first with `signing configure
--hub-url <new-https-base>`; native rebind then validates that target's metadata.
It never automatically carries a signing key to an arbitrary hub URL.

## Rotate, revoke and recover

Keep one active key per enrollment. For rotation, add a new independent key
with a new ID, and mark the old key with an absolute Unix `not_after` timestamp
no more than 24 hours in the future. At most one retiring key may overlap.
Restart the hub to load changed key selections, wait for replay warmup, then
configure the spoke's new source. Remove the retired source after transition.
Remove its key entry before the next rotation to make room for a new retiring
key.
The hub rejects old keys at their deadline, including uploads that began
before it. Enrollment revocation remains immediate through native auth checks;
signatures cannot restore revoked bearer authority.

Replay state is an owner-only file protected by an exclusive process lock.
Every successful admission durably publishes the maximum admitted expiry
before dispatch. Nonces stay in a bounded in-memory cache until expiry. On
restart, the process waits out both the monotonic quarantine and persisted
expiry, so forgetting the cache cannot admit a still-valid old request.
Clock rollback fences admission until time catches up. Missing, corrupt,
inaccessible or failed state stops signing; initialization never overwrites
existing state. Keep state on durable storage with filesystem locking and
fsync support, alongside the daemon's other persistent files.

If replay state is lost, stop ingress and rotate every affected signing secret
before initializing a fresh state file. Never reset state while retaining
accepted secrets. Each simultaneously active replica must accept disjoint
secrets, including retiring keys. Independent replay files with shared keys
are unsupported; route each enrollment to its configured replica. This
implementation does not provide a shared replay service.

## Limits and wire profile

| Contract | Value |
| --- | --- |
| Signature | [RFC 9421](https://www.rfc-editor.org/rfc/rfc9421.html), label `sig1`, HMAC-SHA256 |
| Covered components, in order | `@method`, `@target-uri`, `content-digest`, `content-type`, `authorization` |
| Required parameters | `created`, `expires`, `nonce`, `keyid`, `alg="hmac-sha256"` |
| Body digest | [RFC 9530](https://www.rfc-editor.org/rfc/rfc9530.html) `sha-256`, including empty bodies |
| Content type emitted by Kata | `application/json` |
| Maximum lifetime / future clock skew | 90 seconds / 5 seconds |
| Nonce | 24 random bytes, unpadded base64url |
| Replay capacity | 8,192 live nonces; full cache rejects new admissions |
| Ingest body / signed relay envelope / other request body / header budget | 64 MiB / 128 MiB / 64 KiB / 16 KiB |
| Concurrent admissions across daemon listeners | 2 ingests plus 16 metadata, poll and lease requests |
| Ingress connections / request duration | 16 / 60 seconds |

The parser accepts only the fixed, canonical structured-field profile. Extra
signatures, parameters, duplicate covered headers, trailers and content
encodings are rejected. Established `httpsign` and `httpsfv` implementations
provide RFC canonicalization; Kata restricts their general features to this
profile. MAC and digest comparisons use constant time comparisons. Native
retries generate fresh signatures rather than replaying a saved signature.

Verification checks the MAC before reading the body, hashes at most 128 MiB,
and rechecks freshness, key validity and cancellation after hashing before
atomic nonce admission. The project-event ingest route remains capped at 64
MiB; the larger signed relay envelope budget covers JSON and base64 encoding
around the existing 64 MiB raw relay batch limit. The 90-second signature
lifetime leaves 30 seconds for signing and transit around the 60-second request
budget. A full 64 MiB snapshot still needs enough bandwidth to upload and
finish processing within that request budget; retries cannot make a
consistently slower upload finish.

Two ingests may run at once, retaining the native adoption snapshot limit.
Metadata, event polls and leases use a separate pool so slow uploads do not
block them. Both pools hold admission through handler completion. A full pool
or replay cache returns `429` with `Retry-After: 1`; native sync retries create
fresh signatures. Authentication failure and unavailable keys never dispatch
the operation. Logs and errors do not include secrets or bodies.
