#!/bin/sh
# SPDX-License-Identifier: MIT
#
# Install the latest release of azcp.
#
#     curl -sSfL https://raw.githubusercontent.com/JohanLindvall/azcp/main/scripts/install.sh | sh
#
# It downloads the binary built for this machine, checks it against the
# checksums its release publishes, and installs it as ~/.local/bin/azcp, or
# /usr/local/bin/azcp when run as root. Options go after `sh -s --` when the
# script arrives on a pipe; `--help` lists them.
#
# This is POSIX sh rather than bash because the Linux binaries are static and
# run where bash is not installed — Alpine, for one.

set -eu

repo=JohanLindvall/azcp
releases=https://github.com/$repo/releases

usage() {
  cat <<EOF
Usage: install.sh [--dir DIR] [--version VERSION]

Download a release of azcp from $releases,
check it against the release's SHA256SUMS, and install it.

  --dir DIR          install into DIR rather than ~/.local/bin, or
                     /usr/local/bin when run as root
  --version VERSION  install that release, such as v0.6.5, rather than the
                     latest
  -h, --help         show this help and exit

AZCP_INSTALL_DIR and AZCP_VERSION do the same from the environment.
EOF
}

say() { printf '%s\n' "$*"; }
die() { printf 'install.sh: %s\n' "$*" >&2; exit 1; }
have() { command -v "$1" >/dev/null 2>&1; }

misuse() {
  printf 'install.sh: %s\n' "$1" >&2
  printf "Try 'install.sh --help' for more information.\n" >&2
  exit 1
}

fetch() {
  if have curl; then
    curl --fail --silent --show-error --location --retry 3 --output "$2" "$1"
  else
    wget -q -O "$2" "$1"
  fi
}

sha256() {
  if have sha256sum; then
    sha256sum "$1"
  elif have shasum; then
    shasum -a 256 "$1"
  else
    openssl dgst -sha256 -r "$1"
  fi | cut -d ' ' -f 1
}

cleanup() {
  if [ -n "$staged" ]; then rm -f "$staged"; fi
  if [ -n "$work" ]; then rm -rf "$work"; fi
}

main() {
  dir=${AZCP_INSTALL_DIR:-}
  version=${AZCP_VERSION:-}
  while [ $# -gt 0 ]; do
    case $1 in
      --dir)
        [ $# -ge 2 ] || misuse "option '--dir' requires an argument"
        dir=$2
        shift ;;
      --dir=*) dir=${1#*=} ;;
      --version)
        [ $# -ge 2 ] || misuse "option '--version' requires an argument"
        version=$2
        shift ;;
      --version=*) version=${1#*=} ;;
      -h | --help) usage; exit 0 ;;
      -*) misuse "unrecognized option '$1'" ;;
      *) misuse "unexpected argument '$1'" ;;
    esac
    shift
  done

  case $(uname -s) in
    Linux) os=linux ;;
    Darwin) os=darwin ;;
    MINGW* | MSYS* | CYGWIN*)
      die "this script does not install on Windows; the .zip files at $releases/latest do" ;;
    *) die "no release is built for $(uname -s); go install github.com/$repo/cmd/azcp@latest builds one" ;;
  esac
  case $(uname -m) in
    x86_64 | amd64) arch=amd64 ;;
    aarch64 | arm64) arch=arm64 ;;
    *) die "no release is built for $os/$(uname -m); go install github.com/$repo/cmd/azcp@latest builds one" ;;
  esac
  # A shell running under Rosetta is told x86_64 on Apple silicon, and would
  # install the Intel binary to be translated when a native one is published.
  if [ "$os" = darwin ] && [ "$arch" = amd64 ] &&
    [ "$(sysctl -n sysctl.proc_translated 2>/dev/null)" = 1 ]; then
    arch=arm64
  fi

  have curl || have wget || die "cannot download: neither curl nor wget is installed"
  have sha256sum || have shasum || have openssl ||
    die "cannot check the download: none of sha256sum, shasum or openssl is installed"
  have gzip || die "cannot unpack the download: gzip is not installed"

  if [ -z "$dir" ]; then
    if [ "$(id -u)" -eq 0 ]; then
      dir=/usr/local/bin
    else
      dir=${HOME:?is not set; choose a directory with --dir}/.local/bin
    fi
  fi
  mkdir -p -- "$dir" || die "cannot create $dir"
  dir=$(CDPATH='' cd -- "$dir" && pwd)
  [ -w "$dir" ] || die "cannot write to $dir; run as root, or choose another directory with --dir"
  [ ! -d "$dir/azcp" ] || die "$dir/azcp is a directory"

  # SIGPIPE too: an untrapped signal kills the shell without running the EXIT
  # trap, and piping this script's output into `head` is enough to raise one.
  work='' staged=''
  trap cleanup EXIT
  trap 'exit 1' HUP INT PIPE TERM
  work=$(mktemp -d "${TMPDIR:-/tmp}/azcp-install.XXXXXX")

  # A release's SHA256SUMS names every asset in it, so the one belonging to the
  # latest release says which version that is as well as what its binary must
  # hash to. Reading it costs no request to the GitHub API, whose allowance for
  # an anonymous caller a shared CI runner can exhaust.
  if [ -n "$version" ]; then
    case $version in v*) ;; *) version=v$version ;; esac
    sums=$releases/download/$version/SHA256SUMS
  else
    sums=$releases/latest/download/SHA256SUMS
  fi
  fetch "$sums" "$work/SHA256SUMS" || die "cannot download $sums"

  entry=$(grep "  azcp_v[^ ]*_${os}_${arch}\.bin\.gz\$" "$work/SHA256SUMS" | head -n 1)
  [ -n "$entry" ] || die "$sums names no binary for $os/$arch"
  want=${entry%% *}
  asset=${entry##* }
  version=${asset#azcp_}
  version=${version%_"${os}_${arch}".bin.gz}

  say "Downloading azcp $version for $os/$arch"
  url=$releases/download/$version/$asset
  fetch "$url" "$work/$asset" || die "cannot download $url"
  [ "$(sha256 "$work/$asset")" = "$want" ] ||
    die "$asset does not match the checksum its release publishes"

  # Staged beside its final name, so that renaming it replaces the old binary
  # in one step: a copy of azcp already running keeps the file it has, and
  # nothing ever finds half of the new one.
  staged=$(mktemp "$dir/.azcp.XXXXXX") || die "cannot write to $dir"
  gzip -dc "$work/$asset" >"$staged" || die "cannot unpack $asset"
  chmod 755 "$staged"
  out=$("$staged" --version 2>&1) || die "the downloaded azcp does not run here: $out"
  mv -f "$staged" "$dir/azcp"
  staged=''

  say "Installed azcp $version as $dir/azcp"
  case :${PATH:-}: in
    *:"$dir":* | *:"$dir/":*)
      found=$(command -v azcp || true)
      if [ -n "$found" ] && ! [ "$found" -ef "$dir/azcp" ]; then
        say "$found comes first on your PATH, so 'azcp' still runs that one."
      fi ;;
    *)
      say "$dir is not on your PATH. To put it there, add this to your shell's profile:"
      say ""
      say "    export PATH=\"$dir:\$PATH\""
      ;;
  esac
}

# Everything above only defines, so a download cut short does nothing at all.
main "$@"
