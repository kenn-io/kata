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

download() {
  local url="$1"
  local output="$2"
  if command -v curl >/dev/null 2>&1; then
    curl -fsSL "$url" -o "$output"
  elif command -v wget >/dev/null 2>&1; then
    wget -q "$url" -O "$output"
  else
    error "Neither curl nor wget found"
  fi
}

get_latest_version() {
  local url="https://github.com/${REPO}/releases/latest"
  local final_url=""
  if command -v curl >/dev/null 2>&1; then
    final_url="$(curl -fsSLI -o /dev/null -w '%{url_effective}' "$url")" || return 1
  elif command -v wget >/dev/null 2>&1; then
    final_url="$(wget --spider -S "$url" 2>&1 \
      | awk 'tolower($1)=="location:" {print $2}' \
      | tail -1 \
      | tr -d '\r\n')" || return 1
  else
    return 1
  fi

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
  download "${base_url}/${filename}" "$archive_path"

  download "${base_url}/SHA256SUMS" "$tmpdir/SHA256SUMS"
  verify_checksum "$archive_path" "$tmpdir/SHA256SUMS" "$filename"

  info "Extracting..."
  extract_archive "$os" "$archive_path" "$tmpdir"

  [[ -f "$tmpdir/$binary" ]] || error "Downloaded release did not contain $binary"
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
