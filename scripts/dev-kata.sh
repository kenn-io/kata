#!/usr/bin/env bash
# Build and run the current checkout in disposable state.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
run_root="$(mktemp -d)"
mkdir -p "$run_root/home" "$run_root/workspace"

# Only OS/toolchain settings cross into branch code. In particular, no Kata
# targets, credentials, proxy settings, or hosted PORT are inherited.
child_env=()
for key in PATH HOME USER LOGNAME TMPDIR TMP TEMP SystemRoot SYSTEMROOT COMSPEC PATHEXT \
  GOROOT GOPATH GOCACHE GOMODCACHE GOSUMDB GOFLAGS GOMAXPROCS \
  CC CXX CGO_ENABLED TERM COLORTERM NO_COLOR KATA_COLOR_MODE; do
  if [[ -n "${!key+x}" ]]; then
    child_env+=("$key=${!key}")
  fi
done
child_env+=("KATA_HOME=$run_root/home" "KATA_DB=$run_root/home/kata.db")
run_isolated() { env -i "${child_env[@]}" "$@"; }
cleanup() {
  if [[ -x "$run_root/kata" ]]; then
    run_isolated "$run_root/kata" daemon stop >/dev/null 2>&1 || true
  fi
  rm -rf "$run_root"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

cd "$repo_root"
go build -buildvcs=false -tags kit_posthog_disabled -o "$run_root/kata" ./cmd/kata
cd "$run_root/workspace"
if [[ "${1:-}" == --demo ]]; then
  run_isolated "$run_root/kata" init --project example-project >/dev/null
  run_isolated "$run_root/kata" --as alice create "fix login callback" --owner agent --label tui --label ux >/dev/null
  run_isolated "$run_root/kata" --as alice create "rebuild search index" --label infra >/dev/null
  run_isolated "$run_root/kata" --as bob create "clean up stale tokens" --label cleanup >/dev/null
  set -- tui
fi
if [[ $# -eq 0 ]]; then set -- version; fi
run_isolated "$run_root/kata" "$@"
