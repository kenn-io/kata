# Reliable Agent-Hook Migration Design

## Problem

The bounded legacy agent-hook migration currently edits the live configuration
before source-marked replacements are installed. A later installation failure
can therefore remove working lifecycle hooks. The migration also rewrites the
live JSON file with `os.WriteFile`, which truncates it before the replacement
bytes are durable.

## Design

Keep kit responsible for ordinary hook installation. For both Claude and
Codex, install every source-marked replacement before attempting legacy
cleanup. Claude must complete both lifecycle installations first. If any
installation fails, return the error without removing any legacy handler.

After all replacements succeed, remove only the existing exact legacy handler
shapes. Publish a changed configuration through a sibling temporary file:

1. Read and retain the original bytes and mode.
2. Encode the migrated configuration completely.
3. Create a temporary file beside the live configuration.
4. Apply the original mode, write all bytes, sync, and close the file.
5. Atomically replace the live file with the temporary file using the existing
   platform-specific replacement pattern.

Temporary files are removed on every failure path. A failed write, sync, close,
or replacement leaves the original configuration intact. The migration remains
bounded by kata task `m7d5`, scheduled for 2026-11-02.

## Error Handling

Installation errors are returned before legacy cleanup. Migration decoding and
shape errors are returned after replacements exist but before any migration
write. Publication errors are returned with the original file unchanged.

The install-first order can temporarily leave both legacy and source-marked
hooks if cleanup fails. This is recoverable and preferable to missing hooks;
rerunning init retries exact legacy cleanup.

## Tests

- Inject failure on Claude's second source-marked installation and verify the
  original legacy start and end handlers remain.
- Inject atomic replacement failure after the sibling file is fully written and
  verify the live configuration bytes remain unchanged.
- Retain the existing exact-migration, matcher-ownership, symlink, and
  idempotency coverage.

Run the focused regression tests red before production changes, then run the
direct `go test ./cmd/kata -shuffle=on` and repository lint commands after the
implementation.
