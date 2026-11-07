#!/bin/sh
# send.io CLI installer.
#
#   curl -fsSL https://send.io/install.sh | sh
#   curl -fsSL https://send.io/install.sh | sh -s -- --version 1.2.3
#
# Environment:
#   SENDIO_VERSION        version to install (default: latest release)
#   SENDIO_INSTALL        install root; the binary goes to $SENDIO_INSTALL/bin
#                         (default: $HOME/.sendio)
#   SENDIO_REPO           GitHub owner/name of the release repo (default: send-io/cli)
#   SENDIO_DOWNLOAD_BASE  base URL for release assets; files are fetched from
#                         $SENDIO_DOWNLOAD_BASE/v<version>/<file>
#                         (default: https://github.com/$SENDIO_REPO/releases/download)
#   SENDIO_GITHUB_API     GitHub API base (default: https://api.github.com)
#   SENDIO_NO_MODIFY_PATH set to only print PATH instructions (no rc file edit)
#   SENDIO_NO_LOGO        set to skip the logo
#
# The whole script lives in main(), called on the last line, so a truncated
# download never runs half a script.

# Where releases come from. Empty (the default, and what the web build
# serves): GitHub releases of $SENDIO_REPO. A site that hosts its own builds
# can set these to <site>/cli/download and <site>/cli/download/latest.
SENDIO_DEFAULT_DOWNLOAD_BASE=""
SENDIO_DEFAULT_LATEST_URL=""

main() {
  set -eu
  print_logo

  repo="${SENDIO_REPO:-send-io/cli}"
  version="${SENDIO_VERSION:-}"
  install_root="${SENDIO_INSTALL:-${HOME}/.sendio}"
  download_base="${SENDIO_DOWNLOAD_BASE:-${SENDIO_DEFAULT_DOWNLOAD_BASE:-https://github.com/${repo}/releases/download}}"
  latest_url="${SENDIO_LATEST_URL:-${SENDIO_DEFAULT_LATEST_URL}}"
  github_api="${SENDIO_GITHUB_API:-https://api.github.com}"

  while [ $# -gt 0 ]; do
    case "$1" in
      -h | --help)
        usage
        return 0
        ;;
      -v | --version)
        [ $# -ge 2 ] || die "--version needs a value"
        version="$2"
        shift 2
        ;;
      --version=*)
        version="${1#--version=}"
        shift
        ;;
      -d | --dir)
        [ $# -ge 2 ] || die "--dir needs a value"
        install_root="$2"
        shift 2
        ;;
      --dir=*)
        install_root="${1#--dir=}"
        shift
        ;;
      *)
        die "unknown argument: $1 (see --help)"
        ;;
    esac
  done

  need_cmd uname
  need_cmd tar
  need_cmd mkdir
  need_cmd chmod
  need_cmd mktemp

  os="$(detect_os)"
  arch="$(detect_arch "$os")"

  if [ -z "$version" ] || [ "$version" = "latest" ]; then
    if [ -n "$latest_url" ]; then
      say "resolving latest release..."
      version="$(fetch "$latest_url" | tr -d '[:space:]')" || version=""
    else
      say "resolving latest release of ${repo}..."
      version="$(latest_version "$github_api" "$repo")"
    fi
    [ -n "$version" ] || die "could not determine the latest version (set SENDIO_VERSION)"
  fi
  version="${version#v}"

  archive="sendio_${version}_${os}_${arch}.tar.gz"
  base_url="${download_base%/}/v${version}"

  tmp="$(mktemp -d 2>/dev/null || mktemp -d -t sendio)"
  # shellcheck disable=SC2064 # expand $tmp now, on purpose
  trap "rm -rf '$tmp'" EXIT
  trap 'exit 130' INT TERM HUP

  say "downloading sendio ${version} (${os}/${arch})..."
  download "${base_url}/${archive}" "${tmp}/${archive}"
  download "${base_url}/checksums.txt" "${tmp}/checksums.txt"

  verify_checksum "$tmp" "$archive"

  mkdir -p "${tmp}/x"
  tar -xzf "${tmp}/${archive}" -C "${tmp}/x"
  [ -f "${tmp}/x/sendio" ] || die "archive ${archive} does not contain a sendio binary"

  bin_dir="${install_root}/bin"
  mkdir -p "$bin_dir"
  # Write next to the target then rename, so a running sendio is never
  # left half-overwritten.
  cp "${tmp}/x/sendio" "${bin_dir}/.sendio.new"
  chmod 755 "${bin_dir}/.sendio.new"
  mv -f "${bin_dir}/.sendio.new" "${bin_dir}/sendio"

  say "installed sendio ${version} to ${bin_dir}/sendio"

  case ":${PATH}:" in
    *":${bin_dir}:"*)
      say "run 'sendio login' to get started"
      ;;
    *)
      setup_path "$bin_dir"
      ;;
  esac
}

usage() {
  cat <<'EOF'
send.io CLI installer

Usage: install.sh [--version <version>] [--dir <install root>]

Options:
  -v, --version <v>   version to install, e.g. 1.2.3 (default: latest)
  -d, --dir <path>    install root; binary goes to <path>/bin (default: ~/.sendio)
  -h, --help          show this help

Environment: SENDIO_VERSION, SENDIO_INSTALL, SENDIO_REPO,
SENDIO_DOWNLOAD_BASE, SENDIO_GITHUB_API (see the header of this script).
Windows: use install.ps1 instead.
EOF
}

say() {
  printf 'sendio-install: %s\n' "$*"
}

die() {
  printf 'sendio-install: error: %s\n' "$*" >&2
  exit 1
}

need_cmd() {
  command -v "$1" >/dev/null 2>&1 || die "required command not found: $1"
}

detect_os() {
  uname_s="$(uname -s)"
  case "$uname_s" in
    Darwin) echo darwin ;;
    Linux) echo linux ;;
    MINGW* | MSYS* | CYGWIN* | Windows_NT)
      die "Windows detected. Run this in PowerShell instead:
  irm https://send.io/install.ps1 | iex"
      ;;
    *) die "unsupported operating system: ${uname_s}" ;;
  esac
}

detect_arch() {
  uname_m="$(uname -m)"
  case "$uname_m" in
    x86_64 | amd64) arch=amd64 ;;
    arm64 | aarch64 | armv8*) arch=arm64 ;;
    *) die "unsupported architecture: ${uname_m} (supported: amd64, arm64)" ;;
  esac
  # An x86_64 shell under Rosetta on Apple silicon: prefer the native build.
  if [ "$1" = darwin ] && [ "$arch" = amd64 ]; then
    if [ "$(sysctl -n sysctl.proc_translated 2>/dev/null || echo 0)" = 1 ]; then
      arch=arm64
    fi
  fi
  echo "$arch"
}

# download URL DEST
download() {
  if command -v curl >/dev/null 2>&1; then
    curl --proto '=https,http' -fsSL --retry 3 -o "$2" "$1" || die "download failed: $1"
  elif command -v wget >/dev/null 2>&1; then
    wget -q -O "$2" "$1" || die "download failed: $1"
  else
    die "need curl or wget to download files"
  fi
}

# fetch URL -> stdout
fetch() {
  if command -v curl >/dev/null 2>&1; then
    curl -fsSL --retry 3 -H 'Accept: application/vnd.github+json' "$1"
  elif command -v wget >/dev/null 2>&1; then
    wget -q -O - --header='Accept: application/vnd.github+json' "$1"
  else
    die "need curl or wget to download files"
  fi
}

latest_version() {
  json="$(fetch "${1%/}/repos/$2/releases/latest")" || return 1
  # Pull "tag_name": "v1.2.3" out without needing jq.
  printf '%s\n' "$json" | sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -n 1
}

sha256_of() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum < "$1" | awk '{print $1}' # stdin: no file name, so Git Bash on Windows prints no leading backslash
  elif command -v shasum >/dev/null 2>&1; then
    shasum -a 256 < "$1" | awk '{print $1}'
  elif command -v openssl >/dev/null 2>&1; then
    openssl dgst -sha256 "$1" | awk '{print $NF}'
  else
    die "need sha256sum, shasum or openssl to verify the download"
  fi
}

# verify_checksum DIR ARCHIVE
verify_checksum() {
  expected="$(awk -v f="$2" '$2 == f || $2 == "*" f {print $1}' "$1/checksums.txt" | head -n 1)"
  [ -n "$expected" ] || die "no checksum for $2 in checksums.txt"
  actual="$(sha256_of "$1/$2")"
  if [ "$expected" != "$actual" ]; then
    die "checksum mismatch for $2
  expected: ${expected}
  actual:   ${actual}
refusing to install"
  fi
  say "checksum verified (sha256 ${actual})"
}

# print_logo draws the send.io mark when the output is a terminal: a Signal
# Orange (#FF4A1C) square with "S." cut out of it (blank cells, so it shows the
# terminal's own background) and a soft shine sweeping across it once, the
# same mark the CLI shows. Plain text with NO_COLOR or a dumb terminal.
print_logo() {
  [ -t 1 ] || return 0
  [ -n "${SENDIO_NO_LOGO:-}" ] && return 0
  if [ -n "${NO_COLOR:-}" ] || [ "${TERM:-}" = dumb ]; then
    printf '\n send.io   CLI installer\n\n'
    return 0
  fi
  # 14x7 square; '.' = cut out. Row 2 and 3 carry the text.
  logo_rows='##############
###....#######
###.##########
###....#######
######.#######
###....##..###
##############'
  printf '\n'
  pos=-6
  first=1
  while [ "$pos" -le 22 ]; do
    [ "$first" = 1 ] || printf '\033[7A'
    first=0
    _logo_frame "$pos"
    pos=$((pos + 2))
    sleep 0.03 2>/dev/null || true
  done
  printf '\033[7A'
  _logo_frame -99
  printf '\n'
}

# _logo_frame draws the square with the shine band at column $1 (one awk call
# per frame keeps the whole sweep under half a second).
_logo_frame() {
  printf '%s\n' "$logo_rows" | awk -v pos="$1" '
    BEGIN { blk = "\342\226\210"; e = "\033" }
    {
      r = NR - 1; out = " "
      for (c = 0; c < 14; c++) {
        if (substr($0, c + 1, 1) == ".") { out = out " "; continue }
        d = c - r - pos; if (d < 0) d = -d
        rgb = "255;74;28"
        if (d == 0) rgb = "255;206;184"; else if (d == 1) rgb = "255;160;128"; else if (d == 2) rgb = "255;110;72"
        out = out e "[38;2;" rgb "m" blk e "[0m"
      }
      if (r == 2) out = out "   " e "[1msend.io" e "[0m"
      if (r == 3) out = out "   " e "[1mCLI installer" e "[0m"
      print out
    }'
}

# setup_path makes `sendio` reachable: a link in a personal bin directory
# that is already on PATH (works at once), otherwise a PATH line added to the
# shell's startup file (new terminals). SENDIO_NO_MODIFY_PATH=1 only prints
# the instructions.
setup_path() {
  bin_dir="$1"
  for d in "$HOME/.local/bin" "$HOME/bin"; do
    case ":${PATH}:" in
      *":${d}:"*)
        if [ -d "$d" ] && [ -w "$d" ]; then
          ln -sf "${bin_dir}/sendio" "${d}/sendio"
          say "linked ${d}/sendio (already on your PATH)"
          say "run 'sendio login' to get started"
          return 0
        fi
        ;;
    esac
  done

  shell_name="$(basename "${SHELL:-sh}")"
  line="export PATH=\"${bin_dir}:\$PATH\""
  case "$shell_name" in
    zsh) rc="$HOME/.zshrc" ;;
    bash)
      rc="$HOME/.bashrc"
      [ "$(uname -s)" = Darwin ] && rc="$HOME/.bash_profile"
      ;;
    fish)
      rc="$HOME/.config/fish/config.fish"
      line="fish_add_path ${bin_dir}"
      ;;
    *) rc="$HOME/.profile" ;;
  esac

  if [ -n "${SENDIO_NO_MODIFY_PATH:-}" ]; then
    say "${bin_dir} is not on your PATH. Add this line to ${rc}:"
    printf '\n  %s\n\n' "$line"
  elif [ -f "$rc" ] && grep -qs "${bin_dir}" "$rc"; then
    say "${rc} already adds ${bin_dir} to PATH"
  else
    mkdir -p "$(dirname "$rc")"
    printf '\n# send.io CLI\n%s\n' "$line" >> "$rc"
    say "added ${bin_dir} to PATH in ${rc} (new terminals pick it up)"
  fi
  say "to use sendio in this terminal now, run:"
  if [ "$shell_name" = fish ]; then
    printf '\n  fish_add_path %s && sendio login\n\n' "$bin_dir"
  else
    printf '\n  export PATH="%s:$PATH" && sendio login\n\n' "$bin_dir"
  fi
}

main "$@"
