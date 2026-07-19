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

	runDone := make(chan error, 1)
	go func() { runDone <- service.Run(ctx) }()

	server := &http.Server{
		Addr:              ":8080",
		Handler:           service.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Print(err)
		stop()
	}
	if err := <-runDone; err != nil {
		log.Print(err)
	}
	if err := service.Close(); err != nil {
		log.Print(err)
	}
}
```

`Run` does not open a listener. It blocks while federation, scheduled GitHub
sync, and timed-claim workers are active, then returns when its context is
canceled or `Close` begins. Only one `Run` call may be active for a service.
`Close` cancels active work and requests, waits for them to stop, and closes the
owned storage handle. It is safe to call more than once.

## Authentication is explicit

`kata.New` requires exactly one authentication policy:

- Set `Auth.Token` to make kata enforce a bearer token on its protected HTTP
  routes. The host remains responsible for transport security.
- Set `Auth.TrustCallerAuthentication` to `true` only when trusted middleware
  already authenticates every request before it reaches `service.Handler()`.
  Never expose that handler directly on an untrusted listener.

Construction fails when neither policy is selected or when both are set. The
explicit choice prevents an embedded service from accidentally inheriting the
standalone daemon's local-user trust boundary.

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
