#!/bin/sh
set -eu

repo="${ROAST_INSTALL_REPO:-janiorvalle/roast}"
install_dir="${ROAST_INSTALL_DIR:-$HOME/.local/bin}"
base_url="${ROAST_INSTALL_BASE_URL:-}"
version="${ROAST_INSTALL_VERSION:-}"
archive_name="${ROAST_INSTALL_ARCHIVE:-}"

fail() {
  printf 'roast installer: %s\n' "$*" >&2
  exit 1
}

command -v curl >/dev/null 2>&1 || fail "curl is required"

case "$(uname -s)" in
  Darwin) os=darwin ;;
  Linux) os=linux ;;
  *) fail "unsupported operating system; Windows users should download the release zip" ;;
esac
case "$(uname -m)" in
  x86_64|amd64) arch=amd64 ;;
  arm64|aarch64) arch=arm64 ;;
  *) fail "unsupported architecture: $(uname -m)" ;;
esac

if [ -z "$version" ]; then
  release_json=$(curl -fsSL "https://api.github.com/repos/$repo/releases/latest") || fail "no published release found for $repo; see https://github.com/$repo/releases or use go install github.com/$repo/cmd/roast@latest"
  version=$(printf '%s\n' "$release_json" | sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"v\{0,1\}\([^"]*\)".*/\1/p' | head -n 1)
  [ -n "$version" ] || fail "latest release did not include a tag_name"
fi
version=${version#v}

if [ -z "$archive_name" ]; then
  archive_name="roast_${version}_${os}_${arch}.tar.gz"
fi
if [ -z "$base_url" ]; then
  base_url="https://github.com/$repo/releases/download/v$version"
fi
base_url=${base_url%/}

tmp_dir=$(mktemp -d 2>/dev/null || mktemp -d -t roast-install)
stage_roast=""
dest_roast="$install_dir/roast"
backup_roast="$tmp_dir/previous-roast"
had_roast=false
transaction_active=false

restore_install() {
  transaction_active=false
  if [ "$had_roast" = true ]; then
    mv -f "$backup_roast" "$dest_roast"
  else
    rm -f "$dest_roast"
  fi
}
cleanup() {
  [ "$transaction_active" = false ] || restore_install
  rm -rf "$tmp_dir"
  [ -z "$stage_roast" ] || rm -f "$stage_roast"
}
trap cleanup EXIT HUP INT TERM

archive="$tmp_dir/$archive_name"
checksums="$tmp_dir/checksums.txt"
printf 'Downloading roast %s for %s/%s...\n' "$version" "$os" "$arch"
curl -fsSL "$base_url/$archive_name" -o "$archive" || fail "could not download $archive_name"
curl -fsSL "$base_url/checksums.txt" -o "$checksums" || fail "could not download checksums.txt"

expected=$(awk -v file="$archive_name" '$2 == file || $2 == "*" file { print $1; exit }' "$checksums")
[ -n "$expected" ] || fail "checksums.txt has no entry for $archive_name"
if command -v sha256sum >/dev/null 2>&1; then
  actual=$(sha256sum "$archive" | awk '{print $1}')
elif command -v shasum >/dev/null 2>&1; then
  actual=$(shasum -a 256 "$archive" | awk '{print $1}')
else
  fail "sha256sum or shasum is required to verify the download"
fi
[ "$actual" = "$expected" ] || fail "checksum mismatch for $archive_name"

tar -xzf "$archive" -C "$tmp_dir"
[ -x "$tmp_dir/roast" ] || fail "archive did not contain roast"
"$tmp_dir/roast" --version >/dev/null || fail "release roast failed its version smoke test"

mkdir -p "$install_dir"
stage_roast="$install_dir/.roast.new.$$"
install -m 0755 "$tmp_dir/roast" "$stage_roast"
if [ -e "$dest_roast" ] || [ -L "$dest_roast" ]; then
  cp -p "$dest_roast" "$backup_roast"
  had_roast=true
fi
transaction_active=true
mv -f "$stage_roast" "$dest_roast" || fail "could not replace roast"
stage_roast=""

"$dest_roast" --version >/dev/null || fail "installed roast failed its version smoke test"
transaction_active=false
printf 'Installed roast to %s\n' "$install_dir"
case ":$PATH:" in
  *":$install_dir:"*) ;;
  *) printf 'Add %s to PATH to run roast from any directory.\n' "$install_dir" ;;
esac
