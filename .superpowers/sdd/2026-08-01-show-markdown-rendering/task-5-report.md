# Task 5 report

Implemented field-scoped terminal Markdown rendering for `show`.

## Files

- `cmd/kata/show.go`
- `cmd/kata/show_test.go`
- `cmd/kata/next.go`
- `cmd/kata/tty.go`

## TDD evidence

- RED: `go test ./cmd/kata -run 'TestShowRender(RejectsStructuredModes|NonTTYKeepsPlainOutputAndSkipsDisplayConfig)' -count=1` failed because `--render` was an unknown flag.
- GREEN: `go test ./cmd/kata -run 'TestShowRender(RejectsStructuredModes|NonTTYKeepsPlainOutputAndSkipsDisplayConfig)|TestNext_' -count=1` passed.
- RED: `go test ./cmd/kata -run TestRenderShowFieldsScopesRendererToBodyAndComments -count=1` failed with `undefined: renderShowFields`.
- GREEN: the same field-scope command passed after field preparation was added.
- RED: `go test ./cmd/kata -run TestPrintShowHumanIndentsRenderedCommentWithANSIWidth -count=1` failed with `undefined: printShowHuman`.
- GREEN: `go test ./cmd/kata -run 'Test(PrintShowHumanIndentsRenderedCommentWithANSIWidth|Show_)' -count=1` passed.
- RED: `go test ./cmd/kata -run TestRenderAndPrintShowHumanDoesNotPrintPartialRecord -count=1` failed with `undefined: renderAndPrintShowHuman`.
- GREEN: the same atomic-output command passed after render-then-print orchestration was added.
- Final: `go test ./cmd/kata -run 'TestShow|TestNext' -count=1`, the focused field tests, and `go test ./cmd/kata -count=1` all passed with `KATA_HOME` and `TMPDIR` in `/tmp`.

## Commit

Implementation commit: `00b0138 Render Markdown fields in kata show`.

## Concerns

None known. Non-terminal output deliberately remains plain and does not parse display settings; terminal-size lookup uses the existing 80-column fallback when size discovery is unavailable.
