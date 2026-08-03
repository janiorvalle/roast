#!/bin/sh
set -eu

root=$(CDPATH='' cd -- "$(dirname "$0")/.." && pwd)
case "$(uname -s)" in
  Darwin) os=darwin ;;
  Linux) os=linux ;;
  *) printf 'install smoke is only used on Unix release targets\n'; exit 0 ;;
esac
case "$(uname -m)" in
  x86_64|amd64) arch=amd64 ;;
  arm64|aarch64) arch=arm64 ;;
  *) printf 'unsupported smoke architecture\n' >&2; exit 1 ;;
esac

archive=$(find "$root/dist" -maxdepth 1 -type f -name "roast_*_${os}_${arch}.tar.gz" -print | head -n 1)
[ -n "$archive" ] || { printf 'snapshot archive not found for %s/%s\n' "$os" "$arch" >&2; exit 1; }
archive_name=$(basename "$archive")
version=${archive_name#roast_}
version=${version%_"${os}"_"${arch}".tar.gz}
install_root=$(mktemp -d 2>/dev/null || mktemp -d -t roast-install-smoke)
trap 'rm -rf "$install_root"' EXIT HUP INT TERM

run_installer() {
  ROAST_INSTALL_BASE_URL="file://$root/dist" \
  ROAST_INSTALL_VERSION="$version" \
  ROAST_INSTALL_ARCHIVE="$archive_name" \
  ROAST_INSTALL_DIR="$install_root/bin" \
    "$root/install.sh"
}
run_installer
run_installer
"$install_root/bin/roast" --version

bad_dist="$install_root/bad-dist"
mkdir -p "$bad_dist"
cp "$archive" "$bad_dist/$archive_name"
printf '%064d  %s\n' 0 "$archive_name" > "$bad_dist/checksums.txt"
if ROAST_INSTALL_BASE_URL="file://$bad_dist" \
  ROAST_INSTALL_VERSION="$version" \
  ROAST_INSTALL_ARCHIVE="$archive_name" \
  ROAST_INSTALL_DIR="$install_root/bad-bin" \
  "$root/install.sh" >"$install_root/bad.log" 2>&1; then
  printf 'installer accepted a checksum mismatch\n' >&2
  exit 1
fi
[ ! -e "$install_root/bad-bin/roast" ] || { printf 'checksum failure left a binary behind\n' >&2; exit 1; }
printf 'install smoke passed\n'
