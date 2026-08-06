# Legacy Release Installer Compatibility

## Problem

The hosted installers are served from `main`, while the release they download
can predate `main`. The web UI change made both installers unconditionally run
the hidden `_web-assets-check` command after extraction. The current v0.13.0
release predates that command, so its valid, checksummed binary is rejected
before it can be moved into the install directory.

## Compatibility Contract

- Releases older than v0.14.0 predate the embedded web UI and do not need to
  implement `_web-assets-check`.
- Releases at v0.14.0 or newer must continue to pass `_web-assets-check` before
  installation.
- A version that cannot be recognized as a stable semantic version is not
  treated as legacy and must pass web-asset validation.
- Archive checksum verification, extraction, binary-presence checks, install
  paths, and PATH handling remain unchanged.

## Design

Both `install.sh` and `install.ps1` will pass the resolved release version to
their release-binary validation helper. The helper will classify only stable
`vMAJOR.MINOR.PATCH` releases before v0.14.0 as legacy. It will return without
running the web command for those releases. Every other version will execute
the existing hidden validation command and retain the existing failure message.

This keeps the compatibility exception tied to the release history instead of
interpreting an unknown command as success. A future malformed or incomplete
release therefore cannot bypass web-asset validation merely by omitting the
validator command.

## Tests and Verification

The shell and PowerShell behavior tests will exercise the helpers with real
temporary executable scripts:

- v0.13.0 succeeds even when the temporary binary does not implement a
  successful web-assets check.
- v0.14.0 is rejected when the check fails.
- v0.14.0 succeeds when the check succeeds.
- an unrecognized version is rejected when the check fails.

After the automated release-script tests pass, the shell installer will be run
against the actual macOS v0.13.0 release with a temporary install directory.
The installed binary must report v0.13.0, and no user installation path will be
modified during this verification.
