#!/usr/bin/env bash
#
# Build the release artifacts and their checksums.
#
# Runnable locally, which is the point: the GitHub Actions workflow does nothing but check out the
# code and call this. A release process that only exists inside CI is one nobody can reproduce when
# CI is the thing that is broken.
#
#   ./release.sh                 # version from `git describe`
#   ./release.sh v0.3.0          # explicit
#   OUT=/tmp/rel ./release.sh    # somewhere else
#
# Output, in dist/ by default:
#
#   docpreview_v0.3.0_linux_amd64.tar.gz
#   docpreview_v0.3.0_linux_arm64.tar.gz
#   docpreview_v0.3.0_darwin_arm64.tar.gz
#   docpreview_v0.3.0_windows_amd64.zip
#   SHA256SUMS
#
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")"

VERSION="${1:-}"
if [ -z "$VERSION" ]; then
  # --dirty, so a build from an edited tree cannot be mistaken for the tag it is near. That
  # mistake is expensive precisely once: when somebody is comparing a bug against a release.
  VERSION="$(git describe --tags --always --dirty 2>/dev/null || echo dev)"
fi
COMMIT="$(git rev-parse HEAD 2>/dev/null || echo unknown)"
# Reproducible: the commit's own time, not the clock. Two builds of one commit produce identical
# binaries, which is what makes a checksum worth publishing.
DATE="$(git show -s --format=%cI HEAD 2>/dev/null || date -u +%Y-%m-%dT%H:%M:%SZ)"
OUT="${OUT:-dist}"

echo "version : $VERSION"
echo "commit  : $COMMIT"
echo "date    : $DATE"
echo "out     : $OUT"

rm -rf "$OUT"
mkdir -p "$OUT"

LDFLAGS="-s -w"
LDFLAGS="$LDFLAGS -X main.version=$VERSION"
LDFLAGS="$LDFLAGS -X main.commit=$COMMIT"
LDFLAGS="$LDFLAGS -X main.date=$DATE"

# The targets, and why these four.
#
# linux/amd64 is every cloud VM. linux/arm64 is Graviton and a Raspberry Pi, both of which people
# run this kind of thing on. darwin/arm64 is the laptop somebody tries the quickstart on. Windows
# because it is the primary development environment for this project — the daemon runs there and
# the trap it hits most (a running daemon holding the binary open) is a Windows one.
#
# No darwin/amd64 and no 32-bit anything: not asked for, and every target is one more thing to
# notice is broken.
targets="linux/amd64 linux/arm64 darwin/arm64 windows/amd64"

for target in $targets; do
  goos="${target%/*}"
  goarch="${target#*/}"
  name="docpreview_${VERSION}_${goos}_${goarch}"
  work="$OUT/$name"
  mkdir -p "$work"

  bin="docpreview"
  [ "$goos" = "windows" ] && bin="docpreview.exe"

  echo
  echo "-- $target"
  # CGO off: the sqlite driver is modernc's pure-Go one, so there is nothing to link against, and
  # a cross-compile with cgo enabled needs a toolchain per target.
  CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" \
    go build -trimpath -ldflags "$LDFLAGS" -o "$work/$bin" ./cmd/docpreview

  # What somebody needs beside the binary. The units and the installer are only useful on Linux,
  # and shipping them everywhere invites running them where they cannot work.
  cp LICENSE README.md "$work/"
  if [ "$goos" = "linux" ]; then
    mkdir -p "$work/install"
    cp install/*.service install/install.sh "$work/install/"
    chmod +x "$work/install/install.sh"
  fi

  if [ "$goos" = "windows" ]; then
    # A zip, because a Windows user double-clicks it and Explorer opens it. Three ways to make
    # one, tried in order: `zip` is on CI and on most Linux boxes, `Compress-Archive` is on every
    # Windows one, and bsdtar's `-a` infers the format from the extension. git-bash on Windows
    # has none of the first, which is where this was found.
    if command -v zip >/dev/null 2>&1; then
      (cd "$OUT" && zip -qr "$name.zip" "$name")
    elif ps_exe="$(command -v pwsh || command -v pwsh.exe || command -v powershell.exe)"; then
      # Both spellings, and pwsh first: Windows 11 boxes increasingly have PowerShell 7 and no
      # `powershell.exe` on PATH under git-bash, which is exactly the machine this is built on.
      "$ps_exe" -NoProfile -Command \
        "Compress-Archive -Path '$OUT/$name' -DestinationPath '$OUT/$name.zip' -Force" >/dev/null
    elif tar --version 2>/dev/null | grep -qi bsdtar; then
      (cd "$OUT" && tar -a -cf "$name.zip" "$name")
    else
      echo "no way to make a zip here: install zip, or build on a machine that has one" >&2
      exit 1
    fi
  else
    tar -czf "$OUT/$name.tar.gz" -C "$OUT" "$name"
  fi
  rm -rf "$work"
done

echo
echo "-- checksums"
# Bare names, so `sha256sum -c SHA256SUMS` works from wherever the files were downloaded to.
#
# The sed strips both the `./` and the `*` that sha256sum writes in binary mode — which it does on
# Windows and not on Linux, so the file's format would otherwise depend on where it was built, and
# a consumer matching on one spelling would work for exactly half of the releases.
(cd "$OUT" && sha256sum ./*.tar.gz ./*.zip | sed -e 's| \*\./| \*|' -e 's| \./| |' > SHA256SUMS)
cat "$OUT/SHA256SUMS"

echo
echo "done: $OUT"
