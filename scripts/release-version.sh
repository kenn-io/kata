#!/usr/bin/env bash

is_release_version() {
  local value="$1"
  local core='(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)'
  local ident='(0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*)'
  [[ "$value" =~ ^${core}(-${ident}(\.${ident})*)?$ ]]
}

is_release_tag() {
  [[ "$1" == v* ]] && is_release_version "${1#v}"
}

is_prerelease_version() {
  is_release_version "$1" && [[ "$1" == *-* ]]
}
