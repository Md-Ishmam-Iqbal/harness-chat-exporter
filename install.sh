#!/bin/sh
set -eu

repository="ishmam-iqbal-sazim/harness-chat-exporter"
release_base="${HCE_RELEASE_BASE:-https://github.com/${repository}/releases/latest/download}"

fail() {
  printf 'hce install: %s\n' "$1" >&2
  exit 1
}

case "$(uname -s)" in
  Linux) operating_system="linux" ;;
  Darwin) operating_system="darwin" ;;
  *) fail "unsupported operating system; use install.ps1 on Windows" ;;
esac

case "$(uname -m)" in
  x86_64|amd64) architecture="amd64" ;;
  arm64|aarch64) architecture="arm64" ;;
  *) fail "unsupported CPU architecture: $(uname -m)" ;;
esac

archive="hce_${operating_system}_${architecture}.tar.gz"
temporary_directory="$(mktemp -d 2>/dev/null || mktemp -d -t hce-install)"
trap 'rm -rf "$temporary_directory"' EXIT HUP INT TERM

download() {
  if command -v curl >/dev/null 2>&1; then
    curl -fsSL "$1" -o "$2"
  elif command -v wget >/dev/null 2>&1; then
    wget -q "$1" -O "$2"
  else
    fail "curl or wget is required"
  fi
}

printf 'Downloading hce for %s/%s...\n' "$operating_system" "$architecture"
download "${release_base}/${archive}" "${temporary_directory}/${archive}"
download "${release_base}/checksums.txt" "${temporary_directory}/checksums.txt"

expected="$(awk -v file="$archive" '$2 == file { print $1 }' "${temporary_directory}/checksums.txt")"
[ -n "$expected" ] || fail "release checksum is missing for ${archive}"
if command -v sha256sum >/dev/null 2>&1; then
  actual="$(sha256sum "${temporary_directory}/${archive}" | awk '{ print $1 }')"
elif command -v shasum >/dev/null 2>&1; then
  actual="$(shasum -a 256 "${temporary_directory}/${archive}" | awk '{ print $1 }')"
else
  fail "sha256sum or shasum is required to verify the download"
fi
[ "$actual" = "$expected" ] || fail "download checksum did not match"

tar -xzf "${temporary_directory}/${archive}" -C "$temporary_directory"
target_directory="${HCE_INSTALL_DIR:-/usr/local/bin}"

if { [ -d "$target_directory" ] || mkdir -p "$target_directory"; } 2>/dev/null && [ -w "$target_directory" ]; then
  install -m 0755 "${temporary_directory}/hce" "${target_directory}/hce"
elif [ "$(id -u)" -eq 0 ]; then
  mkdir -p "$target_directory"
  install -m 0755 "${temporary_directory}/hce" "${target_directory}/hce"
elif command -v sudo >/dev/null 2>&1; then
  sudo mkdir -p "$target_directory"
  sudo install -m 0755 "${temporary_directory}/hce" "${target_directory}/hce"
else
  fail "cannot write to ${target_directory}; rerun with HCE_INSTALL_DIR set to a writable directory on PATH"
fi

printf 'Installed %s\n' "${target_directory}/hce"
"${target_directory}/hce" version
printf '\nRun: hce\n'
