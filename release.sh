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

  # What somebody needs beside the binary.
  #
  # The systemd units, on Linux only — the installer reads them out of the extracted archive, so
  # they have to travel with it. **Not `install.sh` itself.** That is curled from the repository at
  # this tag, which makes it one file with one source rather than a copy in every archive that can
  # drift from the one people are told to download.
  cp LICENSE README.md "$work/"
  if [ "$goos" = "linux" ]; then
    mkdir -p "$work/install"
    cp install/*.service "$work/install/"
  fi

  if [ "$goos" = "windows" ]; then
    # A zip, because a Windows user double-clicks it and Explorer opens it. Three ways to make
    # one, tried in order: `zip` is on CI and on most Linux boxes, `Compress-Archive` is on every
    # Windows one, and bsdtar's `-a` infers the format from the extension. git-bash on Windows
    # has none of the first, so the fallback chain below tries the other two.
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

# ── The release notes ────────────────────────────────────────────────────────────────────────
#
# Written here rather than left to `gh --generate-notes`, which produces a list of pull request
# titles. For this project that reads "round 3 updates / Round 5 / round 6 pr", which tells a
# stranger nothing about what the thing is or how to install it — and the release page is the
# first thing anybody sees.
#
# The generated list still goes on the end, under a heading, where it belongs: it is a changelog,
# not an introduction.
notes="$OUT/NOTES.md"
{
  cat <<'INTRO'
Documentation previews for pull requests. A pull request arrives as a webhook, docpreview clones
it, builds the docs, publishes the output at a URL, and comments on the pull request with the
link. Self-hosted, one binary, and no inbound port — the preview reaches the internet through a
zrok tunnel.

## Install on Linux

INTRO

  cat <<INSTALL
\`\`\`bash
curl -fsSLO https://raw.githubusercontent.com/$REPO/$VERSION/install/install.sh
chmod +x install.sh
sudo ./install.sh --version $VERSION
\`\`\`

The installer downloads the right archive for this machine, **verifies it against the
\`SHA256SUMS\` below**, and refuses to install anything that does not match. It creates the
service account and the systemd units, and it starts nothing — the four remaining steps are
decisions, and it prints the command for each.

Full walkthrough, from a provisioned VM to a preview URL:
[install on a Linux VM](https://github.com/$REPO/blob/$VERSION/www/docs/runbooks/linux-service.md).

## Anywhere else

Download the archive for your platform below and put the binary on your PATH. Then:

\`\`\`
docpreview init
docpreview preview -build ./docs
\`\`\`

That publishes one directory and needs no account, no App and no webhook.

## Verifying a download

\`\`\`bash
sha256sum -c SHA256SUMS --ignore-missing
\`\`\`

## Checksums

\`\`\`
INSTALL

  cat "$OUT/SHA256SUMS"

  cat <<'OUTRO'
```

## Requirements

- **4 GB RAM** on the build host. A Docusaurus build peaks during prerendering and a 2 GB machine
  is killed there, after the install has already succeeded.
- **docker**, for the default build driver. `build.driver: local` runs the build directly instead,
  which is fine for repositories only you can push to.
- A **GitHub App** or a **Bitbucket access token**, and outbound HTTPS. Nothing inbound.

## Changelog

OUTRO
} > "$notes"

echo
echo "-- notes"
echo "   $notes"

echo
echo "done: $OUT"
