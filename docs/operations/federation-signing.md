---
title: Federation request signing
description: Configure native signed federation and a restricted HTTPS ingress.
last_edited: 2026-11-20
---

# Federation request signing

Native signing pairs each scoped federation enrollment with an independent HMAC
secret. The spoke signs its requests, and the hub verifies every signature
before decoding the federation payload. The enrollment bearer, project scope,
actor, and capabilities still control access. Both the hub and its spokes must
run Kata 0.19.0 or later.

Existing private deployments stay unsigned unless you enable required signing on
the hub. In optional mode, the hub rejects any request that supplies an invalid
signature, while in required mode it rejects unsigned enrollment requests on
every daemon listener, including the Unix socket, with
`428 hub_signing_required`. A spoke cannot negotiate required signing away. If
you set `required = true`, every spoke that joined unsigned stops working over
both TCP and the Unix socket until you register its signing key on the hub and
select that key on the spoke. Owner-local administration keeps its existing
authority, and the restricted ingress listener always requires signatures even
when signing stays optional on the ordinary daemon listeners.

## Prepare the hub and spoke

1. Enable hub federation and create a project-scoped enrollment with
   [the native enrollment workflow](federation.md). For a required-signing hub,
   run enrollment and join from the CLI. The TUI wizard does not accept signing
   references, and its metadata step cannot complete against a required-signing
   hub. Pass an explicit `--token`, then record both the token and the
   enrollment ID from the `enrollment.id` field in `--json` output. The
   restricted ingress rejects global enrollments even when the signature is
   valid.
2. Generate a separate signing secret of at least 64 bytes, independent of the
   enrollment bearer. For example, `openssl rand -hex 32` produces 64 ASCII
   bytes. Store that secret in an owner-only file or a named environment
   variable on each daemon, and never place the literal secret in a command or
   committed config.
3. On the hub host, initialize the replay state once:

   ```sh
   kata federation signing init-replay
   ```

4. Update the hub's private config, replacing `enrollment_id` with the native
   enrollment's numeric ID. Your reverse proxy must strip `/federation` before
   forwarding requests to the restricted listener.

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

5. Start the hub normally. While replay protection warms up, the hub answers
   signed requests with `503`, code `signing_warming`, and `Retry-After: 5`.
   Warm-up lasts at least 96 seconds and continues until wall time passes the
   persisted maximum admitted signature expiry. The hub goes through this
   warm-up again after every restart. When you see `signing_warming`, wait and
   retry instead of troubleshooting keys or clocks. Unsigned requests receive
   `428` immediately.
6. Re-run the same `enroll` command against the hub's main API with the same
   `--token`. An exact retry returns the existing enrollment and prints its
   signed join command, which points at `external_url` and includes
   placeholders for the signing key ID and absolute spoke key file path. Before
   you run the command on the spoke, replace each full quoted placeholder,
   including the angle brackets, and quote both values for the shell. For
   example, an ID of `spoke'$KEY` becomes `'spoke'"'"'$KEY'`, and the path
   `/path/to/spoke's key` becomes `'/path/to/spoke'"'"'s key'`. If you prefer an
   environment variable, replace `--signing-key-file` with
   `--signing-key-env KATA_SPOKE_SIGNING_KEY`. Make the same source available
   in both the spoke CLI and daemon environments, because the CLI fetches
   metadata before saving the enrollment.

To enable required signing on an existing hub, add a `[[federation.signing.key]]`
entry for each unsigned spoke enrollment and restart the hub. Wait for the
replay warm-up in step 5 to finish, then configure each spoke with the matching
secret without replacing its bearer. Until a spoke signs its requests, the hub
refuses it. The following command targets the selected daemon, including
`KATA_SERVER` or a daemon catalog entry, and requires daemon-owner authority:

```sh
kata federation signing configure --project-uid <local-project-uid> \
  --key-id spoke-key-1 --key-env KATA_SPOKE_SIGNING_KEY
```

If a spoke is bound to an HTTP URL, configure the signing source for the new
HTTPS base first. Without `--hub-url`, the command uses the stored HTTP URL and
fails HTTPS validation. Point a remote daemon catalog entry at the same HTTPS
base, restart the spoke daemon to load it, and then run:

```sh
kata federation signing configure --project-uid <local-project-uid> \
  --key-id spoke-key-1 --key-env KATA_SPOKE_SIGNING_KEY \
  --hub-url <new-https-base>
kata federation rebind <spoke-project> --hub <catalog-name>
```

The selected daemon reads and validates the signing source in its own
environment. For `signing configure`, `--key-file` must be an absolute path on
that daemon's host, and the CLI does not need access to it. By contrast, the
join command still needs the source in both environments for its initial
metadata request.

Configuration stores references to key sources rather than the secret itself,
and selecting a missing, unreadable, or short source fails explicitly. On Unix,
file sources must be regular, owner-only files. The hub rereads sources on every
request, and removing a source revokes that key without waiting for a reload.
An unavailable key rejects its own requests without blocking daemon startup or
affecting other keys. If you restore a file source, it takes effect on the next
request, whereas environment sources must be present in the running daemon's
environment. Expired keys do not need a source at startup, and key IDs are
public selectors, not credentials.

Every unexpired signing key must use its own secret. If two keys share a key
file, hold equal contents in separate files, or resolve to equal environment
values, daemon startup fails with an error naming both key IDs. `kata doctor`
checks the sources available in its own environment and reports the same
conflict. To fix it, rotate the shared secret and give each key a distinct
source value.

## Bridges

A bridge created with `kata federation bridge connect` signs its relay requests
when its credential selects a signing source. When a hub sets `required = true`,
it advertises `federation_signing_required`, and `bridge connect` refuses the
connection with `409 hub_signing_required` until you give the bridge a source.
Any unsigned request receives `428 hub_signing_required` on any federation
listener. If that response causes sync to fail on an older bridge, the bridge
keeps its grant and reports `offline` instead of `signing_rejected`, unless a
higher-priority state applies.

1. On the daemon that holds the bridge, provision the secret as described
   earlier and connect with `--signing-key-id <key-id>` plus
   `--signing-key-env <var>` or `--signing-key-file <absolute-path>`. If a
   bridge credential already exists, select the source instead with
   `kata federation signing configure --project-uid <hub-project-uid> --key-id <key-id> --key-env <var>`.
2. On the hub, look up the bridge's enrollment ID with
   `kata federation enrollments list`, add a `[[federation.signing.key]]` entry
   for that ID, and restart the hub.

Until the hub registers the key, relay sync fails and
`kata federation bridge status` reports `signing_rejected`. The grant stays
live, so the bridge resumes without reconnecting once both sides agree. When
you disconnect the bridge, `bridge disconnect` signs its revocation with the
same source.

## Route only federation

Put an existing HTTPS reverse proxy with a fixed prefix-stripping route in front
of the restricted listener. The listener exposes only project metadata, event
pull and ingest, federation project search, relay bridge delivery (offer,
accept, acknowledge, reset, and grant disconnect), native cron job writes
(create, replace, archive, restore), issue assignment claim, and issue lease
acquire, renew, release, and read. Valid signed requests cannot use this
listener for the UI, arbitrary main API, enrollment creation, administrator
actions, force release, or local lifecycle commands.

Join, leave, rebind, and credential administration run against the spoke's
ordinary owner-authorized daemon API, while join metadata, rebind, and normal
federation traffic use native signed clients. Because `leave` also lists and
revokes hub enrollments with hub-admin authority, which the restricted listener
does not expose, you need a two-step leave when the spoke's hub URL routes only
to this listener. Run `kata federation leave <project> --local-only` on the
spoke, then revoke the recorded enrollment on the hub with
`kata federation revoke <id>`. The local leave does not revoke the hub token,
which stays valid until the hub-side revoke succeeds.

The restricted listener is off by default and requires signing configuration
when you enable it. Because its default bind is `127.0.0.1:0`, pick a fixed
private port when configuring a proxy. Non-loopback literal private binds
require `[auth].trust_private_network = true`, and Kata rejects hostnames,
wildcard binds, and public IPs. Starting the listener does not change the main
listener, firewall, network ACLs, or routing services. Protect the
proxy-to-listener hop with loopback or an explicitly trusted private network.
TLS terminates at the proxy, and spokes validate the HTTPS certificate.

Set `external_url` to the exact public HTTPS base, including any prefix. The hub
reconstructs each target from that fixed base plus the received path and raw
query; forwarded host, scheme, and prefix headers have no authority. The hub
rejects encoded path aliases, dot segments, double slashes, and an empty
trailing `?`. Configure your proxy to preserve the raw query and body. Prefix
support for federation does not add path-prefix support to the browser UI.

Spokes pin both the bearer and the signing source to the configured base, and
reject redirects and requests to other origins or prefixes. Signed requests
disable transparent HTTP transport retries, while native retries generate fresh
signatures. For an intentional rebind, select the new target first with
`signing configure --hub-url <new-https-base>`, and native rebind then validates
that target's metadata. A spoke never carries a signing key automatically to an
arbitrary hub URL.

## Rotate, revoke and recover

Keep one active key per enrollment. To rotate a key:

1. Add a new independent key with a new ID, and mark the old key with an
   absolute Unix `not_after` timestamp no more than 24 hours in the future. At
   most one retiring key may overlap at a time.
2. Restart the hub to load the updated key selections, and wait for replay
   warm-up to finish.
3. Configure the spoke to use the new source.
4. After the transition completes, remove the retired source, and remove its key
   entry before your next rotation to make room for another retiring key.

The hub rejects old keys at their deadline, including uploads that started
before it. Enrollment revocation stays immediate through native auth checks, and
signatures cannot restore revoked bearer authority.

The replay state lives in an owner-only file guarded by an exclusive process
lock. Before dispatching any admitted request, the hub durably records the
maximum admitted expiry, while nonces stay in a bounded in-memory cache until
they expire. On restart, the hub waits out both the monotonic quarantine and the
persisted expiry, so losing the in-memory cache cannot admit an old request that
is still valid. If the system clock rolls back, the hub blocks admission until
time catches up. Missing, corrupt, inaccessible, or failed replay state stops
signing, and initialization never overwrites existing state. Store the replay
file on durable storage with filesystem locking and fsync support, alongside the
daemon's other persistent files.

If you lose replay state, stop ingress and rotate every affected signing secret
before initializing a fresh state file. Never reset state while keeping secrets
that were already accepted. Each simultaneously active replica must accept
disjoint secrets, including retiring keys. Independent replay files with shared
keys are unsupported. Route each enrollment to its configured replica. Kata does
not provide a shared replay service.

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

The parser accepts only the fixed, canonical structured-field profile and
rejects extra signatures, extra parameters, duplicate covered headers, trailers,
and content encodings. Kata relies on established `httpsign` and `httpsfv`
implementations for RFC canonicalization and restricts their general features to
this profile. MAC and digest checks use constant-time comparisons, and native
retries generate fresh signatures instead of replaying a saved one.

During verification, the hub checks the MAC before reading the body, hashes at
most 128 MiB, and then rechecks freshness, key validity, and cancellation before
atomic nonce admission. The project-event ingest route stays capped at 64 MiB,
while the larger signed relay envelope budget covers JSON and base64 encoding
around the existing 64 MiB raw relay batch limit. The 90-second signature
lifetime leaves 30 seconds for signing and transit around the 60-second request
budget. A full 64 MiB snapshot still needs enough bandwidth to upload and finish
processing within that request budget; retries cannot make a consistently slower
upload finish.

Two ingests may run at once, preserving the native adoption snapshot limit.
Metadata, event polls, and leases run in a separate pool so slow uploads do not
block them, and both pools hold admission until the handler finishes. If either
a pool or the replay cache is full, the hub returns `429` with `Retry-After: 1`,
and native sync retries create fresh signatures. During replay warm-up,
including a clock-rollback fence, signed requests receive `503` with code
`signing_warming` and `Retry-After: 5`. Failed authentication and unavailable
keys never dispatch the operation, and logs and errors never include secrets or
bodies.
