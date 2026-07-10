#!/usr/bin/env bash
# Keep kata work.attention truthful across an agent session. Managed by
# `kata init --with-hooks`; local edits are overwritten on refresh. See
# kata's docs/operations/agent-orchestration.md ("Keep attention truthful
# with hooks") for the recipe this implements.
#
# Ref discovery: a launcher-run session knows its tracking issue up front
# ($KATA_REF); a hand-started session doesn't, so the agent's own
# `kata claim <ref>` is treated as the discovery event. Discovered refs
# are remembered in a per-session state file for the stop-time sweep.
#
# Wiring (.claude/settings.json):
#   SessionStart       -> kata-attention.sh start   ($KATA_REF, when launched)
#   PostToolUse [Bash] -> kata-attention.sh claim   (watch for `kata claim`)
#   Stop               -> kata-attention.sh stop    (sweep: still ok -> needs-human)
#
# Hook stdin is Claude Code's hook JSON; needs jq + kata on PATH.

set -u

command -v jq >/dev/null 2>&1 || exit 0
command -v kata >/dev/null 2>&1 || exit 0

mode="${1:?usage: kata-attention.sh start|claim|stop}"
input="$(cat)"

session_id="$(jq -r '.session_id // empty' <<<"$input")"
[[ -n "$session_id" ]] || exit 0
state_dir="${XDG_RUNTIME_DIR:-/tmp}/kata-attention"
state_file="$state_dir/$session_id"

# record_ref remembers a ref for the stop sweep and floors its signal at ok:
# tracked work is visible from the moment it is grabbed.
record_ref() {
  local ref="$1"
  mkdir -p "$state_dir"
  grep -qxF "$ref" "$state_file" 2>/dev/null || echo "$ref" >>"$state_file"
  kata meta set "$ref" work.attention ok >/dev/null 2>&1 || true
}

case "$mode" in
start)
  # Launcher-run sessions pass the tracking issue in the environment.
  [[ -n "${KATA_REF:-}" ]] || exit 0
  record_ref "$KATA_REF"
  ;;
claim)
  cmd="$(jq -r '.tool_input.command // empty' <<<"$input")"
  # Only react to claims that actually happened in this Bash call.
  ref="$(grep -oP 'kata\s+claim\s+\K[A-Za-z0-9/_-]+' <<<"$cmd" | head -1)" || true
  [[ -n "${ref:-}" ]] || exit 0
  record_ref "$ref"
  ;;
stop)
  [[ -f "$state_file" ]] || exit 0
  while IFS= read -r ref; do
    [[ -n "$ref" ]] || continue
    status="$(kata show "$ref" --format json 2>/dev/null | jq -r '.issue.status // empty')"
    [[ -n "$status" && "$status" != "closed" ]] || continue # attention on closed issues is meaningless
    current="$(kata meta get "$ref" work.attention --format json 2>/dev/null | jq -r '.value // empty')"
    if [[ "$current" == "ok" ]]; then
      # The session ended while the issue still claimed to be fine: the
      # agent never handed off, so raise the flag a coordinator can see.
      kata meta set "$ref" work.attention needs-human >/dev/null 2>&1 || true
      kata meta set "$ref" work.attention_msg "session ended without hand-off" >/dev/null 2>&1 || true
    fi
  done <"$state_file"
  ;;
esac
exit 0
