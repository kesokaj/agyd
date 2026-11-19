#!/usr/bin/env bash
set -euo pipefail

# Install agyd from GitHub Releases with mandatory SHA-256 verification.
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/kesokaj/agyd/main/install.sh | bash
#   curl -fsSL https://raw.githubusercontent.com/kesokaj/agyd/main/install.sh | bash -s -- --version v0.2.0 --dir ~/.local/bin

readonly DEFAULT_REPO="kesokaj/agyd"
readonly TAG_RE='^v[0-9]+\.[0-9]+\.[0-9]+(-[A-Za-z0-9][A-Za-z0-9._-]*)?$'
readonly REPO_RE='^[A-Za-z0-9][A-Za-z0-9._-]*/[A-Za-z0-9][A-Za-z0-9._-]*$'
readonly SHA256_RE='^[0-9a-fA-F]{64}$'

TMP_DIR=""
STAGED_BIN=""

cleanup() {
  if [[ -n "$STAGED_BIN" && -e "$STAGED_BIN" ]]; then
    rm -f "$STAGED_BIN"
  fi
  if [[ -n "$TMP_DIR" && -d "$TMP_DIR" ]]; then
    rm -rf "$TMP_DIR"
  fi
}
trap cleanup EXIT

die() {
  printf 'install.sh: %s\n' "$*" >&2
  exit 1
}

usage() {
  cat <<'EOF'
Install agyd from GitHub Releases (verifies SHA-256 checksums before installing).

Usage:
  install.sh [--version <tag|latest>] [--dir <install-dir>] [--repo <owner/repo>]

Options:
  --version <tag>   Release tag to install (default: latest; env AGYD_VERSION)
  --dir <path>      Target directory for the agyd binary (default: ~/.local/bin; env AGYD_INSTALL_DIR)
  --repo <o/r>      GitHub repository (default: kesokaj/agyd; env AGYD_REPO)
  -h, --help        Show this help message
EOF
}

require_cmd() {
  command -v "$1" >/dev/null 2>&1 || die "required command not found: $1"
}

detect_os() {
  local raw
  raw="$(uname -s)"
  case "$raw" in
    Darwin) printf 'darwin\n' ;;
    Linux)  printf 'linux\n' ;;
    *)      die "unsupported operating system: ${raw} (supported: macOS, Linux)" ;;
  esac
}

detect_arch() {
  local raw
  raw="$(uname -m)"
  case "$raw" in
    arm64|aarch64) printf 'arm64\n' ;;
    x86_64|amd64)  printf 'amd64\n' ;;
    *)             die "unsupported architecture: ${raw} (supported: arm64, amd64)" ;;
  esac
}

sha256_file() {
  local file="$1"
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$file" | awk '{print $1}'
  elif command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$file" | awk '{print $1}'
  else
    die "neither sha256sum nor shasum is installed; cannot verify download checksum"
  fi
}

valid_tag() {
  [[ "$1" =~ $TAG_RE && "$1" != *".."* ]]
}

resolve_latest_version() {
  local repo="$1"
  local latest_url="https://github.com/${repo}/releases/latest"
  local effective_url tag

  effective_url="$(curl --proto '=https' --tlsv1.2 -fsSL -o /dev/null -w '%{url_effective}' "$latest_url")" \
    || die "failed to resolve latest release from ${latest_url}"

  local prefix="https://github.com/${repo}/releases/tag/"
  [[ "$effective_url" == "${prefix}"* ]] \
    || die "no published release found at ${latest_url} (redirected to ${effective_url})"

  tag="${effective_url#"$prefix"}"
  tag="${tag%%[?#]*}"
  valid_tag "$tag" || die "unexpected release tag from GitHub: ${tag}"
  printf '%s\n' "$tag"
}

verify_checksum() {
  local checksums_file="$1"
  local archive_name="$2"
  local archive_path="$3"
  local expected="" matches=0 hash name actual

  while IFS= read -r line; do
    line="${line%$'\r'}"
    [[ -n "$line" ]] || continue
    read -r hash name _ <<<"$line"
    name="${name#\*}"
    if [[ "$name" == "$archive_name" ]]; then
      expected="$hash"
      matches=$((matches + 1))
    fi
  done <"$checksums_file"

  (( matches == 1 )) || die "expected 1 checksum entry for ${archive_name}, found ${matches}"
  [[ "$expected" =~ $SHA256_RE ]] || die "malformed SHA-256 checksum for ${archive_name}"

  actual="$(sha256_file "$archive_path")"
  [[ "$actual" =~ $SHA256_RE ]] || die "failed to compute SHA-256 for ${archive_name}"

  expected="$(printf '%s' "$expected" | tr '[:upper:]' '[:lower:]')"
  actual="$(printf '%s' "$actual" | tr '[:upper:]' '[:lower:]')"
  [[ "$actual" == "$expected" ]] || die "checksum mismatch for ${archive_name}: expected ${expected}, got ${actual}"
}

main() {
  umask 022

  local repo="${AGYD_REPO:-$DEFAULT_REPO}"
  local version="${AGYD_VERSION:-latest}"
  local install_dir="${AGYD_INSTALL_DIR:-}"

  while (( $# > 0 )); do
    case "$1" in
      --version)
        (( $# >= 2 )) || die "--version requires a value"
        version="$2"
        shift 2
        ;;
      --dir)
        (( $# >= 2 )) || die "--dir requires a value"
        install_dir="$2"
        shift 2
        ;;
      --repo)
        (( $# >= 2 )) || die "--repo requires a value"
        repo="$2"
        shift 2
        ;;
      -h|--help)
        usage
        exit 0
        ;;
      *)
        die "unknown option: $1 (run with --help)"
        ;;
    esac
  done

  if [[ -z "$install_dir" ]]; then
    [[ -n "${HOME:-}" ]] || die "HOME is not set; pass --dir <path>"
    install_dir="${HOME}/.local/bin"
  fi
  if [[ "$install_dir" != "/" ]]; then
    install_dir="${install_dir%/}"
  fi

  [[ "$repo" =~ $REPO_RE && "$repo" != *".."* ]] || die "invalid repository: ${repo}"
  if [[ "$version" != "latest" ]] && ! valid_tag "$version"; then
    die "invalid version: ${version} (expected latest or vX.Y.Z)"
  fi

  require_cmd curl
  require_cmd tar

  local os arch
  os="$(detect_os)"
  arch="$(detect_arch)"

  if [[ "$version" == "latest" ]]; then
    version="$(resolve_latest_version "$repo")"
  fi

  local ver_no_v="${version#v}"
  local archive="agyd_${ver_no_v}_${os}_${arch}.tar.gz"
  local base_url="https://github.com/${repo}/releases/download/${version}"

  TMP_DIR="$(mktemp -d)"
  mkdir -p "${TMP_DIR}/extract"

  printf 'Downloading agyd %s (%s/%s)...\n' "$version" "$os" "$arch"
  curl --proto '=https' --tlsv1.2 -fsSL -o "${TMP_DIR}/${archive}" "${base_url}/${archive}" \
    || die "failed to download ${base_url}/${archive}"
  curl --proto '=https' --tlsv1.2 -fsSL -o "${TMP_DIR}/checksums.txt" "${base_url}/checksums.txt" \
    || die "failed to download ${base_url}/checksums.txt"

  verify_checksum "${TMP_DIR}/checksums.txt" "$archive" "${TMP_DIR}/${archive}"

  tar -xzf "${TMP_DIR}/${archive}" -C "${TMP_DIR}/extract" agyd \
    || die "failed to extract agyd from ${archive}"

  local extracted="${TMP_DIR}/extract/agyd"
  [[ -f "$extracted" && ! -L "$extracted" ]] || die "archive did not contain a regular agyd binary"

  mkdir -p "$install_dir"
  STAGED_BIN="$(mktemp "${install_dir}/.agyd.XXXXXX")"
  cp "$extracted" "$STAGED_BIN"
  chmod 0755 "$STAGED_BIN"
  mv -f "$STAGED_BIN" "${install_dir}/agyd"
  STAGED_BIN=""

  printf 'Installed agyd %s to %s/agyd\n' "$version" "$install_dir"

  case ":${PATH:-}:" in
    *":${install_dir}:"*) ;;
    *)
      printf 'Note: %s is not on your PATH. Add it with:\n' "$install_dir"
      printf "  export PATH=\"%s:\$PATH\"\n" "$install_dir"
      ;;
  esac

  if ! command -v agy >/dev/null 2>&1; then
    printf 'Note: agy was not found on your PATH. Install the Antigravity CLI (agy) and sign in before running agyd.\n'
  fi
}

main "$@"
