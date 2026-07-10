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

command -v jq >/dev/null 2>&1 || { echo "kata-attention: jq not on PATH; attention hook inactive" >&2; exit 0; }
command -v kata >/dev/null 2>&1 || { echo "kata-attention: kata not on PATH; attention hook inactive" >&2; exit 0; }

mode="${1:?usage: kata-attention.sh start|claim|stop}"
input="$(cat)"
[[ -n "$input" ]] || exit 0

session_id="$(jq -r '.session_id // empty' <<<"$input")"
[[ -n "$session_id" ]] || exit 0
# Per-user state: XDG_RUNTIME_DIR is already private; the /tmp fallback is
# uid-suffixed and chmod'd so users on a shared machine neither collide on
# nor read each other's session refs.
state_dir="${XDG_RUNTIME_DIR:-/tmp}/kata-attention-$(id -u)"
state_file="$state_dir/$session_id"

# issue_status extracts .issue.status from a `kata show` JSON payload.
issue_status() { jq -r '.issue.status // empty' <<<"$1"; }

# remember_ref appends an already-validated ref to the session state file and
# floors its signal at ok: tracked work is visible from the moment it is
# grabbed. Callers validate first (open issue; for claims, ownership too).
remember_ref() {
  local ref="$1"
  mkdir -p "$state_dir" 2>/dev/null && chmod 700 "$state_dir" 2>/dev/null || return 0
  grep -qxF "$ref" "$state_file" 2>/dev/null || echo "$ref" >>"$state_file"
  kata meta set "$ref" work.attention ok >/dev/null 2>&1 || true
}

case "$mode" in
start)
  # Launcher-run sessions pass the tracking issue in the environment. Refuse
  # a dash-leading value — kata would parse it as a flag, not a ref.
  ref="${KATA_REF:-}"
  [[ -n "$ref" && "$ref" != -* ]] || exit 0
  status="$(issue_status "$(kata show "$ref" --format json 2>/dev/null)")"
  # work.attention on closed issues is meaningless.
  [[ -n "$status" && "$status" != "closed" ]] || exit 0
  remember_ref "$ref"
  ;;
claim)
  cmd="$(jq -r '.tool_input.command // empty' <<<"$input")"
  # Match `kata claim <ref>` only in command position (line start or after a
  # separator), not the string merely appearing inside echoed/quoted text.
  # Bash ERE, not `grep -P` — BSD grep on macOS has no -P. The ref's first
  # character must be alphanumeric so a crafted token can't reach kata as a
  # flag.
  claim_re='(^|[;&|(])[[:space:]]*kata[[:space:]]+claim[[:space:]]+(--[^[:space:]]+[[:space:]]+)*([A-Za-z0-9][A-Za-z0-9#_-]*)'
  ref=""
  while IFS= read -r line; do
    if [[ "$line" =~ $claim_re ]]; then
      ref="${BASH_REMATCH[3]}"
      break
    fi
  done <<<"$cmd"
  [[ -n "$ref" ]] || exit 0
  # The command text alone doesn't prove the claim executed or succeeded
  # (failed claims, unexecuted conditionals). Verify it took: the issue must
  # be open and owned by this session's actor — flooring attention on
  # someone else's issue is the harmful false positive. One `kata show`
  # serves both checks.
  info="$(kata show "$ref" --format json 2>/dev/null)"
  status="$(issue_status "$info")"
  [[ -n "$status" && "$status" != "closed" ]] || exit 0
  actor="$(kata whoami --format json 2>/dev/null | jq -r '.actor // empty')"
  owner="$(jq -r '.issue.owner // empty' <<<"$info")"
  [[ -n "$actor" && "$owner" == "$actor" ]] || exit 0
  remember_ref "$ref"
  ;;
stop)
  [[ -f "$state_file" ]] || exit 0
  while IFS= read -r ref; do
    [[ -n "$ref" ]] || continue
    status="$(issue_status "$(kata show "$ref" --format json 2>/dev/null)")"
    [[ -n "$status" && "$status" != "closed" ]] || continue # attention on closed issues is meaningless
    current="$(kata meta get "$ref" work.attention --format json 2>/dev/null | jq -r '.value // empty')"
    if [[ "$current" == "ok" ]]; then
      # The session ended while the issue still claimed to be fine: the
      # agent never handed off, so raise the flag a coordinator can see.
      kata meta set "$ref" work.attention needs-human >/dev/null 2>&1 || true
      kata meta set "$ref" work.attention_msg "session ended without hand-off" >/dev/null 2>&1 || true
    fi
  done <"$state_file"
  # The sweep is terminal for the session; drop its state rather than
  # accumulating orphaned files.
  rm -f "$state_file"
  ;;
esac
exit 0
