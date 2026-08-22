# Install

kata is a single Go binary. It has no runtime service dependency beyond the
daemon it starts itself, and it stores data locally in SQLite.

## Requirements

The release installers below do not require Go. Install Go 1.27 or later from
<https://go.dev/dl/> only when using `go install` or building from a clone.

GitHub release binaries are available starting with `v0.5.0`. Since v0.14.0,
kata publishes stable releases that preserve backward compatibility across
upgrades.

The recommended path on macOS is Homebrew Core:

```sh
brew install kata
```

Homebrew owns this binary. Check Kata's release feed with `kata update --check`,
and install packaged updates with:

```sh
brew upgrade kata
```

The formula may trail a newly published GitHub release. On Linux, or to install
an ordinary release archive on macOS instead:

```sh
curl -fsSL https://katatracker.com/install.sh | bash
```

Linux and WSL 2 users who already use
[Homebrew](https://docs.brew.sh/Homebrew-on-Linux) can install from Core
instead:

```sh
brew install kata
```

On Windows PowerShell:

```powershell
powershell -ExecutionPolicy ByPass -c "irm https://katatracker.com/install.ps1 | iex"
```

The installers detect your OS and CPU architecture and download the latest
archive from [GitHub releases](https://github.com/kenn-io/kata/releases).
They verify the downloaded archive against `SHA256SUMS` before installing it.
For v0.14.x and newer versions, they also ask the extracted binary to validate
its embedded Web UI before replacement. The installer remains able to install
canonical older releases that predate that embedded-asset check.
The shell installer places `kata` in `/usr/local/bin` or `~/.local/bin`.
The PowerShell installer places `kata.exe` in `%USERPROFILE%\.kata\bin` by
default and adds that directory to the user `Path`. Review the installers at
<https://katatracker.com/install.sh> and <https://katatracker.com/install.ps1>
before running them if you prefer.

Linux `.deb` and `.rpm` packages are also published for `amd64` and `arm64`.
Download the package for your distribution from
[GitHub releases](https://github.com/kenn-io/kata/releases), then install it
with your system package tool. Upgrade those installations with the same
package manager; Kata will not replace a package-owned binary itself.

After installing a release build, check for newer releases:

```sh
kata update --check
```

For an ordinary release archive or installer-based installation, install an
available update from GitHub release assets:

```sh
kata update
```

`kata update` verifies the downloaded archive against `SHA256SUMS` before
replacing the installed binary. Package-managed installations keep
`kata update --check`, but install-capable update forms direct you back to the
owning package manager. Packagers can read the complete
[packaging contract](../development/packaging.md). Installing with `go install`
remains supported, and building from a clone is still useful for development
builds.

## Install with `go install`

```sh
go install go.kenn.io/kata/cmd/kata@latest
```

This module-source install includes the CLI, daemon, and TUI, but Go does not
run the browser asset toolchain. To use `kata ui`, install a release binary or
build from a clone with `make install` so the compiled browser bundle is
embedded.

Go writes the binary to `$(go env GOBIN)` when set, otherwise to
`$(go env GOPATH)/bin`. Common defaults are `~/go/bin` on Unix and
`%USERPROFILE%\go\bin` on Windows. Put that directory on `PATH`.

Check the install:

```sh
kata version   # or kata --version
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
