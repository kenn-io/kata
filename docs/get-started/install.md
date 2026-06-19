# Install

kata is a single Go binary. It has no runtime service dependency beyond the
daemon it starts itself, and it stores data locally in SQLite.

## Requirements

Install Go 1.26 or later from <https://go.dev/dl/>.

GitHub release binaries are available starting with `v0.5.0`. Installing with
`go install` remains supported, and building from a clone is still useful for
development builds.

## Install with `go install`

```sh
go install go.kenn.io/kata/cmd/kata@latest
```

Go writes the binary to `$(go env GOBIN)` when set, otherwise to
`$(go env GOPATH)/bin`. Common defaults are `~/go/bin` on Unix and
`%USERPROFILE%\go\bin` on Windows. Put that directory on `PATH`.

Check the install:

```sh
kata version
kata --help
```

## Install from a release

Download the archive for your platform from the
[GitHub releases](https://github.com/kenn-io/kata/releases) page, extract it,
and place the `kata` binary on `PATH`.

After installing a release build, check for newer releases:

```sh
kata update --check
```

Install an available release from GitHub release assets:

```sh
kata update
```

## Build from a clone

On macOS or Linux:

```sh
git clone https://github.com/kenn-io/kata.git
cd kata
make install
```

`make install` honors `GOBIN` and defaults to `~/.local/bin`:

```sh
make install GOBIN=/usr/local/bin
```

On Windows, PowerShell or `cmd.exe`:

```powershell
git clone https://github.com/kenn-io/kata.git
cd kata
go build -o kata.exe ./cmd/kata
```

Move `kata.exe` to a directory on `PATH`.

## Documentation tooling

This site is built with Zensical. Install the docs toolchain into a local
virtual environment:

```sh
make docs-install
```

Build or preview the site:

```sh
make docs-build
make docs-serve
```

`make docs-check` runs the repository's docs structure check and then runs a
strict Zensical build when Zensical is installed.
