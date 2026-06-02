#!/usr/bin/env bash
set -euo pipefail

hub=http://hub:7373
hub_hostname=$hub
hub_token=federation-hub-token
spoke_a=http://spoke-a:7374
spoke_b=http://spoke-b:7375
project=federation-smoke
curl_timeout=${KATA_FEDERATION_DOCKER_CURL_TIMEOUT:-15}

log() {
  printf 'federation-smoke: %s\n' "$*" >&2
}

request() {
  local method=$1
  local url=$2
  local expect=$3
  local body=${4-}
  local out
  local status
  local auth_header=(-H "Authorization: Bearer $hub_token")
  out=$(mktemp)
  if [[ -n "$body" ]]; then
    status=$(curl -sS -o "$out" -w '%{http_code}' \
      --connect-timeout 5 --max-time "$curl_timeout" \
      -X "$method" "${auth_header[@]}" -H 'Content-Type: application/json' --data "$body" "$url")
  else
    status=$(curl -sS -o "$out" -w '%{http_code}' \
      --connect-timeout 5 --max-time "$curl_timeout" -X "$method" "${auth_header[@]}" "$url")
  fi
  if [[ "$status" != "$expect" ]]; then
    log "unexpected HTTP $status for $method $url; expected $expect"
    cat "$out" >&2
    rm -f "$out"
    return 1
  fi
  cat "$out"
  rm -f "$out"
}

get() {
  request GET "$1" 200
}

post() {
  request POST "$1" "$2" "$3"
}

wait_for_issue_title() {
  local base=$1
  local project_id=$2
  local issue_uid=$3
  local title=$4
  local label=$5

  for _ in $(seq 1 120); do
    if get "$base/api/v1/projects/$project_id/issues" \
      | jq -e --arg uid "$issue_uid" --arg title "$title" \
        '.issues[]? | select(.uid == $uid and .title == $title)' >/dev/null; then
      log "$label converged to title: $title"
      return 0
    fi
    sleep 0.5
  done

  log "timed out waiting for $label to converge to title: $title"
  get "$base/api/v1/projects/$project_id/issues" >&2 || true
  return 1
}

wait_for_all_titles() {
  local title=$1
  wait_for_issue_title "$hub" "$hub_project_id" "$issue_uid" "$title" hub
  wait_for_issue_title "$spoke_a" "$spoke_a_project_id" "$issue_uid" "$title" spoke-a
  wait_for_issue_title "$spoke_b" "$spoke_b_project_id" "$issue_uid" "$title" spoke-b
}

kata_for() {
  local home=$1
  local server=$2
  shift 2
  KATA_HOME="$home" KATA_SERVER="$server" kata --project "$project" "$@"
}

expect_cli_failure() {
  local description=$1
  local home=$2
  local server=$3
  local expected=$4
  shift 4
  local out
  out=$(mktemp)
  if kata_for "$home" "$server" "$@" >"$out" 2>&1; then
    log "$description unexpectedly succeeded"
    cat "$out" >&2
    rm -f "$out"
    return 1
  fi
  if ! grep -Eq "$expected" "$out"; then
    log "$description failed without expected pattern: $expected"
    cat "$out" >&2
    rm -f "$out"
    return 1
  fi
  log "$description denied as expected"
  rm -f "$out"
}

log "verifying CLI enrollment creates a missing hub project"
cli_project=federation-cli-enroll-missing
cli_token=federation-cli-enroll-token
spoke_a_uid=$(get "$spoke_a/api/v1/instance" | jq -r '.instance_uid')
cli_enroll_out=$(
  KATA_HOME=/data/kata/runner-cli-enroll KATA_SERVER="$spoke_a" \
    kata --project "$cli_project" federation enroll \
      --spoke-instance "$spoke_a_uid" \
      --hub-url "$hub_hostname" \
      --capabilities pull,push \
      --token "$cli_token" \
      --actor cli-agent \
      --allow-insecure
)
if ! grep -Fq "enrolled $spoke_a_uid for $cli_project" <<<"$cli_enroll_out"; then
  log "CLI enrollment output did not confirm missing project enrollment"
  printf '%s\n' "$cli_enroll_out" >&2
  exit 1
fi
if ! grep -Fq -- "--project $cli_project" <<<"$cli_enroll_out"; then
  log "CLI enrollment output did not include a join command for $cli_project"
  printf '%s\n' "$cli_enroll_out" >&2
  exit 1
fi
if ! grep -Fq -- "--hub-url $hub_hostname" <<<"$cli_enroll_out"; then
  log "CLI enrollment output did not include the hostname hub URL"
  printf '%s\n' "$cli_enroll_out" >&2
  exit 1
fi
if ! grep -Fq -- "--allow-insecure" <<<"$cli_enroll_out"; then
  log "CLI enrollment output did not propagate --allow-insecure to the join command"
  printf '%s\n' "$cli_enroll_out" >&2
  exit 1
fi
cli_join_cmd=$(sed -n 's/^join: //p' <<<"$cli_enroll_out")
if [[ -z "$cli_join_cmd" ]]; then
  log "CLI enrollment output did not include a join command"
  printf '%s\n' "$cli_enroll_out" >&2
  exit 1
fi
cli_project_id=$(get "$hub/api/v1/projects" | jq -r --arg name "$cli_project" '.projects[]? | select(.name == $name) | .id')
if [[ -z "$cli_project_id" ]]; then
  log "CLI enrollment did not create hub project $cli_project"
  get "$hub/api/v1/projects" >&2 || true
  exit 1
fi
get "$hub/api/v1/projects/$cli_project_id/federation" >/dev/null

log "verifying CLI enrollment join command syncs over plaintext hostname"
KATA_HOME=/data/kata/runner-cli-enroll KATA_SERVER="$spoke_a" bash -c "$cli_join_cmd" >/dev/null
cli_spoke_project_id=$(get "$spoke_a/api/v1/projects" | jq -r --arg name "$cli_project" '.projects[]? | select(.name == $name) | .id')
if [[ -z "$cli_spoke_project_id" ]]; then
  log "CLI join did not create spoke project $cli_project"
  get "$spoke_a/api/v1/projects" >&2 || true
  exit 1
fi
cli_issue_json=$(post "$spoke_a/api/v1/projects/$cli_spoke_project_id/issues" 200 \
  '{"actor":"untrusted-cli-input","title":"cli enrollment pushed issue"}')
cli_issue_uid=$(jq -r '.issue.uid' <<<"$cli_issue_json")
wait_for_issue_title "$hub" "$cli_project_id" "$cli_issue_uid" "cli enrollment pushed issue" "cli-enrolled hub"

log "creating hub project and baseline issue"
hub_project_json=$(post "$hub/api/v1/projects" 200 \
  "$(jq -nc --arg name "$project" '{name: $name}')")
hub_project_id=$(jq -r '.project.id' <<<"$hub_project_json")
hub_project_uid=$(jq -r '.project.uid' <<<"$hub_project_json")

issue_json=$(post "$hub/api/v1/projects/$hub_project_id/issues" 200 \
  '{"actor":"hub","title":"baseline from hub","body":"docker federation smoke"}')
issue_uid=$(jq -r '.issue.uid' <<<"$issue_json")
issue_short=$(jq -r '.issue.short_id' <<<"$issue_json")

log "enabling hub federation"
post "$hub/api/v1/projects/$hub_project_id/federation/enable" 200 '{"actor":"hub"}' >/dev/null
metadata=$(get "$hub/api/v1/projects/$hub_project_id/federation")
replay_horizon=$(jq -r '.replay_horizon_event_id' <<<"$metadata")
baseline_through=$(jq -r '.baseline_through_event_id' <<<"$metadata")

spoke_b_uid=$(get "$spoke_b/api/v1/instance" | jq -r '.instance_uid')

log "enrolling and binding spoke-a"
token_a=federation-smoke-token-a
actor_a=agent-a
post "$hub/api/v1/federation/enrollments" 200 \
  "$(jq -nc --arg uid "$spoke_a_uid" --arg token "$token_a" --arg actor "$actor_a" --argjson project_id "$hub_project_id" \
    '{spoke_instance_uid: $uid, project_id: $project_id, capabilities: "pull,push,claim", token: $token, actor: $actor}')" >/dev/null
spoke_a_replica=$(post "$spoke_a/api/v1/federation/replicas" 200 \
  "$(jq -nc \
    --arg hub_url "$hub" \
    --arg hub_uid "$hub_project_uid" \
    --arg name "$project" \
    --arg token "$token_a" \
    --arg actor "$actor_a" \
    --argjson hub_project_id "$hub_project_id" \
    --argjson replay "$replay_horizon" \
    --argjson baseline "$baseline_through" \
    '{hub_url: $hub_url, hub_project_id: $hub_project_id, hub_project_uid: $hub_uid, project_name: $name, replay_horizon_event_id: $replay, baseline_through_event_id: $baseline, token: $token, capabilities: "pull,push,claim", actor: $actor, push_enabled: true, allow_insecure: true}')")
spoke_a_project_id=$(jq -r '.project.id' <<<"$spoke_a_replica")

log "enrolling and binding spoke-b"
token_b=federation-smoke-token-b
actor_b=agent-b
post "$hub/api/v1/federation/enrollments" 200 \
  "$(jq -nc --arg uid "$spoke_b_uid" --arg token "$token_b" --arg actor "$actor_b" --argjson project_id "$hub_project_id" \
    '{spoke_instance_uid: $uid, project_id: $project_id, capabilities: "pull,push,claim", token: $token, actor: $actor}')" >/dev/null
spoke_b_replica=$(post "$spoke_b/api/v1/federation/replicas" 200 \
  "$(jq -nc \
    --arg hub_url "$hub" \
    --arg hub_uid "$hub_project_uid" \
    --arg name "$project" \
    --arg token "$token_b" \
    --arg actor "$actor_b" \
    --argjson hub_project_id "$hub_project_id" \
    --argjson replay "$replay_horizon" \
    --argjson baseline "$baseline_through" \
    '{hub_url: $hub_url, hub_project_id: $hub_project_id, hub_project_uid: $hub_uid, project_name: $name, replay_horizon_event_id: $replay, baseline_through_event_id: $baseline, token: $token, capabilities: "pull,push,claim", actor: $actor, push_enabled: true, allow_insecure: true}')")
spoke_b_project_id=$(jq -r '.project.id' <<<"$spoke_b_replica")

log "verifying baseline pull"
wait_for_all_titles "baseline from hub"

log "acquiring lease on spoke-a and checking arbitration"
kata_for /data/kata/runner-a "$spoke_a" --as agent-a federation lease acquire "$issue_short" >/dev/null
expect_cli_failure "spoke-b competing lease" /data/kata/runner-b "$spoke_b" 'lease denied|claim_denied' \
  --as agent-b federation lease acquire "$issue_short"
expect_cli_failure "spoke-b lease-gated edit" /data/kata/runner-b "$spoke_b" 'lease denied|claim_denied' \
  --as agent-b edit "$issue_short" --title "blocked edit from spoke B"

log "editing under spoke-a lease"
kata_for /data/kata/runner-a "$spoke_a" --as agent-a edit "$issue_short" --title "leased edit from spoke A" >/dev/null
wait_for_all_titles "leased edit from spoke A"

log "releasing from spoke-a and acquiring on spoke-b"
kata_for /data/kata/runner-a "$spoke_a" --as agent-a federation lease release "$issue_short" >/dev/null
kata_for /data/kata/runner-b "$spoke_b" --as agent-b federation lease acquire "$issue_short" >/dev/null

log "editing under spoke-b lease"
kata_for /data/kata/runner-b "$spoke_b" --as agent-b edit "$issue_short" --title "leased edit from spoke B" >/dev/null
wait_for_all_titles "leased edit from spoke B"

log "passed"
