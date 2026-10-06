#!/usr/bin/env bash
# kata installer
# Usage: curl -fsSL https://katatracker.com/install.sh | bash

set -euo pipefail

REPO="kenn-io/kata"
BINARY_NAME="kata"
KATA_INSTALL_TMPDIR=""

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

info() { printf "${GREEN}%s${NC}\n" "$1"; }
warn() { printf "${YELLOW}%s${NC}\n" "$1"; }
error() {
  printf "${RED}%s${NC}\n" "$1" >&2
  exit 1
}

detect_os() {
  case "$(uname -s)" in
    Darwin) echo "darwin" ;;
    Linux) echo "linux" ;;
    MINGW* | MSYS* | CYGWIN*) echo "windows" ;;
    *) error "Unsupported OS: $(uname -s)" ;;
  esac
}

detect_arch() {
  case "$(uname -m)" in
    x86_64 | amd64) echo "amd64" ;;
    aarch64 | arm64) echo "arm64" ;;
    *) error "Unsupported architecture: $(uname -m)" ;;
  esac
}

find_install_dir() {
  if [[ -w "/usr/local/bin" ]]; then
    echo "/usr/local/bin"
  else
    mkdir -p "$HOME/.local/bin"
    echo "$HOME/.local/bin"
  fi
}

# Every network request gets the same limits: give up on a connection after
# CONNECT_TIMEOUT seconds, abort a transfer that stalls for STALL_SECONDS, and
# try it NETWORK_ATTEMPTS times before failing.
CONNECT_TIMEOUT=15
STALL_SECONDS=30
NETWORK_ATTEMPTS=3
RETRY_DELAY=2

retry() {
  local url="$1"
  shift
  local attempt
  for ((attempt = 1; ; attempt++)); do
    "$@" && return 0
    if ((attempt == NETWORK_ATTEMPTS)); then
      error "Could not download ${url} after ${NETWORK_ATTEMPTS} attempts.
Check your network connection and run the installer again, install with Homebrew (brew install kata), or download the release manually from https://github.com/${REPO}/releases"
    fi
    warn "Could not download ${url}; retrying in ${RETRY_DELAY}s (attempt $((attempt + 1)) of ${NETWORK_ATTEMPTS})..." >&2
    sleep "$RETRY_DELAY"
  done
}

curl_limited() {
  # A transfer slower than 1 KB/s for STALL_SECONDS counts as stalled.
  curl --connect-timeout "$CONNECT_TIMEOUT" --speed-limit 1024 --speed-time "$STALL_SECONDS" "$@"
}

is_gnu_wget() {
  [[ "$(wget --version 2>/dev/null)" == *"GNU Wget"* ]]
}

wget_limited() {
  if is_gnu_wget; then
    wget --dns-timeout="$CONNECT_TIMEOUT" --connect-timeout="$CONNECT_TIMEOUT" \
      --read-timeout="$STALL_SECONDS" --tries=1 "$@"
  else
    # BusyBox wget has one timeout that covers connecting and each read.
    wget -T "$STALL_SECONDS" "$@"
  fi
}

# download URL OUTPUT [progress]
# With "progress", show a progress bar when stderr is a terminal. It still is
# under `curl ... | bash`, where only stdin is the pipe.
download() {
  local url="$1"
  local output="$2"
  local show_progress=false
  if [[ "${3-}" == "progress" && -t 2 ]]; then
    show_progress=true
  fi

  local args
  if command -v curl >/dev/null 2>&1; then
    args=(-fL -o "$output")
    if $show_progress; then
      args+=(--progress-bar)
    else
      args+=(-sS)
    fi
    retry "$url" curl_limited "${args[@]}" "$url"
  elif command -v wget >/dev/null 2>&1; then
    args=(-O "$output")
    if ! $show_progress; then
      args+=(-q)
    elif is_gnu_wget; then
      args+=(-q --show-progress)
    fi
    retry "$url" wget_limited "${args[@]}" "$url"
  else
    error "Neither curl nor wget found"
  fi
}

# Print the URL a redirect chain ends at. A failed attempt prints nothing, so
# retry's caller never captures output from an earlier attempt.
resolve_redirect() {
  local url="$1"
  local final_url
  if command -v curl >/dev/null 2>&1; then
    final_url="$(curl_limited -fsSLI -o /dev/null -w '%{url_effective}' "$url")" || return 1
  else
    final_url="$(wget_limited --spider -S "$url" 2>&1)" || return 1
    final_url="$(printf '%s\n' "$final_url" \
      | awk 'tolower($1)=="location:" {print $2}' \
      | tail -1 \
      | tr -d '\r\n')"
  fi
  printf '%s' "$final_url"
}

get_latest_version() {
  local url="https://github.com/${REPO}/releases/latest"
  if ! command -v curl >/dev/null 2>&1 && ! command -v wget >/dev/null 2>&1; then
    return 1
  fi
  local final_url
  final_url="$(retry "$url" resolve_redirect "$url")" || return 1

  case "$final_url" in
    */releases/tag/*) echo "${final_url##*/releases/tag/}" ;;
    *) return 1 ;;
  esac
}

verify_checksum() {
  local file="$1"
  local checksums_file="$2"
  local filename="$3"

  local expected
  expected="$(awk -v f="$filename" '{gsub(/^\*/, "", $2); if ($2==f) {print $1; exit}}' "$checksums_file")"
  if [[ -z "$expected" ]]; then
    error "No checksum found for $filename in SHA256SUMS"
  fi

  local actual
  if command -v sha256sum >/dev/null 2>&1; then
    actual="$(sha256sum "$file" | cut -d' ' -f1)"
  elif command -v shasum >/dev/null 2>&1; then
    actual="$(shasum -a 256 "$file" | cut -d' ' -f1)"
  else
    error "No sha256 tool available. Install coreutils and retry."
  fi

  if [[ "$expected" != "$actual" ]]; then
    error "Checksum verification failed for $filename"
  fi

  info "Checksum verified"
}

extract_archive() {
  local os="$1"
  local archive_path="$2"
  local tmpdir="$3"

  if [[ "$os" == "windows" ]]; then
    if command -v unzip >/dev/null 2>&1; then
      unzip -q "$archive_path" -d "$tmpdir"
    elif command -v powershell.exe >/dev/null 2>&1; then
      KATA_ARCHIVE_PATH="$archive_path" KATA_EXTRACT_DIR="$tmpdir" powershell.exe -NoProfile -Command "Expand-Archive -LiteralPath \$env:KATA_ARCHIVE_PATH -DestinationPath \$env:KATA_EXTRACT_DIR -Force"
    elif command -v powershell >/dev/null 2>&1; then
      KATA_ARCHIVE_PATH="$archive_path" KATA_EXTRACT_DIR="$tmpdir" powershell -NoProfile -Command "Expand-Archive -LiteralPath \$env:KATA_ARCHIVE_PATH -DestinationPath \$env:KATA_EXTRACT_DIR -Force"
    else
      error "Neither unzip nor PowerShell found for extracting Windows archive"
    fi
  else
    tar -xzf "$archive_path" -C "$tmpdir"
  fi
}

install_binary() {
  local binary_path="$1"
  local install_dir="$2"
  local binary_name="$3"
  local target="$install_dir/$binary_name"

  if [[ -w "$install_dir" ]]; then
    mv "$binary_path" "$target"
    chmod +x "$target"
  else
    command -v sudo >/dev/null 2>&1 || error "$install_dir is not writable and sudo is not available"
    sudo mv "$binary_path" "$target"
    sudo chmod +x "$target"
  fi
}

release_predates_web_ui() {
  local version="$1"
  [[ "$version" =~ ^v0\.([0-9]|1[0-3])\.(0|[1-9][0-9]*)$ ]]
}

verify_release_binary() {
  local binary_path="$1"
  local version="$2"
  if release_predates_web_ui "$version"; then
    return 0
  fi
  if ! "$binary_path" _web-assets-check >/dev/null 2>&1; then
    error "Downloaded release does not contain the validated Kata web UI"
  fi
}

install_from_release() {
  local os="$1"
  local arch="$2"
  local install_dir="$3"

  info "Fetching latest release..."
  local version
  version="$(get_latest_version)"
  [[ -n "$version" ]] || return 1

  info "Found version: $version"

  local platform="${os}_${arch}"
  local filename="${BINARY_NAME}_${version#v}_${platform}.tar.gz"
  local binary="$BINARY_NAME"
  if [[ "$os" == "windows" ]]; then
    filename="${BINARY_NAME}_${version#v}_${platform}.zip"
    binary="${BINARY_NAME}.exe"
  fi

  local base_url="https://github.com/${REPO}/releases/download/${version}"
  local tmpdir
  tmpdir="$(mktemp -d)"
  KATA_INSTALL_TMPDIR="$tmpdir"
  trap 'rm -rf "$KATA_INSTALL_TMPDIR"' EXIT

  local archive_path="$tmpdir/release.tar.gz"
  if [[ "$os" == "windows" ]]; then
    archive_path="$tmpdir/release.zip"
  fi

  info "Downloading ${filename}..."
  download "${base_url}/${filename}" "$archive_path" progress

  download "${base_url}/SHA256SUMS" "$tmpdir/SHA256SUMS"
  verify_checksum "$archive_path" "$tmpdir/SHA256SUMS" "$filename"

  info "Extracting..."
  extract_archive "$os" "$archive_path" "$tmpdir"

  [[ -f "$tmpdir/$binary" ]] || error "Downloaded release did not contain $binary"
  verify_release_binary "$tmpdir/$binary" "$version"
  install_binary "$tmpdir/$binary" "$install_dir" "$binary"
}

main() {
  info "Installing kata..."
  echo

  local os
  local arch
  local install_dir
  os="$(detect_os)"
  arch="$(detect_arch)"
  install_dir="$(find_install_dir)"

  info "Platform: ${os}/${arch}"
  info "Install directory: ${install_dir}"
  echo

  install_from_release "$os" "$arch" "$install_dir"

  echo
  info "Installation complete!"
  echo

  if ! printf '%s' ":$PATH:" | grep -Fq ":$install_dir:"; then
    warn "Add this to your shell profile:"
    echo "  export PATH=\"\$PATH:$install_dir\""
    echo
  fi

  echo "Check the install:"
  echo "  kata version"
  echo "  kata update --check"
  echo
  echo "Get started:"
  echo "  cd your-repo"
  echo "  kata init"
  echo "  kata tui"
}

if [[ "${BASH_SOURCE[0]-}" == "${0}" || -z "${BASH_SOURCE[0]-}" ]]; then
  main "$@"
fi
