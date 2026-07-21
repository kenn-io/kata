---
title: Embedding kata in Go
description: Mount kata's HTTP service inside a Go application.
---

# Embedding kata in Go

The module root, `go.kenn.io/kata`, exposes kata as a listener-free application
service. Use it when a Go application should own the HTTP server and process
lifecycle instead of supervising a separate `kata daemon` process. The mounted
handler serves the same [HTTP API](../reference/http-api.md) used by the CLI and
TUI.

The host application remains responsible for the listener, TLS, signal
handling, and HTTP server shutdown. A `kata.Service` owns its storage handle and
the federation, GitHub sync, and timed-claim background workers associated with
that handle.

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
sync, and timed-claim workers are active, then returns when its context is
canceled or `Close` begins, and it may return an error from any background
worker. Treat any `Run` return as service termination: stop accepting HTTP
traffic, cancel the shared context, and inspect the error after both the workers
and server stop. Only one `Run` call may be active for a service. `Close` cancels
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
only when handling the operation begins a SQLite or PostgreSQL transaction,
before the first domain write. This also covers read operations that perform
transactional maintenance. SQL locks acquired by the callback are therefore
held through the domain commit or rollback; the callback must not commit or
roll back the supplied transaction itself. Returning `ErrAccessDenied` rolls
the mutation back and produces the same generic not-found response as an
initial authorization denial. Other failures roll back and make only the
mounted service temporarily unavailable. A missing fence fails closed before
writing.

Serializable transactions may retry, so a fence must be safe to invoke once
per transaction attempt. The transaction exposes only `ExecContext` and
`QueryRowContext`, which is enough to call a fixed host-owned validation
function without exposing Kata's internal storage packages.

The host-supplied actor always replaces an actor in request JSON. This keeps
audit attribution tied to the authenticated principal rather than caller input.
`Auth.TrustCallerAuthentication` preserves the older all-or-nothing trusted
middleware mode; use `Config.Access` when projects or operations need distinct
authorization decisions.

Federation transport and lease routes may authenticate with Kata-managed,
project-scoped bearer credentials. When such a request has no in-process host
principal, Kata validates the scoped credential in the route and does not call
the host controller. The outer server must preserve the `Authorization` header
on those routes. A request without either an in-process principal or a scoped
bearer credential remains unauthenticated.

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
