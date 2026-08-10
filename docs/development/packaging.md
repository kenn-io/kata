# Packaging kata

Package managers must identify themselves in the Kata binary so `kata update`
does not replace a file owned by the package database. Set the version,
distribution, and build date explicitly when building from a release source
archive:

```sh
go build -trimpath -buildvcs=false \
  -ldflags="-X go.kenn.io/kata/internal/version.Version=v0.14.2 \
  -X go.kenn.io/kata/internal/version.Distribution=example-pkg \
  -X go.kenn.io/kata/internal/version.BuildDate=2026-08-08T00:00:00Z" \
  ./cmd/kata
```

Release source archives do not contain `.git`, so the VCS fallback cannot
derive a version. Without the `Version` linker value, `kata version` reports
`dev`. Use a literal release-commit timestamp for `BuildDate` so rebuilds and
architecture-specific packages report the same value. `Commit` may remain
`unknown` when building with `-buildvcs=false`.

## Distribution values

Kata recognizes these package-owner values:

| Value | Owner | Upgrade path |
| --- | --- | --- |
| empty | Source build or ordinary Kata release archive | `kata update` |
| `homebrew` | Official Homebrew tap or Homebrew Core | `brew upgrade kata` |
| `deb` | Kata `.deb` package | Install the newer `.deb` with the system package manager |
| `rpm` | Kata `.rpm` package | Install the newer `.rpm` with the system package manager |

Third-party packagers should set a stable, recognizable non-empty value such
as `nixpkgs` or `arch`. Any non-empty value makes install-capable forms of
`kata update` fail with a usage error before checking the network or writing a
file. `kata update --check` remains available and reports the owning
distribution plus package-manager guidance. `kata version --json` exposes the
value as `distribution` for support diagnostics.

This boundary is required because a self-updating binary inside a managed
prefix would bypass package checksums and fight the package manager over file
ownership. Kata deliberately does not infer ownership from its executable
path.

## Browser assets

The repository keeps a compiling development stub under `internal/web/dist`.
A release package must replace it with the production web bundle before the Go
build. Use Kata's dedicated release source archive, which already contains
those generated assets, or run the pinned frontend build from a clone:

```sh
make web-install web-embed
go run ./cmd/kata _web-assets-check
```

The hidden `_web-assets-check` command is suitable for package tests. Kata's
dedicated source archive includes vendored Go dependencies as well as the
production assets, so package builds need neither Node/Bun nor network access.
Build that archive with `-mod=vendor`; release verification enforces this path
with an empty module cache and `GOPROXY=off`.

## Telemetry

Official archives and package-manager builds use the same anonymous telemetry
policy. Do not add distribution-specific telemetry behavior. Users can opt out
before starting the daemon:

```sh
export KATA_TELEMETRY_ENABLED=0
```

See [Configuration](../reference/configuration.md#telemetry) for the emitted
events and excluded data.

## Official tap publishing

Kata release workflows publish artifacts but never write to
`kenn-io/homebrew-tap`. The tap's hourly workflow reads Kata's latest stable
release, validates it, and opens a same-repository formula update pull request
with the tap repository's short-lived `GITHUB_TOKEN`.

The release repository and tap updater share this exact archive-name contract:

```text
kata_<version>_homebrew_darwin_amd64.tar.gz
kata_<version>_homebrew_darwin_arm64.tar.gz
kata_<version>_homebrew_linux_amd64.tar.gz
kata_<version>_homebrew_linux_arm64.tar.gz
```

Renaming any of these archives requires a coordinated tap updater change before
the release. A missing or renamed archive fails the scheduled tap workflow;
the red workflow run and GitHub's failed scheduled-workflow notification are
the maintainer alert for a broken cross-repository contract.
