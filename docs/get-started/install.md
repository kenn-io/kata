# Install

kata is a single Go binary. It has no runtime service dependency beyond the
daemon it starts itself, and it stores data locally in SQLite.

## Requirements

Install Go 1.26 or later from <https://go.dev/dl/>.

Pre-built release binaries are not published yet. Install from source with
`go install` or build from a clone.

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

## Development install

The main checks are:

```sh
make test
make lint
make vet
make nilaway
```

Federation has additional coverage:

```sh
make test-stress
make test-federation-docker
```

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
