# Install

kata is a single Go binary. It has no runtime service dependency beyond the
daemon it starts itself, and it stores data locally in SQLite.

## Requirements

Install Go 1.26 or later from <https://go.dev/dl/>.

GitHub release binaries are available starting with `v0.5.0`. The recommended
path for most users is the release installer:

```sh
curl -fsSL https://katatracker.com/install.sh | bash
```

The installer detects your OS and CPU architecture and downloads the latest
archive from [GitHub releases](https://github.com/kenn-io/kata/releases).
The installer verifies the downloaded archive against `SHA256SUMS` before installing it.
It places `kata` in `/usr/local/bin` or `~/.local/bin`. Review
the installer at <https://katatracker.com/install.sh> before running it if you
prefer.

Linux `.deb` and `.rpm` packages are also published for `amd64` and `arm64`.
Download the package for your distribution from
[GitHub releases](https://github.com/kenn-io/kata/releases), then install it
with your system package tool.

After installing a release build, check for newer releases:

```sh
kata update --check
```

Install an available update from GitHub release assets:

```sh
kata update
```

`kata update` verifies the downloaded archive against `SHA256SUMS` before
replacing the installed binary. Installing with `go install` remains supported,
and building from a clone is still useful for development builds.

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

## Install from a release manually

Download the archive for your platform from the
[GitHub releases](https://github.com/kenn-io/kata/releases) page, extract it,
and place the `kata` binary on `PATH`.

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
