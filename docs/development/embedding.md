---
title: Embedding kata in Go
description: Mount kata's HTTP service inside a Go application.
---

# Embedding kata in Go

The module root, `go.kenn.io/kata`, exposes kata as a listener-free application
service. Use it when a Go application should own the HTTP server and process
lifecycle instead of supervising a separate `kata daemon` process. The mounted
handler serves the same [HTTP API](../reference/http-api.md) used by the CLI and
TUI. Embedded services are API-only by default; importing `go.kenn.io/kata`
does not include the browser application assets.

The host application remains responsible for the listener, TLS, signal
handling, and HTTP server shutdown. A `kata.Service` owns its storage handle and
the federation, GitHub sync, timed-claim, due-notification, and
assignment-expiry background workers associated with that handle.

## Browser host boundary

The first-party browser application is an ordinary top-level, same-origin page;
a trusted desktop webview may navigate to a standalone daemon origin without a
special embedded layout or private host bridge. It still uses the daemon's
normal loopback/login, paired session, CSRF, snapshot, and event-stream
contracts.

The ported issue editor retains an optional neutral workspace-action component
property, but Kata's root application does not wire it and renders no external
workspace controls. Supplying such an action or adding cross-service deep links
is outside this milestone. The listener-free `kata.Service` API likewise does
not turn host authentication into a browser-session transfer mechanism; hosts
that mount it continue to own their HTTP authentication boundary as documented
below.

## Minimal lifecycle

Construct the service, start its workers with `Run`, and use its `Handler` in a
caller-owned server:

```go
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go.kenn.io/kata"
)

func main() {
	ctx, stop := signal.NotifyContext(
		context.Background(), syscall.SIGINT, syscall.SIGTERM,
	)
	defer stop()

	service, err := kata.New(ctx, kata.Config{
		DSN: "/var/lib/example-app/kata.db",
		Auth: kata.AuthConfig{
			Token: os.Getenv("EXAMPLE_KATA_TOKEN"),
		},
	})
	if err != nil {
		log.Fatal(err)
	}

	server := &http.Server{
		Addr:              "127.0.0.1:8080",
		Handler:           service.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	runDone := make(chan error, 1)
	serveDone := make(chan error, 1)
	go func() { runDone <- service.Run(ctx) }()
	go func() { serveDone <- server.ListenAndServe() }()

	var runErr, serveErr error
	var runFinished, serveFinished bool
	select {
	case runErr = <-runDone:
		runFinished = true
	case serveErr = <-serveDone:
		serveFinished = true
	case <-ctx.Done():
	}
	stop()

	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 10*time.Second)
	shutdownErr := server.Shutdown(shutdownCtx)
	cancelShutdown()
	if !runFinished {
		runErr = <-runDone
	}
	if !serveFinished {
		serveErr = <-serveDone
	}
	if errors.Is(serveErr, http.ErrServerClosed) {
		serveErr = nil
	}
	if err := errors.Join(runErr, serveErr, shutdownErr, service.Close()); err != nil {
		log.Fatal(err)
	}
}
```

`Run` does not open a listener. It blocks while federation, scheduled GitHub
sync, timed-claim, due-notification, and assignment-expiry workers are active,
then returns when its context is canceled or `Close` begins, and it may return
an error from any background worker. Treat any `Run` return as service
termination: stop accepting HTTP traffic, cancel the shared context, and
inspect the error after both the workers and server stop. Only one `Run` call
may be active for a service. `Close` cancels
active work and requests, waits for them to stop, and closes the owned storage
handle. It is safe to call more than once.

## Authentication is explicit

`kata.New` requires exactly one authentication policy:

- Set `Auth.Token` to make kata enforce a bearer token on its protected HTTP
  routes. The host remains responsible for transport security.
- Set `Auth.TrustCallerAuthentication` to `true` only when trusted middleware
  already authenticates every request before it reaches `service.Handler()`.
  Never expose that handler directly on an untrusted listener.
- Set `Config.Access` when the host owns both identity and per-operation
  authorization. The host middleware attaches an authenticated
  `kata.Principal` in process with `kata.WithPrincipal`; Kata never accepts
  that principal from a network header.

Construction fails when no policy is selected or when policies are combined. The
explicit choice prevents an embedded service from accidentally inheriting the
standalone daemon's local-user trust boundary.

The lifecycle example deliberately binds to loopback. To accept remote clients,
use `server.ListenAndServeTLS(certFile, keyFile)` with a valid certificate or
mount the handler behind a TLS-terminating reverse proxy. Never send the bearer
token over plaintext non-loopback HTTP.

## Mount below a URL path

To include the bundled browser application, import `go.kenn.io/kata/webui`,
call `webui.NewEmbeddedHandler()`, and pass the returned handler in
`kata.Config.WebHandler`. The standalone `kata daemon` includes this handler
automatically. Existing embedding hosts that serve the browser application
must now select it explicitly. A custom `WebHandler` must serve public, data-free
assets: Kata's bearer check protects API requests, not the static shell.

Keep the API and any configured browser application under one path with
`service.HandlerAt("/tools/tasks")`. Handle its returned error, then mount the
handler behind the host's authentication middleware. Open `/tools/tasks/` to
use the application.

The handler redirects `/tools/tasks` to `/tools/tasks/` and keeps navigation,
assets, API requests, sessions, and live updates below that path. It returns
`404` outside the mount. Use `HandlerAt` instead of applying `http.StripPrefix`
to `service.Handler()` yourself.

The mount must be an absolute path without a trailing slash or `.`/`..`
segments. `/api` and paths below it are reserved for the browser application's
API routes. `HandlerAt` changes routing only; the host still owns
[authentication](#authentication-is-explicit).

Standalone `/kata/` redirects to `/kata`, preserving view and filter queries
so relative assets load from the origin root.

## Host-owned access

`AccessController` is the fine-grained embedding seam. Kata calls it after a
route matches and before protected data is returned or changed. The request
contains the opaque authenticated subject, the actor snapshot used for new
event and projection rows, and the matched operation ID, method, path template,
path parameters, and Kata's operation policy. The policy contains a stable
domain kind, the required product-neutral `read`, `write`, `manage`, or
`federate` capability, and explicit mutation and long-lived-response flags.
Hosts map those capability classes to their own roles; they do not need to
infer policy from operation names or HTTP verbs. Kata's registration test fails
if a new route has no policy, and host-access mode fails closed if metadata is
unavailable.

`Operation.ProjectIDs`, `ProjectUIDs`, and `AllProjects`
carry the validated effective project scope. Cross-project operations include
both projects; omitting the project filter from the event stream sets
`AllProjects` instead of silently broadening an empty scope. Body- and
query-selected projects are decoded and validated before this decision. When
an operation discovers another project while resolving a link, UID, or graph,
Kata asks again with the expanded scope before returning or changing protected
data. Controllers should therefore make retry-safe decisions from the complete
request rather than treating a call as a one-time notification. Global
operations set `AllProjects` explicitly. Operations such as purge, close,
imports, ready selection, UID-prefix lookup, event feeds, close audits, and
project digests also require `AllProjects` because their results or side effects
can depend on relationships outside the project named in the URL. Parent-link
changes and deletion by a global link ID use the same conservative scope. The
same applies to tolerant relationship removals, whose missing-target no-op
must not reveal whether a target belongs to a denied project. The request
deliberately contains no application roles or tenant model.

For lease operations, Kata derives an opaque holder key from the stable
`Principal.Subject`; a request-provided `holder` cannot select or impersonate
another mounted caller. Subject bytes are compared exactly after rejecting
empty or whitespace-only values, so the embedding host must provide one
canonical subject representation. Subject ownership remains distinct when a
spoke forwards a lease through its shared federation identity. Existing
non-host lease clients keep their established holder and client-kind behavior;
mounting a service does not rewrite their persisted ownership tuples. Host
provenance is carried separately from those caller-visible strings, so
reserved-looking holder or client-kind text cannot opt an ordinary client into
mounted identity handling.
`Principal.Actor` remains the human-readable audit snapshot for replica setup
and other mutations.

An embedding host typically mounts the service behind its ordinary session or
credential middleware:

```go
service, err := kata.New(ctx, kata.Config{
	DSN:    "/var/lib/example-app/kata.db",
	Access: applicationAccessController,
})
if err != nil {
	return err
}

handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	principal, err := authenticateApplicationRequest(r)
	if err != nil {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}
	ctx := kata.WithPrincipal(r.Context(), kata.Principal{
		Subject: principal.StableID,
		Actor:   principal.DisplayName,
	})
	service.Handler().ServeHTTP(w, r.WithContext(ctx))
})
```

The controller returns `kata.ErrAccessDenied` for a rejected operation. Kata
answers with a generic not-found envelope so the response does not confirm that
a protected resource exists. Other controller failures return a bounded
service-unavailable envelope without exposing the underlying error.

Long-lived operations require an `AccessLease` in the successful decision.
Kata revalidates that lease before each event or heartbeat; a failed
revalidation closes the stream before more protected data is written. Bounded
requests may return a decision with no lease.

Return a `TransactionFence` with every successful decision. Kata invokes it
only when handling the operation begins a writable SQLite or PostgreSQL
transaction, before the first domain write. This also covers read operations
that perform transactional maintenance, while explicitly read-only snapshots
remain outside the mutation fence. SQL locks acquired by the callback are
therefore held through the domain commit or rollback; the callback must not
commit or roll back the supplied transaction itself. Returning
`ErrAccessDenied` rolls the mutation back and produces the same generic
not-found response as an initial authorization denial. Other failures roll
back and make only the mounted service temporarily unavailable. A missing
fence fails closed before writing.

If the host must record a denial outside that transaction, return
`kata.AfterTransactionRollback(err, finish)` from the fence. Kata rolls back
and releases its connection before calling `finish` once. The callback can
use a separate host transaction to save an audit event or suspend a credential.
It receives the request context, must tolerate retries, and must not reuse the
rolled-back transaction. Kata still denies the original operation if recording
succeeds or fails. Rollback and callback failures remain in the returned error.
HTTP callers receive a temporary-unavailable response when finalization fails,
not a completed authorization decision. The rejected write still does not run.
An earlier fence's denial prevents later fences and their callbacks from running.

Serializable transactions may retry, so a fence must be safe to invoke once
per transaction attempt. The transaction exposes only `ExecContext` and
`QueryRowContext`, which is enough to call a fixed host-owned validation
function without exposing Kata's internal storage packages.

When `Config.Access` is set, also configure `WorkerTransactionFence`. Kata
applies it to every writable transaction started by the federation, GitHub
sync, timed-claim, due-notification, and assignment-expiry workers. A rejection
rolls the transaction back and finishes host recording before canceling all
service workers. `Run` returns the rejection with any rollback or recording
failure; it cannot be reduced to a logged retry while stale authority remains
active.

The host-supplied actor always replaces an actor in request JSON. This keeps
audit attribution tied to the authenticated principal rather than caller input.
`Auth.TrustCallerAuthentication` preserves the older all-or-nothing trusted
middleware mode; use `Config.Access` when projects or operations need distinct
authorization decisions.

Federation transport and lease routes may authenticate with Kata-managed,
project-scoped bearer credentials. When such a request has no in-process host
principal, Kata validates the scoped credential in the route instead of calling
the ordinary `AccessController`. The outer server must preserve the
`Authorization` header on those routes. A request without either an in-process
principal or a scoped bearer credential remains unauthenticated.

Mounting applications that have additional credential-lifecycle rules can set
`Config.FederationAccess`. Kata calls that controller only after its own token,
project, and capability checks succeed. The request contains the retained
enrollment without its token hash, the resolved project, the exact `pull`,
`push`, or `claim` capability, and the stable operation ID. Returning
`ErrAccessDenied` makes the otherwise valid credential unusable without
disclosing which outside condition changed. Standalone behavior is unchanged
when the controller is absent.

Federation mutations require the controller to return a `TransactionFence`.
Kata runs it inside the same storage transaction as event ingest or lease state,
so a concurrent revocation is ordered with the protected write. A missing fence
fails closed. Federation metadata and lease-status requests also require a fence
because they may refresh baselines, expire claims, or update a spoke's cached
lease state even though their HTTP method is `GET`. Tokenless lease-status reads
remain available in explicitly trusted-caller mode; once a bearer credential is
supplied, Kata authenticates and authorizes that credential. When a request
carries both an in-process principal and a scoped federation credential, the
ordinary host-access fence runs first and the federation fence runs only after
it succeeds; neither authorization layer replaces the other. The callback
follows the same retry and transaction rules as the ordinary host-access fence
described above.

## Restricted embedding profile

Use `EmbeddingProfileRestricted` when the mounting application owns project
lifecycle, API-token administration, federation setup, and external issue-sync
configuration:

```go
service, err := kata.New(ctx, kata.Config{
	DSN:     "/var/lib/example-app/kata.db",
	Access:  applicationAccessController,
	Profile: kata.EmbeddingProfileRestricted,
})
```

Restricted mode returns a generic not-found response for native project
mutations, token administration, federation administration, and issue-sync
administration. Rejection happens before host authorization or request-body
processing. Project and task reads, ordinary task changes, event streams, and
Kata-authenticated federation transport remain available under their normal
access checks. The zero-value profile preserves the full standalone-compatible
HTTP API.

Use the in-process project lifecycle methods below instead of enabling native
project administration in restricted mode. Later profile revisions may add
more host-owned application methods without changing standalone defaults.

## Host-managed projects

Applications that keep their own project catalog can establish one stable Kata
project without calling the HTTP API internally:

```go
result, err := service.EnsureProject(ctx, kata.ProjectSpec{
	UID:  "01HZNQ7VFPK1XGD8R5MABCD4EX",
	Name: "example-host-project",
})
```

`EnsureProject` is idempotent across processes. An exact UID-and-name match
returns the existing numeric identity and history; a reused UID or name that
points at a different project returns `kata.ErrProjectConflict`. The caller can
therefore retry after an interrupted catalog update without creating a second
project. Archived projects are returned as `kata.ProjectArchived` and are not
silently reactivated.

`ArchiveProject` retains that same UID, numeric identity, tasks, and events
while removing the project from ordinary active reads. It is idempotent and
requires an actor for the retained event history. These methods are in-process
application methods: they do not authenticate a network caller and should be
invoked only after the host has authorized its own catalog lifecycle change.

## Host-managed federation enrollments

Restricted embedding keeps Kata's scoped federation transport routes but
removes its native enrollment-administration routes. After applying its own
authorization policy, a host can issue a project-scoped transport credential
directly through the service:

```go
created, err := service.CreateFederationEnrollment(ctx, kata.FederationEnrollmentSpec{
	ProjectUID:       "01HZNQ7VFPK1XGD8R5MABCD4EX",
	SpokeInstanceUID: "01HZNQ7VFPK1XGD8R5MABCD4EA",
	Capabilities:     "pull,push,claim",
	Actor:            "Example Operator",
})
```

Creation atomically enables hub federation for the active project when
necessary. Kata generates the bearer secret, stores only its hash, and returns
the plaintext in `created.Token` exactly once. `ListFederationEnrollments`
returns retained history for one stable project UID without either form of the
secret.
`RevokeFederationEnrollment` requires both the project UID and enrollment ID,
so an ID from another project is not accepted; repeating an exact revocation is
harmless. History remains listable and credentials remain revocable after a
project is archived, but archived projects cannot receive new enrollments.

### Accept a saved token

Use `EnsureFederationEnrollment` when the caller already owns a token and must
retry safely after an interrupted request. Generate 32 random bytes, encode
them as unpadded base64url, and save the token before calling:

```go
enrollment, err := service.EnsureFederationEnrollment(ctx, kata.FederationEnrollmentSpec{
	ProjectUID:       "01HZNQ7VFPK1XGD8R5MABCD4EX",
	SpokeInstanceUID: "01HZNQ7VFPK1XGD8R5MABCD4EA",
	Capabilities:     "claim,pull,push",
	Actor:            "Example Operator",
}, savedToken)
```

- The result contains enrollment metadata, not the token or its hash.
- Repeating the call with the same token and scope returns the same active
  enrollment. The token authenticates ordinary scoped federation requests.
- Exact retries also work after project archival. They return the retained
  enrollment without reactivating the project or creating a credential.
- Reusing that token with a different project, spoke instance, actor,
  capabilities, or adoption permission returns
  `ErrFederationEnrollmentTokenConflict`. Revoked tokens return the same error;
  retrying does not restore access.
- A different token may create another credential for the same project and
  instance. Hosts that require one live credential must enforce that rule
  themselves. This method does not rotate or revoke another credential.

Like the project lifecycle methods, these are trusted in-process application
methods rather than network authentication boundaries. The embedding host must
authorize create, ensure, list, and revoke operations before calling them.

## Federation credential providers

A credential provider lets a host application approve Kata federation using
its own account and project permissions. The caller supplies a saved token;
the provider authorizes that exact token for one project and installation.
The request carries a candidate federation token, not a daemon administration
token. The helper itself is trusted local code, not a sandboxed process.

**Development branch; not a tagged release.** The Go package
`go.kenn.io/kata/pkg/federationprovider` handles the helper exchange. Choose the
entry point for the code you are writing:

| Your code | Entry point | Responsibility |
| --- | --- | --- |
| Kata caller | `Exchange` | Run the configured helper with a previously saved request. |
| Helper | `DecodeRequest`, `WriteResponse` | Read a request, decide access, then write a checked reply. |
| Helper forwarding another service's reply | `DecodeResponse` | Check that reply against the original request before forwarding it. |
| Application embedding Kata | `Service.EnsureFederationEnrollment` | Register the exact saved token after approving access. |

Pending approval and denial are normal responses, not Go errors. Errors mean
the exchange could not finish or a document was invalid. Callers still own
credential storage and retries; the package does not create either for them.

The reconciler can obtain approval, read federation metadata with the approved
token, and attach a local replica. Normal leave commands, mapping removal, and
redacted status use the same saved request. See the
[operator guide](../operations/federation.md#external-credential-providers)
for configuration and cleanup instructions.

Kata handles interrupted requests as follows:

- Saves the request and candidate token before running the helper.
- Reuses them after pending approval, a failed exchange, or restart.
- Saves confirmed project, actor, permission, and optional expiry details before binding.
- Reuses a confirmed result without contacting the helper again.
- Keeps failed releases pending and blocks further authorization for that request.

Provider-backed reconciliation makes no catalog-administration calls. It checks
the metadata's project ID and UID against the saved approval and retains that
approval when metadata retrieval or local attachment fails. The provider resolves
the requested project key to those identifiers. That key need not equal Kata's
internal project name, and a rename does not change the approved identity.

- `read_only` and `collaborate` attach only an empty local project. Existing
  tasks, recurring tasks, or project metadata require `migrate` approval.
- `migrate` uses the existing adoption path to import local data.
- Push is enabled only when the approved credential permits it.
- Attachment replaces old local catalog events when the project takes the
  hub identity. Their checksums name the old identity. One new local event
  updates browser readers without pushing empty metadata to the hub.
- Normal leave stops transport and asks the saved provider to release the
  exact request. Unconfirmed cleanup remains pending rather than deleting its
  credential. Once confirmed, the secret is removed; a closed marker prevents
  the still-configured mapping from reopening after restart.

Kata's credential file can retain the provider operation beside its saved
token. It stores the request UUID, executable arguments, intent, original local
project and installation identifiers, and any accepted enrollment and expiry.
Moving a credential to the hub project UID preserves the original request.
Updates and cleanup compare the whole saved operation by value, so rereading
the file does not break a retry and stale cleanup cannot delete a newer request.
The executable arguments remain available for release after a mapping is removed.
On restart, the reconciler releases removed provider mappings, detaches their
local replicas, and removes their closed markers. Other credentials are untouched.
It matches projects by their saved identifiers, not their names. Renaming a
project or reusing its old name cannot redirect cleanup to another request.
If an explicit leave was interrupted, restart can finish remote release, but the
user retries the original leave command to finish local teardown. The saved
marker does not authorize the reconciler to guess whether to archive local data.

### Responsibilities

- The caller saves a random request UUID and a random 32-byte token before
  calling the provider. Every retry uses the same pair.
- The provider verifies the person's authority and obtains approval for the
  project and permissions. It approves the supplied token, not a replacement.
- The caller saves the accepted project and enrollment identifiers before
  federation. Later replies for that request must match the saved identifiers.
  `Exchange` checks the request and target; the caller owns this saved-state
  comparison.
- Release cancels or revokes exactly the saved request. Keep that request until
  the provider confirms `released`, including after a failed exchange.
- A host that expires credentials supplies and enforces the expiry time.
  Standalone Kata storage does not enforce expiry.

### Executable exchange

Use a trusted executable and argument array, for example
`["example-credential-provider", "--profile", "work"]`. `Exchange` runs it
directly, without a shell, and inherits the caller's environment. A provider can
use the operator's normal account configuration.

Only configure helpers you trust with the daemon's operating-system account.
They can read inherited variables, including `KATA_AUTH_TOKEN` if exported,
and files and sockets available to that account. The protocol's restricted
credential fields do not isolate a local executable from those resources.
Kata's federation HTTP requests use the approved project token, not the
daemon administration token.

- Stdin and stdout each carry one UTF-8 JSON object. No progress text or prompts.
- Each document is at most 16 KiB, including whitespace. It contains operation
  metadata, not project data.
- An invocation lasts at most 60 seconds, or the caller's shorter context
  deadline. Cancellation and excess output terminate the helper's process tree.
- Unknown, duplicate, incorrectly cased, or null fields are invalid. So are
  extra documents and unsupported versions.
- The client discards stderr and does not include parser details, executable
  arguments, or helper output in errors. Providers must avoid logging secrets.
- The package does not retry, store tokens, select an account, or open a browser.

| Exit | Meaning |
| --- | --- |
| `0` | One valid response, including pending approval or denial. |
| `2` | Invalid input. Perform no authorization or release action. |
| `1` | The provider could not finish a valid exchange. |

Any nonzero exit discards all stdout, even a complete `ready` response. A failed
exchange does not prove that the host made no changes. Keep the saved request
and token for retry or release.

Exit `2` returns `ErrInvalidRequest`. Reconciliation reports
`configuration_conflict` in health so the operator can check the helper command
and protocol version. Other process failures return `ErrProviderFailed`.
Retries never replace or forget the saved request.

### Request fields

Both operations require `version` (integer `1`), `operation`, and `request_id`.
The UUID uses lowercase, hyphenated text and must not be nil. Kata identity
fields use uppercase ULIDs. String fields must be nonempty when present.

An `authorize` request also requires:

| Field | Meaning |
| --- | --- |
| `hub_url` | Expected HTTPS base, including its mount. No user info, query, or fragment. |
| `project` | Destination project key understood by the host. |
| `spoke_instance_uid` | The caller's Kata installation identity. |
| `local_project_uid` | The caller's local project identity. |
| `intent` | `read_only`, `collaborate`, or `migrate`. |
| `candidate_token` | The saved 32-byte token encoded as unpadded base64url. |

A minimal `release` request needs only the shared fields:

```json
{
  "version": 1,
  "operation": "release",
  "request_id": "8b60f249-b495-4f17-8999-c64382e05680"
}
```

Release may also repeat `hub_url`, `project`, `spoke_instance_uid`,
`local_project_uid`, and `intent` from the saved request. These optional fields
help the provider find that request; they never select a different project or
enrollment to revoke. Kata sends its saved values, not values from a provider
response. Release must not include `candidate_token`.

Providers use `DecodeRequest(io.Reader)` and
`WriteResponse(io.Writer, Request, Response)`. Clients use
`Exchange(context.Context, []string, Request)`.
Providers that relay a decision can use `DecodeResponse(io.Reader, Request)`
to validate the complete response before forwarding it. This checks the raw
JSON fields and size as well as the requested target and permissions.

### Response fields

Every response echoes `version`, `operation`, and `request_id`, and includes a
`status`. An optional `message` is non-secret display text, never a command.

| Status | Caller action |
| --- | --- |
| `ready` | Save the enrollment and start federation. |
| `approval_required` | Wait for approval on the hub; retry the same request. |
| `sign_in_required` | Report that account sign-in is needed. Do not fall back to another credential. |
| `denied` | Stop automatic authorization retries for this request. |
| `conflict` | Keep state and ask for an explicit correction. |
| `unavailable` | Keep state and retry later. |
| `released` | Release only: cleanup is complete; the request no longer grants access. |

A release returns only `released`, `conflict`, `denied`, or `unavailable`.
It cannot grant a connection. Only `ready` includes the following fields.
All except `expires_at` are required:

| Field | Requirement |
| --- | --- |
| `hub_url` | Matches the requested base, including its mount. |
| `project_id`, `enrollment_id` | Positive integers assigned by the host. |
| `project_uid` | The destination project's uppercase ULID. |
| `actor` | Nonempty actor name; `bootstrap` is reserved, ignoring case and surrounding spaces. |
| `capabilities` | Exactly `pull` for `read_only`; `pull,push` or `claim,pull,push` for `collaborate` or `migrate`. |
| `expires_at` | Optional host expiry as UTC RFC 3339 text ending in `Z`. Omit it when the credential does not expire. |

`claim` is the canonical wire capability. `lease` is a human-facing spelling,
not a response value. A provider cannot silently change the requested
permissions and report `ready`. Read-only never permits pushing. Write replicas
need both pull and push; claiming work is optional. Responses never return a token.

The Go package exports `Intent` and `Status` constants for callers and helpers.
It shares URL and actor validation with Kata without importing daemon configuration.

URL matching uses Kata's canonical HTTP base rules: normalize host case and
the default HTTPS port, remove trailing slashes, and preserve the mount path.
A different host, nondefault port, or mount is rejected. A successful `Exchange`
returns the canonical base.

## Storage and PostgreSQL policy

`Config.DSN` is required and accepts a bare SQLite path, a `sqlite://` URL, or a
`postgres://` / `postgresql://` URL. A PostgreSQL-backed service can make schema
handling explicit:

```go
service, err := kata.New(ctx, kata.Config{
	DSN: os.Getenv("EXAMPLE_KATA_DSN"),
	Postgres: kata.PostgresConfig{
		Schema:      "kata",
		SchemaMode:  kata.PostgresSchemaValidate,
		SchemaOwner: "kata_schema_owner",
	},
	Auth: kata.AuthConfig{Token: os.Getenv("EXAMPLE_KATA_TOKEN")},
})
```

`PostgresSchemaBootstrap` installs missing migrations before serving.
`PostgresSchemaValidate` performs no schema installation and requires an
already compatible schema, which lets production applications separate schema
preparation from runtime serving. The standalone
[PostgreSQL operator ceremony](../operations/postgres.md) describes the same
role, migration, TLS, and rollback requirements.

## Service-scoped credentials

GitHub sync credentials are supplied through `Config.GitHubSync`. Federation
credentials are isolated per service through `Config.FederationCredentials`.
When that field is nil, kata uses a service-owned in-memory credential store;
credentials then disappear when the process exits. Applications that need
federation enrollment to survive restarts should implement
`kata.FederationCredentialStore` with durable, concurrency-safe storage.

The embedded service does not read a listener address, install signal handlers,
or take over the host application's logger. Pass a `*slog.Logger` in
`Config.Logger` when kata should use an application-specific logger.
