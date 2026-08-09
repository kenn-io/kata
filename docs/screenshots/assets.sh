#!/usr/bin/env bash

kata_docs_expected_assets=(
  "README.md"
  "manifest.json"
  "tui/hero.svg"
  "federation-tui/list.svg"
  "federation-tui/select-hub.svg"
  "federation-tui/select-hub-project.svg"
  "federation-tui/preview.svg"
  "federation-tui/result.svg"
  "web-ui/workspace.png"
  "web-ui/issue-detail.png"
  "web-ui/relationships.png"
  "web-ui/daemon-switcher.png"
)

kata_docs_is_expected_asset() {
  local candidate="$1"
  local asset
  for asset in "${kata_docs_expected_assets[@]}"; do
    [[ "$asset" == "$candidate" ]] && return 0
  done
  return 1
}

kata_docs_validate_assets() {
  local source_dir="$1"
  local asset path rel

  if [[ ! -d "$source_dir" ]]; then
    printf 'docs asset source does not exist: %s\n' "$source_dir" >&2
    return 1
  fi

  for asset in "${kata_docs_expected_assets[@]}"; do
    path="$source_dir/$asset"
    if [[ -L "$path" ]]; then
      printf 'docs asset source must not be a symlink: %s\n' "$asset" >&2
      return 1
    fi
    if [[ ! -f "$path" ]]; then
      printf 'docs asset source is missing expected asset: %s\n' "$asset" >&2
      return 1
    fi
    if [[ ! -s "$path" ]]; then
      printf 'docs asset source contains an empty asset: %s\n' "$asset" >&2
      return 1
    fi
  done

  while IFS= read -r -d '' path; do
    rel="${path#"$source_dir"/}"
    case "$rel" in
      .DS_Store|*/.DS_Store)
        continue
        ;;
    esac
    if ! kata_docs_is_expected_asset "$rel"; then
      printf 'docs asset source has unexpected file: %s\n' "$rel" >&2
      return 1
    fi
  done < <(find "$source_dir" -mindepth 1 \( -type f -o -type l \) -print0)
}
