#!/usr/bin/env bash
# Generate federation TUI SVG screenshots from disposable simulated daemons.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
FREEZE_CFG="$SCRIPT_DIR/freeze.json"
OUTPUT_DIR="$REPO_ROOT/docs/assets/screenshots"
WIDTH=120
HEIGHT=30
SESSION="kata-docs-$$"
DEMO_HUB_AUTH_VALUE="demo-hub-auth-value"

usage() {
    cat <<EOF
Usage: $(basename "$0") [--out DIR]

Generate federation enrollment screenshots into DIR/federation-tui.
The workflow uses only temporary demo daemons and simulated project data.
EOF
}

while [[ $# -gt 0 ]]; do
    case "$1" in
        --out)
            [[ $# -ge 2 ]] || { echo "ERROR: --out requires a directory" >&2; exit 1; }
            OUTPUT_DIR="$2"
            shift 2
            ;;
        -h|--help)
            usage
            exit 0
            ;;
        *)
            echo "Unknown option: $1" >&2
            usage >&2
            exit 1
            ;;
    esac
done

require_cmd() {
    if ! command -v "$1" >/dev/null 2>&1; then
        echo "ERROR: $1 is required for docs screenshot generation" >&2
        exit 1
    fi
}

shell_quote() {
    printf "%q" "$1"
}

wait_for_log() {
    local file="$1"
    local pattern="$2"
    local timeout="${3:-30}"
    for _ in $(seq 1 "$((timeout * 10))"); do
        if grep -q "$pattern" "$file" 2>/dev/null; then
            return 0
        fi
        sleep 0.1
    done
    echo "ERROR: timed out waiting for $pattern in $file" >&2
    [[ -f "$file" ]] && sed -n '1,120p' "$file" >&2
    exit 1
}

tmux_cmd() {
    tmux -L "$SESSION" -f /dev/null "$@"
}

wait_until() {
    local pattern="$1"
    local timeout="${2:-30}"
    for _ in $(seq 1 "$((timeout * 10))"); do
        if tmux_cmd capture-pane -pt "$SESSION" | grep -q "$pattern"; then
            return 0
        fi
        sleep 0.1
    done
    echo "ERROR: timed out waiting for TUI pattern: $pattern" >&2
    tmux_cmd capture-pane -pt "$SESSION" >&2 || true
    exit 1
}

send() {
    tmux_cmd send-keys -t "$SESSION" "$@"
}

capture() {
    local name="$1"
    local dir="$OUTPUT_DIR/$(dirname "$name")"
    mkdir -p "$dir"
    tmux_cmd capture-pane -pet "$SESSION" | \
        freeze -o "$OUTPUT_DIR/${name}.svg" --language ansi -c "$FREEZE_CFG"
    echo "captured ${name}.svg"
}

normalize_svg_assets() {
    find "$OUTPUT_DIR" -name '*.svg' -print0 |
        xargs -0 perl -0pi -e '
            s/demo-hub\.localhost:\d+/demo-hub.localhost:7777/g;
            s/01[0-9A-HJKMNP-TV-Z]{24}/01D3M0D3M0D3M0D3M0D3M0D3/g;
        '
}

create_demo_issue() {
    local title="$1"
    shift
    KATA_HOME="$SPOKE_HOME" \
    KATA_AUTHOR=demo-operator \
        "$KATA_BIN" --workspace "$SPOKE_WS" create "$title" --agent "$@" |
        awk '/^OK create / {print $3}'
}

cleanup() {
    set +e
    if tmux_cmd has-session -t "$SESSION" 2>/dev/null; then
        tmux_cmd kill-session -t "$SESSION" 2>/dev/null
    fi
    if [[ -n "${SPOKE_HOME:-}" && -n "${KATA_BIN:-}" ]]; then
        KATA_HOME="$SPOKE_HOME" "$KATA_BIN" daemon stop >/dev/null 2>&1
    fi
    if [[ -n "${HUB_PID:-}" ]]; then
        kill "$HUB_PID" >/dev/null 2>&1
        wait "$HUB_PID" >/dev/null 2>&1
    fi
    [[ -n "${TMP_ROOT:-}" ]] && rm -rf "$TMP_ROOT"
}
trap cleanup EXIT

require_cmd go
require_cmd tmux
require_cmd freeze

TMP_ROOT="$(mktemp -d)"
KATA_BIN="${KATA_BIN:-$TMP_ROOT/kata}"
SPOKE_HOME="$TMP_ROOT/spoke-home"
HUB_HOME="$TMP_ROOT/hub-home"
SPOKE_WS="$TMP_ROOT/spoke-workspace"
HUB_WS="$TMP_ROOT/hub-workspace"
HUB_LOG="$TMP_ROOT/hub.log"

mkdir -p "$SPOKE_HOME" "$HUB_HOME" "$SPOKE_WS" "$HUB_WS" "$OUTPUT_DIR"

if [[ ! -x "$KATA_BIN" ]]; then
    GOFLAGS=-buildvcs=false go -C "$REPO_ROOT" build -o "$KATA_BIN" ./cmd/kata
fi

cat >"$HUB_HOME/config.toml" <<EOF
[auth]
token = "$DEMO_HUB_AUTH_VALUE"
trust_private_network = true
EOF

KATA_HOME="$HUB_HOME" "$KATA_BIN" daemon start --foreground --listen 127.0.0.1:0 >"$HUB_LOG" 2>&1 &
HUB_PID=$!
wait_for_log "$HUB_LOG" "kata daemon: listening on"
HUB_BOUND="$(sed -n 's/.*kata daemon: listening on //p' "$HUB_LOG" | tail -n 1)"
HUB_PORT="${HUB_BOUND##*:}"
HUB_URL="http://demo-hub.localhost:${HUB_PORT}"

KATA_HOME="$HUB_HOME" \
KATA_SERVER="$HUB_URL" \
KATA_AUTH_TOKEN="$DEMO_HUB_AUTH_VALUE" \
KATA_ALLOW_INSECURE=1 \
    "$KATA_BIN" --workspace "$HUB_WS" init --project demo-hub-project >/dev/null

KATA_HOME="$SPOKE_HOME" \
KATA_AUTHOR=demo-operator \
    "$KATA_BIN" --workspace "$SPOKE_WS" init --project demo-spoke-project >/dev/null

cat >"$SPOKE_HOME/config.toml" <<EOF
active_daemon = "demo-spoke"

[[daemon]]
name = "demo-spoke"
local = true

[[daemon]]
name = "demo-hub"
url = "$HUB_URL"
token_env = "KATA_DEMO_HUB_AUTH"
allow_insecure = true
EOF

parent_ref="$(create_demo_issue \
    "ship federation TUI enrollment" \
    --owner demo-operator \
    --label federation \
    --priority 1)"
child_ref="$(create_demo_issue \
    "show selected spoke project everywhere" \
    --parent "$parent_ref" \
    --owner demo-operator \
    --label tui \
    --priority 0)"
create_demo_issue \
    "separate hub auth from spoke auth" \
    --parent "$parent_ref" \
    --owner demo-reviewer \
    --label auth \
    --priority 1 >/dev/null
create_demo_issue \
    "capture docs screenshots with simulated data" \
    --parent "$parent_ref" \
    --owner demo-docs \
    --label docs \
    --priority 2 >/dev/null
create_demo_issue \
    "cover project-selector F shortcut" \
    --parent "$child_ref" \
    --owner demo-agent \
    --label tui >/dev/null
create_demo_issue \
    "prepare docs asset branch workflow" \
    --owner demo-docs \
    --label docs \
    --priority 2 >/dev/null
create_demo_issue \
    "review remote daemon catalog config" \
    --owner demo-reviewer \
    --label operations >/dev/null

KATA_HOME="$SPOKE_HOME" \
KATA_AUTHOR=demo-operator \
    "$KATA_BIN" --workspace "$SPOKE_WS" edit "$parent_ref" \
    --priority 0 \
    --agent >/dev/null

tmux_cmd new-session -d -s "$SESSION" -x "$WIDTH" -y "$HEIGHT"
tmux_cmd set-option -g default-terminal "tmux-256color"
tmux_cmd set-option -ga terminal-overrides ",*:Tc"

RUN_TUI="env -u NO_COLOR KATA_HOME=$(shell_quote "$SPOKE_HOME") KATA_AUTHOR=demo-operator KATA_DEMO_HUB_AUTH=$(shell_quote "$DEMO_HUB_AUTH_VALUE") KATA_COLOR_MODE=dark $(shell_quote "$KATA_BIN") --workspace $(shell_quote "$SPOKE_WS") --project demo-spoke-project tui"
send "$RUN_TUI" Enter
wait_until "demo-spoke-project"
send "j"
wait_until "project: demo-spoke-project"
send "C-m"
wait_until "ship federation TUI enrollment"
send "Right"
wait_until "show selected spoke project everywhere"
sleep 0.3
capture "tui/hero"

send "F"
wait_until "kata / federation"
wait_until "selected project: demo-spoke-project"
sleep 0.3
capture "federation-tui/list"

send "n"
wait_until "Select hub daemon"
sleep 0.3
capture "federation-tui/select-hub"

send "j"
send "Enter"
wait_until "Select hub project"
wait_until "demo-hub-project"
sleep 0.3
capture "federation-tui/select-hub-project"

send "Enter"
wait_until "Enrollment Preview"
wait_until "Operation: adopt existing local project"
sleep 0.3
capture "federation-tui/preview"

send "Enter"
wait_until "Enrollment Result" 45
sleep 0.5
capture "federation-tui/result"
normalize_svg_assets

cat >"$OUTPUT_DIR/README.md" <<EOF
# kata docs assets

This branch is generated by \`docs/screenshots/update-assets-branch.sh\`.
It contains static documentation screenshots generated from disposable
simulated kata daemons.
EOF

cat >"$OUTPUT_DIR/manifest.json" <<EOF
{
  "generator": "docs/screenshots/generate-federation-tui.sh",
  "source": "simulated disposable daemons",
  "screenshots": [
    "tui/hero.svg",
    "federation-tui/list.svg",
    "federation-tui/select-hub.svg",
    "federation-tui/select-hub-project.svg",
    "federation-tui/preview.svg",
    "federation-tui/result.svg"
  ]
}
EOF

echo "Generated federation TUI screenshots in $OUTPUT_DIR"
